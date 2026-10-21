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
# Coverage block (#704): OPTIONAL — absent means the round makes no
# coverage claim (legacy rounds, full back-compat). Present, it is the
# round's honest statement of what it examined:
#   covered  — files FULLY reviewed at reviewed_sha (path required)
#   frontier — files seen but not examined, or never reached (path
#              required; reason free-text, defaulted downstream)
# The APPROVE contract: a declared frontier means the surface was NOT
# fully reviewed — APPROVE is refused (declare REQUEST_CHANGES with the
# frontier, or examine it). Symmetrically, a frontier-only round (no
# findings) is a legitimate REQUEST_CHANGES: the unfinished coverage IS
# the blocking statement, and the next round spends its budget there.
cov=review.get("coverage")
frontier_count=0
if cov is not None:
    if not isinstance(cov,dict):
        print('ERROR: coverage must be an object {covered:[], frontier:[]}',file=sys.stderr);sys.exit(1)
    for key in ("covered","frontier"):
        entries=cov.get(key,[])
        if entries is None: entries=[]
        if not isinstance(entries,list):
            print(f"ERROR: coverage.{key} must be a list",file=sys.stderr);sys.exit(1)
        for i,e in enumerate(entries):
            if not isinstance(e,dict) or not isinstance(e.get("path"),str) or not e.get("path").strip():
                print(f"ERROR: coverage.{key}[{i}] must have a non-empty path",file=sys.stderr);sys.exit(1)
    frontier_count=len(cov.get("frontier") or [])
    if d=="APPROVE" and frontier_count:
        print(f"ERROR: APPROVE with a declared frontier ({frontier_count} file(s) not examined) — the surface was not fully reviewed; "
              "either examine the frontier or declare REQUEST_CHANGES carrying it",file=sys.stderr);sys.exit(1)
# Completeness (#704 r1): an APPROVE's coverage must account for the WHOLE
# changed surface — every file in pr-context.json's files_changed is either
# covered or frontiered. Declaring 1 of 12 and approving is the coverage
# lie the frontier contract exists to prevent. (REQUEST_CHANGES may
# honestly declare partial coverage — only APPROVE claims the whole.)
ctx_path=os.path.join(os.path.dirname(path),"pr-context.json")  # defined later for the sha check; needed here first
ctx=None
try:
    with open(ctx_path) as f: ctx=json.load(f)
except (FileNotFoundError, json.JSONDecodeError): ctx=None
if cov is not None and d=="APPROVE" and isinstance(ctx,dict):
    changed={f.get("filename") for f in (ctx.get("files_changed") or []) if isinstance(f,dict)}
    declared={e.get("path") for e in cov.get("covered") or []}|{e.get("path") for e in cov.get("frontier") or []}
    missing=sorted(changed-declared)
    if missing:
        print(f"ERROR: APPROVE coverage incomplete — {len(missing)} changed file(s) neither covered nor frontiered "
              f"(first: {', '.join(missing[:3])}) — examine them, declare them frontier, or do not APPROVE",file=sys.stderr);sys.exit(1)
# Mandatory declaration after carry (#704 r1): when the workspace carried a
# ledger (pr-context carries the coverage fact block — only happens when a
# ledger existed), a round that declares NOTHING would silently reset it in
# the deploy's merge. The advisory becomes a requirement exactly there.
if cov is None and isinstance(ctx,dict) and isinstance(ctx.get("coverage"),dict):
    print("ERROR: this round carried a coverage ledger but declared no coverage block — "
          "write coverage {covered:[], frontier:[]} with the round's honest statement "
          "(an undeclared round resets the ledger and the next round loses the frontier)",file=sys.stderr);sys.exit(1)
if d=="REQUEST_CHANGES" and not findings and not frontier_count:
    print("ERROR: REQUEST_CHANGES requires at least one blocking finding in findings (threads are the review) — or a coverage frontier when the round ran out of budget before the surface (the frontier is the blocking statement)",file=sys.stderr);sys.exit(1)
for i,c in enumerate(findings):
    if not isinstance(c,dict) or not c.get("path") or not c.get("body"):
        print(f"ERROR: findings[{i}] must have path+body",file=sys.stderr);sys.exit(1)
# TODO lane: completely-missing pieces ride todos[]. They publish as
# threads of the round and count in the verdict like any thread (the
# closure mandate): disposition says how each ends —
#   fix-in-pr (default): the dev fixes it in this PR, replies, resolves.
#   architecture-carry: a design-scale item outliving the diff; the
#     deploy carries it (one deduped issue, milestone-assigned, thread
#     resolved) — never blocking, never a finding.
todos=review.get("todos",[])
if not isinstance(todos,list):
    print("ERROR: todos must be a list",file=sys.stderr);sys.exit(1)
for i,t in enumerate(todos):
    if not isinstance(t,dict) or not t.get("path") or not t.get("body"):
        print(f"ERROR: todos[{i}] must have path+body",file=sys.stderr);sys.exit(1)
    disp=t.get("disposition","fix-in-pr")
    if disp not in ("fix-in-pr","architecture-carry"):
        print(f"ERROR: todos[{i}] disposition must be fix-in-pr|architecture-carry (got {disp!r})",file=sys.stderr);sys.exit(1)
    if t.get("issue") is not None and not isinstance(t.get("issue"),str):
        print(f"ERROR: todos[{i}] issue must be a string reference (e.g. '#431')",file=sys.stderr);sys.exit(1)
review["decision"]=d
# Skill output contract (v2): reviewed_sha must equal the PR head SHA and
# ── Output contract (#680): the LEDGER. findings[] carries blocking
# findings, each posted by the agent itself as a native anchored thread
# as it was verified (thread_ref = the platform reference once posted).
# The deploy publishes ONLY remainders (entries without thread_ref —
# session-death resilience) and composes the one-line verdict from the
# threads actually on the PR. Legacy "comments" is accepted as an alias
# for findings (one shape at the boundary, normalized here). MINOR/NIT
# never enter the ledger; todos[] are completely-missing pieces that
# BLOCK THEIR OWN ROUND under the thread-closure mandate (disposition
# fix-in-pr default: fixed here, reply+resolve; architecture-carry:
# carried to a tracker issue by the deploy).
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
