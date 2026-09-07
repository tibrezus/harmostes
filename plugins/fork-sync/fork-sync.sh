#!/bin/bash
# =============================================================================
# fork-sync.sh — harmostes plugin wrapper for the fork-maintenance sync payload
# =============================================================================
# This is the harmostes entry point for fork maintenance. It ships IN the
# worker image as a built-in plugin (/usr/local/lib/harmostes/plugins/
# fork-sync.sh, ADR-0011) and is resolved by the harmostes worker's
# BuiltinResolver when a Workflow CR specifies prepare.plugin.name: fork-sync.
# Its engine payload (scripts/checks/hooks/skill) rides in the Helm chart as
# ConfigMaps mounted at /workspace/*.
#
# The wrapper calls the real sync plugin (sync-fork.sh) with the fork name
# passed as the first argument, then outputs the harmostes PluginResult JSON.
#
# Usage: fork-sync.sh <fork-name> [phase]
#   phase (merge|hook|gates|validate|pr|tag) — graph-native phased mode: exec
#   the plugin for that one phase and propagate its exit code + PluginResult
#   JSON verbatim (each phase is one harmostes graph node).
#   no phase — legacy single-shot (all): exit 0 = up to date / PR opened
#   (auto-merged if auto.merge), 2 = conflict (mapped to green + message:
#   the resolver handles it async), 3+ = hard failure.
# =============================================================================
set -euo pipefail

# Fork selection: argv (graph-native nodes pass "<fork> <phase>") or, when
# invoked bare by a thin declarative instance, the prepare config the UI
# creation form produces — spec.config.fork, falling back to repos[0]
# (the same config shape pr-fetch reads).
if [ $# -ge 1 ]; then
  FORK_NAME="$1"
  PHASE="${2:-}"
else
  FORK_NAME="$(printf '%s' "${HARMOSTES_SPEC:-}" | python3 -c "import sys,json;d=json.load(sys.stdin).get('config',{});print(d.get('fork') or (d.get('repos') or [''])[0])" 2>/dev/null || true)"
  [ -n "$FORK_NAME" ] || { echo "ERROR: fork-sync: no fork name — pass argv or set spec.config.fork / repos[0]" >&2; exit 1; }
  PHASE=""
fi

export MAINT_DIR="/workspace"
export HOME="/tmp"

# Bridge harmostes env var naming to fork-maintenance naming convention.
# The harmostes worker provides HARMOSTES_GIT_TOKEN; sync-fork.sh expects GITHUB_TOKEN.
export GITHUB_TOKEN="${HARMOSTES_GIT_TOKEN:?HARMOSTES_GIT_TOKEN must be set}"

if [ -n "$PHASE" ]; then
  # Phased (graph-native): the plugin emits its own PluginResult JSON and its
  # exit code IS the node outcome (non-zero = failed node; conflict = exit 2
  # routed via the when:failed edge to the external conflict-resolver node).
  exec bash /workspace/scripts/sync-fork.sh "$FORK_NAME" "$PHASE"
fi

# Run the sync plugin. sync-fork.sh exit codes:
#   0 = up to date or PR opened (auto-merged if auto.merge)
#   2 = merge conflict (needs resolution PR created)
#   3 = push failure
set +e
SYNC_OUT="$(bash /workspace/scripts/sync-fork.sh "$FORK_NAME" 2>&1)"
SYNC_EXIT=$?
printf '%s\n' "$SYNC_OUT"
SYNC_MSG="$(printf '%s' "$SYNC_OUT" | grep -v '^[[:space:]]*$' | tail -n 1 || true)"
[ -n "$SYNC_MSG" ] || SYNC_MSG="sync completed"
set -e

case "$SYNC_EXIT" in
  0)
    # Success — up to date, PR created/merged, or a self-hosted pointer
    # (mapping-table def: the plugin defers to the fork repo's sync.yml).
    # `changed` is the plugin contract's core signal: derived from the sync
    # output, never assumed. sync-fork.sh prints its unique no-op banner
    # ("Already up to date") ONLY on the up-to-date path; every productive
    # path ends in the "=== Sync complete ===" block. Reporting a no-op as
    # changed:true made every fork sync look productive (r1 review, P4).
    if grep -q "Already up to date" <<<"$SYNC_OUT"; then
      CHANGED=false
    else
      CHANGED=true
    fi
    # The plugin's own final line becomes the PluginResult message so the
    # UI run shows what actually happened, verbatim.
    printf '{"changed":%s,"artifact":"fork-sync-%s","message":%s}\n' \
      "$CHANGED" "$FORK_NAME" "$(python3 -c 'import json,sys;print(json.dumps(sys.argv[1]))' "$SYNC_MSG")"
    exit 0
    ;;
  2)
    # Conflict — sync-fork.sh created a needs-conflict-resolution PR.
    # The conflict-resolver deployment picks it up asynchronously.
    # Exit 0 so harmostes doesn't mark the run as failed.
    printf '{"changed":false,"artifact":"conflict","message":"merge conflict — resolution PR created"}\n' "$FORK_NAME"
    exit 0
    ;;
  *)
    # Hard failure (push error, etc.)
    printf '{"changed":false,"artifact":"","message":"sync failed (exit %d)"}\n' "$SYNC_EXIT"
    exit 1
    ;;
esac
