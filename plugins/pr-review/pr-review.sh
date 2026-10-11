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
    print('Write VALID JSON: {"decision":"APPROVE|REQUEST_CHANGES","reviewed_sha":"<head sha>","comments":[]}',file=sys.stderr)
    sys.exit(1)
d=str(review.get("decision","")).upper()
# #680: findings[] is the ledger's canonical key; comments[] is the
# legacy alias, tolerated one shape at this boundary and normalized away.
findings=review.get("findings", review.get("comments",[]))
if "comments" in review:
    print("WARN: legacy comments[] — normalized to findings[] (update the task contract)",file=sys.stderr)
if d not in {"APPROVE","REQUEST_CHANGES","COMMENT"}:
    print(f'ERROR: decision must be APPROVE/REQUEST_CHANGES/COMMENT, got "{d}"',file=sys.stderr);sys.exit(1)
if not isinstance(findings,list): print("ERROR: findings must be a list",file=sys.stderr);sys.exit(1)
if d=="REQUEST_CHANGES" and not findings:
    print("ERROR: REQUEST_CHANGES requires at least one blocking finding in findings (threads are the review)",file=sys.stderr);sys.exit(1)
for i,c in enumerate(findings):
    if not isinstance(c,dict) or not c.get("path") or not c.get("body"):
        print(f"ERROR: findings[{i}] must have path+body",file=sys.stderr);sys.exit(1)
# TODO lane (r7-r10): completely-missing pieces ride todos[] — anchored as
# non-blocking threads the dev must address; they never change the decision.
todos=review.get("todos",[])
if not isinstance(todos,list):
    print("ERROR: todos must be a list",file=sys.stderr);sys.exit(1)
for i,t in enumerate(todos):
    if not isinstance(t,dict) or not t.get("path") or not t.get("body"):
        print(f"ERROR: todos[{i}] must have path+body",file=sys.stderr);sys.exit(1)
review["decision"]=d
# Skill output contract (v2): reviewed_sha must equal the PR head SHA and
# ── Output contract (#680): the LEDGER. findings[] carries blocking
# findings, each posted by the agent itself as a native anchored thread
# as it was verified (thread_ref = the platform reference once posted).
# The deploy publishes ONLY remainders (entries without thread_ref —
# session-death resilience) and composes the one-line verdict from the
# threads actually on the PR. Legacy "comments" is accepted as an alias
# for findings (one shape at the boundary, normalized here). MINOR/NIT
# never enter the ledger; todos[] are completely-missing pieces —
# non-blocking, anchored by the deploy, followed up next round.
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
# The verdict trailer is embedded by the DEPLOY step in the one-line
# verdict it builds — the agent does not write prose, so there is no body
# for it to end with (r7; the old body-trailer check here stranded the
# rhesadox agent in the 4-attempt gate loop).
review["findings"]=findings; review.pop("comments",None)
with open(path,"w") as f: json.dump(review,f,indent=2)
posted=sum(1 for c in findings if c.get("thread_ref"))
print(f"review.json valid: decision={d} sha={sha[:12]} findings={len(findings)} (agent-posted: {posted}, remainders: {len(findings)-posted})")
PYEOF
echo '{"status":"ok"}'
