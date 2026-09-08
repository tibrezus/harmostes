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

# ── Inline findings are the AGENT's voice now: during the run it posts
# anchored threads via review-api (github/gitlab/forgejo backends), replies
# with fix SHAs, and resolves. This plugin keeps only the VERDICT and the
# unresolved-thread GATE below — no duplicate posting. ──
# ── Unresolved-thread gate (the merge currency, mechanically enforced) ──
# Threads from PRIOR rounds (commit_id != this reviewed_sha) with NO reply
# must be addressed before an APPROVE is lawful. Any open prior thread
# downgrades APPROVE → REQUEST_CHANGES; the reply+resolve protocol lives
# in the pr-review skill.
if [ "$DEC" = "APPROVE" ]; then
export REVIEWED_SHA="$(python3 -c "import json;print(json.load(open('$REVIEW'))['reviewed_sha'])")"
# ── Thread listing is HOST-DIALECT (S2, r26): the three hosts serve review
# comments from different routes with different thread semantics.
#   GitHub : GET /repos/{r}/pulls/{n}/comments — in_reply_to links replies.
#   Forgejo: GET /repos/{r}/pulls/{n}/reviews → per-review comments
#            (the flat /pulls/{n}/comments route 404s — verified live on
#            git.rezus.cloud v16.0.3-rezus.1); replies also use in_reply_to.
#   GitLab : GET /projects/:id/merge_requests/{n}/discussions — native
#            resolvable/resolved flags; position.head_sha dates the thread.
CS_JSON=$(python3 - << 'PYEOF'
import json, os, urllib.request, urllib.parse
base=os.environ["API_BASE"]; tok=os.environ["TOKEN"]
repo=os.environ["REPO"]; pr=os.environ["PR_NUM"]
H={"authorization": f"token {tok}"}
def get(path):
    req=urllib.request.Request(base+path, headers=H)
    return json.load(urllib.request.urlopen(req, timeout=30))
try:
    if os.environ.get("HOST")=="gitlab":
        proj=urllib.parse.quote(repo, safe="")
        ds=get(f"/projects/{proj}/merge_requests/{pr}/discussions")
        out=[]
        for d in ds:
            for n in d.get("notes",[]):
                if n.get("position"):
                    out.append({"id":n["id"], "path":n["position"].get("new_path","?"),
                                "line":n["position"].get("new_line","?"),
                                "commit_id":n["position"].get("head_sha",""),
                                "in_reply_to":None,
                                "resolvable":bool(n.get("resolvable")), "resolved":bool(n.get("resolved"))})
        print(json.dumps(out)); raise SystemExit
    if os.environ.get("IS_FJ")=="true":
        cs=[]
        for r in get(f"/repos/{repo}/pulls/{pr}/reviews"):
            cs+=get(f"/repos/{repo}/pulls/{pr}/reviews/{r['id']}/comments")
        print(json.dumps(cs)); raise SystemExit
    print(json.dumps(get(f"/repos/{repo}/pulls/{pr}/comments")))
except SystemExit:
    raise
except Exception as e:
    import sys as _s
    print(f"[post-review] WARN: unresolved-thread check failed ({e}) — gate NOT evaluated", file=_s.stderr)
    print(json.dumps(None))
PYEOF
)
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
    # GitHub/Forgejo dialect: replies link roots via in_reply_to.
    replied={c["in_reply_to"] for c in cs if c.get("in_reply_to")}
    open_threads=[c for c in cs
        if not c.get("in_reply_to")
        and c["id"] not in replied
        and str(c.get("commit_id") or "")!=sha]
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
  # and merges over open threads.
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
echo "{\"artifact\":\"pr-$HOST-$REPO-$PR_NUM\",\"status\":\"ok\",\"event\":{\"host\":\"$HOST\",\"repo\":\"$REPO\",\"number\":$PR_NUM,\"decision\":\"$DEC\"}}"
