#!/usr/bin/env bash
# harmostes builtin "rig-emit" prepare plugin (lc4 workflow).
#
# Clones the project source (HARMOSTES_SOURCE_URL/BRANCH), runs the universal
# emit-rig.py to produce a RIG, places it into the workspace repo (the wiki) at
# raw/arch/<project>/rig.json, and reports changed=true.
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

# Embed the git token into an https source URL for auth (no-op for SSH/public).
if [ -n "${HARMOSTES_GIT_TOKEN:-}" ]; then
  case "$SRC_URL" in https://*) SRC_URL="https://x-access-token:${HARMOSTES_GIT_TOKEN}@${SRC_URL#https://}";; esac
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

DEST_DIR="$WS_DIR/raw/arch/$PROJECT"
mkdir -p "$DEST_DIR"
cp "$RIG_FILE" "$DEST_DIR/rig.json"

echo "{\"changed\":true,\"artifact\":\"raw/arch/$PROJECT/rig.json\",\"status\":\"ok\",\"event\":{\"components\":$COMPONENTS}}"
