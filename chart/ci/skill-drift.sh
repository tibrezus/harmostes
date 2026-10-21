#!/usr/bin/env bash
# =============================================================================
# skill-drift.sh — the agents-repo fork-maintenance skill stays verbatim with
# the chart's live engine (#606).
# =============================================================================
# The chart is canonical (ADR-0011): chart/fork-maintenance/{scripts,checks}/
# are the scripts the cluster's per-Attempt Jobs run. The agents repo's
# skills/fork-maintenance/templates/ (this checkout carries it via the
# .agents submodule) are VERBATIM copies an agent loads as its skill. When
# they drift, the agent reads a different engine than the cluster runs —
# the #420-era lossy-rewrite class at the documentation layer.
#
# Delegates to the skill's own check-drift.sh (the agents repo owns the
# mapping); this wrapper only resolves the two checkouts.
# =============================================================================
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
AGENTS_SKILL="$ROOT/.agents/skills/fork-maintenance"
CHART_DIR="$ROOT/chart/fork-maintenance"

if [ ! -d "$AGENTS_SKILL" ]; then
  echo "ERROR: $AGENTS_SKILL missing — the .agents submodule is not initialized"
  exit 2
fi
exec env HARMOSTES_CHART_DIR="$CHART_DIR" bash "$AGENTS_SKILL/scripts/check-drift.sh"
