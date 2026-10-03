#!/usr/bin/env bash
# Tests for conflict-event.sh — the unified conflict-escalation contract (#637).
# Hermetic: stubs curl/sleep via a PATH shim, a stub read_yaml, no network, no
# hosts. Covers:
#   - conflict_branch_for: stable per-row branch (NO date — the PR-pileup fix)
#   - emit_conflict_event: manifest fallback + CloudEvent shape (type/source,
#     row context, conflict branch, conflict files) + legacy empty-row shape
#     + the retry-then-warning path
#   - event_row_context: mapping-mode payload, legacy payload, garbage input
# Mutation-probed before landing (see PR).
#
# Run: bash chart/fork-maintenance/scripts/tests/conflict-contract.test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../conflict-event.sh
source "$SCRIPT_DIR/conflict-event.sh"

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ok: $1"; }
fail() { FAIL=$((FAIL+1)); echo "  FAIL: $1" >&2; }
assert_eq() { # assert_eq <got> <want> <what>
  if [ "$1" = "$2" ]; then ok "$3"; else fail "$3 (got '$1', want '$2')"; fi
}

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# ── conflict_branch_for: stable, date-free, slug-safe ───────────────────────
echo "# conflict_branch_for"
assert_eq "$(conflict_branch_for "v16.0/forgejo")" "conflict/v16.0-forgejo" "row slug: slashes dashed"
assert_eq "$(conflict_branch_for "master")" "conflict/master" "single-segment row"
assert_eq "$(conflict_branch_for "v16.0/forgejo")" "$(conflict_branch_for "v16.0/forgejo")" "stable across calls (same day or not)"
if conflict_branch_for "v16.0/forgejo" | grep -qE '[0-9]{4}-[0-9]{2}-[0-9]{2}'; then
  fail "branch must not embed a date (the daily-PR-pileup bug)"
else
  ok "branch embeds no date"
fi

# ── emit_conflict_event ─────────────────────────────────────────────────────
echo "# emit_conflict_event"
# Stub read_yaml (the lib calls the caller's parser — here: one declared patch,
# present and signature-intact, mirroring a healthy def).
read_yaml() {
  case "$1" in
    '.patches | length') echo 1 ;;
    '.patches[0].file') echo "$WORK/upstream.cfg" ;;
    '.patches[0].signature') echo "rezus-magic-string" ;;
    '.patches[0].description') echo "the one true patch" ;;
    *) echo 0 ;;
  esac
}
echo "keep rezus-magic-string intact" > "$WORK/upstream.cfg"

# curl/sleep shims: capture the POSTed body; no real sleeps.
SHIM="$WORK/bin"; mkdir -p "$SHIM"
cat > "$SHIM/curl" <<SHIM
#!/usr/bin/env bash
for prev in "\$0" "\${CAPTURED:-}"; do :; done
args=("\$@")
prev=""; for a in "\${args[@]}"; do if [ "\$prev" = "-d" ]; then printf '%s' "\$a" > "\$CAPTURE_FILE"; fi; prev="\$a"; done
exit \${CURL_RC:-0}
SHIM
chmod +x "$SHIM/curl"
printf '#!/bin/sh\nexit 0\n' > "$SHIM/sleep" && chmod +x "$SHIM/sleep"

export FORK_NAME=testfork
export UPSTREAM_URL="https://github.com/upstream/x"
export UPSTREAM_BRANCH="master"
export MERGE_BASE="abc123def456"
export UPSTREAM_HEAD="def456abc789"
export MAINT_DIR="$WORK"
export PATH="$SHIM:$PATH"
CAPTURE_FILE="$WORK/ce.json"
export CAPTURE_FILE

emit_conflict_event $'src/a.go\nsrc/b.go' "v16.0/forgejo" "rezus/forgejo-16" "conflict/v16.0-forgejo" >/dev/null

[ -f "$WORK/manifests/testfork-needs-fix.json" ] \
  && ok "needs-fix manifest written (audit / manual-runs fallback)" \
  || fail "needs-fix manifest missing"

assert_eq "$(jq -r '.type' "$CAPTURE_FILE")" "fork.conflict.needs-resolution" "CloudEvent type"
assert_eq "$(jq -r '.source' "$CAPTURE_FILE")" "fork-sync" "CloudEvent source"
assert_eq "$(jq -r '.data.fork' "$CAPTURE_FILE")" "testfork" "payload names the fork"
assert_eq "$(jq -r '.data.row.theirs' "$CAPTURE_FILE")" "v16.0/forgejo" "payload carries row.theirs"
assert_eq "$(jq -r '.data.row.ours' "$CAPTURE_FILE")" "rezus/forgejo-16" "payload carries row.ours"
assert_eq "$(jq -r '.data.conflict_branch' "$CAPTURE_FILE")" "conflict/v16.0-forgejo" "payload carries the conflict branch"
assert_eq "$(jq -r '.data.conflict_files | length' "$CAPTURE_FILE")" "2" "conflict files array populated"
assert_eq "$(jq -r '.data.patches_at_risk[0].status' "$CAPTURE_FILE")" "OK" "patch signature verified (OK)"
assert_eq "$(jq -r '.data.patches_at_risk[0].description' "$CAPTURE_FILE")" "the one true patch" "patch intent carried"

# Legacy call shape: no row args → empty row fields (def-fallback contract).
emit_conflict_event $'one.go' >/dev/null
assert_eq "$(jq -r '.data.row.theirs' "$CAPTURE_FILE")" "" "legacy event: empty row.theirs (def fallback)"
assert_eq "$(jq -r '.data.conflict_branch' "$CAPTURE_FILE")" "" "legacy event: empty conflict_branch"

# Retry-then-warning: resolver unreachable → emit still exits 0 (the conflict
# escalation itself is the caller's exit 2; the emit's job is warn + manifest).
CURL_RC=7 emit_conflict_event $'x.go' "t" "o" "conflict/t" > "$WORK/unreachable.out" 2>&1 \
  && ok "unreachable resolver: emit exits 0 (warning, not failure)" \
  || fail "unreachable resolver made emit fail"
grep -q "WARNING: resolver unreachable" "$WORK/unreachable.out" \
  && ok "unreachable resolver → explicit warning (manifest is the fallback)" \
  || fail "no warning on unreachable resolver"

# ── event_row_context ───────────────────────────────────────────────────────
echo "# event_row_context"
IFS='|' read -r T O B <<<"$(event_row_context '{"row":{"theirs":"v16.0/forgejo","ours":"rezus/forgejo-16"},"conflict_branch":"conflict/v16.0-forgejo","fork":"forgejo"}')"
assert_eq "${T}/${O}/${B}" "v16.0/forgejo/rezus/forgejo-16/conflict/v16.0-forgejo" "mapping-mode payload parsed"
IFS='|' read -r T O B <<<"$(event_row_context '{"fork":"dapr","conflict_files":["go.mod"]}')"
assert_eq "${T}${O}${B}" "" "legacy payload → empty row context (def fallback)"
IFS='|' read -r T O B <<<"$(event_row_context '{"row":{"theirs":"","ours":"rezus/x"}}')"
assert_eq "${T}|${O}|${B}" "|rezus/x|" "partial row: empty field survives (no shift)"
IFS='|' read -r T O B <<<"$(event_row_context 'not json at all')" ; T=${T:-x}; O=${O:-x}; B=${B:-x}
assert_eq "${T}${O}${B}" "xxx" "garbage payload → empty context, no crash"

echo ""
echo "passed: $PASS  failed: $FAIL"
[ "$FAIL" -eq 0 ]
