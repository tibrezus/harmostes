#!/usr/bin/env bash
# =============================================================================
# sync-fork.sh — Universal fork sync script (MERGE model, phase-addressable)
# =============================================================================
# Merges the upstream release branch INTO the fork's release branch (off a sync
# branch → PR), runs post-merge hooks, verifies patches + divergence integrity,
# validates, and opens a host-routed PR. Merge model: the fork's
# release branch carries our customizations as immutable commits; each sync
# appends an upstream merge, so custom SHAs never change (append-only, bisectable,
# no replay/accumulation). Conflicts are localized 3-way regions — the ideal
# substrate for the LLM conflict-resolver (resolve-conflict.sh). Parameterized by
# a fork definition YAML.
#
# PHASE MODE (graph-native fork-maintenance workflows): each phase is one harmostes
# graph node (plugin fork-sync <fork> <phase>). Phases share the clone via
# HARMOSTES_WORKDIR/fork-<name> and pass state through .git/harmostes-state.env.
# Phased mode merges the LOCAL MIRROR BRANCH (fork.mirror_branch, kept current by
# the upstream-mirror action + push webhook) — it never fetches the upstream host.
# Result semantics (PluginResult JSON on the last stdout line):
#   merge     exit 0 {changed:false}            upstream unchanged (no-op run)
#             exit 0 {changed:true}             merged cleanly
#             exit 2                            conflict (resolver delegated, async)
#   hook      exit 0/1                          post-merge codegen
#   gates     exit 0 {changed:true}             divergence + patches intact
#             exit 1                            lost feature → needs-fix PR pushed
#   validate  exit 0/1                          the fork's declared validation suite
#   pr        exit 0 {changed:true|false}       false = PR left for review (no auto-merge)
#   tag       exit 0 {changed:true|false}       machine-cut v<upstream>-rezus.<N+1>
#
# ALL MODE (default — legacy single-shot for forks not yet graph-native):
# exit codes: 0 = up to date or PR opened (auto-merged if auto.merge);
#             2 = merge conflict (needs resolution PR created); 3 = push failure.
#
# Usage: sync-fork.sh <fork-name> [merge|hook|gates|validate|pr|tag|all]
# Requires: git, gh (GitHub CLI), yq, bash
# Environment: GITHUB_TOKEN (PAT with repo + workflow scope on the forks)
# =============================================================================

set -euo pipefail

FORK_NAME="${1:?Usage: sync-fork.sh <fork-name> [merge|hook|gates|validate|pr|tag|all]}"
PHASE="${2:-all}"
case "$PHASE" in
  merge|hook|gates|validate|pr|tag) PHASED=1 ;;
  all) PHASED=0 ;;
  *) echo "ERROR: unknown phase '$PHASE'" >&2; exit 1 ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# MAINT_DIR is the parent of scripts/ — either platform/harmostes/fork-maintenance/ in the
# repo, or /workspace/ in the worker (ConfigMap mount).
MAINT_DIR="$(dirname "$SCRIPT_DIR")"
DEF_FILE="$MAINT_DIR/forks/${FORK_NAME}.yaml"

if [ ! -f "$DEF_FILE" ]; then
  echo "ERROR: fork definition not found: $DEF_FILE" >&2
  exit 1
fi

echo "=== [$PHASE] fork: $FORK_NAME ==="

# Parse YAML definition into shell variables
read_yaml() { yq -r "$1" "$DEF_FILE"; }

# Mode dispatch (schema v2): merge (default, whole-repo forks) vs subtree
# (vendored upstream trees) vs mapping (the forgejo self-sync — sync.yml's
# mapping-table walk in the plugin). The subtree/mapping machinery lives at
# the END of this script — dispatching there and exiting; every merge-mode
# path below is unreachable for those defs, keeping the merge-mode code
# byte-untouched.
MODE="$(read_yaml '.mode // "merge"' 2>/dev/null || echo merge)"
case "$MODE" in
  merge|subtree|mapping) ;;
  *) echo "ERROR: unknown mode '$MODE' for fork '$FORK_NAME'" >&2; exit 1 ;;
esac

# Mapping-table defs are SELF-HOSTED (sync.yml in the fork repo walks the
# rows) unless they opt into the plugin transport (mode: mapping — #97).
if [ "$(read_yaml '.mappings | length' 2>/dev/null || echo 0)" != "0" ] && [ "$MODE" != "mapping" ]; then
  echo "fork ${FORK_NAME} is self-hosted (mapping table; transport declared per row)"
  echo "sync runs in the fork repo: .github/workflows/sync.yml — nothing to do here"
  exit 0
fi

UPSTREAM_URL=$(read_yaml '.upstream.url')
UPSTREAM_BRANCH=$(read_yaml '.upstream.branch')
FORK_URL=$(read_yaml '.fork.url')
FORK_DEFAULT_BRANCH=$(read_yaml '.fork.default_branch')
FORK_MIRROR_BRANCH=$(read_yaml '.fork.mirror_branch' 2>/dev/null || echo "")

# Deliver a fork.conflict.needs-resolution event (structured needs-fix payload) to
# the conflict-resolver. Delivered by direct HTTP POST to the resolver's /events
# endpoint (conflict-subscriber.py), with retries. We previously routed this
# through Dapr pub/sub (pubsub.redis), but Redis pub/sub is fire-and-forget —
# events published while the resolver was restarting were silently lost, AND
# the daprd sidecar in the sync Job never terminated so the Job hung and re-emission
# (every 30m) never happened. Direct HTTP fixes both. The payload is ALWAYS
# written to manifests/<fork>-needs-fix.json first (audit / manual-runs fallback).
# See skill/references/conflict-resolution.md "Agentic integration shape".
emit_conflict_event() {
  local cfiles="${1:-}" payload patches_json pcount i
  patches_json="[]"
  pcount=$(read_yaml '.patches | length' 2>/dev/null || true)
  for i in $(seq 0 $((pcount - 1))); do
    local pf ps pd st="LOST"
    pf=$(read_yaml ".patches[$i].file"); ps=$(read_yaml ".patches[$i].signature"); pd=$(read_yaml ".patches[$i].description")
    if [ -f "$pf" ]; then { [ "$(grep -cF "$ps" "$pf" 2>/dev/null || true)" -gt 0 ] && st="OK"; } || st="LOST"; else st="MISSING"; fi
    patches_json=$(echo "$patches_json" | jq --arg f "$pf" --arg s "$ps" --arg d "$pd" --arg st "$st" '. += [{file:$f,signature:$s,description:$d,status:$st}]')
  done
  payload=$(jq -n \
    --arg fork "$FORK_NAME" \
    --arg upstream_url "$UPSTREAM_URL" --arg upstream_branch "$UPSTREAM_BRANCH" \
    --arg upstream_range "${MERGE_BASE:0:12}..${UPSTREAM_HEAD:0:12}" \
    --argjson patches "$patches_json" \
    --arg conflict_files "$cfiles" \
    '{fork:$fork, upstream_url:$upstream_url, upstream_branch:$upstream_branch,
      upstream_range:$upstream_range, patches_at_risk:$patches,
      conflict_files: ($conflict_files | split("\n") | map(select(length>0)))}')
  mkdir -p "$MAINT_DIR/manifests" 2>/dev/null || true
  echo "$payload" > "$MAINT_DIR/manifests/${FORK_NAME}-needs-fix.json"
  # Direct HTTP delivery to the resolver (replaces Dapr pub/sub — see header).
  RESOLVER_URL="${RESOLVER_URL:-http://fork-conflict-resolver.harmostes.svc.cluster.local/events}"
  ce=$(echo "$payload" | jq -c '{source:"fork-sync", type:"fork.conflict.needs-resolution", data:.}')
  delivered=false
  for attempt in 1 2 3 4 5; do
    if curl -sf -m 5 -X POST "$RESOLVER_URL" -H "Content-Type: application/json" -d "$ce" >/dev/null 2>&1; then
      echo "  delivered fork.conflict.needs-resolution → resolver"
      delivered=true; break
    fi
    echo "  resolver unreachable (attempt $attempt); retrying in ${attempt}s"
    sleep "$attempt"
  done
  $delivered || echo "  WARNING: resolver unreachable after retries — needs-fix payload at manifests/${FORK_NAME}-needs-fix.json"
}

# ── Phase-mode plumbing ──────────────────────────────────────────────────────
# The clone lives under HARMOSTES_WORKDIR (the graph executor shares one workdir
# across the run's nodes) and state passes through .git/harmostes-state.env
# (inside .git so it never pollutes the working tree).
STATE_FILE=""
state_save() {
  { for v in FORK_NAME UPSTREAM_URL UPSTREAM_BRANCH FORK_URL FORK_DEFAULT_BRANCH \
             FORK_MIRROR_BRANCH SYNC_DATE SYNC_BRANCH MERGE_BASE UPSTREAM_HEAD \
             NEW_COMMITS BASELINE PATCH_STATUS PATCH_RESULTS HOOK_RAN; do
      printf '%s=%q\n' "$v" "${!v:-}"
    done
    printf 'VALIDATION_FAILED=%q\n' "${VALIDATION_FAILED:-false}"
    printf 'DIVERGENCE_FAILED=%q\n' "${DIVERGENCE_FAILED:-false}"
    printf 'VALIDATION_RESULTS=%q\n' "${VALIDATION_RESULTS:-(validation not run)}"
  } > "$STATE_FILE"
}
state_load() {
  # STATE_FILE is per-process state: recompute it for phased runs (the merge
  # phase that created it ran in a different process/node).
  if [ "$PHASED" = "1" ] && [ -z "${STATE_FILE:-}" ]; then
    STATE_FILE="${HARMOSTES_WORKDIR:-/tmp}"
    STATE_FILE="${STATE_FILE%/}/fork-${FORK_NAME}/.git/harmostes-state.env"
  fi
  [ -n "${STATE_FILE:-}" ] && [ -f "$STATE_FILE" ] || { echo "ERROR: no sync state — run the merge phase first" >&2; exit 1; }
  # shellcheck disable=SC1090
  source "$STATE_FILE"
}

# push_sync_branch pushes the disposable sync branch: force-with-lease when a
# remote-tracking ref is known (protects foreign updates), plain --force when the
# shallow clone lacks the tracking ref (same-day re-runs legitimately recreate
# the branch; it is derived state owned by this script).
push_sync_branch() {
  if git rev-parse -q --verify "origin/$SYNC_BRANCH" >/dev/null 2>&1; then
    git push --force-with-lease --quiet origin "$SYNC_BRANCH"
  else
    git push --force --quiet origin "$SYNC_BRANCH"
  fi
}

# result_json prints the PluginResult contract line (LAST stdout line wins).
result_json() {
  local changed="$1" artifact="$2" phase="$3" msg="$4"
  printf '{"changed":%s,"artifact":"%s","event":{"phase":"%s"},"message":"%s"}\n' \
    "$changed" "$artifact" "$phase" "$msg"
}

# ── Phase: merge (clone → merge-base check → merge → divergences) ────────────
phase_merge() {
  if [ "$PHASED" = "1" ]; then
    WORKDIR_ROOT="${HARMOSTES_WORKDIR:-/tmp}"
    WORKDIR="${WORKDIR_ROOT%/}/fork-${FORK_NAME}"
    rm -rf "$WORKDIR"          # stale clone from a prior failed run on this pod
    mkdir -p "$WORKDIR_ROOT"
    STATE_FILE="$WORKDIR/.git-state.env"  # placeholder; real .git exists after clone
  else
    WORKDIR=$(mktemp -d)
  fi

  echo ""
  echo "=== Cloning fork (shallow) ==="
  git clone --depth 100 "$FORK_URL" "$WORKDIR"
  cd "$WORKDIR"
  STATE_FILE="$WORKDIR/.git/harmostes-state.env"

  # shellcheck source=scripts/git-host.sh
  # shellcheck disable=SC1091
  source "$SCRIPT_DIR/git-host.sh"
  host_setup

  # Phased mode merges the LOCAL MIRROR BRANCH (same host as the fork, kept
  # current by the upstream-mirror action + push webhook) — no upstream-host
  # fetch. The alias remote keeps every downstream `upstream/<branch>` ref
  # working. Legacy (all) mode fetches the upstream host directly.
  if [ "$PHASED" = "1" ]; then
    git remote add upstream "$FORK_URL"
    git fetch --depth 100 upstream "$UPSTREAM_BRANCH" --tags
  else
    git remote add upstream "$UPSTREAM_URL"
    git fetch --depth 100 upstream "$UPSTREAM_BRANCH" --tags
  fi

  CURRENT_BRANCH=$(git branch --show-current)
  if [ "$CURRENT_BRANCH" != "$FORK_DEFAULT_BRANCH" ]; then
    git checkout "$FORK_DEFAULT_BRANCH" 2>/dev/null || git checkout -b "$FORK_DEFAULT_BRANCH" "origin/$FORK_DEFAULT_BRANCH"
  fi

  MERGE_BASE=$(git merge-base "HEAD" "upstream/$UPSTREAM_BRANCH" 2>/dev/null || echo "")
  UPSTREAM_HEAD=$(git rev-parse "upstream/$UPSTREAM_BRANCH")

  if [ "$MERGE_BASE" = "$UPSTREAM_HEAD" ]; then
    echo ""
    echo "=== Already up to date — mirror has no new commits ==="
    if [ "$PHASED" = "1" ]; then
      result_json false "fork-sync-$FORK_NAME" merge "up to date — nothing to merge"
      exit 0
    fi
    exit 0
  fi

  NEW_COMMITS=$(git rev-list --count "${MERGE_BASE}..upstream/${UPSTREAM_BRANCH}" 2>/dev/null || echo "?")
  echo ""
  echo "=== Upstream has $NEW_COMMITS new commits — syncing ==="

  # ── Capture divergence baseline (deterministic, BEFORE sync) ────────────
  DT="$SCRIPT_DIR/divergence-track.sh"
  { read_yaml '.additive_paths[]' 2>/dev/null || true; } > "/tmp/${FORK_NAME}-additive.txt"
  { read_yaml '.deletions[]'       2>/dev/null || true; } > "/tmp/${FORK_NAME}-deletions.txt"
  BASELINE="/tmp/${FORK_NAME}-divergence-baseline.json"
  echo ""
  echo "=== Capturing divergence baseline (declared additive + auto + deletions) ==="
  bash "$DT" capture "$WORKDIR" "upstream/$UPSTREAM_BRANCH" "$FORK_DEFAULT_BRANCH" "$BASELINE" \
       "/tmp/${FORK_NAME}-additive.txt" "/tmp/${FORK_NAME}-deletions.txt"

  SYNC_DATE=$(date +%Y-%m-%d)
  SYNC_BRANCH="rezus/sync-${SYNC_DATE}"

  # Branch the sync work off the RELEASE branch (not upstream) so the merge
  # result is release + upstream-delta — a clean superset that PRs back.
  git checkout -b "$SYNC_BRANCH" "$FORK_DEFAULT_BRANCH"

  # --no-ff guarantees an explicit, auditable merge commit (revertible via -m 1).
  if ! git merge --no-ff --no-edit \
       -m "RZ/sync: merge upstream ${UPSTREAM_BRANCH} into ${FORK_DEFAULT_BRANCH} (${SYNC_DATE})" \
       "upstream/${UPSTREAM_BRANCH}"; then
    # The merge stopped on conflicts (localized 3-way regions). Conclude it WITH
    # markers on the sync branch, open a needs-conflict-resolution PR for the LLM
    # resolver, and emit fork.conflict.needs-resolution. The resolver re-creates
    # this exact merge, resolves each region from the patch's wiki intent, then
    # re-runs the gates as proof before merging.
    CONFLICT_FILES=$(git diff --name-only --diff-filter=U 2>/dev/null | grep -v '^$' || true)
    echo ""
    echo "=== Merge conflict — opening a resolution PR for the LLM resolver ==="
    echo "Conflicting files:"; echo "$CONFLICT_FILES" | sed 's/^/  - /'
    git add -A 2>/dev/null || true
    git commit --no-edit --quiet 2>/dev/null || true   # conclude the merge WITH markers
    push_sync_branch 2>&1 || { echo "ERROR: cannot push conflict branch" >&2; exit 3; }
    host_label_create "needs-conflict-resolution" 0E8A16 2>/dev/null || true
    PR_URL=$(host_pr_create "$FORK_DEFAULT_BRANCH" "$SYNC_BRANCH" \
      "sync: $FORK_NAME — needs conflict resolution ($SYNC_DATE)" \
      "Upstream merge produced conflicts (merge model — localized 3-way regions). The LLM conflict-resolver will resolve on this branch using each patch's declared intent; the monitor merges once every gate passes." \
      "needs-conflict-resolution" 2>/dev/null || echo "")
    echo "  resolution PR: ${PR_URL:-<none>}"
    emit_conflict_event "$CONFLICT_FILES"
    exit 2
  fi

  # ── Permanent divergences (deletions from fork definition) ──────────────
  DELETIONS=$(read_yaml '.deletions[]' 2>/dev/null || true)
  if [ -n "$DELETIONS" ]; then
    echo ""
    echo "=== Applying permanent divergences ==="
    for del_path in $DELETIONS; do
      clean_path="${del_path%/}"
      if [ -e "$clean_path" ]; then
        echo "  rm -rf $clean_path"
        git rm -rf --quiet "$clean_path" 2>/dev/null || true
      fi
    done
    git add -A 2>/dev/null || true
    if [ -f .git/MERGE_HEAD ]; then
      git commit --no-edit --quiet 2>/dev/null || true
    fi
  fi

  # ── Re-apply divergences the merge dropped (self-heal) ──────────────────
  echo ""
  echo "=== Re-applying dropped divergences (self-heal) ==="
  bash "$DT" reapply "$WORKDIR" "$FORK_DEFAULT_BRANCH" "$BASELINE" || true

  # ── Final marker guard (defensive assertion) ────────────────────────────
  CONFLICT_FILES=$(git grep -l -E '^(<<<<<<<|>>>>>>>|=======) ' -- . 2>/dev/null || true)
  if [ -n "$CONFLICT_FILES" ]; then
    echo "ERROR: unexpected conflict markers after a clean merge — aborting sync" >&2
    echo "$CONFLICT_FILES" | sed 's/^/  - /' >&2
    exit 3
  fi

  echo ""
  echo "=== Merge completed cleanly ==="
  if [ "$PHASED" = "1" ]; then
    state_save
    result_json true "fork-sync-$FORK_NAME" merge "merged ${NEW_COMMITS} upstream commit(s)"
  fi
}

# ── Phase: hook (per-fork codegen: SDK regen, chart vendor, …) ───────────────
phase_hook() {
  cd_repo
  # Def-level post_merge_hook wins (lets channels SHARE a hook — e.g.
  # forgejo-next reuses forgejo.sh); convention fallback keeps old defs working.
  HOOK_DEF=$(read_yaml '.post_merge_hook' 2>/dev/null || echo "")
  HOOK_FILE="$MAINT_DIR/${HOOK_DEF:-post-merge-hooks/${FORK_NAME}.sh}"
  HOOK_RAN=false
  if [ -f "$HOOK_FILE" ]; then
    echo ""
    echo "=== Running post-merge hook: ${FORK_NAME}.sh ==="
    FORK_DIR="$PWD" MAINT_DIR="$MAINT_DIR" bash "$HOOK_FILE" || {
      echo "WARNING: post-merge hook failed — continuing but review needed"
    }
    HOOK_RAN=true
  fi
  if [ "$PHASED" = "1" ]; then
    state_save
    result_json true "fork-sync-$FORK_NAME" hook "post-merge hook complete"
  fi
}

# ── Phase: gates (divergence integrity + patch signatures + manifest) ────────
phase_gates() {
  cd_repo
  DIVERGENCE_FAILED=false
  echo ""
  echo "=== Verifying divergence integrity ==="
  if DIVERGENCE_REPORT=$(bash "$SCRIPT_DIR/divergence-track.sh" verify "$PWD" "$BASELINE" 2>&1); then
    echo "  divergence: ✅ intact"
  else
    echo "$DIVERGENCE_REPORT" >&2
    echo "  divergence: ❌ LOST — a fork feature is missing"
    DIVERGENCE_FAILED=true
  fi

  echo ""
  echo "=== Verifying patches ==="
  PATCH_STATUS="all-intact"
  PATCH_RESULTS=""
  PATCH_COUNT=$(read_yaml '.patches | length')
  for i in $(seq 0 $((PATCH_COUNT - 1))); do
    PATCH_FILE=$(read_yaml ".patches[$i].file")
    PATCH_SIG=$(read_yaml ".patches[$i].signature")
    PATCH_DESC=$(read_yaml ".patches[$i].description")
    if [ ! -f "$PATCH_FILE" ]; then
      STATUS="MISSING"
      PATCH_STATUS="needs-review"
    else
      OCCURRENCES=$(grep -cF "$PATCH_SIG" "$PATCH_FILE" 2>/dev/null || true)
      if [ "$OCCURRENCES" -gt 0 ]; then
        STATUS="OK (${OCCURRENCES}x)"
      else
        STATUS="LOST"
        PATCH_STATUS="needs-review"
      fi
    fi
    echo "  ${STATUS} ${PATCH_FILE} — ${PATCH_DESC}"
    PATCH_RESULTS="${PATCH_RESULTS}  ${STATUS} ${PATCH_FILE}\n"
  done
  echo ""
  echo "Patch status: $PATCH_STATUS"

  # Audit manifest.
  MANIFEST_FILE="$MAINT_DIR/manifests/${FORK_NAME}-rezus.yaml"
  mkdir -p "$MAINT_DIR/manifests"
  if [ -f "$MAINT_DIR/scripts/generate-manifest.sh" ]; then
    echo ""
    echo "=== Generating divergence manifest ==="
    bash "$MAINT_DIR/scripts/generate-manifest.sh" "$FORK_DEFAULT_BRANCH" "upstream/$UPSTREAM_BRANCH" > "$MANIFEST_FILE" 2>/dev/null || true
    echo "  manifest written: $MANIFEST_FILE"
  fi

  if [ "$PHASED" = "1" ]; then
    if $DIVERGENCE_FAILED || [ "$PATCH_STATUS" != "all-intact" ]; then
      # Honest red node + a review surface: push the sync branch and open a
      # needs-fix PR so a human can fix on the merged branch (no auto-merge).
      echo ""
      echo "=== Gates RED — pushing sync branch + needs-fix PR ==="
      push_sync_branch 2>&1 || { echo "ERROR: failed to push sync branch" >&2; exit 3; }
      host_label_create "needs-fix" D93F0B 2>/dev/null || true
      host_pr_create "$FORK_DEFAULT_BRANCH" "$SYNC_BRANCH" \
        "sync: $FORK_NAME — gates RED ($SYNC_DATE)" \
        "Divergence or patch verification failed after the upstream merge. See the run envelopes; fix on this branch, then re-run." \
        "needs-fix" 2>/dev/null || echo "  (PR creation failed)"
      state_save
      echo "ERROR: gates red — a fork feature or patch signature did not survive the merge" >&2
      exit 1
    fi
    state_save
    result_json true "fork-sync-$FORK_NAME" gates "divergence + patches intact"
  fi
}

# ── Phase: validate (the fork's declared validation suite) ───────────────────
phase_validate() {
  cd_repo
  VALIDATION_FILE="/tmp/fork-validation-${FORK_NAME}.md"
  rm -f "$VALIDATION_FILE"
  VALIDATION_RESULTS="(validation not run)"
  VALIDATION_FAILED=false
  if [ -f "$MAINT_DIR/checks/validate-fork.sh" ]; then
    echo ""
    echo "=== Running validation ==="
    VALIDATION_OUTPUT=$(bash "$MAINT_DIR/checks/validate-fork.sh" "$FORK_NAME" "$PWD" 2>&1) && VALIDATION_RC=0 || VALIDATION_RC=$?
    echo "$VALIDATION_OUTPUT"
    echo "$VALIDATION_OUTPUT" > "$VALIDATION_FILE"
    VALIDATION_RESULTS="$VALIDATION_OUTPUT"
    if [ "$VALIDATION_RC" -eq 0 ]; then
      echo "  validation: ✅"
    else
      echo "  validation: ❌ (see above)"
      VALIDATION_FAILED=true
    fi
  else
    echo "=== No validator available — skipping ==="
  fi
  if [ "$PHASED" = "1" ]; then
    state_save
    if $VALIDATION_FAILED; then
      echo "ERROR: validation failed — see output above" >&2
      exit 1
    fi
    result_json true "fork-sync-$FORK_NAME" validate "validation green"
  fi
}

# ── Phase: pr (push + PR + opt-in auto-merge) ────────────────────────────────
phase_pr() {
  cd_repo
  AUTO_MERGE=$(read_yaml '.auto.merge // false')

  echo ""
  echo "=== Pushing sync branch ==="
  push_sync_branch 2>&1 || {
    echo "ERROR: failed to push sync branch" >&2
    exit 3
  }

  UPSTREAM_TAG=$(git describe --tags --abbrev=0 "upstream/$UPSTREAM_BRANCH" 2>/dev/null || echo "HEAD")
  UPSTREAM_COMMITS_RANGE="${MERGE_BASE:0:8}..${UPSTREAM_HEAD:0:8}"

  PR_TITLE="RZ/sync: merge upstream ${UPSTREAM_BRANCH} (${SYNC_DATE})"
  PR_LABEL="auto-merge"
  if [ "$PATCH_STATUS" = "needs-review" ]; then
    PR_LABEL="needs-conflict-resolution"
  elif $VALIDATION_FAILED || $DIVERGENCE_FAILED; then
    PR_LABEL="needs-fix"
  fi

  PR_BODY=$(compose_pr_body)

  echo ""
  echo "=== Opening PR ==="
  for label in "$PR_LABEL" "needs-conflict-resolution"; do
    host_label_create "$label" 0E8A16
  done
  PR_URL=$(host_pr_create "$FORK_DEFAULT_BRANCH" "$SYNC_BRANCH" "$PR_TITLE" "$PR_BODY" "$PR_LABEL") || {
    echo "WARNING: failed to open PR: $PR_URL" >&2
  }
  echo "  PR: ${PR_URL:-<none>} (label: $PR_LABEL)"

  if [ "$PR_LABEL" = "auto-merge" ] && [ "$AUTO_MERGE" = "true" ]; then
    echo ""
    echo "=== Auto-merging PR (all gates green, auto.merge enabled) ==="
    MERGE_SHA=$(host_pr_merge "$SYNC_BRANCH" 2>&1) || {
      echo "WARNING: auto-merge failed: $MERGE_SHA" >&2
    }
    [ "$PHASED" = "1" ] && printf 'MERGE_SHA=%q\n' "${MERGE_SHA:-}" >> "$STATE_FILE"
    if [ "$PHASED" = "1" ]; then
      result_json true "fork-sync-$FORK_NAME" pr "PR merged (${MERGE_SHA:0:12})"
    fi
  elif [ "$PHASED" = "1" ]; then
    result_json false "fork-sync-$FORK_NAME" pr "PR left for review (label=$PR_LABEL; auto.merge=$AUTO_MERGE)"
  fi
}

# ── Phase: tag (machine-cut release trigger) ─────────────────────────────────
phase_tag() {
  cd_repo
  AUTO_RELEASE=$(read_yaml '.auto.release // false')
  MERGE_SHA="${MERGE_SHA:-}"
  RELEASE_TAG=""

  if [ "$AUTO_RELEASE" != "true" ] || [ -z "$MERGE_SHA" ]; then
    echo ""
    echo "=== No release (auto.release=$AUTO_RELEASE, merged=${MERGE_SHA:+yes}${MERGE_SHA:-no}) ==="
    if [ "$PHASED" = "1" ]; then
      result_json false "fork-sync-$FORK_NAME" tag "no release"
    fi
    return 0
  fi

  # Re-sync the release branch so we can tag the merged commit.
  git fetch --quiet origin "$FORK_DEFAULT_BRANCH"
  git checkout --quiet "$FORK_DEFAULT_BRANCH" 2>/dev/null || true
  git reset --hard --quiet "origin/$FORK_DEFAULT_BRANCH"

  # Nearest upstream tag → release identity. Two failure modes in shallow
  # clones: (1) describe can't reach the real tag (--tags at depth N only
  # fetches within the window — v16.0.2 wasn't even in the clone), (2) describe
  # succeeds but returns a NON-RELEASE tag (v16.0.0-dev) reachable in the
  # window. So: describe under || true (never under pipefail into set -e),
  # then VALIDATE the result is a pure version; anything else (-dev/-rc/-rezus
  # suffixes) falls back to the highest pure-version tag ON THE REMOTE for this
  # release line (branch prefix v16.0/forgejo → v16.0.*, via ls-remote).
  UPSTREAM_VER=$( { git describe --tags --abbrev=0 "upstream/$UPSTREAM_BRANCH" 2>/dev/null || true; } \
    | sed -E 's/(-rc\.[0-9]+|-rezus\.[0-9]+).*$//')
  TAG_PREFIX="${UPSTREAM_BRANCH%%/*}"
  if ! echo "$UPSTREAM_VER" | grep -qE "^v[0-9]+\.[0-9]+\.[0-9]+$"; then
    # Pure-version tags live on the UPSTREAM repo (the fork only carries our
    # v*-rezus.N triggers + possibly legacy hand tags) — query the real host.
    # Version = three segments: <prefix=vMAJOR.MINOR>.<PATCH>.
    UPSTREAM_VER=$(git ls-remote --tags "$UPSTREAM_URL" "refs/tags/${TAG_PREFIX}.*" 2>/dev/null \
      | awk -F/ '{print $NF}' | grep -E "^${TAG_PREFIX//./\\.}\.[0-9]+$" | sort -V | tail -1)
  fi
  if [ -z "$UPSTREAM_VER" ]; then
    echo "WARNING: could not determine upstream version for release tag — skipping auto-release"
  else
    # Highest existing rezus build for this upstream version — query the
    # REMOTE (shallow clones may not have fetched a prior run's tags); default 0.
    LAST_REZUS=$(git ls-remote --tags origin "refs/tags/${UPSTREAM_VER}-rezus.*" 2>/dev/null \
      | awk -F/ '{print $NF}' | sort -V | tail -1)
    LAST_N=$(echo "${LAST_REZUS}" | sed -nE 's/.*-rezus\.([0-9]+).*/\1/p')
    [ -z "$LAST_N" ] && LAST_N=0
    NEXT_N=$((LAST_N + 1))
    RELEASE_TAG="${UPSTREAM_VER}-rezus.${NEXT_N}"

    HEAD_TAG=$(git tag --points-at HEAD | grep -E "${UPSTREAM_VER}-rezus\." || true)
    if [ -n "$HEAD_TAG" ]; then
      echo "=== HEAD already tagged ($HEAD_TAG) — skipping auto-release ==="
      RELEASE_TAG=""
    else
      echo "=== Auto-releasing: tagging $FORK_DEFAULT_BRANCH as $RELEASE_TAG ==="
      git tag "$RELEASE_TAG"
      if git push --quiet origin "$RELEASE_TAG" 2>&1; then
        echo "  tagged $RELEASE_TAG → fork release workflow will build + publish image"
      else
        echo "WARNING: failed to push tag $RELEASE_TAG"
        RELEASE_TAG=""
      fi
    fi
  fi
  if [ "$PHASED" = "1" ]; then
    [ -n "$RELEASE_TAG" ] && result_json true "$RELEASE_TAG" tag "release tag cut" \
                         || result_json false "fork-sync-$FORK_NAME" tag "no release"
  fi
}

# cd into the phased clone (state must exist). Re-sources the host abstraction:
# each phase runs as its own process (its own graph node) and needs the
# host_* helpers + credentials.
cd_repo() {
  if [ "$PHASED" = "1" ]; then
    state_load                 # separate process per node — reload state
    WORKDIR="${HARMOSTES_WORKDIR:-/tmp}"; WORKDIR="${WORKDIR%/}/fork-${FORK_NAME}"
    cd "$WORKDIR"
  else
    cd "$WORKDIR"              # single process — state is in memory
  fi
  # shellcheck source=scripts/git-host.sh
  # shellcheck disable=SC1091
  source "$SCRIPT_DIR/git-host.sh"
  host_setup
}

# compose_pr_body builds the PR body from the current state variables.
compose_pr_body() {
  cat <<EOF
## Upstream sync: ${FORK_NAME}

**Upstream**: ${UPSTREAM_URL} (\`${UPSTREAM_BRANCH}\`)
**New commits**: ${NEW_COMMITS} since last sync
**Range**: ${MERGE_BASE:0:8}..${UPSTREAM_HEAD:0:8}

### Patch verification

$(echo -e "$PATCH_RESULTS")

**Status**: ${PATCH_STATUS}

$(if [ "$PATCH_STATUS" = "needs-review" ]; then echo "⚠️ Some patches need manual review — a signature was not found after the merge."; else echo "✅ All patches verified — this PR is auto-mergeable."; fi)

### Post-merge hook

$(if [ "${HOOK_RAN:-false}" = "true" ]; then echo "Ran \`post-merge-hooks/${FORK_NAME}.sh\`"; else echo "(no post-merge hook)"; fi)

---

${VALIDATION_RESULTS}

---

### Divergence integrity

$(if $DIVERGENCE_FAILED; then echo "❌ A fork feature was LOST in this sync (see job logs). PR is labelled \`needs-fix\` — it will not auto-merge."; else echo "✅ All declared additive paths + auto divergences intact (Gate 4b)."; fi)

---

_Automated by platform/harmostes/fork-maintenance (k8s-config GitOps)._
EOF
}

# ── Dispatch ─────────────────────────────────────────────────────────────────
if [ "$MODE" = "mapping" ]; then
  # Mapping-table transport (the forgejo self-sync, sync.yml's walk in the
  # plugin). See docs/forgejo-cutover.md for the equivalence contract.
  # shellcheck disable=SC1091
  source "$SCRIPT_DIR/mapping-mode.sh"
  mapping_dispatch
  exit 0
fi

if [ "$MODE" = "subtree" ]; then
  # Vendored-tree mode: everything lives in scripts/subtree-mode.sh (sourced
  # here so the phases see this script's helpers: read_yaml, result_json,
  # push_sync_branch). Merge-mode code below is never reached for subtree defs.
  # shellcheck source=scripts/subtree-mode.sh
  # shellcheck disable=SC1091
  source "$SCRIPT_DIR/subtree-mode.sh"
  subtree_dispatch
  exit 0
fi

case "$PHASE" in
  merge)    phase_merge ;;
  hook)     phase_hook ;;
  gates)    phase_gates ;;
  validate) phase_validate ;;
  pr)       phase_pr ;;
  tag)      phase_tag ;;
  all)
    # Legacy single-shot flow for forks not yet graph-native. Exit codes:
    # 0 = up to date or PR opened (auto-merged if auto.merge); 2 = conflict;
    # 3 = push failure. Identical semantics to the pre-phase-mode single-shot flow.
    phase_merge          # exits 0 (up-to-date) or 2 (conflict) or 3 on its own
    phase_hook
    phase_validate
    phase_gates
    phase_pr
    AUTO_RELEASE=$(read_yaml '.auto.release // false')
    if [ "$PR_LABEL" = "auto-merge" ] && [ "$AUTO_MERGE" = "true" ]; then
      phase_tag
    fi
    echo ""
    echo "=== Sync complete ==="
    echo "  PR: ${PR_URL:-<none>}"
    echo "  Label: ${PR_LABEL}"
    echo "  Patch status: ${PATCH_STATUS}"
    echo "  Divergence: $(if $DIVERGENCE_FAILED; then echo '❌ LOST (needs-fix)'; else echo '✅ intact'; fi)"
    [ -n "${RELEASE_TAG:-}" ] && echo "  Released tag: $RELEASE_TAG (image build triggered)"
    ;;
esac

# Exit 0 explicitly: with set -e, a bare `[ -n "" ] && ...` as the final
# statement returns 1, which made every successful sync (auto.release: false
# → empty RELEASE_TAG) report "sync failed (exit 1)" despite a merged PR.
exit 0
