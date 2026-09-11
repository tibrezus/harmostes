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
# ── Divergence ledger (r18-r20 lessons) — check the CLASS, every round ──
# 1. Mutation-probe load-bearing tests (BOGUS the guarded value; red required).
# 2. One fact, one home: grep every home of any fact a fix touches.
# 3. Deployment claims must be falsified against job.go/chart/ops manifests,
#    never accepted from ADR prose (r20: /tmp lineage blocker).
# 4. Author==ADR-author ⇒ adversarial pass on the premise FIRST.
# ── Session continuity (ADR-0010): rounds are ONE lineage. If a prior
# verdict exists at an earlier head: previously-addressed findings stay
# addressed; review the DELTA between heads; carry this ledger forward. ──
# ── Inline review protocol — the merge currency, published by post-review:
# a bullet list is NOT a review. EVERY NEW finding rides review.json's
# "comments" array (path + line + body) — the DEPLOY step posts them as
# native anchored threads deterministically. NEVER post NEW findings via
# the CLI yourself: a self-post duplicates what the deploy publishes.
# You DO use the CLI dialects below on prior rounds: when the gate lists
# open prior-round threads, REPLY on each with the fixing SHA and RESOLVE
# it — that is N round-trips, so budget them alongside the review. An
# APPROVE over an open prior thread is downgraded by post-review; the
# reply+resolve is how threads close. Dialects:
#   gh   (GitHub):  VERIFIED LIVE. Inline: gh api repos/{o}/{r}/pulls/$N/
#        comments -f commit_id=$SHA -f path=F -F line=N -f body="…"
#        reply: POST pulls/$N/comments -f body="…" -F in_reply_to=$ID
#        (the /replies subpath 404s — use in_reply_to)
#        resolve: gh api graphql -f query='mutation($t:ID!){…' -f t=<id>
#        (ID! — String! fails); map databaseId → thread id via a
#        reviewThreads query first
#   fj   (Forgejo/Codeberg): NATIVE since v16.0.3-rezus.1 — create-pull-
#        review takes the whole payload as --body JSON:
#          fj api repo create-pull-review --owner O --repo R --index N \
#            --body '{"body":"summary","event":"COMMENT","commit_id":"<sha>",
#                     "comments":[{"path":"F","new_position":N,"body":"finding"}]}'
#        THE LINE FIELD IS new_position (not new_line — server 500s on
#        new_line: ReverseLineBlame -L 0). Reply via create-pull-review-
#        comment --id <comment-id> --body '{"body":"…","new_position":N,
#        "path":"F"}'. Resolution state is server-side on reviews.
#   glab (GitLab): positioned discussions — fetch diff_refs from the MR
#        first, then glab api projects/:id/merge_requests/$N/discussions
#        -X POST with position{base_sha,start_sha,head_sha,new_path,
#        old_path,new_line}; reply: …/discussions/$ID/notes;
#        resolve: PUT …/discussions/$ID {"resolved":true}
#
# Rules: comments[] carries ONLY blocking findings (CRITICAL/verified
# MAJOR — each becomes a thread that must close before merge). MINOR/NIT
# are dropped, not posted. There is NO long verdict body: the deploy step
# writes the one-line verdict (decision + SHA + blocking count) itself.
# On a re-review of an addressed round: verify the fix in the diff, REPLY
# on the thread with the fixing SHA (the CLI dialects above), then RESOLVE
# it. An APPROVE is lawful ONLY when zero threads remain unresolved —
# post-review downgrades an APPROVE issued over open prior threads. ──
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
