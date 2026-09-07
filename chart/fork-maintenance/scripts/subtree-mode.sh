#!/usr/bin/env bash
# =============================================================================
# subtree-mode.sh — vendored upstream trees, as data (fork.yaml schema v2)
# =============================================================================
# Sourced by sync-fork.sh when a fork def declares `mode: subtree`. The sync
# unit is (target repo, vendored path, pin file) — not a fork repo. The plugin
# NEVER edits vendored content: detection compares upstream tags
# (upstream.selector) against the pin, the policy matrix decides, and
# propagation is DELEGATED to the target repo's own sync_command; the pristine
# gate then verifies the delegate's result against the upstream archive.
#
# Severity/policy model (mirrors the monorepo's vendored-guards rule — RED =
# something automation should have already fixed):
#   patch → propagate: auto, merge: auto (only AFTER the PR's CI is green —
#           host_pr_watch; never merge red)
#   minor → propagate: auto, merge: manual (merge is ALWAYS manual unless a
#           policy explicitly says auto)
#   major → propagate: never (evaluation required — verdict-only run)
# Releases are structural: they ride the target repo's tag cycle — the tag
# phase only reports unreleased-pending; auto.release is not consulted.
#
# DRY_RUN=1 stops after planning (no branch content pushed, no PR) — the
# onboarding tool for new subtree sources and this script's acceptance harness.
#
# CR shape (schema v2 — merge-mode defs are unchanged and never reach here):
#   mode: subtree
#   upstream:
#     url: https://host/owner/upstream
#     selector: '^v[0-9]+\.[0-9]+\.[0-9]+$'   # tag regex (subtrees pin releases)
#   subtree:
#     url: https://host/owner/target           # repo carrying the vendored tree
#     branch: release-branch                   # PR target
#     platform: github|forgejo|local
#     token_env: GITHUB_TOKEN
#     path: vendor/ tree location              # trailing slash optional
#     pin_file: path/to/PIN                    # single source of truth
#     sync_command: hack/sync-thing.sh         # delegate; called with <tag>
#     archive: 'https://host/owner/upstream/archive/{tag}.tar.gz'
#     patches_dir: ''                          # optional; see gate limitation
#   policy:
#     patch: { propagate: auto, merge: auto }
#     minor: { propagate: auto, merge: manual }
#     major: { propagate: never }
#   lockstep:                                  # alert-level expectations (data)
#     - "k8s-iac module image tag == pin"
# =============================================================================

sub_state_save() {
  { for v in FORK_NAME UPSTREAM_URL SELECTOR SUB_URL SUB_BRANCH SUB_PATH \
             SUB_PIN_FILE SUB_SYNC_CMD SUB_ARCHIVE SUB_PATCHES_DIR \
             PIN LATEST LANE_TAG CLASS ADVISORIES SYNC_BRANCH; do
      printf '%s=%q\n' "$v" "${!v:-}"
    done
  } > "$STATE_FILE"
}

sub_state_load() {
  STATE_FILE="${HARMOSTES_WORKDIR:-/tmp}"; STATE_FILE="${STATE_FILE%/}/fork-${FORK_NAME}/.git/harmostes-state.env"
  [ -f "$STATE_FILE" ] || { echo "ERROR: no subtree sync state — run the merge phase first" >&2; exit 1; }
  # shellcheck disable=SC1090
  source "$STATE_FILE"
}

sub_cd_repo() {
  if [ "$PHASED" = "1" ]; then
    sub_state_load
    WORKDIR="${HARMOSTES_WORKDIR:-/tmp}"; WORKDIR="${WORKDIR%/}/fork-${FORK_NAME}"
    cd "$WORKDIR"
  else
    cd "$WORKDIR"
  fi
  FORK_URL="$SUB_URL"             # host_setup's local-platform log line reads FORK_URL
  FORK_DEFAULT_BRANCH="$SUB_BRANCH"  # host_pr_merge's local case merges into this
  # shellcheck source=scripts/git-host.sh
  # shellcheck disable=SC1091
  source "$SCRIPT_DIR/git-host.sh"
  host_setup
}

sub_parse_cfg() {
  SUB_URL=$(read_yaml '.subtree.url')
  SUB_BRANCH=$(read_yaml '.subtree.branch')
  SUB_PATH=$(read_yaml '.subtree.path' | sed 's:/*$::')
  SUB_PIN_FILE=$(read_yaml '.subtree.pin_file')
  SUB_SYNC_CMD=$(read_yaml '.subtree.sync_command')
  SUB_ARCHIVE=$(read_yaml '.subtree.archive // ""' 2>/dev/null || echo "")
  SUB_PATCHES_FILE=$(read_yaml '.subtree.patches_file // ""' 2>/dev/null || echo "")
  SELECTOR=$(read_yaml '.upstream.selector')
  local v
  for v in "$SUB_URL" "$SUB_BRANCH" "$SUB_PATH" "$SUB_PIN_FILE" "$SUB_SYNC_CMD" "$SELECTOR"; do
    if [ -z "$v" ] || [ "$v" = "null" ]; then
      echo "ERROR: subtree def missing required field (url='$SUB_URL' branch='$SUB_BRANCH' path='$SUB_PATH' pin_file='$SUB_PIN_FILE' sync_command='$SUB_SYNC_CMD' selector='$SELECTOR')" >&2
      exit 1
    fi
  done
  # archive tarball is the git-upstream probe; OCI upstreams probe via helm pull
  case "$UPSTREAM_URL" in
    oci://*) ;;
    *) case "$SUB_ARCHIVE" in
         *'{tag}'*) ;;
         *) echo "ERROR: subtree.archive must contain {tag} for git upstreams" >&2; exit 1;;
       esac ;;
  esac
}

sub_read_pin() {  # $1 = clone dir → stdout: pin tag (last non-comment line)
  awk 'NF && $0 !~ /^[[:space:]]*#/ {v=$0} END {print v}' "$1/$SUB_PIN_FILE" 2>/dev/null | tr -d '[:space:]'
}

oci_tags() {  # $1 = oci://host/ns/repo → newline-separated tags (v2 API, anonymous token dance)
  local host path reg hdr realm service tok json
  path="${1#oci://}"; host="${path%%/*}"; path="${path#*/}"
  reg="https://$host"
  # flip to plain-http only on connect/TLS failure — an HTTP 401 on /v2/
  # (anonymous) still proves the HTTPS registry is there
  curl -s -o /dev/null --max-time 10 "$reg/v2/" || reg="http://$host"
  hdr=$(curl -s "$reg/v2/" -o /dev/null -D - | tr -d '\r' | awk 'tolower($1)=="www-authenticate:"{print $0}')
  tok=""
  if [ -n "$hdr" ]; then
    realm=$(echo "$hdr" | sed -nE 's/.*realm="([^"]+)".*/\1/p')
    service=$(echo "$hdr" | sed -nE 's/.*service="([^"]+)".*/\1/p')
    if [ -n "$realm" ]; then
      tok=$(curl -s "$realm?service=${service}&scope=repository:${path}:pull" | jq -r '.token // .access_token // empty')
    fi
  fi
  if [ -n "$tok" ]; then
    json=$(curl -s -H "Authorization: Bearer $tok" "$reg/v2/$path/tags/list")
  else
    json=$(curl -s "$reg/v2/$path/tags/list")   # registry without auth (drills)
  fi
  echo "$json" | jq -r '.tags[]?'
}

sub_fetch_upstream_tree() {  # $1 = tag → stdout: upstream tree dir (under $TMP)
  local tag="$1"
  case "$UPSTREAM_URL" in
    oci://*)
      mkdir -p "$TMP/up"
      helm pull "$UPSTREAM_URL" --version "$tag" --untar --untardir "$TMP/up" >/dev/null 2>&1 \
        || helm pull "$UPSTREAM_URL" --version "$tag" --untar --untardir "$TMP/up" --plain-http >/dev/null 2>&1 \
        || { echo "ERROR: helm pull $UPSTREAM_URL:$tag failed" >&2; return 1; }
      find "$TMP/up" -mindepth 1 -maxdepth 1 -type d | head -1
      ;;
    *)
      local archive_url="${SUB_ARCHIVE/\{tag\}/$tag}"
      mkdir -p "$TMP/src"
      curl -fsSL "$archive_url" -o "$TMP/src.tar.gz" \
        || { echo "ERROR: archive fetch failed: $archive_url" >&2; return 1; }
      tar -xzf "$TMP/src.tar.gz" -C "$TMP/src"
      find "$TMP/src" -mindepth 1 -maxdepth 1 -type d | head -1
      ;;
  esac
}

sub_class() {  # $1=candidate $2=pin → major|minor|patch|same
  local cMa cMi cPa pMa pMi pPa
  IFS=. read -r cMa cMi cPa <<< "${1#v}"
  IFS=. read -r pMa pMi pPa <<< "${2#v}"
  [ "$cMa" != "$pMa" ] && { echo major; return; }
  [ "$cMi" != "$pMi" ] && { echo minor; return; }
  [ "$cPa" != "$pPa" ] && { echo patch; return; }
  echo same
}

sub_lane_latest() {  # $1 = newline-separated tags, $2 = pin → newest tag in the pin's major.minor line
  local lane
  lane="$(echo "$2" | sed -E 's/^v?([0-9]+\.[0-9]+)\..*/\1/')"   # v-optional (OCI chart tags carry none)
  lane="${lane//./\\.}"
  echo "$1" | grep -E "^v?${lane}\." | sort -V | tail -1
}

sub_major_latest() {  # $1 = newline-separated tags, $2 = pin → newest tag in the pin's MAJOR (minor candidate)
  local major
  major="$(echo "$2" | sed -E 's/^v?([0-9]+)\..*/\1/')"
  major="${major//./\\.}"
  echo "$1" | grep -E "^v?${major}\." | sort -V | tail -1
}

sub_advisories() {  # human summary of upstream classes ahead beyond the propagated lane
  local cls pol
  cls=$(sub_class "$LATEST" "$PIN")
  [ "$cls" = "same" ] && return 0
  pol=$(read_yaml ".policy.$cls.propagate // \"auto\"")
  case "$cls" in
    patch)
      # a patch gap is the auto lane's own work — no review ask, just pending
      echo "ADVISORY: patch $LATEST ahead of pin $PIN — auto-propagates on the next live run"
      ;;
    major)
      echo "ADVISORY: upstream major release $LATEST ahead of pin $PIN — policy: major bump = evaluation required"
      ;;
    *)
      echo "ADVISORY: upstream $cls release $LATEST ahead of pin $PIN — policy: $cls bump = manual merge + validation contract"
      ;;
  esac
}

sub_lockstep_note() {
  local note
  note=$(read_yaml '.lockstep[]' 2>/dev/null || true)
  if [ -n "$note" ]; then
    echo "Lockstep expectations (alert-level):"
    echo "$note" | sed 's/^/  - /'
  fi
}

# ── phase: merge (detect → policy guard → delegate propagation) ──────────────
phase_subtree_merge() {
  sub_parse_cfg
  if [ "$PHASED" = "1" ]; then
    WORKDIR="${HARMOSTES_WORKDIR:-/tmp}"; WORKDIR="${WORKDIR%/}/fork-${FORK_NAME}"
    rm -rf "$WORKDIR"; mkdir -p "$(dirname "$WORKDIR")"
  else
    WORKDIR=$(mktemp -d)
  fi

  echo ""
  echo "=== Cloning target repo (shallow): $SUB_URL @ $SUB_BRANCH ==="
  git clone --depth 50 --branch "$SUB_BRANCH" "$SUB_URL" "$WORKDIR"
  cd "$WORKDIR"
  FORK_URL="$SUB_URL"
  # shellcheck source=scripts/git-host.sh
  # shellcheck disable=SC1091
  source "$SCRIPT_DIR/git-host.sh"
  host_setup

  PIN=$(sub_read_pin "$WORKDIR")
  if [ -z "$PIN" ]; then
    echo "ERROR: pin file '$SUB_PIN_FILE' carries no version" >&2; exit 1
  fi
  echo "$PIN" | grep -qE "$SELECTOR" || { echo "ERROR: pin '$PIN' does not match selector '$SELECTOR'" >&2; exit 1; }

  case "$UPSTREAM_URL" in
    oci://*)
      UPSTREAM_TAGS=$(oci_tags "$UPSTREAM_URL" | grep -E "$SELECTOR" | sort -V)
      ;;
    *)
      UPSTREAM_TAGS=$(git ls-remote --tags "$UPSTREAM_URL" 2>/dev/null | sed 's#.*refs/tags/##' \
                      | grep -E "$SELECTOR" | grep -v '\^{}$' | sort -V)
      ;;
  esac
  LATEST=$(echo "$UPSTREAM_TAGS" | tail -1)
  if [ -z "$LATEST" ]; then
    echo "ERROR: no upstream tags match selector '$SELECTOR' at $UPSTREAM_URL" >&2; exit 1
  fi
  echo "=== detect: pin=$PIN  upstream latest=$LATEST ==="

  # Decide from the pin's LANE first: a patch-class bump in the pin's own
  # major.minor line is the auto-propagatable unit (auto-merge after CI watch).
  # Next, the newest MINOR in the pin's major propagates as a prepared PR with
  # manual merge (policy minor.propagate=auto) — humans review and run smoke.
  # A newer MAJOR is an advisory, never the bump.
  LANE_TAG=$(sub_lane_latest "$UPSTREAM_TAGS" "$PIN")
  LANE_CLASS=$(sub_class "${LANE_TAG:-$PIN}" "$PIN")
  CLASS=$(sub_class "$LATEST" "$PIN")   # overall-latest class (for verdicts/advisories)

  if [ "$LANE_CLASS" = "patch" ]; then
    # patch bump available — the patch lane's policy decides
    POLICY_PROP=$(read_yaml '.policy.patch.propagate // "auto"')
    ADVISORIES=$(sub_advisories)
    if [ "$POLICY_PROP" != "auto" ]; then
      echo "=== Patch $LANE_TAG available — policy: propagate=$POLICY_PROP, verdict only ==="
      [ -n "$ADVISORIES" ] && echo "$ADVISORIES"
      sub_lockstep_note
      [ "$PHASED" = "1" ] && result_json false "fork-sync-$FORK_NAME" merge "patch $LANE_TAG available — propagate=$POLICY_PROP"
      exit 0
    fi
    CLASS=patch   # what we are propagating — downstream phases (pr/tag) key off this
  else
    # lane current — minor-of-major or verdict
    if [ "$CLASS" = "same" ]; then
      echo "=== Up to date: pin $PIN is the newest upstream tag ==="
      [ "$PHASED" = "1" ] && result_json false "fork-sync-$FORK_NAME" merge "up to date — pin $PIN"
      exit 0
    fi
    ADVISORIES=$(sub_advisories)
    # Minor candidate: newest tag sharing the pin's MAJOR (pin v12.7.3 → newest
    # v12.*). A newer overall major upstream does NOT suppress it — the bump
    # matrix treats minors as review-gated bumps, majors as evaluations.
    local minor_tag minor_policy
    minor_tag=$(sub_major_latest "$UPSTREAM_TAGS" "$PIN")
    minor_policy=$(read_yaml '.policy.minor.propagate // "auto"')
    if [ -n "$minor_tag" ] && [ "$minor_tag" != "$PIN" ] && [ "$minor_policy" = "auto" ]; then
      # The plugin PREPARES the PR (delegate does the vendoring); merge is
      # manual by hard policy — only the patch class may auto-merge. Humans
      # review the changelog/CVEs and run the validation contract (smoke).
      echo "=== minor-class $minor_tag — policy: propagate=auto, PR waits for manual merge (+ validation contract) ==="
      [ -n "$ADVISORIES" ] && echo "$ADVISORIES"
      LANE_TAG="$minor_tag"   # what we propagate is the newest minor
      CLASS=minor              # ...and the propagation class IS minor
    else
      POLICY_PROP=$(read_yaml ".policy.$CLASS.propagate // \"auto\"")
      echo "=== Pin's lane is current; $CLASS-class $LATEST — policy: propagate=$POLICY_PROP, verdict only ==="
      [ -n "$ADVISORIES" ] && echo "$ADVISORIES"
      sub_lockstep_note
      [ "$PHASED" = "1" ] && result_json false "fork-sync-$FORK_NAME" merge "$CLASS-class $LATEST — propagate=$POLICY_PROP"
      exit 0
    fi
  fi

  SYNC_BRANCH="rezus/sync-subtree-${FORK_NAME}-${LANE_TAG#v}"
  if [ "${DRY_RUN:-0}" = "1" ]; then
    echo "=== DRY RUN — plan (nothing written) ==="
    echo "  branch:      $SYNC_BRANCH (off $SUB_BRANCH)"
    echo "  propagate:   $SUB_SYNC_CMD $LANE_TAG"
    echo "  gate:        pristine diff vs ${SUB_ARCHIVE/\{tag\}/$LANE_TAG}"
    echo "  pr:          → $SUB_BRANCH"
    [ -n "$ADVISORIES" ] && echo "  $ADVISORIES"
    sub_lockstep_note
    [ "$PHASED" = "1" ] && result_json false "fork-sync-$FORK_NAME" merge "dry-run: would propagate $LANE_TAG"
    exit 0
  fi

  echo ""
  echo "=== Branching $SYNC_BRANCH off $SUB_BRANCH ==="
  git checkout -b "$SYNC_BRANCH"

  echo "=== Propagating via delegate: $SUB_SYNC_CMD $LANE_TAG ==="
  UPSTREAM_OCI="$UPSTREAM_URL" bash "$SUB_SYNC_CMD" "$LANE_TAG"   # the plugin knows the upstream; the delegate honors it

  git add -A
  if [ -z "$(git status --porcelain)" ]; then
    echo "ERROR: delegate produced no changes for $LANE_TAG — silent no-op, refusing" >&2
    exit 1
  fi
  git commit --quiet -m "chore(sync-${FORK_NAME}): vendor ${LANE_TAG}"
  echo "  committed vendored ${LANE_TAG}"

  STATE_FILE="$WORKDIR/.git/harmostes-state.env"
  sub_state_save
  [ "$PHASED" = "1" ] && result_json true "fork-sync-$FORK_NAME" merge "propagated $LANE_TAG (patch-class, auto)"
  return 0
}

# ── phase: gates (pristine diff vs the upstream archive) ─────────────────────
phase_subtree_gates() {
  sub_cd_repo
  sub_parse_cfg
  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT
  local src_dir
  src_dir="$(sub_fetch_upstream_tree "$LANE_TAG")" || exit 1
  echo ""

  if [ -z "$SUB_PATCHES_FILE" ] || [ ! -f "$SUB_PATCHES_FILE" ]; then
    # ── pristine gate (no patch contract) ──
    echo "=== Pristine gate: vendored tree vs upstream at $LANE_TAG ==="
    # --strip-trailing-cr: the target repo's EOL normalization policy is not drift.
    if diff -r --strip-trailing-cr "$src_dir" "$SUB_PATH" >/dev/null; then
      echo "  pristine: OK — byte-identical to upstream $LANE_TAG (modulo EOL policy)"
    else
      echo "  pristine: DRIFT — the delegate left unaccounted edits in $SUB_PATH" >&2
      diff -rq --strip-trailing-cr "$src_dir" "$SUB_PATH" | head -20 >&2
      exit 1
    fi
  else
    # ── patch-accounting gate: the diff must be exactly the declared contract ──
    echo "=== Patch-accounting gate: $SUB_PATH vs upstream $LANE_TAG (contract: $SUB_PATCHES_FILE) ==="
    local red=0
    local d line rel
    while IFS= read -r d; do
      case "$d" in
        "Files $src_dir"*)
          rel=$(echo "$d" | sed -E "s#^Files $src_dir/(.*) and .*#\1#")
          local sig
          sig=$(yq -r ".patches[] | select(.path == \"$rel\") | .signature" "$SUB_PATCHES_FILE" 2>/dev/null)
          if [ -z "$sig" ] || [ "$sig" = "null" ]; then
            echo "  RED: $rel differs from upstream and is not in the patch contract" >&2; red=1
          elif ! grep -qF -- "$sig" "$SUB_PATH/$rel" 2>/dev/null; then
            echo "  RED: $rel differs but signature '$sig' not found — patch unaccounted or rebased away" >&2; red=1
          else
            echo "  patch: OK — $rel (signature '$sig' present)"
          fi
          ;;
        "Only in $SUB_PATH"*)
          rel=$(echo "$d" | sed -E "s#^Only in $SUB_PATH/?(.*): (.*)#\1/\2#" | sed 's#^/##')
          if yq -r ".preserve[]" "$SUB_PATCHES_FILE" 2>/dev/null | grep -qxF "$(echo "$rel" | sed -E 's#^([^/]+/).*#\1#')" \
             || yq -r ".preserve[]" "$SUB_PATCHES_FILE" 2>/dev/null | grep -qxF "$rel"; then
            echo "  preserve: OK — $rel (locally-added, declared)"
          else
            echo "  RED: $rel exists only locally and is not in the preserve list" >&2; red=1
          fi
          ;;
        "Only in $src_dir"*)
          rel=$(echo "$d" | sed -E "s#^Only in $src_dir/?(.*): (.*)#\1/\2#" | sed 's#^/##')
          echo "  RED: $rel exists upstream but is missing from $SUB_PATH — deletions are not modeled" >&2; red=1
          ;;
      esac
    done < <(diff -rq --strip-trailing-cr "$src_dir" "$SUB_PATH" 2>/dev/null)
    # preserve entries are promises of PRESENCE (a vanished preserved file is
    # invisible to the diff — nothing catches it otherwise)
    while IFS= read -r p; do
      [ -z "$p" ] && continue
      [ -e "$SUB_PATH/$p" ] || { echo "  RED: preserve entry $p missing from $SUB_PATH" >&2; red=1; }
    done < <(yq -r '.preserve[]' "$SUB_PATCHES_FILE" 2>/dev/null)
    # stale patches: declared but no longer differing
    while IFS= read -r p; do
      [ -z "$p" ] && continue
      if [ ! -e "$SUB_PATH/$p" ]; then
        echo "  RED: patch entry $p missing from $SUB_PATH" >&2; red=1
      elif diff -q --strip-trailing-cr "$src_dir/$p" "$SUB_PATH/$p" >/dev/null 2>&1; then
        echo "  WARN: patch entry $p is byte-identical to upstream — stale contract entry"
      fi
    done < <(yq -r '.patches[].path' "$SUB_PATCHES_FILE" 2>/dev/null)
    [ "$red" = "1" ] && { echo "  patch-accounting: RED — unaccounted differences in $SUB_PATH" >&2; exit 1; }
    echo "  patch-accounting: OK — every difference is declared and signed"
  fi
  sub_lockstep_note
  [ "$PHASED" = "1" ] && result_json true "fork-sync-$FORK_NAME" gates "${SUB_PATCHES_FILE:+patches accounted vs }$LANE_TAG"
  return 0
}

# ── phase: pr (push → PR → CI watch → merge strictly per policy) ─────────────
phase_subtree_pr() {
  sub_cd_repo
  local merge_policy
  merge_policy=$(read_yaml '.policy.patch.merge // "manual"')
  [ "$CLASS" = "patch" ] || merge_policy="manual"   # only the auto class may auto-merge

  if [ "${DRY_RUN:-0}" = "1" ]; then
    echo "=== DRY RUN — PR plan (nothing pushed) ==="
    echo "  push:   $SYNC_BRANCH → origin"
    echo "  PR:     $SYNC_BRANCH → $SUB_BRANCH"
    echo "  merge:  policy.$CLASS.merge=$merge_policy (auto merges only after host_pr_watch sees the PR CI green)"
    [ "$PHASED" = "1" ] && result_json false "fork-sync-$FORK_NAME" pr "dry-run: PR planned"
    exit 0
  fi

  echo ""
  echo "=== Pushing $SYNC_BRANCH ==="
  push_sync_branch 2>&1 || { echo "ERROR: failed to push sync branch" >&2; exit 3; }

  local pr_title="chore(sync-${FORK_NAME}): vendor ${LANE_TAG} (${CLASS}-class)"
  local pr_body
  pr_body="## Vendored-tree sync: ${FORK_NAME} → ${LANE_TAG}"
  pr_body+=$'\n\n'
  pr_body+="**Pin**: ${PIN} → **${LANE_TAG}** (${CLASS}-class; policy merge=${merge_policy})"$'\n'
  pr_body+="**Upstream newest**: ${LATEST}"$'\n\n'
  if [ -n "${ADVISORIES:-}" ]; then pr_body+="${ADVISORIES}"$'\n\n'; fi
  local validate_list
  validate_list=$(read_yaml '.validate // []' | sed 's/^- //')
  if [ -n "$validate_list" ]; then
    pr_body+="### Validation contract"$'\n'
    while IFS= read -r v; do
      [ -n "$v" ] && pr_body+="- [ ] ${v}"$'\n'
    done <<EOF
$validate_list
EOF
    pr_body+=$'\n'
  fi
  pr_body+="### Pristine gate"$'\n'"Byte-diff of \`${SUB_PATH}/\` against the upstream archive at ${LANE_TAG} — green in the run logs."$'\n\n'
  pr_body+="### Release"$'\n'"Rides the target repo's tag cycle (\`v*-rezus.*\`) — this PR does not release."$'\n\n'
  pr_body+="---"$'\n\n_Automated by the fork-maintenance workflow (subtree mode) — k8s-config GitOps._'$'\n'

  local label="needs-review"
  [ "$merge_policy" = "auto" ] && label="auto-merge"
  host_label_create "$label" 0E8A16 2>/dev/null || true

  echo "=== Opening PR ==="
  local pr_url
  pr_url=$(host_pr_create "$SUB_BRANCH" "$SYNC_BRANCH" "$pr_title" "$pr_body" "$label") || {
    echo "WARNING: failed to open PR: $pr_url" >&2
  }
  echo "  PR: ${pr_url:-<none>} (label: $label)"

  if [ "$merge_policy" = "auto" ]; then
    echo ""
    echo "=== Watching PR CI (auto-merge only on green — never red) ==="
    if host_pr_watch "$SYNC_BRANCH"; then
      local merge_sha
      merge_sha=$(host_pr_merge "$SYNC_BRANCH" 2>&1) || {
        echo "WARNING: auto-merge failed: $merge_sha" >&2
      }
      [ "$PHASED" = "1" ] && result_json true "fork-sync-$FORK_NAME" pr "PR merged (${merge_sha:0:12})"
    else
      echo "  CI red or pending — PR left open for review (never merge red)"
      [ "$PHASED" = "1" ] && result_json false "fork-sync-$FORK_NAME" pr "PR left open — CI not green"
    fi
  else
    [ "$PHASED" = "1" ] && result_json false "fork-sync-$FORK_NAME" pr "PR left for review (policy merge=$merge_policy)"
  fi
  return 0
}

# ── phase: tag (structural: releases ride the target repo's tag) ─────────────
phase_subtree_tag() {
  sub_cd_repo
  local latest_release
  latest_release=$(git ls-remote --tags origin 'refs/tags/v*-rezus.*' 2>/dev/null \
                   | sed 's#.*refs/tags/##' | sort -V | tail -1)
  local note="releases ride the target repo's tag cycle (structural)"
  if [ -n "$LANE_TAG" ] && [ -n "$latest_release" ]; then
    local cls; cls=$(sub_class "$latest_release" "$LANE_TAG")
    [ "$cls" != "same" ] && note="unreleased-pending: vendored ${LANE_TAG}, latest target release ${latest_release}"
  fi
  echo "=== $note ==="
  [ "$PHASED" = "1" ] && result_json false "fork-sync-$FORK_NAME" tag "$note"
  return 0
}

# ── phase: validate (subtree validation IS the target repo's CI) ─────────────
phase_subtree_validate() {
  sub_cd_repo
  echo "=== Subtree validation = the target repo's own CI (watched in the pr phase) ==="
  [ "$PHASED" = "1" ] && result_json false "fork-sync-$FORK_NAME" validate "monorepo-ci is the validation surface"
  return 0
}

subtree_dispatch() {
  case "$PHASE" in
    merge)    phase_subtree_merge ;;
    gates)    phase_subtree_gates ;;
    validate) phase_subtree_validate ;;
    pr)       phase_subtree_pr ;;
    tag)      phase_subtree_tag ;;
    all)
      # Single-shot: propagate → gate → PR. Exit codes match merge-mode all:
      # 0 = up to date / PR opened (auto-merged if policy allows); 3 = push failure.
      phase_subtree_merge
      phase_subtree_gates
      phase_subtree_pr
      phase_subtree_tag
      echo ""
      echo "=== Subtree sync complete ==="
      echo "  Pin: $PIN → $LANE_TAG ($CLASS-class)"
      [ -n "${ADVISORIES:-}" ] && echo "  $ADVISORIES"
      ;;
    *) echo "ERROR: unknown phase '$PHASE'" >&2; exit 1 ;;
  esac
}
