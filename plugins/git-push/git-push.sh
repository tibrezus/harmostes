#!/usr/bin/env bash
# harmostes builtin "git-push" deploy plugin.
#
# Publishes the workspace repo. The agent usually commits its own work; this
# plugin ALSO commits anything left uncommitted, then pushes if the local HEAD is
# ahead of origin (NOT merely if there are staged changes — the agent's commit
# would otherwise be stranded locally). If HARMOSTES_SHADOW is set (a parallel /
# dry-run branch), pushes there instead. Rebases onto the fetched tip to avoid
# spurious non-fast-forwards.
set -euo pipefail
log() { echo "[git-push] $*"; }
WS_DIR="${HARMOSTES_WORKSPACE_DIR:-$HARMOSTES_WORKDIR}"
cd "$WS_DIR"

git config --global --add safe.directory '*' 2>/dev/null || true
BRANCH=$(git rev-parse --abbrev-ref HEAD)

# 1. commit anything the agent left uncommitted (the agent usually commits first;
# both use the global 'harmostes-bot' identity set in the Dockerfile)
git add -A
if ! git diff --cached --quiet; then
  git commit --no-edit -q -m "docs(harmostes): $HARMOSTES_WORKFLOW" 2>&1 | tail -1
fi

# 2. fetch the latest + count commits the local branch is ahead of origin
git fetch origin "$BRANCH" 2>/dev/null || git fetch origin 2>/dev/null || true
AHEAD=$(git rev-list --count "origin/$BRANCH..HEAD" 2>/dev/null || echo 0)
if [ "$AHEAD" -eq 0 ]; then
  log "nothing to push (local == origin/$BRANCH)"
  echo '{"artifact":"no-change","status":"ok"}'
  exit 0
fi
log "$AHEAD commit(s) ahead of origin/$BRANCH"

# 3. rebase onto the fetched tip (the branch may have advanced during the run)
if ! git rebase "origin/$BRANCH" 2>/dev/null; then
  git rebase --abort 2>/dev/null || true
  log "WARN: rebase onto origin/$BRANCH conflicted — pushing the local commit(s) as-is"
fi

# 4. push — shadow branch (parallel/dry-run) if set, else the branch itself
if [ -n "${HARMOSTES_SHADOW:-}" ]; then
  TARGET="$HARMOSTES_SHADOW"
  log "pushing to shadow branch $TARGET"
  git push origin "HEAD:refs/heads/$TARGET" --force-with-lease 2>&1 | tail -1 || \
    git push origin "HEAD:refs/heads/$TARGET" --force 2>&1 | tail -1
else
  TARGET="$BRANCH"
  log "pushing to $TARGET"
  git push origin "HEAD:refs/heads/$TARGET" 2>&1 | tail -1
fi

COMMIT=$(git rev-parse HEAD)
echo "{\"artifact\":\"$TARGET\",\"status\":\"ok\",\"event\":{\"commit\":\"$COMMIT\"}}"
