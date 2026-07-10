#!/usr/bin/env bash
# harmostes builtin "git-push" deploy plugin.
#
# Commits the workspace repo + pushes. If HARMOSTES_SHADOW is set (a parallel /
# dry-run branch), pushes there instead of the fetched branch — so a harmostes
# Workflow can run alongside the live controller without touching main. Rebases
# onto the fetched branch tip (FETCH_HEAD) to avoid spurious non-fast-forwards.
#
# This is the harmostes equivalent of the llm-wiki controller's push step
# (rebase onto FETCH_HEAD + union-merge the shared changelog + push).
set -euo pipefail
log() { echo "[git-push] $*"; }
WS_DIR="${HARMOSTES_WORKSPACE_DIR:-$HARMOSTES_WORKDIR}"
cd "$WS_DIR"

git config --global --add safe.directory '*' 2>/dev/null || true

# commit whatever the agent produced
git add -A
if git diff --cached --quiet; then
  log "nothing to commit"
  echo '{"artifact":"no-change","status":"ok"}'
  exit 0
fi
git -c user.name='harmostes-bot' -c user.email='harmostes-bot@harmostes.dev' \
  commit --no-edit -q -m "docs(harmostes): $HARMOSTES_WORKFLOW" 2>&1 | tail -1

# rebase onto the latest fetched tip (the workspace repo may have advanced)
git fetch origin 2>/dev/null || true
if ! git rebase FETCH_HEAD 2>/dev/null; then
  git rebase --abort 2>/dev/null || true
  log "WARN: rebase onto FETCH_HEAD conflicted — pushing the local commit as-is"
fi

# target branch: shadow (parallel/dry-run) if set, else the current branch
if [ -n "${HARMOSTES_SHADOW:-}" ]; then
  TARGET="$HARMOSTES_SHADOW"
  log "pushing to shadow branch $TARGET (parallel run)"
  git push origin "HEAD:refs/heads/$TARGET" --force-with-lease 2>&1 | tail -1 || \
    git push origin "HEAD:refs/heads/$TARGET" --force 2>&1 | tail -1
else
  TARGET=$(git rev-parse --abbrev-ref HEAD)
  log "pushing to $TARGET"
  git push origin "HEAD:refs/heads/$TARGET" 2>&1 | tail -1
fi

COMMIT=$(git rev-parse HEAD)
echo "{\"artifact\":\"$TARGET\",\"status\":\"ok\",\"event\":{\"commit\":\"$COMMIT\"}}"
