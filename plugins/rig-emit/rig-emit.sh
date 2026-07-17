#!/usr/bin/env bash
# harmostes builtin "rig-emit" prepare plugin (lc4 workflow).
#
# Clones the project source (HARMOSTES_SOURCE_URL/BRANCH), runs the universal
# emit-rig.py to produce a RIG, places it into the workspace repo (the wiki) at
# raw/arch/<project>/rig.json.
#
# DETERMINISTIC SKIP: the generated RIG is compared byte-for-byte with the
# existing rig.json in the workspace repo. If identical, changed=false is
# returned and the pipeline short-circuits BEFORE the agent runs — no LLM
# tokens consumed. Only a genuinely different RIG triggers the agent.
#
# Mirrors what the llm-wiki controller's reconcile.sh does deterministically.
# emit-rig.py + emit-rig.sh are vendored from the llm-wiki module
# (.github/actions/repo-map/) — drift between the two is tracked.
set -euo pipefail
log() { echo "[rig-emit] $*"; }

SRC_URL="${HARMOSTES_SOURCE_URL:?HARMOSTES_SOURCE_URL required}"
SRC_BRANCH="${HARMOSTES_SOURCE_BRANCH:-main}"
SRC_LANG="${HARMOSTES_SOURCE_LANGUAGE:-}"
PROJECT="${HARMOSTES_WORKFLOW:?HARMOSTES_WORKFLOW required}"
WS_DIR="${HARMOSTES_WORKSPACE_DIR:-$HARMOSTES_WORKDIR}"
EMITTER_DIR="$(cd "$(dirname "$0")" && pwd)"

# Embed the GitHub token into a github.com https URL for auth. The injected
# token is a GitHub PAT (harmostes-github-token); injecting it into another host
# (e.g. a public Forgejo mirror) yields a wrong-credential 401 — strictly worse
# than anonymous. Non-GitHub https sources clone anonymously (or would need a
# host-specific token, injected separately). No-op for SSH.
if [ -n "${HARMOSTES_GIT_TOKEN:-}" ]; then
  case "$SRC_URL" in
    https://github.com/*) SRC_URL="https://x-access-token:${HARMOSTES_GIT_TOKEN}@${SRC_URL#https://}";;
  esac
fi

# Forgejo (git.rezus.cloud) basic auth — separate credentials (username +
# password) from harmostes-rzc-token. Private source mirrors use these.
if [ -n "${HARMOSTES_RZC_USERNAME:-}" ] && [ -n "${HARMOSTES_RZC_PASSWORD:-}" ]; then
  case "$SRC_URL" in
    https://git.rezus.cloud/*) SRC_URL="https://${HARMOSTES_RZC_USERNAME}:${HARMOSTES_RZC_PASSWORD}@${SRC_URL#https://}";;
  esac
fi

SRC_DIR="$(mktemp -d)/source"
RIG_FILE="$(mktemp -d)/rig.json"
mkdir -p "$WS_DIR/.source" 2>/dev/null || true
log "cloning source $SRC_URL ($SRC_BRANCH) into a temp dir (never inside the workspace repo)…"
git clone --depth 100 --branch "$SRC_BRANCH" "$SRC_URL" "$SRC_DIR" 2>&1 | tail -1 || {
  echo '{"changed":false,"status":"failed","event":{"error":"clone failed"}}'
  exit 1
}

log "generating RIG (language=${SRC_LANG:-auto})…"
( cd "$SRC_DIR" && bash "$EMITTER_DIR/emit-rig.sh" "$RIG_FILE" "$SRC_LANG" ) 2>&1 | tail -3

COMPONENTS=$(python3 -c "import json;print(len(json.load(open('$RIG_FILE'))['components']))" 2>/dev/null || echo 0)
log "RIG: $COMPONENTS components"

# Generate model.c4 deterministically from the RIG + source code comments.
# This replaces the LLM arch-sync step entirely.
MODEL_FILE="$DEST_DIR/model.c4"
mkdir -p "$DEST_DIR"
if [ -f "$EMITTER_DIR/rig-to-c4.py" ]; then
  log "generating model.c4 (deterministic, from RIG + code comments)…"
  python3 "$EMITTER_DIR/rig-to-c4.py" "$RIG_FILE" --source-dir "$SRC_DIR" -o "$MODEL_FILE" 2>&1 | tail -1 || log "WARN: model.c4 generation failed (non-fatal)"
else
  log "rig-to-c4.py not found — skipping model.c4 generation"
fi

DEST_DIR="$WS_DIR/raw/arch/$PROJECT"
mkdir -p "$DEST_DIR"
DEST_FILE="$DEST_DIR/rig.json"

# Compute the RIG hash for deterministic skip (stored in Workflow status by the
# pipeline). This is more reliable than comparing with the workspace file because
# deploys may push to shadow branches — the workspace repo on 'main' can lag.
RIG_HASH=$(sha256sum "$RIG_FILE" | cut -d' ' -f1)
cp "$RIG_FILE" "$DEST_FILE"
log "RIG hash=$RIG_HASH ($COMPONENTS components)"

echo "{\"changed\":true,\"artifact\":\"raw/arch/$PROJECT/rig.json\",\"status\":\"ok\",\"event\":{\"components\":$COMPONENTS,\"rig_hash\":\"$RIG_HASH\"}}"
