#!/usr/bin/env bash
# harmostes builtin "wiki-lint" gate plugin (llm-wiki).
#
# Runs the wiki instance's OWN lint pipeline (the same one remote CI runs):
# init the .llm-wiki submodule → ci-consistency.sh (drift) → ci-lint.sh
# (markdownlint + mdlint + remark + mermaid render + likec4 + health + RIG
# compliance). Exit 0 = green (the agent's work passes); non-zero = failed, and
# the combined output becomes the feedback the agent receives in the next loop
# turn.
#
# This is the harmostes equivalent of the llm-wiki controller's gate-lint.sh.
set -euo pipefail
WS_DIR="${HARMOSTES_WORKSPACE_DIR:-$HARMOSTES_WORKDIR}"
cd "$WS_DIR"

# Defend against git "dubious ownership" on bare-clone worktrees / mounts.
git config --global --add safe.directory '*' 2>/dev/null || true

# 1. the lint scripts live in the .llm-wiki submodule — make sure it's present.
if [ ! -f .llm-wiki/scripts/ci-lint.sh ]; then
  echo "ERROR: .llm-wiki/scripts/ci-lint.sh not found — init the submodule" >&2
  git submodule update --init --recursive 2>&1 | tail -2
fi
[ -f .llm-wiki/scripts/ci-lint.sh ] || {
  echo "ERROR: .llm-wiki not available (ci-lint.sh missing) — cannot validate" >&2
  exit 2
}

# 2. consistency (generated files vs submodule)
bash .llm-wiki/scripts/ci-consistency.sh

# 3. the full lint pipeline
export PATH="$HOME/.local/bin:$(npm prefix -g 2>/dev/null)/bin:$PATH"
bash .llm-wiki/scripts/ci-lint.sh

echo '{"status":"ok"}'
