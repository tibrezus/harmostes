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
export API_BASE TOKEN HOST REPO PR_NUM REVIEW LABEL IS_FJ IS_GITLAB WORKDIR
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
PAYLOAD=$(python3 << 'PYEOF'
import json, os
with open(os.environ["REVIEW"]) as f: review=json.load(f)
d=review["decision"]
# Forgejo review events: APPROVED | REQUEST_CHANGES | COMMENT. Canonical
# skill decisions: APPROVE | REQUEST_CHANGES | COMMENT.
if os.environ.get("IS_FJ")=="true":
    event={"APPROVE":"APPROVED"}.get(d, d if d in ("REQUEST_CHANGES","COMMENT") else "COMMENT")
else:
    event=d
p={"body":review["body"],"event":event}
cs=review.get("comments",[])
if cs:
    if os.environ.get("IS_FJ")=="true": p["comments"]=[{"path":c["path"],"line":int(c.get("line",1)),"body":c["body"]} for c in cs]
    else: p["comments"]=[{"path":c["path"],"line":int(c.get("line",1)),"side":c.get("side","RIGHT"),"body":c["body"]} for c in cs]
print(json.dumps(p))
PYEOF
)
DEC=$(python3 -c "import json;print(json.load(open('$REVIEW'))['decision'])")

# ── Inline findings are the AGENT's voice now: during the run it posts
# anchored threads via review-api (github/gitlab/forgejo backends), replies
# with fix SHAs, and resolves. This plugin keeps only the VERDICT and the
# unresolved-thread GATE below — no duplicate posting. ──
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
# databaseId (REST never shows a GraphQL-side resolve, C1); Forgejo = always
# false (closure is a closing reply); GitLab = native flag. On a listing
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
    # silently hid every thread past 30).
    sep = "&" if "?" in path else "?"
    out, page = [], 1
    while True:
        part = get(f"{path}{sep}per_page=100&page={page}")
        out += part
        if len(part) < 100:
            return out
        page += 1
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
        for c in cs: c["resolved"]=False  # Forgejo resolves by closing reply
        print(json.dumps(cs)); raise SystemExit
    cs=paged(f"/repos/{repo}/pulls/{pr}/comments")
    owner, name = repo.split("/", 1)
    q={"query":'{ repository(owner: "%s", name: "%s") { pullRequest(number: %s) { reviewThreads(first: 100) { nodes { isResolved comments(first: 100) { nodes { databaseId } } } } } } }' % (owner, name, pr)}
    req=urllib.request.Request(base+"/graphql", data=json.dumps(q).encode(),
        headers={"authorization": f"bearer {tok}", "content-type": "application/json"})
    nodes=(json.load(urllib.request.urlopen(req, timeout=30))
           .get("data",{}).get("repository",{}).get("pullRequest",{})
           .get("reviewThreads",{}).get("nodes") or [])
    resolved=set()
    for t in nodes:
        if t.get("isResolved"):
            for cm in (t.get("comments",{}).get("nodes") or []):
                if cm.get("databaseId") is not None: resolved.add(cm["databaseId"])
    for c in cs: c["resolved"]=c["id"] in resolved
    print(json.dumps(cs))
except SystemExit:
    raise
except Exception as e:
    fail(f"{type(e).__name__}: {e}"[:120])
PYEOF
)
GATE_STATUS="evaluated"; [ -s "$GATE_STATUS_FILE" ] && GATE_STATUS="$(cat "$GATE_STATUS_FILE")"; rm -f "$GATE_STATUS_FILE"
NEWDEC=$(printf '%s' "$CS_JSON" | python3 - << 'PYEOF'
# GATE-CLASSIFIER-START (tested verbatim by TestPostReviewGateClassifier —
# everything between the markers must be a self-contained script)
import json, os, sys
cs=json.load(sys.stdin)
sha=os.environ.get("REVIEWED_SHA","")
if cs is None:
    print("APPROVE"); raise SystemExit   # fetch failed — fail-open with a loud WARN upstream
dec="APPROVE"
if cs and "resolvable" in cs[0]:
    # GitLab dialect: native resolve is authoritative.
    open_threads=[c for c in cs
        if c["resolvable"] and not c["resolved"]
        and str(c.get("commit_id") or "")!=sha]
else:
    # GitHub/Forgejo dialect: a thread is CLOSED by real resolve state
    # (C1 — a GraphQL-side resolve leaves no reply) or by a reply.
    replied={c["in_reply_to"] for c in cs if c.get("in_reply_to")}
    open_threads=[c for c in cs
        if not c.get("in_reply_to")
        and c["id"] not in replied
        and not c.get("resolved")
        # No commit_id → round unattributable: never downgrade on it (C4).
        and c.get("commit_id") not in (None, "")
        and str(c.get("commit_id"))!=sha]
if open_threads:
    dec="REQUEST_CHANGES"
    for c in open_threads[:10]:
        print(f"[post-review] unresolved thread {c.get('path','?')}:{c.get('line','?')} id={c.get('id')} — reply+resolve required before APPROVE", file=sys.stderr)
print(dec)
# GATE-CLASSIFIER-END
PYEOF
)
if [ "$NEWDEC" != "$DEC" ]; then
  log "APPROVE downgraded: unresolved prior-round threads"
  DEC="$NEWDEC"
  # S1 (r26): the downgrade must rewrite the WHOLE verdict — decision field
  # AND the trailer in the body — or dw_wait_review keeps polling APPROVE
  # and merges over open threads. The trailer shape is review.go:733's
  # verdictTrailer (the contract's canonical home) — keep both in step.
  python3 - "$REVIEW" "$DEC" << 'PYEOF'
import json,re,sys
p,newdec=sys.argv[1],sys.argv[2]; r=json.load(open(p))
r["decision"]=newdec
r["body"]=re.sub(r"pr-review:\s*[A-Z_]+", f"pr-review: {newdec}", r["body"])
r["body"] += "\n\n---\n\n**Downgraded from APPROVE: unresolved review threads from prior rounds exist.** Address each (reply with the fix SHA), resolve the thread, and re-arm.\n"
json.dump(r, open(p,"w"))
PYEOF
fi
fi

# The verdict as a plain issue/PR comment — the ONE surface (verified on
# both hosts) that always renders and always carries the trailer (the
# merge currency dw_wait_review polls for). The old secondary review
# event (/pulls/N/reviews) was removed: as the PR author, the shared
# identity gets "reject your own pull is not allowed" — it errored on
# every run and rendered nowhere (#29).
BODY=$(python3 - << 'PYEOF'
import json, os
with open(os.environ["REVIEW"]) as f: review=json.load(f)
body=review["body"]
cs=review.get("comments",[])
if cs:
    body += "\n\n---\n\n**Inline findings**\n\n"
    for c in cs:
        body += f"- `{c['path']}:{c.get('line','?')}` — {c['body']}\n"
print(json.dumps({"body": body}))
PYEOF
)
curl -fsSL -X POST -H "authorization: token $TOKEN" -H "content-type: application/json" \
  "$API_BASE/repos/$REPO/issues/$PR_NUM/comments" -d "$BODY" >/dev/null \
  || { echo "ERROR: issue comment rejected">&2; exit 1; }
log "verdict comment posted to $REPO#$PR_NUM ($DEC)"

log "removing label '$LABEL'…"
curl -fsSL -X DELETE -H "authorization: token $TOKEN" -H "accept: application/json" \
  "$API_BASE/repos/$REPO/issues/$PR_NUM/labels/$LABEL" 2>/dev/null||log "WARN: could not remove label"
echo "{\"artifact\":\"pr-$HOST-$REPO-$PR_NUM\",\"status\":\"ok\",\"event\":{\"host\":\"$HOST\",\"repo\":\"$REPO\",\"number\":$PR_NUM,\"decision\":\"$DEC\",\"thread_gate\":\"$GATE_STATUS\"}}"
