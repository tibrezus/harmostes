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
# post-review always authenticates (POST comment, consume label) — fail fast
# at resolve time rather than at curl time.
TOKEN=$(host::token "$HOST" required)
export API_BASE TOKEN HOST REPO PR_NUM REVIEW LABEL IS_FJ WORKDIR
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
echo "{\"artifact\":\"pr-$HOST-$REPO-$PR_NUM\",\"status\":\"ok\",\"event\":{\"host\":\"$HOST\",\"repo\":\"$REPO\",\"number\":$PR_NUM,\"decision\":\"$DEC\"}}"
