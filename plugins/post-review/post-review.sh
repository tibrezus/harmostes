#!/usr/bin/env bash
set -euo pipefail
log() { echo "[post-review] $*"; }
. "$(dirname "$0")/../lib/git-host.sh"
WORKDIR="${HARMOSTES_WORKDIR:-/workspace}"
REVIEW="$WORKDIR/review.json"
CONTEXT="$WORKDIR/pr-context.json"
SPEC="${HARMOSTES_SPEC:-}"
LABEL="needs-review"
if [ -n "$SPEC" ]; then LABEL=$(echo "$SPEC"|python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('config',{}).get('label','needs-review'))" 2>/dev/null||echo "needs-review"); fi
[ -f "$REVIEW" ]||{ echo "ERROR: review.json missing">&2;exit 1; }
[ -f "$CONTEXT" ]||{ echo "ERROR: pr-context.json missing">&2;exit 1; }
HOST=$(python3 -c "import json;print(json.load(open('$CONTEXT'))['host'])")
REPO=$(python3 -c "import json;print(json.load(open('$CONTEXT'))['repo'])")
PR_NUM=$(python3 -c "import json;print(json.load(open('$CONTEXT'))['number'])")
API_BASE=$(host::api_base "$HOST")
IS_FJ=$(host::is_fj "$HOST")
IS_GITLAB=$(host::is_gitlab "$HOST")
# post-review always authenticates (POST comment, consume label) — fail fast
# at resolve time rather than at curl time.
TOKEN=$(host::token "$HOST" required)
# Native review OBJECTS (APPROVED / REQUEST_CHANGES + inline threads) must
# post as the whitelisted bot identity (#480): the primary token usually
# belongs to the PR author, and both Forgejo and GitHub 422 self-reviews
# ("approve/reject your own pull is not allowed") — observed live on
# rhesadox#2169: twice-APPROVED green, required_approvals unsatisfiable.
# Verdict COMMENTS stay on TOKEN (authors may comment).
# EXACT-HOST-GATED (r2 review t3): IS_FJ only means "not github.com" — it
# is true for codeberg.org and ANY *) host built from the untrusted
# pr-context. The bot credential goes ONLY to the forge it was minted
# for: HOST must equal HARMOSTES_FORGEJO_BOT_HOST exactly.
if [ "${IS_FJ:-}" = "true" ] \
   && [ -n "${HARMOSTES_FORGEJO_BOT_TOKEN:-}" ] \
   && [ "${HARMOSTES_FORGEJO_BOT_HOST:-git.rezus.cloud}" = "$HOST" ]; then
  REVIEW_TOKEN="$HARMOSTES_FORGEJO_BOT_TOKEN"
else
  REVIEW_TOKEN="$TOKEN"
fi
export API_BASE TOKEN REVIEW_TOKEN HOST REPO PR_NUM REVIEW LABEL IS_FJ IS_GITLAB WORKDIR
# ── Moved-head guard (ADR-0006): the verdict is only valid at the exact ──
# reviewed SHA. If the PR head moved while the agent worked, do NOT post and
# do NOT consume the label — the synchronize event has already re-armed the
# Review-Ready Gate at the new head and a fresh review will run there.
REVIEWED_SHA=$(python3 -c "import json;print(json.load(open('$REVIEW')).get('reviewed_sha',''))" 2>/dev/null||true)
LIVE_SHA=$(curl -fsSL -H "authorization: token $TOKEN" -H "accept: application/json" \
  "$API_BASE/repos/$REPO/pulls/$PR_NUM" 2>/dev/null | python3 -c "import sys,json;print(json.load(sys.stdin).get('head',{}).get('sha',''))" 2>/dev/null||true)
if [ -n "$REVIEWED_SHA" ] && [ -n "$LIVE_SHA" ] && [ "$REVIEWED_SHA" != "$LIVE_SHA" ]; then
  log "head moved (reviewed ${REVIEWED_SHA:0:8} → live ${LIVE_SHA:0:8}) — NOT posting; gate re-arms at the new head"
  echo "{\"status\":\"ok\",\"event\":{\"host\":\"$HOST\",\"repo\":\"$REPO\",\"number\":$PR_NUM,\"skipped\":\"head_moved\",\"live_sha\":\"$LIVE_SHA\"}}"
  exit 0
fi

log "posting review to $HOST/$REPO#$PR_NUM…"
case "$API_BASE" in
  "https://api.github.com"|"https://codeberg.org/api/v1"|"https://git.rezus.cloud/api/v1") ;;
  *) log "WARN: non-canonical API base in use ($API_BASE) — a test seam or a misconfiguration is redirecting forge traffic";;
esac
DEC=$(python3 -c "import json;print(json.load(open('$REVIEW'))['decision'])")

# ── Inline findings are published HERE, deterministically (#429): the
# agent's findings ride review.json's comments[] and this plugin posts them
# as native anchored threads below. The agent must NOT self-post (the task
# prompt says so; the dedupe guard in the publisher is the second line of
# defense for a skill-following agent). This plugin owns: the VERDICT
# comment, the inline-thread PUBLISH, and the unresolved-thread GATE. ──
# ── Unresolved-thread gate (the merge currency, mechanically enforced) ──
# Threads from PRIOR rounds (commit_id != this reviewed_sha) with NO reply
# must be addressed before an APPROVE is lawful. Any open prior thread
# downgrades APPROVE → REQUEST_CHANGES; the reply+resolve protocol lives
# in the pr-review skill.
GATE_STATUS="not-evaluated"  # non-APPROVE verdicts skip the gate entirely
if [ "$DEC" = "APPROVE" ]; then
export REVIEWED_SHA="$(python3 -c "import json;print(json.load(open('$REVIEW'))['reviewed_sha'])")"
# ── Thread listing is HOST-DIALECT (S2/r26, r28): the hosts serve review
# comments from different routes with different thread semantics.
#   GitHub : GET /repos/{r}/pulls/{n}/comments — in_reply_to links replies.
#   Forgejo: GET /repos/{r}/pulls/{n}/reviews → per-review comments
#            (the flat /pulls/{n}/comments route 404s — verified live on
#            git.rezus.cloud v16.0.3-rezus.1); replies also use in_reply_to.
#   GitLab : DESCOPED (r28) — the dialect is not wired (no token chain, no
#            glab in the image); gitlab.com hosts emit a structured skip.
#            The classifier's GitLab branch ships tested for the follow-up.
GATE_STATUS_FILE="$(mktemp)"; : > "$GATE_STATUS_FILE"; export GATE_STATUS_FILE
CS_JSON=$(python3 - << 'PYEOF'
# Emits the unified comment list on stdout; each comment carries a boolean
# "resolved": GitHub = GraphQL reviewThreads.isResolved mapped from
# databaseId (REST never shows a GraphQL-side resolve, C1); Forgejo = the
# fork's NATIVE resolver state (a resolved review comment serializes the
# resolve_doer object as `resolver`; absent/null = unresolved â closing
# replies cannot be expressed through this REST shape, the fork's
# in_reply_to is a rezuscloud/forgejo follow-up, so native resolve is the
# only closable path and must be authoritative); GitLab = native flag.
# On a listing
# failure or an unwired dialect it emits `null` and writes the structured
# skip reason to $GATE_STATUS_FILE (C3: a skip must be alarmable, never
# silent) — the shell puts it in the run's event JSON.
import json, os, urllib.request
base=os.environ["API_BASE"]; tok=os.environ["TOKEN"]
repo=os.environ["REPO"]; pr=os.environ["PR_NUM"]
H={"authorization": f"token {tok}"}
def get(path):
    req=urllib.request.Request(base+path, headers=H)
    return json.load(urllib.request.urlopen(req, timeout=30))
def paged(path):
    # per_page=100&page=N until a short page (C4: a page-1-only listing
    # silently hid every thread past 30). CUMULATIVE budget (r6 P1): five
    # sequential 30s node timeouts SIGKILL the deploy before the artifact
    # line — the budget fires first and fail() names it. A cap exit marks
    # the truncation in the gate status (r6 P1: "no open threads" and
    # "we stopped looking" must be distinguishable).
    import time as _t
    sep = "&" if "?" in path else "?"
    t0=_t.time()
    out, page = [], 1
    while page <= 5:
        if _t.time()-t0 > 40:
            fail("scan budget exceeded — listing truncated")
        part = get(f"{path}{sep}per_page=100&page={page}")
        out += part
        if len(part) < 100:
            return out
        page += 1
    with open(os.environ["GATE_STATUS_FILE"], "a") as f: f.write(",truncated:true")
    return out
def fail(why):
    import sys as _s
    print(f"[post-review] WARN: thread gate skipped ({why})", file=_s.stderr)
    with open(os.environ["GATE_STATUS_FILE"], "w") as f: f.write("skipped:"+why)
    print(json.dumps(None))
    raise SystemExit
try:
    if os.environ.get("IS_GITLAB")=="true":
        # GitLab dialect not wired yet (token chain + glab land with the
        # credential follow-up) — descoped from this PR, structured skip.
        fail("gitlab-not-wired")
    if os.environ.get("IS_FJ")=="true":
        cs=[]
        for r in paged(f"/repos/{repo}/pulls/{pr}/reviews"):
            cs+=paged(f"/repos/{repo}/pulls/{pr}/reviews/{r['id']}/comments")
        for c in cs: c["resolved"]=bool(c.get("resolver"))  # fork-native resolve_doer object; absent = open (in_reply_to is the fork gap)
        print(json.dumps(cs)); raise SystemExit
    cs=paged(f"/repos/{repo}/pulls/{pr}/comments")
    owner, name = repo.split("/", 1)
    resolved=set()
    after=""
    for _ in range(10):  # pageInfo-paginated (r29 P4-2): >100 threads truncate silently otherwise
        q={"query":'{ repository(owner: "%s", name: "%s") { pullRequest(number: %s) { reviewThreads(first: 100%s) { pageInfo { hasNextPage endCursor } nodes { isResolved comments(first: 100) { nodes { databaseId } } } } } } }' % (owner, name, pr, (', after: "%s"' % after) if after else "")}
        req=urllib.request.Request(base+"/graphql", data=json.dumps(q).encode(),
            headers={"authorization": f"bearer {tok}", "content-type": "application/json"})
        rt=(json.load(urllib.request.urlopen(req, timeout=30))
            .get("data",{}).get("repository",{}).get("pullRequest",{})
            .get("reviewThreads",{}) or {})
        for t in (rt.get("nodes") or []):
            if t.get("isResolved"):
                for cm in (t.get("comments",{}).get("nodes") or []):
                    if cm.get("databaseId") is not None: resolved.add(cm["databaseId"])
        if rt.get("pageInfo",{}).get("hasNextPage"):
            after = rt["pageInfo"]["endCursor"]
        else:
            break
    for c in cs: c["resolved"]=c["id"] in resolved
    print(json.dumps(cs))
except SystemExit:
    raise
except Exception as e:
    fail(f"{type(e).__name__}: {e}"[:120])
PYEOF
)
GATE_STATUS="evaluated"; [ -s "$GATE_STATUS_FILE" ] && GATE_STATUS="$(cat "$GATE_STATUS_FILE")"
# A degraded gate must never post a verdict it could not evaluate (r6 P1):
# on an APPROVE with a failed/truncated listing, skip the verdict comment
# AND the label removal — emit the structured skip and let the next cycle
# retry with a healthy forge. (Non-APPROVE verdicts are unaffected: the
# listing cannot downgrade what is already not an approval.)
if [ "$DEC" = "APPROVE" ] && [ "$GATE_STATUS" != "evaluated" ]; then
  log "thread gate unavailable ($GATE_STATUS) on an APPROVE — skipping verdict post and label removal; next cycle retries"
  echo "{\"artifact\":\"pr-$HOST-$REPO-$PR_NUM\",\"status\":\"ok\",\"event\":{\"host\":\"$HOST\",\"repo\":\"$REPO\",\"number\":$PR_NUM,\"decision\":\"$DEC\",\"thread_gate\":\"$GATE_STATUS\",\"skipped\":\"gate-unavailable\"}}"
  exit 0
fi
rm -f "$GATE_STATUS_FILE"
# The classifier program lives verbatim between the GATE-CLASSIFIER markers
# (the golden test extracts exactly these bytes). It is held in a QUOTED
# heredoc so bash never parses it; do not unquote.
CLASSIFIER_PY=$(cat << 'GATE_PYEOF'
# GATE-CLASSIFIER-START (tested verbatim by TestPostReviewGateClassifier —
# everything between the markers must be a self-contained script; the
# invocation below extracts exactly this block via sed and feeds the
# comments JSON on stdin — do NOT move the program into a heredoc: the
# heredoc clobbers the pipe's stdin, r29 P4-1).
import json, os, sys
cs=json.load(sys.stdin)
sha=os.environ.get("REVIEWED_SHA","")
open_n=0
if not isinstance(cs, list):
    print(0); print("APPROVE"); raise SystemExit   # fetch failed — fail-open with a loud WARN upstream
dec="APPROVE"
# Degraded-read posture (r7 review of 90a24e63): a shape this program
# cannot classify must DEGRADE (treat as unresolvable-but-not-open), never
# KeyError — the classifier runs under `set -e` inside the APPROVE branch,
# and a crash there aborts the deploy before the verdict + label DELETE
# (the exact lost-round signature this plugin exists to prevent). Anything
# whose dialect/identity we cannot read is counted as CLOSED-BY-DEFAULT
# here: a false APPROVE re-reviews next round, a crash wedges the head.
if cs and isinstance(cs[0], dict) and "resolvable" in cs[0]:
    # GitLab dialect: native resolve is authoritative.
    open_threads=[c for c in cs
        if isinstance(c, dict)
        and c.get("resolvable") and not c.get("resolved")
        and str(c.get("commit_id") or "")!=sha]
else:
    # GitHub/Forgejo dialect: a thread is CLOSED by real resolve state
    # (C1 — a GraphQL-side resolve leaves no reply) or by a reply.
    # Reply linkage is host-dialectal: GitHub REST names it
    # in_reply_to_id, Forgejo in_reply_to — read both (the live review
    # loop of #467 tripped this: replies never closed threads on GitHub,
    # so every APPROVE downgraded on phantom open threads).
    def reply_parent(c):
        return c.get("in_reply_to_id") or c.get("in_reply_to")
    replied={reply_parent(c) for c in cs if isinstance(c, dict) and reply_parent(c)}
    # Fork-gap addressal marker (#572): on Forgejo the REST create-review
    # API cannot SET in_reply_to (a rezuscloud/forgejo follow-up) and the
    # fork-native resolver (resolve_doer) is only settable through the
    # web UI — so the documented author/reviewer protocol (the pr-review
    # skill) addresses a thread with a follow-up review comment at the
    # SAME anchor whose body references the original comment id
    # (`path:line (comment N) — fix SHA + rationale`). Without parsing
    # that marker, a REST-only review round can never close a prior-round
    # thread: every round downgrades until a human uses the browser (the
    # rhesadox#2359 burn — five consecutive downgrades over a fixed
    # diff; the protocol-shaped replies themselves added unresolvable
    # flat threads to the count). Closure requirements, all deliberate:
    #   same anchor  — path + (line, else position): a marker for another
    #                  conversation never closes this one
    #   later        — created_at strictly after the thread's. Both
    #                  sides must be Z-suffixed RFC3339 (both hosts emit
    #                  Z today): a +hh:mm offset sorts lexically by
    #                  wall-clock text and can read "later" while being
    #                  earlier (#573 review round 1). Non-Z entries never
    #                  close via the marker — conservative, same posture
    #                  as the missing-timestamp case.
    #   id in body   — the thread's id as a BOUNDED token anywhere in the
    #                  body (`(?<!\d)N(?!\d)`): no substring matching
    #                  ('4026' must not match inside '40268' — dense ids
    #                  at one anchor are the collision domain, #573
    #                  round 1) and not limited to a parenthesized lead
    #                  (round 3: 'path:line — fixed at abc (see comment
    #                  N)' is protocol-conformant and must close). An id
    #                  is REQUIRED — an id-less reply cannot be attributed
    #                  to one thread at a shared anchor.
    #   leads path:line — the body must OPEN with the anchor's own
    #                  `path:line` enumeration lead (markdown emphasis /
    #                  bullet prefixes tolerated).
    #   other author  — the marker's author must DIFFER from the thread's
    #                  (user.login): an addressal is written by the party
    #                  being reviewed (the PR author), a finding by the
    #                  reviewer. #573 round 4: the reviewer itself emits
    #                  path:line-leading re-findings at an unresolved
    #                  anchor ('w.yml:433 — still wrong, see 40268' — raw
    #                  bodies on Forgejo, no marker prefix) which under a
    #                  lead-only grammar closed the older thread AND
    #                  erased themselves — a false APPROVE through the
    #                  merge currency. Identity restores the round-2
    #                  invariant (closure evidence never deletes
    #                  itself): a same-author comment — sibling findings,
    #                  reviewer cross-references — closes nothing; a
    #                  missing user on either side reads as same-author
    #                  (conservative). The reviewer's own verification
    #                  still cannot close mechanically — the dev's reply
    #                  is mandatory per the protocol, and THAT closes.
    import re as _re
    _lead_form=_re.compile(r'^\s*[*_>\-\s]*[\w./+-]+:\d+\b')
    def _bounded(tid):
        return _re.compile(r'(?<!\d)'+_re.escape(str(tid))+r'(?!\d)')
    def anchor_key(c):
        return (c.get("path"), c.get("line") if c.get("line") is not None else c.get("position"))
    def author_of(c):
        u=c.get("user")
        return (u.get("login") if isinstance(u, dict) else None) or ""
    addressed=set()   # thread ids a marker closed
    marker_ids=set()  # the marker replies that closed them (a reply is not
                      # itself a finding thread — without this, the flat
                      # model counts every protocol reply as ANOTHER open
                      # thread: the rhesadox#2359 count grew 1→2→3 as the
                      # author followed the documented protocol)
    for r in cs:
        if not isinstance(r, dict) or r.get("id") is None:
            continue
        body=str(r.get("body") or "")
        if not _lead_form.match(body):
            continue
        for t in cs:
            if (isinstance(t, dict) and t.get("id") is not None
                and str(t.get("id"))!=str(r.get("id"))
                and author_of(r)!=author_of(t)
                and _bounded(t.get("id")).search(body)
                and anchor_key(t)==anchor_key(r)
                and str(r.get("created_at") or "").endswith("Z")
                and str(t.get("created_at") or "").endswith("Z")
                and str(r.get("created_at"))>str(t.get("created_at"))):
                addressed.add(t.get("id")); marker_ids.add(r.get("id"))
    open_threads=[c for c in cs
        if isinstance(c, dict)
        and not reply_parent(c)
        and c.get("id") not in replied
        and c.get("id") not in addressed
        and c.get("id") not in marker_ids
        and not c.get("resolved")
        # No commit_id → round unattributable: never downgrade on it (C4).
        and c.get("commit_id") not in (None, "")
        and str(c.get("commit_id"))!=sha]
open_n=len(open_threads)
if open_threads:
    dec="REQUEST_CHANGES"
    for c in open_threads[:10]:
        print(f"[post-review] unresolved thread {c.get('path','?')}:{c.get('line','?')} id={c.get('id')} — reply+resolve required before APPROVE", file=sys.stderr)
# stdout: count line FIRST, decision LAST — the shell and the golden test
# read the decision as the last line; the count feeds the downgrade verdict
# line (DOWNGRADED path) so the two never drift into separate
# implementations.
print(open_n)
print(dec)
# GATE-CLASSIFIER-END
GATE_PYEOF
)

# stdout of the classifier: count line first, decision last (the golden
# test reads the decision as the last line — keep that invariant). If the
# classifier itself dies, the gate was NOT evaluated: apply the r6 P1
# posture (skip the verdict + label removal, next cycle retries) instead
# of posting a verdict from an unknown decision.
CLASS_OUT=$(printf '%s' "$CS_JSON" | python3 -c "$CLASSIFIER_PY") || {
  log "WARN: gate classifier failed — gate not evaluated; skipping verdict post and label removal; next cycle retries"
  echo "{\"artifact\":\"pr-$HOST-$REPO-$PR_NUM\",\"status\":\"ok\",\"event\":{\"host\":\"$HOST\",\"repo\":\"$REPO\",\"number\":$PR_NUM,\"decision\":\"$DEC\",\"thread_gate\":\"classifier-crash\",\"skipped\":\"gate-unavailable\"}}"
  exit 0
}
NEWDEC=$(printf '%s\n' "$CLASS_OUT" | tail -n 1)
OPEN_THREADS=$(printf '%s\n' "$CLASS_OUT" | head -n 1)
if [ -z "$NEWDEC" ] || [ "$NEWDEC" != "APPROVE" ] && [ "$NEWDEC" != "REQUEST_CHANGES" ]; then
  log "WARN: gate classifier produced no decision — gate not evaluated; skipping verdict post and label removal; next cycle retries"
  echo "{\"artifact\":\"pr-$HOST-$REPO-$PR_NUM\",\"status\":\"ok\",\"event\":{\"host\":\"$HOST\",\"repo\":\"$REPO\",\"number\":$PR_NUM,\"decision\":\"$DEC\",\"thread_gate\":\"classifier-crash\",\"skipped\":\"gate-unavailable\"}}"
  exit 0
fi

if [ "$NEWDEC" != "$DEC" ]; then
  log "APPROVE downgraded: $OPEN_THREADS unresolved prior-round thread(s)"
  DEC="$NEWDEC"
  DOWNGRADED=1
  export DOWNGRADED OPEN_THREADS   # the verdict builder emits the downgrade line
  # S1 (r26): the downgrade rewrites the whole verdict — the decision field
  # here AND the trailer of the posted verdict comment below (rebuilt from
  # decision + SHA, so the downgrade is visible to dw_wait_review).
  # r7: review.json has NO body field — the reviewer no longer writes one,
  # so the downgrade marker rides the decision field + the comment note
  # (DOWNGRADED), not a body rewrite. verdictTrailer in internal/review/review.go
  # is the contract's canonical home — keep both in step.
  python3 - "$REVIEW" "$DEC" << 'PYEOF'
import json,sys
p,newdec=sys.argv[1],sys.argv[2]; r=json.load(open(p))
r["decision"]=newdec
json.dump(r, open(p,"w"))
PYEOF
fi
fi

# The verdict as a ONE-LINE comment, BUILT here from decision + SHA +
# blocking count (r7, owner directive): the review's posted output is
# blocking threads + this line. review.json's analysis prose is never
# posted — there is no pillar-structured body, no findings summary; the
# threads ARE the findings record. The trailer inside is the merge
# currency the gate polls for.
VERDICT_COMMENT=$(python3 - << 'PYEOF'
import json, os
with open(os.environ["REVIEW"]) as f: review=json.load(f)
dec=review["decision"]; sha=review.get("reviewed_sha","")
n=len(review.get("comments",[]) or [])
if os.environ.get("DOWNGRADED"):
    # Deploy-composed downgrade: NOT the reviewer's verdict — the ≥1-finding
    # rule (pr-review.sh validation) governs review.json, not this line. It
    # must stay self-describing: the downgrade fires on PRIOR-round threads,
    # so "N blocking findings" would be false here.
    line=(f"{dec} at {sha} — downgraded: {os.environ.get('OPEN_THREADS','0')} unresolved "
          "prior-round thread(s); reply with the fix SHA, resolve, then re-arm.")
elif dec=="APPROVE":
    line=f"APPROVE at {sha} — all pillars clean, no blocking findings. Label consumed; re-arm with the label to review again."
    todos=review.get("todos",[]) or []
    if todos:
        # TODO lane (r7-r10): the approval stands; the anchored TODO threads
        # still owe the dev a follow-up — say so on the record line.
        line=f"APPROVE at {sha} — all pillars clean, no blocking findings; {len(todos)} TODO thread(s) anchored to address. Label consumed; re-arm with the label to review again."
else:
    plural="finding" if n==1 else "findings"
    line=f"{dec} at {sha} — {n} blocking {plural} posted as review threads; close them, then re-arm with the label to re-review."
print(json.dumps({"body": line+"\n\n<!-- pr-review: "+dec+" @ "+sha+" -->"}))
PYEOF
)
curl -fsSL -X POST -H "authorization: token $TOKEN" -H "content-type: application/json" \
  "$API_BASE/repos/$REPO/issues/$PR_NUM/comments" -d "$VERDICT_COMMENT" >/dev/null \
  || { echo "ERROR: issue comment rejected">&2; exit 1; }
log "verdict comment posted to $REPO#$PR_NUM ($DEC)"

# ── Native inline threads (deterministic, #429): the agent never reliably
# self-posts (attempt 90f9fd63ad9a: 81 bash tools, zero review-api calls —
# the task's review.json contract and the round-trip budget both push it to
# prose), so the DEPLOY step publishes review.json's comments as real
# anchored threads. NON-FATAL by construction (#430 r1 P5): the verdict is
# already posted above — a thread failure must never skip the consume step
# (label removal) or the artifact JSON. One call per finding on GitHub (a
# line the host rejects skips that finding, not the batch); Forgejo batches
# the create-pull-review and falls back per finding on rejection (r2 P7).
# The artifact always carries "inline_threads" with one unified key set —
# zero findings, skips and scan failures all speak (r3 P3/P5/P9).
THREAD_STATUS_FILE="$(mktemp)"
DEDUPE_FLAG_FILE="$(mktemp)"; export DEDUPE_FLAG_FILE
echo '{"posted":0,"rejected":0,"capped":0}' > "$THREAD_STATUS_FILE"; export THREAD_STATUS_FILE
# Single source for the dedupe key (r3 P2): dedupe scan and publisher must
# agree on what "our thread" looks like — two literals here is how the
# guard silently stops recognising its own posts.
SHA8="$(echo "${REVIEWED_SHA:-}" | cut -c1-8)"; export SHA8
MARKER="automated review of $SHA8"; export MARKER
# The verdict's native review state (#470): the thread gate has already
# passed for an APPROVE, so its review carries zero unresolved threads and
# submits as the host's APPROVAL event — the whitelisted bot identity then
# SATISFIES required_approvals=1 (rhesadox main) and the merge unblocks
# without a human re-click. REQUEST_CHANGES submits the host's rejection
# event: block_on_rejected_reviews holds the PR until re-review clears it,
# and dismiss_stale_approvals (branch protection) keeps the approval bound
# to the reviewed head. Events are REQUEST vocabularies — the response
# states (APPROVED / CHANGES_REQUESTED) are 422 bait (#470 r1 t1).
if [ "${IS_FJ:-}" = "true" ]; then
  # Forgejo CreatePullReview event enum: APPROVED | PENDING | COMMENT | REQUEST_CHANGES
  REVIEW_EVENT="REQUEST_CHANGES"; [ "$DEC" = "APPROVE" ] && REVIEW_EVENT="APPROVED"
else
  # GitHub CreateReview event enum: APPROVE | REQUEST_CHANGES | COMMENT
  REVIEW_EVENT="REQUEST_CHANGES"; [ "$DEC" = "APPROVE" ] && REVIEW_EVENT="APPROVE"
fi
export REVIEW_EVENT
# The threads anchor at reviewed_sha: an absent/malformed SHA cannot anchor
# (r3 P5 — trailers allow 7-40 hex, so validate, never assume full length).
if echo "${REVIEWED_SHA:-}" | grep -qE '^[0-9a-f]{7,40}$'; then
  THREADS_ANCHOR=1
else
  THREADS_ANCHOR=0
  log "WARN: reviewed_sha missing/malformed — threads skipped (verdict stands)"
  echo '{"posted":0,"rejected":0,"capped":0,"skipped":"sha-invalid"}' > "$THREAD_STATUS_FILE"
fi

# Dedupe scan, hoisted (#470 r2): BOTH consumers — the thread publisher and
# the zero-findings native approval — must agree on "already posted at this
# SHA", or a re-run double-posts. Runs whenever the SHA can anchor.
EXISTING=0
if [ "$THREADS_ANCHOR" = "1" ]; then
  EXISTING=$(python3 - << 'PYDEDUP'
import json, os, subprocess, sys
base=os.environ["API_BASE"]; tok=os.environ["TOKEN"]
repo=os.environ["REPO"]; pr=os.environ["PR_NUM"]
sha=json.load(open(os.environ["REVIEW"])).get("reviewed_sha","")
marker=os.environ["MARKER"]  # built from the shell-normalised SHA8 — one fact, one home
def get(path):
    # single sep logic — the r4 P5c bug rebuilt the query with a second "?",
    # 404ing page 2 exactly on the >100-comment PRs the guard protects
    sep = "&" if "?" in path else "?"
    out, page = [], 1
    while page <= 5:  # hard page cap (r4 P5): a >100-comment PR must not wedge the deploy
        r=subprocess.run(["curl","-fsS","--max-time","20","-H",f"authorization: token {tok}",
            "-H","accept: application/json", f"{base}{path}{sep}page={page}"],
            capture_output=True,text=True)
        if r.returncode!=0: raise RuntimeError(f"{path}: {r.stderr.strip()[:80]}")
        cs=json.loads(r.stdout) or []
        out+=cs
        if len(cs)<100: return out
        page+=1
    return out
    return out
try:
    found=0
    if os.environ.get("IS_FJ")=="true":
        for r in get(f"/repos/{repo}/pulls/{pr}/reviews"):
            if str(r.get("commit_id") or "").startswith(sha) and marker in (r.get("body") or ""):
                found+=1; break
    else:
        for c in get(f"/repos/{repo}/pulls/{pr}/comments?per_page=100"):
            if str(c.get("commit_id") or "").startswith(sha) and marker in (c.get("body") or ""):
                found+=1; break
    print(found)
except Exception as e:
    # Fail open, but SPEAK on the failure path itself (r4 P5a/b): the flag
    # lands in the artifact wherever the scan died.
    with open(os.environ["DEDUPE_FLAG_FILE"],"w") as f: f.write(',"dedupe":"scan-failed"')
    print(f"[post-review] WARN: dedupe scan failed ({e}) — publishing anyway, flagged in artifact", file=sys.stderr)
    print(0)
PYDEDUP
  ) || EXISTING=0
fi

if [ "${IS_GITLAB:-}" = "true" ]; then
  # Unwired dialect — it must SPEAK (r4 P8): a green artifact here is
  # ambiguous between "nothing to post" and "this host cannot be posted to".
  echo '{"posted":0,"rejected":0,"capped":0,"skipped":"gitlab-not-wired"}' > "$THREAD_STATUS_FILE"
elif [ "$THREADS_ANCHOR" = "1" ] && python3 -c "import json,sys;cs=json.load(open('$REVIEW')).get('comments',[]);sys.exit(0 if cs else 1)" 2>/dev/null; then
  # Dedupe guard keyed on the MARKER (r2): already posted at this SHA → skip.
  if [ "${EXISTING:-0}" != "0" ]; then
    log "inline threads already posted at ${REVIEWED_SHA:0:8} — not duplicating"
    echo '{"posted":0,"rejected":0,"capped":0,"skipped":"already-posted"}' > "$THREAD_STATUS_FILE"
  else
    python3 - << 'PYTHREADS' || { echo '{"posted":0,"rejected":0,"capped":0,"skipped":"publisher-crashed"}' > "$THREAD_STATUS_FILE"; } && log "WARN: inline thread publish failed — verdict stands, threads skipped"
import json, os, subprocess, sys
review=json.load(open(os.environ["REVIEW"]))
all_cs=review.get("comments",[])
all_todos=review.get("todos",[])
# TODO lane (r7-r10): completely-missing pieces anchor as NON-blocking
# threads after the findings, sharing the 20-thread cap. They never count
# in the verdict's blocking number; teeth come from the classifier — an
# unresolved TODO thread downgrades the NEXT round's APPROVE.
TODO_PREFIX="TODO (non-blocking — reply with the follow-up, then resolve): "
todo_bodies=[TODO_PREFIX+str(t.get("body","")) for t in all_todos
             if isinstance(t,dict) and t.get("path") and t.get("body")]
merged=all_cs[:20]+[{**t,"body":b} for t,b in zip(
    [t for t in all_todos if isinstance(t,dict) and t.get("path") and t.get("body")][:max(0,20-len(all_cs[:20]))],
    todo_bodies[:max(0,20-len(all_cs[:20]))])]
cs=merged
base=os.environ["API_BASE"]
# Native reviews post as the bot when provisioned (#480) — see REVIEW_TOKEN above.
tok=os.environ.get("REVIEW_TOKEN") or os.environ["TOKEN"]
repo=os.environ["REPO"]; pr=os.environ["PR_NUM"]; sha=review.get("reviewed_sha","")
fj = os.environ.get("IS_FJ")=="true"
marker=os.environ["MARKER"]
posted=0; rejected=0; last_error=""
def curl(path, payload):
    # No -f: with -f the response body never reaches stdout and the WARN
    # drops the host's actual reason (r2 P8). Status parsed manually.
    r=subprocess.run(["curl","-sS","--max-time","20","-X","POST",
        "-w","\n%{http_code}","-H",f"authorization: token {tok}",
        "-H","content-type: application/json",
        base+path,"-d",json.dumps(payload)], capture_output=True,text=True)
    out=r.stdout
    code=out.rsplit("\n",1)[-1].strip()
    body=out[:out.rfind("\n")] if "\n" in out else ""
    ok = r.returncode==0 and code.startswith("2")
    return ok, (body or r.stderr).strip()[:120]
valid=[]
for c in cs:
    path=c.get("path"); body=c.get("body")
    if not path or not body:
        rejected+=1   # a malformed finding skips itself, never the batch (r1 P5)
        print("[post-review] WARN: malformed finding (missing path/body) — skipped", file=sys.stderr)
        continue
    try:
        line=int(str(c.get("line")))
    except (ValueError, TypeError):
        # r4 P5d: line is optional by the review.json contract but the
        # threads anchor to a line — a finding without one is carried by
        # the verdict body, never silently pinned to line 1.
        rejected+=1
        print(f"[post-review] WARN: finding {path} has no usable line — carried by the verdict body", file=sys.stderr)
        continue
    valid.append((path, line, c.get("side","RIGHT"), body))
if fj:
    # Forgejo accepts a comments ARRAY in one create-pull-review — batch
    # first (one review object on the UI), fall back per finding on
    # rejection so one bad line cannot kill the batch (r2 P7).
    payload={"event":os.environ["REVIEW_EVENT"],"commit_id":sha,"body":marker,
             "comments":[{"path":p,"new_position":l,"body":b} for p,l,_,b in valid]}
    ok, reason = curl(f"/repos/{repo}/pulls/{pr}/reviews", payload)
    if ok:
        posted += len(valid)
    else:
        last_error = reason  # r5 t12: the artifact must carry the host's reason (the #480 symptom was an unattributable rejection)
        print(f"[post-review] WARN: batch publish rejected ({reason[:120]}) — falling back per finding", file=sys.stderr)
        for p,l,_,b in valid:
            ok2, reason2 = curl(f"/repos/{repo}/pulls/{pr}/reviews", {"event":os.environ["REVIEW_EVENT"],"commit_id":sha,"body":marker,
                                     "comments":[{"path":p,"new_position":l,"body":b}]})
            if ok2: posted+=1
            else:
                rejected+=1
                if not last_error:
                    last_error = reason2
                print(f"[post-review] WARN: inline thread {p}:{l} rejected — {reason2}", file=sys.stderr)
else:
    for p,l,side,b in valid:
        ok, reason = curl(f"/repos/{repo}/pulls/{pr}/comments", {"commit_id":sha,"path":p,"line":l,"side":side,
                 "body":"_"+marker+"_"+chr(10)+chr(10)+b})
        if ok: posted+=1
        else:
            rejected+=1
            if not last_error:
                last_error = reason
            print(f"[post-review] WARN: inline thread {p}:{l} rejected — {reason}", file=sys.stderr)
dropped=[c.get("path","?") for c in all_cs[len(cs):]]
if dropped:
    print(f"[post-review] capped: {len(dropped)} findings anchor only in the verdict body: {', '.join(dropped)}", file=sys.stderr)
summary={"posted":posted,"rejected":rejected,"capped":max(0,len(all_cs)+len(all_todos)-len(cs)),"todos":len(todo_bodies)}
if last_error:
    summary["last_error"]=last_error[:60]
with open(os.environ["THREAD_STATUS_FILE"],"w") as f:
    json.dump(summary, f)
PYTHREADS
  fi
elif [ "${IS_FJ:-}" = "true" ] && [ "$THREADS_ANCHOR" = "1" ] && [ "${EXISTING:-0}" = "0" ] && [ "$REVIEW_EVENT" = "APPROVED" ]; then
  # APPROVE with zero blocking findings, Forgejo only (#470): the thread
  # publisher above never runs (nothing to anchor), so the native APPROVED
  # review — the whitelisted bot identity satisfying required_approvals —
  # is posted here. TODOs (r7-r10) ride the same review as anchored
  # comments; body carries the same MARKER the dedupe scan recognises; the
  # hoisted EXISTING scan guarantees no double-post.
  APPROVAL_JSON="$(mktemp)"; export APPROVAL_JSON
  python3 - "$REVIEW" > "$APPROVAL_JSON" << 'PYTODO'
import json, os, sys
r=json.load(open(sys.argv[1]))
P="TODO (non-blocking — reply with the follow-up, then resolve): "
cs=[{"path":t["path"],"new_position":int(t["line"]),"body":P+str(t["body"])}
    for t in (r.get("todos") or []) if isinstance(t,dict) and t.get("path") and t.get("line")]
json.dump({"event":"APPROVED","commit_id":r.get("reviewed_sha",""),
           "body":os.environ["MARKER"],"comments":cs}, sys.stdout)
PYTODO
  APPROVAL_RESP_FILE="$(mktemp)"
  # `|| true`: under set -euo pipefail a plain assignment propagates the
  # substitution's exit status — a curl TRANSPORT failure (timeout, DNS,
  # reset; distinct from HTTP 4xx/5xx which exit 0 without -f) would abort
  # the whole plugin before label-consume + artifact (r1 review t2). A
  # transport failure yields APPROVAL_CODE=000 → the * branch speaks.
  # stderr lands in the response file: with -sS transport errors (timeout,
  # DNS, reset — the cases that motivated || true) report ONLY on stderr and
  # write no body, so without this the WARN degrades to http 000 with no
  # cause (r14 t22 — the #480 unattributable-rejection symptom, one layer
  # down). HTTP responses carry a body and empty stderr: unambiguous.
  APPROVAL_CODE="$(curl -sS --max-time 20 -o "$APPROVAL_RESP_FILE" -w "%{http_code}" -X POST \
    -H "authorization: token $REVIEW_TOKEN" -H "content-type: application/json" \
    -d @"$APPROVAL_JSON" \
    "$API_BASE/repos/$REPO/pulls/$PR_NUM/reviews" 2>>"$APPROVAL_RESP_FILE" || true)"
  case "$APPROVAL_CODE" in
    200|201) echo '{"posted":1,"rejected":0,"capped":0}' > "$THREAD_STATUS_FILE" ;;
    000)
      APPROVAL_BODY="$(head -c 120 "$APPROVAL_RESP_FILE" 2>/dev/null || true)"
      echo '{"posted":0,"rejected":1,"capped":0,"skipped":"approval-transport-failed"}' > "$THREAD_STATUS_FILE"
      log "WARN: native approval transport failure (no HTTP response) — $APPROVAL_BODY"
      ;;
    *)
      # SPEAK (r4-P8): the artifact must distinguish "credential missing, host
      # rejected a self-review" (#480 — bot token not provisioned) from any
      # other rejection; the response body lands in the deploy log either way.
      APPROVAL_BODY="$(head -c 120 "$APPROVAL_RESP_FILE" 2>/dev/null || true)"
      case "$APPROVAL_BODY" in
        *"own pull"*) APPROVAL_SKIP="self-review-forbidden" ;;
        *) APPROVAL_SKIP="approval-post-failed" ;;
      esac
      echo "{\"posted\":0,\"rejected\":1,\"capped\":0,\"skipped\":\"$APPROVAL_SKIP\"}" > "$THREAD_STATUS_FILE"
      log "WARN: native approval rejected (http ${APPROVAL_CODE:-000}) — $APPROVAL_BODY"
      ;;
  esac
  rm -f "$APPROVAL_JSON" "$APPROVAL_RESP_FILE"
fi
THREADS=$(cat "$THREAD_STATUS_FILE")
# The scan-failed flag (written by the dedupe scan on failure) splices into
# WHATEVER shape the file carries — publish, skip, or default (r4 P8
# blocker: reading+rm'ing the flag inside a branch made the splice dead).
if [ -s "$DEDUPE_FLAG_FILE" ]; then
  FLAG="$(cat "$DEDUPE_FLAG_FILE")"
  THREADS="${THREADS%\}}${FLAG}}"
fi
rm -f "$THREAD_STATUS_FILE" "$DEDUPE_FLAG_FILE"

log "removing label '$LABEL'…"
curl -fsSL -X DELETE -H "authorization: token $TOKEN" -H "accept: application/json" \
  "$API_BASE/repos/$REPO/issues/$PR_NUM/labels/$LABEL" 2>/dev/null||log "WARN: could not remove label"
echo "{\"artifact\":\"pr-$HOST-$REPO-$PR_NUM\",\"status\":\"ok\",\"event\":{\"host\":\"$HOST\",\"repo\":\"$REPO\",\"number\":$PR_NUM,\"decision\":\"$DEC\",\"thread_gate\":\"$GATE_STATUS\",\"inline_threads\":$THREADS}}"
