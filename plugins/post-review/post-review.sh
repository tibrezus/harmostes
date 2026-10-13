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
#      only escape is the ARCHITECTURE CARRY: a design-scale finding
#      outliving the diff becomes ONE deduped tracker issue (milestone =
#      the repo's most recent open milestone), referenced by reply on the
#      thread, then resolved. Carried threads do not downgrade; the issue
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
# scale items escape the PR as ONE deduped tracker issue each. Inputs: the
# ledger (todos[] with disposition "architecture-carry") and the thread
# listing (open roots whose body opens with the Architecture-carry
# prefix — the skill's machine-read class grammar; agent-posted threads
# the agent could not mint for). Dedup: a body-referenced issue or an
# OPEN issue with the identical title is REFERENCED, never re-minted.
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
# 1) open milestones — the carry assigns the most recent open one.
_,msraw=curl("GET", "/repos/%s/milestones?state=open" % repo)
try: ms=json.loads(msraw)
except Exception: ms=[]
milestone=None
if isinstance(ms,list) and ms:
    # "most recent open" = newest CREATED — NOT ms[0]: GitHub's /milestones
    # default sort is due_on ASC (ms[0] is the OLDEST — verified live #685
    # r2 on this very repo) and no cross-host sort param exists, so the
    # ordering is computed client-side from the serialized timestamps
    # (#685 r2: both wired hosts carry created_at on milestone objects).
    def _ms_key(m):
        return str(m.get("created_at") or m.get("updated_at") or "")
    newest=max(ms, key=_ms_key)
    milestone=newest.get("id") if is_fj else newest.get("number")
    if len(ms)>1: notes.append("milestone: %d open, using newest (%s)" % (len(ms), newest.get("title")))
else:
    notes.append("no open milestone — carried issue minted unmilestoned (assign manually)")
# 2) open issues for exact-title dedup.
if is_fj:
    _,israw=curl("GET", "/repos/%s/issues?state=open&type=issues&limit=50" % repo)
else:
    # GitHub has no type=issues filter on /issues and no limit param (both
    # 422 — probed live #685 r1): per_page + PRs skipped client-side (a
    # same-title PR is not a dedup hit).
    _,israw=curl("GET", "/repos/%s/issues?state=open&per_page=50" % repo)
open_titles={}
try:
    issues=json.loads(israw)
    if isinstance(issues,list):
        for i in issues:
            if not is_fj and i.get("pull_request"): continue
            t=(i.get("title") or "").strip()
            if t: open_titles[t]=i.get("number")
except Exception:
    pass
def ensure_issue(title, body):
    if title in open_titles:
        return open_titles[title], False
    payload={"title":title,"body":body}
    if milestone is not None: payload["milestone"]=milestone
    ok,resp=curl("POST", "/repos/%s/issues" % repo, payload)
    try: r=json.loads(resp)
    except Exception: r={}
    if ok and isinstance(r,dict) and r.get("number"):
        return r["number"], True
    return None, False
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
    if ref and re.search(r"\d+", str(ref)):
        num=re.search(r"\d+", str(ref)).group(0)
    else:
        num,minted_here=ensure_issue(title, "from PR #%s — %s\n(carried by the review pipeline; milestone auto-assigned)" % (pr, body))
        if num is None:
            notes.append("carry mint FAILED for: %s" % title[:60])
            continue
        if minted_here: minted.append({"issue":num,"title":title})
    tref=t.get("thread_ref")
    if tref:
        rid=str(tref).split("/")[-1]
        reply_thread(rid, "carried to #%s — tracked as architecture follow-up; the thread closes here" % num)
        if resolve(rid): resolved_ids.append(str(rid))
        else: notes.append("resolve FAILED for thread %s (issue #%s exists — resolve manually)" % (rid, num))
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
        if m:
            num=m.group(1)
        else:
            first=body.replace("**Architecture-carry**","").strip()
            first=first.splitlines()[0][:140] if first else "architecture follow-up"
            title="architecture: "+first
            num,minted_here=ensure_issue(title, "from PR #%s thread %s — %s\n(carried by the review pipeline)" % (pr, c["id"], body))
            if num is None:
                notes.append("carry mint FAILED for thread %s" % c["id"])
                continue
            if minted_here: minted.append({"issue":num,"title":title})
        reply_thread(c["id"], "carried to #%s — tracked as architecture follow-up; the thread closes here" % num)
        if resolve(c["id"]): resolved_ids.append(str(c["id"]))
        else: notes.append("resolve FAILED for thread %s (issue #%s exists — resolve manually)" % (c["id"], num))
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
summary={"posted":posted,"posted_findings":posted_findings,"posted_todos":posted_todos,"rejected":rejected,"capped":capped,"todos":len(todo_bodies)}
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
THREADS=$(cat "$THREAD_STATUS_FILE")
# The scan-failed flag (written by the dedupe scan on failure) splices into
# WHATEVER shape the file carries — publish, skip, or default (r4 P8
# blocker: reading+rm'ing the flag inside a branch made the splice dead).
if [ -s "$DEDUPE_FLAG_FILE" ]; then
  FLAG="$(cat "$DEDUPE_FLAG_FILE")"
  THREADS="${THREADS%\}}${FLAG}}"
fi
rm -f "$THREAD_STATUS_FILE" "$DEDUPE_FLAG_FILE"
export THREADS


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
# The publisher's true counts + everything it could not land (#685 r5):
# rejected anchors, lineless findings, and the 20-thread cap overage all
# ride here — the verdict may not claim a thread the forge never took.
try: th=json.loads(os.environ.get("THREADS") or "{}")
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
        line=f"{dec} at {sha} — {n} blocking {plural} posted as review threads; close them, then re-arm with the label to re-review."
body=line
if carried:
    body+="\n\nUnanchorable this round (the host rejected the inline anchor) — the finding stands:\n"
    # every carried item stays addressable ON the PR (#685 r8): the
    # [:10]-plus-punt dropped real findings into an ephemeral artifact.
    # 30 in-body + explicit fallback beyond (a pathological ledger).
    for c in carried[:30]:
        # THE display cap (r9): the single authority — the artifact's
        # carried entry is complete, so this marker's pointer is true.
        b=str(c.get("body",""))
        shown=b if len(b)<=1600 else b[:1600]+(" …[truncated %d chars — full text in the run record (timeline, 7d)]" % (len(b)-1600))
        body+="\n- **%s:%s** %s" % (c.get("path","?"), c.get("line","?"), shown)
    if len(carried)>30: body+="\n- … %d more in the run record (timeline, 7d — ledger pathological: inspect the round)" % (len(carried)-30)
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
CARRY_SUMMARY=$(printf '%s' "$CARRY" | python3 -c "import json,sys;d=json.load(sys.stdin);print(json.dumps({k:d[k] for k in ('minted','notes')}))")
if [ -n "$STATUS_STATE" ]; then
  STATUS_FIELD="\"status_post\":{\"state\":\"$STATUS_STATE\",\"context\":\"pr-review/verdict\""
  [ -n "$STATUS_NOTE" ] && STATUS_FIELD="$STATUS_FIELD,\"note\":\"$STATUS_NOTE\""
  STATUS_FIELD="$STATUS_FIELD},"
else
  STATUS_FIELD="\"status_post\":{\"note\":\"$STATUS_NOTE\"},"
fi
echo "{\"artifact\":\"pr-$HOST-$REPO-$PR_NUM\",\"status\":\"ok\",\"event\":{\"host\":\"$HOST\",\"repo\":\"$REPO\",\"number\":$PR_NUM,\"decision\":\"$DEC\",\"thread_gate\":\"$GATE_STATUS\",${STATUS_FIELD}\"carry\":$CARRY_SUMMARY,\"inline_threads\":$THREADS}}"
