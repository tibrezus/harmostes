#!/usr/bin/env bash
# workspace: deterministic Workspace Provisioning (ADR-0006) — the kernel's
# knowns for the reviewing agent. The Review-Ready Gate (native Go) has
# ALREADY decided: label present ∧ merge-rule contexts green at head SHA.
# This plugin receives the Trigger Envelope via env and provisions the
# workspace: repo clone at the reviewed head SHA, PR context (metadata,
# diff, files, CI), wiki + RIG when configured, tool availability.
# It performs NO decision logic — no label scanning, no polling.
set -euo pipefail
log() { echo "[workspace] $*"; }
. "$(dirname "$0")/../lib/git-host.sh"
WORKDIR="${HARMOSTES_WORKDIR:-/workspace}"

# ── Trigger Envelope (set by the Go gate in the worker) ──────────────────
TRIG_REPO="${HARMOSTES_TRIGGER_REPO:-}"
TRIG_PR="${HARMOSTES_TRIGGER_PR:-}"
HEAD_SHA="${HARMOSTES_TRIGGER_SHA:-}"
TRIG_BASE="${HARMOSTES_TRIGGER_BASE:-}"
TRIG_LABEL="${HARMOSTES_TRIGGER_LABEL:-}"
TRIG_CONTEXTS="${HARMOSTES_TRIGGER_CONTEXTS:-}"

if [ -z "$TRIG_REPO" ] || [ -z "$TRIG_PR" ] || [ -z "$HEAD_SHA" ]; then
  # The gate (native Go, at the one-shot seam) proceeds BEFORE this plugin
  # runs — so a missing/partial envelope here is a wiring failure, not an
  # idle cycle. FAIL LOUD: a silent changed:false would orphan the label
  # (no verdict, no consume, nothing re-arms) — the exact stuck state seen
  # in the first live run.
  echo "ERROR: incomplete trigger envelope (REPO=${TRIG_REPO:-} PR=${TRIG_PR:-} SHA=${HEAD_SHA:-}) — the gate must export the full envelope" >&2
  exit 1
fi

SPEC="${HARMOSTES_SPEC:-}"
WIKI_URL=""
if [ -n "$SPEC" ]; then
  WIKI_URL=$(echo "$SPEC" | python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('config',{}).get('wiki',''))" 2>/dev/null || echo "")
fi

# repo path → host + owner/name (platform convention)
case "$TRIG_REPO" in
  */*) HOST="${TRIG_REPO%%/*}"; REPO="${TRIG_REPO#*/}";;
  *)   HOST="github.com"; REPO="$TRIG_REPO";;
esac
API_BASE=$(host::api_base "$HOST")
IS_FJ=$(host::is_fj "$HOST")
PR_NUM="$TRIG_PR"
GIT_HOST_TOKEN=$(host::token "$HOST")   # optional here: public repos clone/fetch anonymously
export HOST REPO PR_NUM HEAD_SHA API_BASE IS_FJ WIKI_URL TRIG_CONTEXTS WORKDIR GIT_HOST_TOKEN
# Clear known artifacts from previous runs in the shared WORKDIR — a
# stale review.json/review-diff.patch from another repo's review
# confuses the agent (observed live: reviewers disregarding foreign
# files instead of reading fresh ones).
rm -f "$WORKDIR/review.json" "$WORKDIR/review-diff.patch" "$WORKDIR/pr-context.json" "$WORKDIR/pr-diff.patch"
log "provisioning workspace for $HOST/$REPO#$PR_NUM (head=${HEAD_SHA:0:8}, base=$TRIG_BASE)"

# ── PR context: metadata, files, linked issue, CI, diff ───────────────────
python3 << 'PYEOF'
import json, os, re, time, urllib.request
host=os.environ["HOST"]; base=os.environ["API_BASE"]; repo=os.environ["REPO"]
num=int(os.environ["PR_NUM"]); sha=os.environ["HEAD_SHA"]; ref=""
workdir=os.environ["WORKDIR"]; wiki_url=os.environ.get("WIKI_URL","")
is_fj=os.environ.get("IS_FJ","false")=="true"
token=os.environ["GIT_HOST_TOKEN"]  # resolved above via host::token (mirrors review.go TokenEnvNames)
gate_ctx=os.environ.get("TRIG_CONTEXTS","")  # the envelope (already verified green by the gate)
def api(path, accept="application/json"):
    # Bounded retry (3 attempts, 2s/4s backoff) around every fetch:
    # python resolves single-shot with no resolver retry, so one dropped
    # DNS UDP response during a cluster DNS burst fails the whole
    # prepare step and stalls the review for the burst duration
    # (harmostes#267 — observed twice on 2026-08-30). Retries ONLY
    # transport-level failures (URLError/socket), never HTTPError: a
    # 401/403/404 is a deterministic answer, not weather.
    req=urllib.request.Request(base+path, headers={"authorization":f"token {token}" if token else "","accept":accept})
    last=None
    for attempt in range(3):
        try:
            with urllib.request.urlopen(req) as resp:
                if "diff" in accept or "text/plain" in accept: return resp.read().decode("utf-8","replace")
                return json.loads(resp.read())
        except urllib.error.HTTPError: raise
        except (urllib.error.URLError, OSError) as e:
            last=e
            if attempt<2: time.sleep(2*(attempt+1))
    raise last
pr=api(f"/repos/{repo}/pulls/{num}")
ref=pr.get("head",{}).get("ref","")
open(f"{workdir}/.head_ref","w").write(ref)
files=api(f"/repos/{repo}/pulls/{num}/files?limit=100")
fs=[{"status":f["status"],"filename":f["filename"],"additions":f["additions"],"deletions":f["deletions"]} for f in files]
issue_num=None
m=re.search(r"(?:close[sd]?|fix(?:es|ed)?|resolve[sd]?|refs?)\s+#(\d+)",pr.get("body") or "",re.I)
if m: issue_num=int(m.group(1))
issue_data=None;milestone=None
if issue_num:
    try:
        issue_data=api(f"/repos/{repo}/issues/{issue_num}")
        ms=issue_data.get("milestone")
        if ms: milestone={"title":ms["title"],"description":ms.get("description",""),"state":ms["state"]}
    except Exception as e: print(f"WARN issue: {e}",file=sys.stderr)
ci={"status":"none"}
try:
    if is_fj:
        statuses=api(f"/repos/{repo}/commits/{sha}/statuses")
        # First-wins per context (list is newest-first): superseded
        # attempts (label-event treadmill re-dispatches) must not clobber
        # the freshest state — mirrors the kernel gate evaluator
        # (internal/review/review.go ContextStates dedupe).
        states={}
        for s in statuses:
            states.setdefault(s.get("context"), s.get("status"))
        if states:
            vals=list(states.values())
            ci={"total":len(vals),"all_success":all(v=="success" for v in vals),"states":sorted(set(vals))}
    else:
        cr=api(f"/repos/{repo}/commits/{sha}/check-runs?per_page=30")
        runs=cr.get("check_runs",[])
        if runs:
            c=[r.get("conclusion") for r in runs if r.get("conclusion")]
            ci={"total":len(runs),"completed":len(c),"all_success":bool(c) and all(x=="success" for x in c),"conclusions":sorted(set(c))}
except Exception as e: print(f"WARN ci: {e}",file=sys.stderr)
diff=""
try:
    if is_fj:
        # Forgejo does NOT honor Accept content negotiation on /pulls/N
        # (it returns the API JSON object — observed: pr-diff.patch with
        # zero 'diff --git' lines). The .diff suffix is the supported
        # surface.
        diff=api(f"/repos/{repo}/pulls/{num}.diff",accept="text/plain")
    else: diff=api(f"/repos/{repo}/pulls/{num}",accept="application/vnd.github.v3.diff")
except: pass
with open(f"{workdir}/pr-diff.patch","w") as f: f.write(diff[:100000])
# Deterministic scope signal (#30): facts in prepare, interpretation
# in the task. Counts from the RAW patch — the agent only ever sees
# the truncated file, so metrics describe the real review surface.
dfiles=len(set(re.findall(r"^diff --git (\S+)",diff,re.M)))
dins=sum(1 for l in diff.splitlines() if l.startswith("+") and not l.startswith("+++"))
ddel=sum(1 for l in diff.splitlines() if l.startswith("-") and not l.startswith("---"))
dlines=dins+ddel
scope="huge" if dlines>2000 or dfiles>40 else ("large" if dlines>400 or dfiles>10 else "focused")
ctx={"host":host,"repo":repo,"number":num,"title":pr["title"],"body":pr.get("body") or "",
     "user":pr.get("user",{}).get("login","?"),"url":pr.get("html_url",""),
     "head_sha":sha,"head_ref":ref,"base":os.environ.get("TRIG_BASE",""),"issue_number":issue_num,
     "issue":{"title":issue_data["title"],"body":issue_data.get("body") or "","labels":[l["name"] for l in issue_data.get("labels",[])]} if issue_data else None,
     "milestone":milestone,"ci_status":ci,"files_changed":fs,"repo_dir":f"{workdir}/repo",
 "diff_stats":{"files":dfiles,"insertions":dins,"deletions":ddel},"scope":scope,
     "wiki_dir":f"{workdir}/wiki" if wiki_url else "","review_path":f"{workdir}/review.json"}
with open(f"{workdir}/pr-context.json","w") as f: json.dump(ctx,f,indent=2)
PYEOF
HEAD_REF=$(cat "$WORKDIR/.head_ref" 2>/dev/null || echo "")

# ── Tool availability (the agent's knowns — no self-discovery) ────────────
TOOLS=$(python3 - << 'PYEOF'
import json, shutil
tools={}
for t in ("fj","gh","glab","kubectl","vela","flux","jq","python3","git"):
    tools[t]= bool(shutil.which(t))
print(json.dumps(tools))
PYEOF
)
log "tools: $TOOLS"

# ── Clone the repo at the reviewed head SHA ───────────────────────────────
REPO_DIR="$WORKDIR/repo"; rm -rf "$REPO_DIR"
CLONE_URL=$(host::clone_url "$HOST" "$REPO")
if [ -n "$HEAD_REF" ]; then
  git clone --quiet --depth 50 --branch "$HEAD_REF" "$CLONE_URL" "$REPO_DIR" 2>&1|tail -1 || {
    git clone --quiet --depth 50 "$CLONE_URL" "$REPO_DIR" 2>&1|tail -1; }
else
  git clone --quiet --depth 50 "$CLONE_URL" "$REPO_DIR" 2>&1|tail -1
fi
git -C "$REPO_DIR" fetch --quiet --depth 50 origin "$HEAD_SHA" 2>/dev/null || true
git -C "$REPO_DIR" checkout --quiet "$HEAD_SHA" 2>/dev/null || true
git config --global --add safe.directory '*' 2>/dev/null || true

# ── Wiki + RIG (architecture graph for the Architect stance) ──────────────
if [ -n "$WIKI_URL" ]; then
  WIKI_DIR="$WORKDIR/wiki"; rm -rf "$WIKI_DIR"
  WC="$WIKI_URL"; case "$WIKI_URL" in https://github.com/*) WC="https://x-access-token:$(host::token github.com)@${WIKI_URL#https://}";; esac
  git clone --quiet --depth 50 "$WC" "$WIKI_DIR" 2>&1|tail -1 || log "WARN: wiki clone failed"
fi
if [ -n "$WIKI_URL" ] && [ -d "$WORKDIR/wiki" ]; then
  PROJECT_NAME=$(basename "$REPO")
  RIG_DIR="$WORKDIR/wiki/raw/arch/$PROJECT_NAME"
  if [ -d "$RIG_DIR" ] && [ -f "$RIG_DIR/rig.json" ]; then
    RIG_PATH=$(realpath "$RIG_DIR/rig.json")
    C4_PATH=$(realpath "$RIG_DIR/model.c4" 2>/dev/null || echo "")
    log "RIG found for $PROJECT_NAME: $RIG_PATH"
    jq --arg rig "$RIG_PATH" --arg c4 "$C4_PATH" '. + {rig_path:$rig, c4_path:$c4}' "$WORKDIR/pr-context.json" > "$WORKDIR/pr-context.json.tmp" && mv "$WORKDIR/pr-context.json.tmp" "$WORKDIR/pr-context.json"
  else
    log "no RIG for $PROJECT_NAME in wiki (architect stance skips component-graph checks)"
  fi
fi

# ── Architecture graph: SHA-exact rig.db for THIS checkout (ADR-0009) ──────
# The wiki's synced rig.* describes the default branch and may be stale by
# any number of merged PRs; a review navigates by the graph of what it
# reviews. Generated from the head-SHA checkout with the in-image emitter.
# Best-effort + time-boxed: a graph-less review still works (the rig-query
# tool reports absence and the agent falls back to bash) — prepare never
# fails here. Put AFTER pr-context.json exists: we stamp the path in.
if [ -f /usr/local/lib/harmostes/plugins/emit-rig.py ]; then
  if ( cd "$REPO_DIR" && timeout 180 python3 /usr/local/lib/harmostes/plugins/emit-rig.py "$WORKDIR/rig.json" ) > "$WORKDIR/rig-emit.log" 2>&1 && [ -f "$WORKDIR/rig.db" ]; then
    log "rig.db generated from $HEAD_SHA: $(wc -c < "$WORKDIR/rig.db") bytes"
    echo -n "$HEAD_SHA" > "$WORKDIR/rig.db.sha"  # ADR-0009 provenance: the rig-query extension warns on mismatch
    jq --arg db "$WORKDIR/rig.db" '. + {rig_db:$db}' "$WORKDIR/pr-context.json" > "$WORKDIR/pr-context.json.tmp" && mv "$WORKDIR/pr-context.json.tmp" "$WORKDIR/pr-context.json"
    # Context enrichment (#443): pre-digest the graph into the agent's
    # orientation — overview, the diff's touched components, their symbols
    # (file:line) and blast radius. The r4 forensics showed the model ignores
    # "query rig first" and burns 30+ greps rediscovering the graph before
    # dying of context exhaustion; the briefing removes the need to ask.
    if [ -f /usr/local/lib/harmostes/plugins/rig-brief.py ]; then
      timeout 60 python3 /usr/local/lib/harmostes/plugins/rig-brief.py "$WORKDIR/rig.db" "$WORKDIR/pr-context.json" "$WORKDIR" 2>&1 | sed 's/^/[rig-brief] /' || true
    fi
  else
    log "WARN: rig.db generation failed (tail of $WORKDIR/rig-emit.log): $(tail -2 "$WORKDIR/rig-emit.log" 2>/dev/null | tr '\n' ' ')"
  fi
fi

# record tools into the context
jq --argjson t "$TOOLS" '. + {tools:$t}' "$WORKDIR/pr-context.json" > "$WORKDIR/pr-context.json.tmp" && mv "$WORKDIR/pr-context.json.tmp" "$WORKDIR/pr-context.json"
log "workspace ready → $WORKDIR/pr-context.json"
echo "{\"changed\":true,\"artifact\":\"$WORKDIR/pr-context.json\",\"status\":\"ok\",\"event\":{\"host\":\"$HOST\",\"repo\":\"$REPO\",\"number\":$PR_NUM,\"head_sha\":\"$HEAD_SHA\"}}"
