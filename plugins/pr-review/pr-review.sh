#!/usr/bin/env bash
set -euo pipefail
REVIEW="${HARMOSTES_WORKDIR:-/workspace}/review.json"
[ -f "$REVIEW" ] || { echo "ERROR: review.json not found at $REVIEW — the agent MUST write its review there" >&2; exit 1; }
python3 << 'PYEOF'
import json,sys,os
path=os.environ.get("HARMOSTES_WORKDIR","/workspace")+"/review.json"
raw=open(path).read().strip()
if raw.startswith('```'):
    lines=raw.split('\n')
    if lines[0].startswith('```') and lines[-1].strip()=='```':
        raw='\n'.join(lines[1:-1])
try: review=json.loads(raw)
except json.JSONDecodeError as e:
    print(f"ERROR: review.json invalid JSON: {e}",file=sys.stderr)
    print(f"First 300 chars: {raw[:300]}",file=sys.stderr)
    print('Write VALID JSON: {"decision":"APPROVE","body":"...","comments":[]}',file=sys.stderr)
    sys.exit(1)
d=str(review.get("decision","")).upper()
body=review.get("body","");comments=review.get("comments",[])
if d not in {"APPROVE","REQUEST_CHANGES","COMMENT"}:
    print(f'ERROR: decision must be APPROVE/REQUEST_CHANGES/COMMENT, got "{d}"',file=sys.stderr);sys.exit(1)
if not body.strip(): print("ERROR: body is empty — write a review summary",file=sys.stderr);sys.exit(1)
if not isinstance(comments,list): print("ERROR: comments must be a list",file=sys.stderr);sys.exit(1)
for i,c in enumerate(comments):
    if not isinstance(c,dict) or not c.get("path") or not c.get("body"):
        print(f"ERROR: comments[{i}] must have path+body",file=sys.stderr);sys.exit(1)
review["decision"]=d
# Skill output contract (v2): reviewed_sha must equal the PR head SHA and
# the body must END with the verdict trailer — the merge-currency token.
sha=review.get("reviewed_sha","")
ctx_path=os.path.join(os.path.dirname(path),"pr-context.json")
head=os.environ.get("HARMOSTES_PR_HEAD_SHA","")
try:
    with open(ctx_path) as f: head=head or json.load(f).get("head_sha","")
except FileNotFoundError: pass
if not sha:
    print("ERROR: review.json missing reviewed_sha (head SHA of /workspace/repo)",file=sys.stderr);sys.exit(1)
if head and sha!=head:
    print(f"ERROR: reviewed_sha {sha[:12]} != PR head {head[:12]} — review the current head",file=sys.stderr);sys.exit(1)
trailer=f"<!-- pr-review: {d} @ {sha} -->"
if not body.rstrip().endswith(trailer):
    print("ERROR: body must end with the exact trailer: " + trailer,file=sys.stderr);sys.exit(1)
with open(path,"w") as f: json.dump(review,f,indent=2)
print(f"review.json valid: decision={d} sha={sha[:12]} comments={len(comments)}")
PYEOF
echo '{"status":"ok"}'
