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
rm -f "$WORKDIR/review.json" "$WORKDIR/review-diff.patch" "$WORKDIR/pr-context.json" "$WORKDIR/pr-diff.patch" "$WORKDIR/.head_ref" "$WORKDIR/.pr-meta.json"
log "provisioning workspace for $HOST/$REPO#$PR_NUM (head=${HEAD_SHA:0:8}, base=$TRIG_BASE)"

# ── PR metadata + head ref (pr_context.py meta — network tier) ────────────
# Runs BEFORE the clone: the clone wants the branch name, and the merge
# state it fetches is a FACT the context later states (#428 finding 1).
python3 "$(dirname "$0")/pr_context.py" meta
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
# ── Stale-dispatch guard (#2190 churn): the label re-arms on every push, so
# a review dispatched at SHA N can start after the dev pushed N+1 (rebase
# mid-loop). The post-review gate refuses sha != head, so a superseded round
# burns ~15 min of agent turns on a verdict that cannot publish. The clone
# above is the branch TIP — compare BEFORE checking out the dispatched SHA
# and fail fast; the loop re-dispatches at the new head.
TIP=$(git -C "$REPO_DIR" rev-parse HEAD 2>/dev/null || echo "")
if [ -n "$TIP" ] && [ "$TIP" != "$HEAD_SHA" ]; then
  log "SUPERSEDED: dispatched at ${HEAD_SHA:0:10} but branch head is now ${TIP:0:10} — skipping (re-dispatch lands at the new head)"
  exit 2
fi
git -C "$REPO_DIR" checkout --quiet "$HEAD_SHA" 2>/dev/null || true
git config --global --add safe.directory '*' 2>/dev/null || true
# The base branch for merge-base diffing (#428): the context derives the
# patch from merge-base(origin/base, HEAD_SHA)..HEAD_SHA in THIS clone.
git -C "$REPO_DIR" fetch --quiet --depth 50 origin "$TRIG_BASE" 2>/dev/null || true

# ── PR context (pr_context.py context — git tier, falls back to API) ──────
# After the checkout: diff, files and stats come from the reviewed tree
# itself — full, untruncated, attributable to the dispatched SHA (#428).
# CI/issue/metadata come from the API; merge state and the gate's verified
# contexts are stated as facts.
python3 "$(dirname "$0")/pr_context.py" context

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
  if ( cd "$REPO_DIR" && timeout 180 python3 /usr/local/lib/harmostes/plugins/emit-rig.py "$WORKDIR/rig.json" --source-sha "$HEAD_SHA" ) > "$WORKDIR/rig-emit.log" 2>&1 && [ -f "$WORKDIR/rig.db" ]; then
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
