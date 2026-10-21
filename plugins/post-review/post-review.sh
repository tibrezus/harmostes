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

# ── Inline findings: the agent posts each verified finding itself (#680,
# comment-scoped token); this plugin publishes ONLY the remainders —
# ledger entries without thread_ref (session-death resilience) — and
# composes the verdict from the round's threads on the PR
# as native anchored threads below. The agent must NOT self-post (the task
# prompt says so; the dedupe guard in the publisher is the second line of
# defense for a skill-following agent). This plugin owns: the CARRY, the
# unresolved-thread GATE, the VERDICT comment, the inline-thread PUBLISH,
# and the merge STATUS. ──
# ── Thread-closure mandate (live burns: rhesadox #2744 merged with its
# round-1 BLOCKING findings never answered — the gate then counted only
# the newest prior round; #2745 APPROVEd over two TODO threads born in the
# approving round itself — the gate listed threads only BEFORE this
# round's publish). The mandate, made mechanical:
#   1. TOTAL counting — every review-thread root on the PR, any round,
#      any author. No dedup, no round scoping: the decision is binary and
#      any dedup key tried so far burned (exact path:line drifted because
#      each comment's position serializes against its own head; newest-
#      prior-round scoping is what let #2744 through).
#   2. RESOLVE-ONLY closure — a reply is the record, the native resolve
#      is the close-out. The reply/marker closure grammar is retired:
#      resolve is API-settable on every wired host (the fork: PUT
#      /repos/{o}/{r}/pulls/{n}/comments/{id}/resolutions — verified
#      live; GitHub: GraphQL resolveReviewThread).
#   3. TODOs block their own round — everything is fixed in this PR; the
#      only escape is the ARCHITECTURE CARRY — and minting its tracker
#      issue is the EXCEPTION, not the default (#700): dedup (fuzzy, open
#      AND closed) first; generated artifacts never mint; an unmatched
#      carry mints only when marked severe/structural (severe|security|
#      data loss|outage|corruption, or an explicit "mint:" marker) —
#      otherwise the thread resolves as a review-record carry with NO
#      new issue. A design-scale finding that outlives the diff becomes
#      ONE deduped tracker issue at most, referenced by reply on the
#      thread, then resolved. The mint carries NO milestone (#712):
#      "newest open" tracked the running arc, not the finding's scope —
#      scoping a carried issue is a maintainer decision. Carried threads do not downgrade; the issue
#      gates nothing. Severe items are never only carried — the skill's
#      dual-thread rule keeps a Blocking quick fix in the PR.
# The listing feeds THREE consumers (carry scan, gate, artifact) — one
# fetch, before any mutation, so the gate's reads are pre-carry state and
# the carry reports exactly which ids it resolved.
CS_JSON=""
GATE_STATUS="not-evaluated"
CARRY_JSON_FILE="$(mktemp)"
echo '{"resolved_ids":[],"ledger_carried":[],"minted":[],"notes":[]}' > "$CARRY_JSON_FILE"
if [ "${IS_GITLAB:-}" != "true" ]; then
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
        part = get(f"{path}{sep}per_page=100&page={page}") or []
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
# The gate status interpolates into the double-quoted artifact line at
# three points — the listing-failure reason is host/exception text that
# can carry quotes, and a raw splice unbalances the line so lastJSONLine
# zeroes the whole round artifact (#685 r2: the same class as the status
# note). Sanitized ONCE here, at the single source.
GATE_STATUS="$(printf '%s' "$GATE_STATUS" | tr '\n\r\t' '   ' | tr -d '"' | tr '\\' '/')"
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
# ── CARRY (CARRY-CLASSIFIER markers — tested verbatim): architecture-
# scale items escape the PR as ONE deduped tracker issue AT MOST — minting
# is exception-gated (#700: rhesadox #2808-#2827 were the same few
# findings re-minted ~20 times across rounds because dedup was
# exact-title-only over 50 open issues). Inputs: the
# ledger (todos[] with disposition "architecture-carry") and the thread
# listing (open roots whose body opens with the Architecture-carry
# prefix — the skill's machine-read class grammar; agent-posted threads
# the agent could not mint for). Dedup: a body-referenced issue, or an
# open OR closed issue whose NORMALIZED title matches (class prefixes and
# round annotations stripped, token/bigram similarity — titles drift per
# round), is REFERENCED, never re-minted. A carry that dedups against
# nothing and carries no severity marker resolves as a review-record
# carry with NO tracker issue.
# Every carried thread is replied (issue reference) and RESOLVED — the
# resolve is what keeps it out of the gate's count; the minted issue
# gates nothing (non-blocking by construction — the whole point of the
# carry). Failures are SPOKEN in the artifact and the entry stays open
# for the gate to count: a failed carry must downgrade, never lose the
# thread.
export CARRY_JSON_FILE
# CARRY_ENV carries [FULL-ledger index, todo] pairs — the index space
# EVERY consumer reads (UNCARRIED_TODOS, the publish gate, the publisher's
# all_todos all enumerate the raw ledger). The first cut emitted indices
# from the FILTERED list (#685 r1 blocking: a mixed ledger [fix-in-pr,
# carry] inverted the bookkeeping — the carried item published as an open
# TODO while the fix-in-pr item was dropped).
ARCH_TODOS=$(python3 -c "
import json
r=json.load(open('$REVIEW'))
pairs=[[i,t] for i,t in enumerate(r.get('todos') or []) if isinstance(t,dict) and t.get('disposition')=='architecture-carry']
print(json.dumps(pairs))")
if [ "$ARCH_TODOS" != "[]" ] || printf '%s' "${CS_JSON:-}" | grep -q 'Architecture-carry'; then
  CARRY_ENV="$ARCH_TODOS" python3 - <<'PYCARRY' || log "WARN: carry step failed — entries stay open (the gate counts them)"
# CARRY-CLASSIFIER-START (tested verbatim by TestPostReviewCarryStep —
# everything between the markers must be self-contained; env: CARRY_ENV
# (JSON array of [FULL-ledger index, todo] pairs — the index space every
# consumer reads; a filtered-space index inverts mixed ledgers, #685 r1),
# IS_FJ (dialect: the fork resolves via PUT /resolutions, replies via
# in_reply_to, milestones by id, dedup via type=issues&limit — GitHub via
# GraphQL resolveReviewThread, /comments/{id}/replies, milestone NUMBER,
# dedup via per_page with PRs filtered client-side; #685 r1 blocking: the
# fork verbs 404/422 on GitHub and every carry ends unclosable);
# reads CS_JSON on stdin; writes the unified carry report to
# $CARRY_JSON_FILE).
import json, os, re, subprocess, sys
repo=os.environ["REPO"]; pr=os.environ["PR_NUM"]
base=os.environ["API_BASE"]; tok=os.environ["TOKEN"]
is_fj=os.environ.get("IS_FJ")=="true"
try:
    cs=json.load(sys.stdin)
except Exception:
    cs=[]
arch_todos=json.loads(os.environ.get("CARRY_ENV") or "[]")
def curl(method, path, payload=None, auth=None):
    # The HTTP status is parsed, not implied by curl's exit: without this a
    # 403/422 resolution or mint "succeeds" at transport level and the
    # carry reports threads resolved the host never resolved — a false
    # APPROVE through the merge currency.
    cmd=["curl","-sS","--max-time","20","-X",method,
         "-H","authorization: "+(auth or "token "+tok),"-H","content-type: application/json",
         "-w","\n%{http_code}",base+path]
    if payload is not None: cmd+=["-d",json.dumps(payload)]
    r=subprocess.run(cmd,capture_output=True,text=True)
    out=r.stdout or ""
    code=out.rsplit("\n",1)[-1].strip() if "\n" in out else "000"
    body=out.rsplit("\n",1)[0] if "\n" in out else out
    ok=r.returncode==0 and code.startswith("2")
    return ok, body
resolved_ids=[]; ledger_carried=[]; minted=[]; notes=[]
def gh_thread_map():
    # GitHub resolve is GraphQL-only (PUT /resolutions does not exist —
    # probed live #685 r1): map databaseId → reviewThread id, then mutate.
    owner,name=repo.split("/",1)
    q={"query":'{ repository(owner: "%s", name: "%s") { pullRequest(number: %s) { reviewThreads(first: 100) { nodes { id comments(first: 5) { nodes { databaseId } } } } } } }' % (owner,name,pr)}
    ok,body=curl("POST","/graphql",q,auth="bearer "+tok)
    try: data=json.loads(body or "{}")
    except Exception: return {}
    out={}
    nodes=((data.get("data",{}) or {}).get("repository",{}) or {}).get("pullRequest",{}).get("reviewThreads",{}).get("nodes",[]) or []
    for t in nodes:
        for cm in (t.get("comments",{}) or {}).get("nodes",[]) or []:
            if cm.get("databaseId") is not None: out[str(cm["databaseId"])]=t.get("id")
    return out
def resolve(comment_id):
    if is_fj:
        ok,_=curl("PUT", "/repos/%s/pulls/%s/comments/%s/resolutions" % (repo,pr,comment_id))
        return ok
    tid=gh_thread_map().get(str(comment_id))
    if not tid:
        return False
    ok,body=curl("POST","/graphql",
              {"query":'mutation($t:ID!){ resolveReviewThread(input:{threadId:$t}){ thread { isResolved } } }',
               "variables":{"t":tid}},
              auth="bearer "+tok)
    if not ok:
        return False
    # GraphQL answers 200 even when the mutation does not perform: a null
    # payload or an errors[] entry means the thread is STILL OPEN — count
    # only a confirmed resolve (fail-closed carry).
    try:
        performed=(((json.loads(body).get("data",{}) or {}).get("resolveReviewThread",{}) or {}).get("thread",{}) or {}).get("isResolved")
    except Exception:
        performed=None
    return performed is True
# 1) milestones — deliberately NOT fetched or assigned (#712): the
#    previous "newest-created open" rule tracked whatever arc was
#    running when the review minted (rhesadox: 56 carries landed in the
#    Zig 0.17 milestone), which is scope-noise, not scoping.
milestone=None
# 2) issues for dedup — OPEN and CLOSED (#700: a carried problem often
# has its issue already, sometimes closed after an earlier fix attempt;
# the dedup listing must see both or every round re-mints).
if is_fj:
    _,israw=curl("GET", "/repos/%s/issues?state=all&type=issues&limit=100" % repo)
else:
    # GitHub has no type=issues filter on /issues and no limit param (both
    # 422 — probed live #685 r1): per_page + PRs skipped client-side (a
    # same-title PR is not a dedup hit).
    _,israw=curl("GET", "/repos/%s/issues?state=all&per_page=100&sort=created&direction=desc" % repo)
known=[]
try:
    issues=json.loads(israw)
    if isinstance(issues,list):
        for i in issues:
            if not is_fj and i.get("pull_request"): continue
            t=(i.get("title") or "").strip()
            n=i.get("number")
            if t and n: known.append({"number":n,"title":t})
except Exception:
    pass
# Round annotations the reviewers append per round ("(6th round,
# unchanged)", "(narrowed but still open)", "(already minted — do NOT
# create a sixth)") and the class prefixes are noise: stripped before
# comparing, so a recurring finding matches the issue it came from.
ROUND_NOISE=re.compile(r"\([^)]*(?:round|unchanged|narrowed|unaddressed|still open|already minted|do not create)[^)]*\)", re.I)
CLASS_PREFIX=re.compile(r"^\s*(?:\**(?:architecture|architecture-carry|todo|blocking)\**\s*[:\u2014-]?\s*)+", re.I)
STOP=set("the a an and or but not is are was were be been being this that these those it its with without for from into over under again further then once here there when where why how all any both each few more most other some such only own same so than too very can will just dont should now still has have had also while".split())
def norm_tokens(title):
    s=CLASS_PREFIX.sub("", str(title or ""))
    s=ROUND_NOISE.sub(" ", s)
    s=re.sub(r"[^a-z0-9]+"," ", s.lower())
    return [w for w in s.split() if len(w)>2 and w not in STOP]
def find_dup(title):
    # normalized+ranked, not exact: exact-title dedup NEVER matched a
    # reworded round (#700). Distinctive-token Jaccard catches shared
    # vocabulary, word-bigram Jaccard catches shared PHRASES, and a
    # >=4-token subset catches elaborations of an existing title.
    a=norm_tokens(title)
    if not a: return None
    ga=set(zip(a,a[1:])); sa=set(a)
    for k in known:
        b=norm_tokens(k["title"])
        if not b: continue
        gb=set(zip(b,b[1:])); sb=set(b)
        if sa&sb and len(sa&sb)/len(sa|sb)>=0.55: return k["number"]
        if ga and gb and len(ga&gb)/len(ga|gb)>=0.45: return k["number"]
        if len(a)>=4 and sa<=sb: return k["number"]
        if len(b)>=4 and sb<=sa: return k["number"]
    return None
def best_overlap(title):
    # fallback reference-picker for DECLARED recurrences: the issue whose
    # distinctive vocabulary overlaps most (>=2 tokens).
    a=set(norm_tokens(title))
    best=None; hits=1
    for k in known:
        b=set(norm_tokens(k["title"]))
        n=len(a&b)
        if n>hits: hits=n; best=k["number"]
    return best
# A recurrence declaration in the carry itself ("(6th round, unchanged)",
# "narrowed but still open", "already minted — do NOT create a sixth"):
# the reviewer is TELLING us an issue exists. Never mint on a declared
# recurrence — reference the best candidate or carry in the record.
RECURRENCE=re.compile(r"\(\s*\d+(?:st|nd|rd|th)?\s+round|\bround\s+\d+\b|\b(?:unchanged|narrowed|already minted|do not create|recurring)\b", re.I)
# Minting is the EXCEPTION (#700): a tracker issue is minted only for a
# severe/structural finding the tracker does not already cover. The skill
# mirrors this contract (mark "mint:" or name the severity); an unmarked
# or recurring carry resolves as a review-record carry with NO new issue.
MINT_WORTHY=re.compile(r"\b(?:severe|security|data[- ]?loss|outage|corruption)\b|^mint:|\smint:", re.I|re.M)
# Deterministically GENERATED content (arch-sync artifacts and their
# commits) is owned by its generator — findings about it never mint.
GENERATED=re.compile(r"raw/arch/|model\.c4|\.mmd|rig\.json|arch[- ]sync|arch graph|update architecture page", re.I)
def ensure_issue(title, body):
    dup=find_dup(title)
    if dup is not None:
        return dup, "referenced"
    if GENERATED.search(title) or GENERATED.search(body):
        return None, "generated"
    if RECURRENCE.search(title) or RECURRENCE.search(body):
        cand=best_overlap(title)
        if cand is not None:
            return cand, "referenced"
        return None, "recurrence"
    if not MINT_WORTHY.search(body):
        return None, "unmarked"
    payload={"title":title,"body":body}  # no milestone key — #712
    ok,resp=curl("POST", "/repos/%s/issues" % repo, payload)
    try: r=json.loads(resp)
    except Exception: r={}
    if ok and isinstance(r,dict) and r.get("number"):
        known.append({"number":r["number"],"title":title})  # in-run dedup
        return r["number"], "minted"
    return None, "failed"
def reply_thread(comment_id, body_text):
    # fork: in_reply_to on the flat comments route (the fork cannot REST
    # a create-review reply); GitHub: the documented /replies route — it
    # rejects in_reply_to as a not-permitted key (422, probed live #685
    # r1). Best-effort either way: the RESOLVE is the closure that counts.
    if is_fj:
        curl("POST", "/repos/%s/pulls/%s/comments" % (repo,pr),
             {"body":body_text, "in_reply_to":comment_id})
    else:
        curl("POST", "/repos/%s/pulls/%s/comments/%s/replies" % (repo,pr,comment_id),
             {"body":body_text})
# 3) ledger todos with disposition architecture-carry (FULL-ledger idx).
for idx,t in arch_todos:
    if not isinstance(t,dict) or not t.get("body"): continue
    body=str(t.get("body"))
    title=("architecture: "+body.splitlines()[0])[:140]
    ref=t.get("issue")  # the agent may pre-reference an existing issue
    note=None
    if ref and re.search(r"\d+", str(ref)):
        num=re.search(r"\d+", str(ref)).group(0)
    else:
        num,outcome=ensure_issue(title, "from PR #%s — %s\n(carried by the review pipeline)" % (pr, body))
        if outcome=="minted":
            minted.append({"issue":num,"title":title})
        elif outcome=="referenced":
            notes.append("carry references existing issue #%s — no re-mint: %s" % (num, title[:60]))
        elif outcome=="failed":
            notes.append("carry mint FAILED for: %s" % title[:60])
            continue
        elif outcome=="generated":
            num=None
            note="carried in the review record — generated artifact, owned by its generator (arch-sync), not the tracker"
            notes.append("carry SKIPPED mint (generated): %s" % title[:60])
        elif outcome=="recurrence":
            num=None
            note="carried in the review record — declared recurrence with no covering issue found; raise one manually only if this is genuinely new"
            notes.append("carry SKIPPED mint (recurrence): %s" % title[:60])
        else:  # unmarked — the exception gate
            num=None
            note="carried in the review record — no tracker issue minted (minting is the exception; mark severe/structural or add 'mint:' to promote)"
            notes.append("carry SKIPPED mint (unmarked): %s" % title[:60])
    tref=t.get("thread_ref")
    if tref:
        rid=str(tref).split("/")[-1]
        reply_thread(rid, note or ("carried to #%s — tracked as architecture follow-up; the thread closes here" % num))
        if resolve(rid): resolved_ids.append(str(rid))
        else: notes.append("resolve FAILED for thread %s (%s)" % (rid, ("issue #"+str(num)) if num else "review-record carry"))
    ledger_carried.append(idx)
# 4) open Architecture-carry THREADS on the PR (agent-posted, unresolved).
if isinstance(cs,list):
    seen=set()
    for c in cs:
        if not isinstance(c,dict) or c.get("id") is None: continue
        if c.get("in_reply_to") or c.get("in_reply_to_id"): continue
        if c.get("resolved"): continue
        body=str(c.get("body") or "")
        if not body.lstrip().startswith("**Architecture-carry**"): continue
        if str(c["id"]) in seen: continue
        seen.add(str(c["id"]))
        m=re.search(r"#(\d+)", body)
        note=None
        if m:
            num=m.group(1)
        else:
            first=body.replace("**Architecture-carry**","").strip()
            first=first.splitlines()[0][:140] if first else "architecture follow-up"
            title="architecture: "+first
            num,outcome=ensure_issue(title, "from PR #%s thread %s — %s\n(carried by the review pipeline)" % (pr, c["id"], body))
            if outcome=="minted":
                minted.append({"issue":num,"title":title})
            elif outcome=="referenced":
                notes.append("carry references existing issue #%s — no re-mint (thread %s)" % (num, c["id"]))
            elif outcome=="failed":
                notes.append("carry mint FAILED for thread %s" % c["id"])
                continue
            elif outcome=="generated":
                num=None
                note="carried in the review record — generated artifact, owned by its generator (arch-sync), not the tracker"
                notes.append("carry SKIPPED mint (generated) for thread %s" % c["id"])
            elif outcome=="recurrence":
                num=None
                note="carried in the review record — declared recurrence with no covering issue found; raise one manually only if this is genuinely new"
                notes.append("carry SKIPPED mint (recurrence) for thread %s" % c["id"])
            else:  # unmarked — the exception gate
                num=None
                note="carried in the review record — no tracker issue minted (minting is the exception; mark severe/structural or add 'mint:' to promote)"
                notes.append("carry SKIPPED mint (unmarked) for thread %s" % c["id"])
        reply_thread(c["id"], note or ("carried to #%s — tracked as architecture follow-up; the thread closes here" % num))
        if resolve(c["id"]): resolved_ids.append(str(c["id"]))
        else: notes.append("resolve FAILED for thread %s (%s)" % (c["id"], ("issue #"+str(num)) if num else "review-record carry"))
report={"resolved_ids":resolved_ids,"ledger_carried":ledger_carried,"minted":minted,"notes":notes}
with open(os.environ["CARRY_JSON_FILE"],"w") as f: json.dump(report,f)
# CARRY-CLASSIFIER-END
PYCARRY
fi
CARRY=$(cat "$CARRY_JSON_FILE"); export CARRY
CARRY_LEDGER=$(printf '%s' "$CARRY" | python3 -c "import json,sys;print(json.dumps(json.load(sys.stdin)['ledger_carried']))")
export CARRY_LEDGER
CARRY_RESOLVED=$(printf '%s' "$CARRY" | python3 -c "import json,sys;print(json.dumps(json.load(sys.stdin)['resolved_ids']))")
export CARRY_RESOLVED
UNCARRIED_TODOS=$(ARCH="$CARRY" python3 -c "
import json,os
r=json.load(open(os.environ['REVIEW'])); c=set(json.loads(os.environ['ARCH'])['ledger_carried'])
ts=[t for i,t in enumerate(r.get('todos') or []) if isinstance(t,dict) and i not in c]
print(len(ts))")
REMAINDER_FINDINGS=$(python3 -c "
import json
r=json.load(open('$REVIEW'))
fs=r.get('findings', r.get('comments',[]) or []) or []
print(len([f for f in fs if isinstance(f,dict) and not f.get('thread_ref')]))")

# The classifier program lives verbatim between the GATE-CLASSIFIER markers
# (the golden test extracts exactly these bytes). It is held in a QUOTED
# heredoc so bash never parses it; do not unquote.
CLASSIFIER_PY=$(cat << 'GATE_PYEOF'
# GATE-CLASSIFIER-START (tested verbatim by TestPostReviewGateClassifier —
# everything between the markers must be a self-contained script; it reads
# the comments JSON on stdin and CARRY_RESOLVED (JSON id array) from the
# environment; stdout: open count, then decision).
# TOTAL counting, RESOLVE-ONLY closure (the thread-closure mandate):
#   open = root AND NOT resolved AND id not in carry_resolved
# A root is a comment without in_reply_to linkage (both hosts' shapes;
# the fork serializes 0 for roots — falsy under the same read). Rounds
# do NOT scope the count (rhesadox #2744: round-1 blockers merged
# unanswered under newest-prior-round scoping) and commit_id is
# irrelevant — the round's OWN threads count too (#2745: TODOs born in
# the approving round). Replies close NOTHING: the reply is the record,
# the resolve is the close-out (resolve is API-settable on every wired
# host; the reply/marker grammar existed only for the fork's REST gap).
# Degraded-read posture: a shape this program cannot read is SKIPPED,
# never a KeyError — the classifier runs under set -e and a crash aborts
# the deploy before the verdict + label DELETE. A false APPROVE
# re-reviews next round; a crash wedges the head.
import json, os, sys
cs=json.load(sys.stdin)
try:
    carried=set(str(x) for x in json.loads(os.environ.get("CARRY_RESOLVED") or "[]"))
except Exception:
    carried=set()
open_threads=[]
if isinstance(cs,list):
    for c in cs:
        if not isinstance(c,dict) or c.get("id") is None:
            continue  # unreadable shape: skipped
        if c.get("in_reply_to_id") or c.get("in_reply_to"):
            continue  # reply — part of a thread, not a root
        if c.get("resolved") or c.get("resolver"):
            continue  # resolved: the one lawful close-out
        if str(c["id"]) in carried:
            continue  # resolved by this run's carry step
        open_threads.append(c)
dec="APPROVE"
if open_threads:
    dec="REQUEST_CHANGES"
    for c in open_threads[:10]:
        print("[post-review] open thread %s:%s id=%s — reply+resolve (or carry) required before APPROVE" % (c.get("path","?"), c.get("line") or c.get("line_num") or "?", c.get("id")), file=sys.stderr)
# stdout: count line FIRST, decision LAST — the shell and the golden test
# read the decision as the last line.
print(len(open_threads))
print(dec)
# GATE-CLASSIFIER-END
GATE_PYEOF
)

# stdout of the classifier: count line first, decision last. If the
# classifier itself dies, the gate was NOT evaluated: skip the verdict post
# and label removal; next cycle retries (a verdict from an unknown decision
# is worse than no verdict).
CLASS_OUT=$(printf '%s' "$CS_JSON" | CARRY_RESOLVED="$CARRY_RESOLVED" python3 -c "$CLASSIFIER_PY") || {
  log "WARN: gate classifier failed — gate not evaluated; skipping verdict post and label removal; next cycle retries"
  echo "{\"artifact\":\"pr-$HOST-$REPO-$PR_NUM\",\"status\":\"ok\",\"event\":{\"host\":\"$HOST\",\"repo\":\"$REPO\",\"number\":$PR_NUM,\"decision\":\"$DEC\",\"thread_gate\":\"classifier-crash\",\"skipped\":\"gate-unavailable\"}}"
  exit 0
}
if [ -z "$CS_JSON" ]; then
  LISTED_OPEN=0
else
  LISTED_OPEN=$(printf '%s\n' "$CLASS_OUT" | head -n 1)
fi
# TOTAL open = listed open roots + this round's own yet-unpublished ledger
# items (remainder findings and uncarried todos — they ARE threads of this
# round's verdict, publishing right after this gate; counting them pre-
# publish is what makes same-round threads visible, the #2745 hole).
TOTAL_OPEN=$(( ${LISTED_OPEN:-0} + ${REMAINDER_FINDINGS:-0} + ${UNCARRIED_TODOS:-0} ))
if [ "$DEC" = "APPROVE" ] && [ "$GATE_STATUS" != "evaluated" ]; then
  log "thread gate unavailable ($GATE_STATUS) on an APPROVE — skipping verdict post and label removal; next cycle retries"
  echo "{\"artifact\":\"pr-$HOST-$REPO-$PR_NUM\",\"status\":\"ok\",\"event\":{\"host\":\"$HOST\",\"repo\":\"$REPO\",\"number\":$PR_NUM,\"decision\":\"$DEC\",\"thread_gate\":\"$GATE_STATUS\",\"skipped\":\"gate-unavailable\"}}"
  exit 0
fi
if [ "$DEC" = "APPROVE" ] && [ "$TOTAL_OPEN" -gt 0 ]; then
  log "APPROVE downgraded: $TOTAL_OPEN open thread(s) ($LISTED_OPEN listed + $REMAINDER_FINDINGS remainder findings + $UNCARRIED_TODOS uncarried todos)"
  DEC="REQUEST_CHANGES"
  DOWNGRADED=1
  export DOWNGRADED OPEN_THREADS="$TOTAL_OPEN"
  # The trailer contract: verdictTrailer in internal/review/review.go is
  # the canonical home — the decision field and the posted trailer must
  # move together.
  python3 - "$REVIEW" "$DEC" << 'PYEOF'
import json,sys
p,newdec=sys.argv[1],sys.argv[2]; r=json.load(open(p))
r["decision"]=newdec
json.dump(r, open(p,"w"))
PYEOF
fi

# ── Coverage ledger (#704): merge this round's coverage block into the
# per-PR ledger in the repository-scoped shared store, downgrade an
# APPROVE that slipped a declared frontier past the gate (belt and braces
# — pr-review.sh already refuses it; the artifact must never lie even if
# a future caller bypasses the gate), and hand the verdict a compact
# coverage projection (COV_FILE, the THREADS_FILE pattern: a file path,
# never a blob through envp). The store write is NON-FATAL: the ledger
# is cross-round carry; losing it costs the next round its delta, never
# this round its verdict.
# COVERAGE-MERGE-START (tested verbatim by TestPostReviewCoverageLedger —
# keep this block between the markers so the golden test runs the
# shipped code)
COV_FILE="$WORKDIR/.coverage-carry.json"
rm -f "$COV_FILE"
COV_STORE="${HARMOSTES_SOL_PI_STORE:-}"
# TOTAL_OPEN rides in (#704 r1): the ledger's findings_open is the
# convergence metric (#704 item 5) — it must be OPEN THREADS (listed open
# roots + this round's remainders + uncarried todos), not the raw length
# of the round's findings array, or the metric lies in both directions.
TOTAL_OPEN="$TOTAL_OPEN" python3 - "$REVIEW" "$COV_FILE" "$COV_STORE" "$PR_NUM" << 'PYCOV' || { echo '{}' > "$COV_FILE" 2>/dev/null; log "WARN: coverage merge crashed (non-fatal — the verdict, label consume and artifact stand; the ledger write is skipped)"; }
import json, os, sys, tempfile
from datetime import datetime, timezone

review_path, out_path, store, pr_num = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
with open(review_path) as f:
    review = json.load(f)
cov_raw = review.get("coverage")
# `declared` (#704 r1): a round that declares NO coverage block must not
# touch the ledger at all — merging an empty declaration ERASES the prior
# frontier (files nobody examined would carry frontier_count 0 into the
# next round's carry). Undeclared = the ledger is read-only this round.
cov = cov_raw if isinstance(cov_raw, dict) else {}
covered_now = cov.get("covered") or []
frontier_now = cov.get("frontier") or []
# Vacuous declarations don't count (#704 r2): with the gate refusing
# undeclared rounds after carry, `{covered:[], frontier:[]}` is the one
# shape a budget-starved round can still emit — and merging it would
# erase the frontier exactly like the undeclared round did. Empty AND
# declared = read-only; a real statement names at least one path.
declared = cov_raw is not None and bool(covered_now or frontier_now)
sha = review.get("reviewed_sha", "")
findings = review.get("findings", review.get("comments") or []) or []
try:
    open_threads = int(os.environ.get("TOTAL_OPEN") or 0)
except ValueError:
    open_threads = 0

# ── the downgrade: an APPROVE plus a declared frontier is a coverage lie.
# Mirrors the thread downgrade above: rewrite review.json's decision (the
# verdict compose and the merge status both read it) and speak via the
# artifact file.
downgraded = False
if str(review.get("decision","")).upper() == "APPROVE" and frontier_now:
    review["decision"] = "REQUEST_CHANGES"
    json.dump(review, open(review_path, "w"))
    downgraded = True
    print(f"[post-review] APPROVE downgraded: coverage incomplete — frontier names "
          f"{len(frontier_now)} file(s) never fully examined", file=sys.stderr)

proj = {
    "downgraded": downgraded,
    "covered_now": len(covered_now),
    "frontier_now": len(frontier_now),
    "carried": False,
    "rounds": 0,
    "covered_total": len(covered_now),
}

if store and declared:
    ledger_path = os.path.join(store, "coverage", "%s.json" % pr_num)
    try:
        with open(ledger_path) as f:
            ledger = json.load(f)
        if not isinstance(ledger, dict):
            ledger = {}
    except Exception:
        ledger = {}
    try:
        prev_covered = ledger.get("covered") or []
        if not isinstance(prev_covered, list):
            prev_covered = []
        # path-keyed accumulation: this round's declarations win (newest
        # sha/round); older entries for paths NOT re-declared survive —
        # they were covered at their own sha and the delta tells the next
        # round whether that still holds.
        by_path = {}
        for e in prev_covered:
            if isinstance(e, dict) and isinstance(e.get("path"), str):
                by_path[e["path"]] = e
        rnd = int(ledger.get("rounds") or 0) + 1
        for e in covered_now:
            entry = {"path": e["path"], "sha": sha, "round": rnd}
            if isinstance(e.get("note"), str) and e["note"].strip():
                entry["note"] = e["note"]
            by_path[e["path"]] = entry
        # The frontier is per-path (#716 r2): a carried frontier path
        # leaves by being EXAMINED (declared covered this round) or by
        # LEAVING THE CHANGED SURFACE (#716 r3 — the retirement condition
        # the r2 merge lacked: without it, a same-head re-check round had
        # no honest exit — it cannot declare covered what it did not
        # open, so the only mechanical escape was a false covered entry,
        # and a lie certifies itself: touched:false forever, free
        # APPROVEs). Files_changed comes from the same pr-context the
        # gate reads; a path outside it is no longer review surface.
        declared_covered = {e["path"] for e in covered_now if isinstance(e, dict) and isinstance(e.get("path"), str)}
        try:
            with open(os.path.join(os.path.dirname(review_path), "pr-context.json")) as f:
                review_ctx = json.load(f)
            changed_surface = {f.get("filename") for f in (review_ctx.get("files_changed") or []) if isinstance(f, dict)}
        except (OSError, json.JSONDecodeError, ValueError):
            changed_surface = None  # unknowable surface → nothing retires
        frontier_out = [
            {"path": e["path"], **({"reason": e["reason"]} if isinstance(e.get("reason"), str) and e["reason"].strip() else {"reason": "not examined"})}
            for e in frontier_now if isinstance(e, dict) and isinstance(e.get("path"), str)
        ]
        frontier_paths = {e["path"] for e in frontier_out}
        retired = 0
        for pe in ledger.get("frontier") or []:
            if not (isinstance(pe, dict) and isinstance(pe.get("path"), str)):
                continue
            p = pe["path"]
            if p in declared_covered or p in frontier_paths:
                continue
            if changed_surface is not None and p not in changed_surface:
                retired += 1
                continue  # out of the changed surface — nothing left to examine
            frontier_out.append({"path": p, "reason": pe.get("reason") or "not examined"})
        if retired:
            print(f"[post-review] frontier retirement: {retired} path(s) left the changed surface — carried out of the ledger", file=sys.stderr)
        ledger_out = {
            "version": 1,
            "pr": int(pr_num) if str(pr_num).isdigit() else pr_num,
            "rounds": rnd,
            "last_reviewed_sha": sha,
            "updated_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "covered": sorted(by_path.values(), key=lambda e: e["path"]),
            "frontier": sorted(frontier_out, key=lambda e: e["path"]),
            "findings_open": open_threads,
        }
        os.makedirs(os.path.dirname(ledger_path), exist_ok=True)
        fd, tmp = tempfile.mkstemp(dir=os.path.dirname(ledger_path), prefix=".cov-")
        with os.fdopen(fd, "w") as f:
            json.dump(ledger_out, f, indent=2)
        os.replace(tmp, ledger_path)
        proj.update({"carried": True, "rounds": rnd, "covered_total": len(ledger_out["covered"])})
        print(f"[post-review] coverage ledger written: rounds={rnd} covered={len(ledger_out['covered'])} "
              f"frontier={len(ledger_out['frontier'])} findings_open={open_threads}", file=sys.stderr)
    except Exception as e:
        # non-fatal by contract: the carry is an optimization layered on a
        # complete round; losing it costs the next round its delta.
        print(f"[post-review] WARN: coverage ledger not written ({e}) — next round re-reviews full-surface", file=sys.stderr)

with open(out_path, "w") as f:
    json.dump(proj, f)
PYCOV
# The coverage downgrade must move the SHELL's decision too (#704 r1):
# every downstream consumer — the native review event (a coverage-
# downgraded APPROVE otherwise posts a native APPROVED review), the
# pr-review/verdict merge status (otherwise success) and the round
# artifact — reads $DEC, not review.json. Mirrors the thread downgrade:
# the compose differs (cov.downgraded speaks in the coverage branch), so
# DOWNGRADED stays untouched.
if grep -q '"downgraded": true' "$COV_FILE" 2>/dev/null; then
  log "coverage downgrade: DEC follows review.json to REQUEST_CHANGES (native review, merge status, artifact)"
  DEC="REQUEST_CHANGES"
  export COV_DOWNGRADED=1
fi
export COV_FILE
# COVERAGE-MERGE-END
fi   # IS_GITLAB guard closes: the listing/carry/gate are wired dialects only

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
elif [ "$THREADS_ANCHOR" = "1" ] && CARRY_LEDGER="$CARRY_LEDGER" python3 -c "
import json,os,sys
r=json.load(open(os.environ['REVIEW']))
cs=r.get('findings', r.get('comments',[])) or []
try: carried=set(json.loads(os.environ.get('CARRY_LEDGER','[]')))
except Exception: carried=set()
todos=[t for i,t in enumerate(r.get('todos',[]) or []) if i not in carried]
sys.exit(0 if (cs or todos) else 1)" 2>/dev/null; then
  # Dedupe guard keyed on the MARKER (r2): already posted at this SHA → skip.
  if [ "${EXISTING:-0}" != "0" ]; then
    log "inline threads already posted at ${REVIEWED_SHA:0:8} — not duplicating"
    echo '{"posted":0,"rejected":0,"capped":0,"skipped":"already-posted"}' > "$THREAD_STATUS_FILE"
  else
    python3 - << 'PYTHREADS' || { echo '{"posted":0,"rejected":0,"capped":0,"skipped":"publisher-crashed"}' > "$THREAD_STATUS_FILE"; } && log "WARN: inline thread publish failed — verdict stands, threads skipped"
import json, os, subprocess, sys
review=json.load(open(os.environ["REVIEW"]))
# #680: the agent posts each finding as it verifies it (thread_ref filled);
# this publisher handles ONLY the remainders — findings never posted before
# the session ended (session-death resilience). Carried findings need no
# publish: their threads are already on the PR and already counted.
all_cs=[c for c in (review.get("findings", review.get("comments",[])) or [])
        if not c.get("thread_ref")]
# carried todos are terminated by the carry step (issue minted + thread
# resolved or never posted) — they must NOT publish as threads: an open
# carried thread would downgrade the NEXT verdict by this same gate.
try:
    carried_idx=set(json.loads(os.environ.get("CARRY_LEDGER","[]")))
except Exception:
    carried_idx=set()
all_todos=[t for i,t in enumerate(review.get("todos",[]) or [])
           if i not in carried_idx]
# TODO lane (r7-r10): completely-missing pieces anchor as NON-blocking
# threads after the findings, sharing the 20-thread cap. They never count
# in the verdict's blocking number; teeth come from the classifier — an
# unresolved TODO thread downgrades the NEXT round's APPROVE.
TODO_PREFIX="**TODO** — missing complement; fix in this PR, reply+resolve: "
anchorable=[t for t in all_todos if isinstance(t,dict) and t.get("path") and t.get("body")]
todo_bodies=[TODO_PREFIX+str(t.get("body","")) for t in anchorable]
todo_room=max(0,20-len(all_cs[:20]))
# #685 r6: the class rides every merged entry so posted counts stay
# per-class (a mixed 'posted' number inflated the blocking count) and the
# cap overage names BOTH classes — a capped-out TODO evaporated exactly
# like an unanchored finding (the r5 hole's symmetric twin).
todo_published=anchorable[:todo_room]
todo_dropped=anchorable[todo_room:]
merged=all_cs[:20]+[{**t,"body":b,"cls":"todo"} for t,b in zip(todo_published, todo_bodies[:todo_room])]
cs=merged
base=os.environ["API_BASE"]
# Native reviews post as the bot when provisioned (#480) — see REVIEW_TOKEN above.
tok=os.environ.get("REVIEW_TOKEN") or os.environ["TOKEN"]
repo=os.environ["REPO"]; pr=os.environ["PR_NUM"]; sha=review.get("reviewed_sha","")
fj = os.environ.get("IS_FJ")=="true"
marker=os.environ["MARKER"]
posted=0; rejected=0; last_error=""
posted_findings=0; posted_todos=0  # #685 r6: per-class truth for the verdict
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
carried=[]  # findings the publisher could not LAND (#685 r5): they ride
             # the verdict body — the round may not claim threads the
             # forge never took, nor let the text exist nowhere.
# r9: the carried list in the ARTIFACT keeps FULL bodies — the artifact
# is the recovery surface the verdict's truncation marker points at; a
# cut here (the r8 shape) made the pointer false and dropped the FIX
# tail from every surface at once. The single display cap lives at the
# verdict render site.
for c in cs:
    path=c.get("path"); body=c.get("body")
    if not path or not body:
        rejected+=1   # a malformed finding skips itself, never the batch (r1 P5)
        carried.append({"path":str(path or "?"),"line":"?","body":str(body or ""),"why":"malformed","cls":c.get("cls") or "finding"})
        print("[post-review] WARN: malformed finding (missing path/body) — skipped", file=sys.stderr)
        continue
    try:
        line=int(str(c.get("line")))
    except (ValueError, TypeError):
        # r4 P5d: line is optional by the review.json contract but the
        # threads anchor to a line — a finding without one is carried by
        # the verdict body (now literally true), never silently pinned to line 1.
        rejected+=1
        carried.append({"path":path,"line":"?","body":str(body),"why":"no-usable-line","cls":c.get("cls") or "finding"})
        print(f"[post-review] WARN: finding {path} has no usable line — carried by the verdict body", file=sys.stderr)
        continue
    valid.append((path, line, c.get("side","RIGHT"), body, c.get("cls") or "finding"))
if fj:
    # Forgejo accepts a comments ARRAY in one create-pull-review — batch
    # first (one review object on the UI), fall back per finding on
    # rejection so one bad line cannot kill the batch (r2 P7).
    payload={"event":os.environ["REVIEW_EVENT"],"commit_id":sha,"body":marker,
             "comments":[{"path":p,"new_position":l,"body":b} for p,l,_,b,_c in valid]}
    ok, reason = curl(f"/repos/{repo}/pulls/{pr}/reviews", payload)
    if ok:
        posted += len(valid)
        posted_findings += sum(1 for v_ in valid if v_[4]!="todo")
        posted_todos    += sum(1 for v_ in valid if v_[4]=="todo")
    else:
        last_error = reason  # r5 t12: the artifact must carry the host's reason (the #480 symptom was an unattributable rejection)
        print(f"[post-review] WARN: batch publish rejected ({reason[:120]}) — falling back per finding", file=sys.stderr)
        for p,l,_,b,cl in valid:
            ok2, reason2 = curl(f"/repos/{repo}/pulls/{pr}/reviews", {"event":os.environ["REVIEW_EVENT"],"commit_id":sha,"body":marker,
                                     "comments":[{"path":p,"new_position":l,"body":b}]})
            if ok2:
                posted+=1
                if cl=="todo": posted_todos+=1
                else: posted_findings+=1
            else:
                rejected+=1
                carried.append({"path":p,"line":l,"body":str(b),"why":str(reason2)[:80],"cls":cl})
                if not last_error:
                    last_error = reason2
                print(f"[post-review] WARN: inline thread {p}:{l} rejected — {reason2}", file=sys.stderr)
else:
    for p,l,side,b,cl in valid:
        ok, reason = curl(f"/repos/{repo}/pulls/{pr}/comments", {"commit_id":sha,"path":p,"line":l,"side":side,
                 "body":"_"+marker+"_"+chr(10)+chr(10)+b})
        if ok:
            posted+=1
            if cl=="todo": posted_todos+=1
            else: posted_findings+=1
        else:
            rejected+=1
            carried.append({"path":p,"line":l,"body":str(b),"why":str(reason)[:80],"cls":cl})
            if not last_error:
                last_error = reason
            print(f"[post-review] WARN: inline thread {p}:{l} rejected — {reason}", file=sys.stderr)
# the cap overage lands in the verdict body for BOTH classes (#685 r6:
# findings-only was the r5 fix's symmetric hole — a capped TODO evaporated)
for c in (all_cs[len(cs):]):
    carried.append({"path":str(c.get("path","?")),"line":c.get("line","?"),"body":str(c.get("body","")),"why":"cap","cls":"finding"})
for t in todo_dropped:
    carried.append({"path":str(t.get("path","?")),"line":t.get("line","?"),"body":TODO_PREFIX+str(t.get("body","")),"why":"cap","cls":"todo"})
capped=len(all_cs[len(cs):])+len(todo_dropped)
dropped=[str(c.get("path","?")) for c in all_cs[len(cs):]]+[str(t.get("path","?")) for t in todo_dropped]
if dropped:
    print(f"[post-review] capped: {len(all_cs[len(cs):])} findings + {len(todo_dropped)} TODOs anchor only in the verdict body: {', '.join(dropped)}", file=sys.stderr)
# r11 CRITICAL: the carried list rides the plugin's artifact line and the
# timeline event — an UNBOUNDED list (r9's full-fidelity fix) overflows
# Linux MAX_ARG_STRLEN via export THREADS (exit 126, round lost, label
# not consumed, re-arm crash loop — reproduced at n=30) and can breach
# lastJSONLine's 1MB scanner cap (zeroed parse — the lost-artifact class).
# Budget: bodies stay COMPLETE while the aggregate is under 64KB; beyond
# it each new body caps at 1600 with why=budget and the summary counts
# them. The run record keeps what fits; the counts stay honest.
BUDGET=64*1024
# r12: over-budget ≠ cut. A body under the display window rides WHOLE
# (tagged budget-uncut for forensics); only a body actually shortened is
# budget_capped — the counter and the verdict note speak CUTS, and the
# render site treats a [budget-capped] body as final (never re-caps it
# into a false 'full text in the run record' pointer: the run record
# holds the same cut body).
_total=0; _capped=0; _uncut=0
for c in carried:
    b=str(c.get("body",""))
    if _total+len(b)<=BUDGET:
        _total+=len(b); continue
    if len(b)>1600:
        c["body"]=b[:1600]+" …[budget-capped]"
        c["why"]=(str(c.get("why") or "")+"+budget-cut").lstrip("+")
        _capped+=1
    else:
        c["why"]=(str(c.get("why") or "")+"+budget-uncut").lstrip("+")
        _uncut+=1
# r12 todo: the budget bounds per-entry size, NOT entry count — a
# 700-finding pathological ledger still breaches the 1MB scanner
# (~1.4KB × 700 + framing; reproduced: n=700 → 1.07MB line, zeroed
# parse). Hard bound on the ARTIFACT's carried list: full entries while
# they fit the list budget, then count-only rows (path+line+why, no
# body) — the verdict body and the timeline event carry the same
# bounded shape, so the recovery pointer stays true within it.
ART_ENTRY_BUDGET=96*1024
_art=0; _rows=[]; _bound=0
for c in carried:
    b=str(c.get("body",""))
    row_sz=len(b)+len(str(c.get("path","")))+64
    if _art+row_sz<=ART_ENTRY_BUDGET:
        _art+=row_sz; _rows.append(c)
    else:
        _rows.append({"path":c.get("path","?"),"line":c.get("line","?"),"why":str(c.get("why") or "")+"+artifact-bound","cls":c.get("cls") or "finding"})
        _bound+=1
carried=_rows
summary={"posted":posted,"posted_findings":posted_findings,"posted_todos":posted_todos,"rejected":rejected,"capped":capped,"todos":len(todo_bodies)}
if _capped: summary["budget_capped"]=_capped
if _uncut: summary["budget_uncut"]=_uncut
if _bound: summary["artifact_bound"]=_bound
if carried:
    summary["carried"]=carried
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
# CARRY_LEDGER excludes successfully-carried todos (#685 r5, fifth-round
# blocker): the zero-findings APPROVED review re-published carried items
# as open roots — an APPROVE + success status posted over a newborn
# thread, the exact #2745 shape. A FAILED carry (index absent from
# CARRY_LEDGER) still rides: it must not vanish (fail-closed).
try: carried=set(json.loads(os.environ.get("CARRY_LEDGER","[]")))
except Exception: carried=set()
P="TODO (non-blocking — reply with the follow-up, then resolve): "
cs=[{"path":t["path"],"new_position":int(t["line"]),"body":P+str(t["body"])}
    for i,t in enumerate(r.get("todos") or [])
    if isinstance(t,dict) and t.get("path") and t.get("line") and i not in carried]
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
# r11 CRITICAL: this used to be `THREADS=$(cat …); export THREADS` — the
# exported blob rode envp into the NEXT exec (the verdict heredoc's
# python3) and died at MAX_ARG_STRLEN once the carried list grew (exit
# 126, crash site unrelated to cause). The summary now travels as a FILE
# PATH — small constant env — and every consumer reads the file.
# The scan-failed flag still splices into the file itself (r4 P8: the
# splice must survive whichever branch wrote the shape).
if [ -s "$DEDUPE_FLAG_FILE" ]; then
  FLAG="$(cat "$DEDUPE_FLAG_FILE")"
  python3 - "$THREAD_STATUS_FILE" "$FLAG" <<'PYFLAG'
import json,sys
p,frag=sys.argv[1],sys.argv[2].strip().lstrip(",")
k,_,v=frag.partition(":")
d=json.load(open(p)); d[k.strip().strip('"')]=v.strip().strip('"')
json.dump(d,open(p,"w"))
PYFLAG
fi
rm -f "$DEDUPE_FLAG_FILE"
export THREADS_FILE="$THREAD_STATUS_FILE"


# ── Verdict comment (r7, owner directive; repositioned #685 r5): posted
# AFTER the thread publisher so its counts are TRUE. The first cut
# claimed "N blocking findings posted as review threads" before the
# publisher ran — a finding whose anchor the host 422s (line not in the
# diff) never reached the PR and existed nowhere (review.json is
# workspace-ephemeral): the platform asserted a thread the forge never
# took, for five consecutive rounds on this very PR (the reviewer's
# r1-r4 anchors all 422'd — which is ALSO how a real blocking finding
# survived undelivered). The one-line counts clause stays; findings the
# publisher could not land are CARRIED in the body below it — visible,
# addressable, and honest about being unanchorable. review.json's
# analysis prose is still never posted: the threads + the carried list
# are the findings record.
VERDICT_COMMENT=$(python3 - << 'PYEOF'
import json, os
with open(os.environ["REVIEW"]) as f: review=json.load(f)
dec=review["decision"]; sha=review.get("reviewed_sha","")
fs=review.get("findings", review.get("comments",[]) or []) or []
n=len(fs)
# Coverage projection (#704): the round's coverage statement rides the
# verdict — files covered at this head, the declared frontier (bounded
# like the carried list), and a downgrade note when the frontier refused
# an APPROVE. Absent coverage block → no coverage line (legacy shape).
try: cov=json.load(open(os.environ["COV_FILE"]))
except Exception: cov={}
cov_declared = bool(cov) and bool(
    cov.get("downgraded") or (cov.get("covered_now") or 0) + (cov.get("frontier_now") or 0) > 0)
# The publisher's true counts + everything it could not land (#685 r5):
# rejected anchors, lineless findings, and the 20-thread cap overage all
# ride here — the verdict may not claim a thread the forge never took.
try: th=json.load(open(os.environ["THREADS_FILE"]))
except Exception: th={}
carried=th.get("carried") or []
posted=int(th.get("posted") or 0)
posted_findings=int(th.get("posted_findings") or 0)
posted_todos=int(th.get("posted_todos") or 0)
if os.environ.get("DOWNGRADED"):
    line=(f"{dec} at {sha} — downgraded: {os.environ.get('OPEN_THREADS','0')} open thread(s) "
          "(blocking, TODO, or carried-failed); reply the fix SHA and RESOLVE each — or carry "
          "architecture-scale items to an issue — then push (any commit) and re-arm; a same-head re-arm is refused.")
elif dec=="APPROVE":
    line=f"APPROVE at {sha} — all pillars clean, all threads closed. Label consumed; re-arm with the label to review again."
else:
    plural="finding" if n==1 else "findings"
    if carried:
        # per-class truth (#685 r6): a mixed 'posted' number counted TODO
        # threads as blocking findings — the counts must not lie in either
        # direction. Posted numbers come from the publisher's summary;
        # carried entries carry their own class.
        c_f=sum(1 for c in carried if c.get("cls")!="todo")
        c_t=sum(1 for c in carried if c.get("cls")=="todo")
        parts=[f"{dec} at {sha} — {n} blocking {plural}"]
        if int(th.get("posted_todos") or 0) or c_t:
            parts[0]=f"{dec} at {sha} — {n} blocking {plural} + {int(th.get('posted_todos') or 0)+c_t} TODO(s)"
        parts.append(f"{posted} posted as review threads ({posted_findings} findings, {posted_todos} TODOs)")
        parts.append(f"{len(carried)} could not anchor (line not in the diff or beyond the cap: {c_f} findings, {c_t} TODOs) and are carried below")
        line="; ".join(parts)+"; close them, then re-arm with the label to re-review."
    else:
        if n==0 and (review.get("coverage") or {}).get("frontier"):
            # The frontier-only round (#704): zero findings, the declared
            # frontier IS the blocking statement — "close them" would lie.
            nf=len((review.get("coverage") or {}).get("frontier") or [])
            line=(f"{dec} at {sha} — no new blocking findings; coverage incomplete: {nf} file(s) "
                  "never fully examined (frontier below). Re-arm to spend the next round on it.")
        else:
            line=f"{dec} at {sha} — {n} blocking {plural} posted as review threads; close them, then re-arm with the label to re-review."
body=line
if carried:
    body+="\n\nUnanchorable this round (the host rejected the inline anchor) — the finding stands:\n"
    # every carried item stays addressable ON the PR (#685 r8): the
    # [:10]-plus-punt dropped real findings into an ephemeral artifact.
    # 30 in-body + explicit fallback beyond (a pathological ledger).
    for c in carried[:30]:
        # THE display cap (r9/r12): the single authority — a body already
        # ending [budget-capped] is FINAL (the run record holds the same
        # cut text; promising recovery would lie); a whole body beyond
        # 1600 truncates with the run-record pointer, which is true.
        b=str(c.get("body",""))
        if b.endswith("[budget-capped]"):
            shown=b
        elif len(b)<=1600:
            shown=b
        else:
            shown=b[:1600]+(" …[truncated %d chars — full text in the run record (timeline, 7d)]" % (len(b)-1600))
        body+="\n- **%s:%s** %s" % (c.get("path","?"), c.get("line","?"), shown)
    if len(carried)>30: body+="\n- … %d more in the run record (timeline, 7d — ledger pathological: inspect the round)" % (len(carried)-30)
    if int(th.get("budget_capped") or 0):
        body+="\n\n(carried budget: %d bodies CUT at 1600 chars — the run record holds the same cut text; counts above are exact)" % int(th.get("budget_capped"))
    if int(th.get("artifact_bound") or 0):
        body+="\n\n(artifact bound: %d carried entries beyond the list budget appear as count-only rows — no body text)" % int(th.get("artifact_bound"))
# Coverage projection (#704): one compact block — what this round declares
# examined (at this head) and what it explicitly does NOT (the frontier,
# bounded at 10 entries like the carried list). When the frontier
# downgraded an APPROVE, the note says so — the verdict must not look
# like a thread-driven REQUEST_CHANGES when the driver was coverage.
# Coverage projection (#704): the counts ride THE LINE (r7 briefness —
# the verdict is one line + trailer; body newlines are earned by DATA,
# like the carried-findings list above). The frontier list (what the
# author and the next round need) rides the body, bounded at 10 entries.
# The carried branch builds its own composite line — the counts clause
# splices only there; the frontier list still appends after the carried
# entries (data, not counts).
if cov_declared and not carried:
    line += "; coverage: %d file(s) covered at this head, frontier %d" % (cov.get("covered_now") or 0, cov.get("frontier_now") or 0)
    body = line
if cov_declared:
    if cov.get("downgraded"):
        body+="\n\nDowngraded from APPROVE: the declared frontier means the surface was not fully reviewed — the next round spends its budget there first."
    frontier=review.get("coverage",{}).get("frontier") or []
    if frontier:
        body+="\n\nFrontier (not examined this round — carried to the next):"
        for e in frontier[:10]:
            r=e.get("reason") or "not examined"
            body+="\n- **%s** — %s" % (e.get("path","?"), r)
        if len(frontier)>10:
            body+="\n- … %d more (full list in the round's coverage-ledger.json on the shared store)" % (len(frontier)-10)
print(json.dumps({"body": body+"\n\n<!-- pr-review: "+dec+" @ "+sha+" -->"}))
PYEOF
)

# ── Verdict comment (r7, owner directive; repositioned #685 r5): posted
# AFTER the thread publisher so its counts are TRUE. The first cut
# claimed "N blocking findings posted as review threads" before the
# publisher ran — a finding whose anchor the host 422s (line not in the
# diff) never reached the PR and existed nowhere (review.json is
# workspace-ephemeral): the platform asserted a thread the forge never
# took, for five consecutive rounds on this very PR (the reviewer's
# r1-r4 anchors all 422'd — which is ALSO how a real blocking finding
# survived undelivered). The one-line counts clause stays; findings the
# publisher could not land are CARRIED in the body below it — visible,
# addressable, and honest about being unanchorable. review.json's
# analysis prose is still never posted: the threads + the carried list
# are the findings record.
curl -fsSL -X POST -H "authorization: token $TOKEN" -H "content-type: application/json" \
  "$API_BASE/repos/$REPO/issues/$PR_NUM/comments" -d "$VERDICT_COMMENT" >/dev/null \
  || { echo "ERROR: issue comment rejected">&2; exit 1; }
log "verdict comment posted to $REPO#$PR_NUM ($DEC)"

# ── Merge status (the merge-time re-check, project-independent): the
# verdict is bound to this head as a commit status ANY project may
# require in branch protection (adopting it is per-repo data — the same
# context name everywhere: pr-review/verdict). success ⇔ final APPROVE
# (which now MEANS: reviewed at this head with every thread resolved or
# carried); failure on REQUEST_CHANGES; nothing posted when the gate
# could not evaluate (a missing context blocks exactly like a failing
# one where required — the fail-closed posture). NON-FATAL: a status
# transport failure must never skip the consume step; it speaks in the
# artifact. GitLab: not wired (the descoped dialect), speaks the skip.
STATUS_STATE=""; STATUS_NOTE=""
if [ "${IS_GITLAB:-}" = "true" ]; then
  STATUS_NOTE="gitlab-not-wired"
elif echo "${REVIEWED_SHA:-}" | grep -qE '^[0-9a-f]{7,40}$'; then
  if [ "$DEC" = "APPROVE" ] && [ "$GATE_STATUS" = "evaluated" ]; then
    STATUS_STATE="success"
    STATUS_DESC="reviewed: APPROVE, all threads closed"
  else
    STATUS_STATE="failure"
    STATUS_DESC="reviewed: REQUEST_CHANGES — open threads must be resolved/carried"
  fi
  STATUS_CODE="$(curl -sS --max-time 20 -o /tmp/.pr-status.$$ -w "%{http_code}" -X POST \
    -H "authorization: token $TOKEN" -H "content-type: application/json" \
    -d "{\"state\":\"$STATUS_STATE\",\"context\":\"pr-review/verdict\",\"description\":\"$STATUS_DESC\"}" \
    "$API_BASE/repos/$REPO/statuses/$REVIEWED_SHA" 2>/dev/null || true)"
  case "$STATUS_CODE" in
    201|200) : ;;
    *) STATUS_NOTE="status-post-failed-http-${STATUS_CODE:-000}: $(head -c 80 /tmp/.pr-status.$$ 2>/dev/null | tr '\n\r\t' '   ' | tr -d '"' | tr '\\' '/')"
       # tr, not trust: the note splices into the double-quoted artifact
       # JSON — a raw error body (GitHub 422s are JSON: quotes, braces)
       # would unbalance the line and lastJSONLine would zero the ENTIRE
       # round artifact (#685 r1 blocking: the loss fires exactly when the
       # status could not be posted). Quotes drop, backslashes slash,
       # control characters space — the note is diagnostic, not data.
       log "WARN: verdict status post rejected (${STATUS_CODE:-000}) — verdict stands; $STATUS_NOTE" ;;
  esac
  rm -f /tmp/.pr-status.$$
else
  STATUS_NOTE="status-skipped-sha-invalid"
fi

log "removing label '$LABEL'…"
curl -fsSL -X DELETE -H "authorization: token $TOKEN" -H "accept: application/json" \
  "$API_BASE/repos/$REPO/issues/$PR_NUM/labels/$LABEL" 2>/dev/null||log "WARN: could not remove label"
# r11: the artifact line is BUILT IN PYTHON, not echo-interpolated — the
# echo shipped the whole carried list through a bash string (quoting
# hazards) and its size rode the process env; json.dumps makes the line
# and the budget keeps it far under lastJSONLine's 1MB scanner cap.
DEC="$DEC" GATE_STATUS="$GATE_STATUS" STATUS_STATE="$STATUS_STATE" STATUS_NOTE="$STATUS_NOTE" \
python3 - <<'PYARTIFACT'
import json, os
H=os.environ["HOST"]; R=os.environ["REPO"]; P=int(os.environ["PR_NUM"])
ev={"host":H,"repo":R,"number":P,
    "decision":os.environ["DEC"],"thread_gate":os.environ["GATE_STATUS"]}
ss=os.environ.get("STATUS_STATE",""); sn=os.environ.get("STATUS_NOTE","")
if ss:
    ev["status_post"]={"state":ss,"context":"pr-review/verdict",**({"note":sn} if sn else {})}
else:
    ev["status_post"]={"note":sn}
carry=json.load(open(os.environ["CARRY_JSON_FILE"]))
ev["carry"]={k:carry.get(k) for k in ("minted","notes")}
ev["inline_threads"]=json.load(open(os.environ["THREADS_FILE"]))
# compact separators: the legacy echo's byte shape (no spaces) is part of
# the contract — consumers grep the line, not parse-and-rebuild.
print(json.dumps({"artifact":"pr-%s-%s-%d"%(H,R,P),"status":"ok","event":ev},separators=(",",":")))
PYARTIFACT
rm -f "$THREAD_STATUS_FILE"
