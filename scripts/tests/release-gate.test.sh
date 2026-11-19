#!/usr/bin/env bash
# release-gate contract tests (#919).
#
# The gate is the ONLY path from merged conventional commits to a shipped
# version, and it runs in an environment no Go test can reach. Its whole
# correctness is discovery + classification + the automerge call, so the
# contract is pinned here: canned GitHub responses, the REAL script, and
# assertions on what it decided.
#
# The load-bearing case is the creation race. release-please creates the
# release PR in the SAME run, and `gh pr list --label` is GitHub's SEARCH
# index, which lags a brand-new PR by seconds to minutes — the creation run
# saw nothing, armed nothing, and the patch sat un-automerged until the NEXT
# merge into main re-ran the gate (live: 1.11.17 needed a manual
# workflow_dispatch to arm). The stub models exactly that: the index-free
# head-branch list sees the PR, the label search does not.
#
# Run: bash scripts/tests/release-gate.test.sh   (exit != 0 on failure)
# CI:  wired next to boundary-guard in .github/workflows/ci.yml.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
GATE="$REPO_ROOT/scripts/release-gate.sh"
[ -f "$GATE" ] || { echo "TEST FAILURE: gate not found at $GATE" >&2; exit 1; }

pass=0
fail=0
ok()  { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1"; }
check() { # $1 = case name, $2 = expected substring, $3 = actual output
  if grep -qF "$2" <<<"$3"; then ok "$1"; else bad "$1 — expected '$2', got: $3"; fi
}

# ── the stub `gh` ───────────────────────────────────────────────────────────
# Every handler emits the JSON gh would emit for that call, then applies the
# caller's --jq filter. A stub that ignores --jq lets the gate misread the
# canned state and report green while testing nothing — the failure mode this
# suite exists to prevent.
STUB_DIR="$(mktemp -d)"
cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
jq_filter=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--jq" ]; then jq_filter="$a"; fi
  prev="$a"
done

emit() {
  # gh's --jq is raw output (jq -r), and a null filter result prints NOTHING —
  # printing the literal "null" would make the gate think it found a PR.
  if [ -n "$jq_filter" ]; then
    out="$(jq -r "$jq_filter" <<<"$1" || true)"
    [ "$out" != "null" ] && printf '%s\n' "$out"
  else
    printf '%s\n' "$1"
  fi
}

case " $*" in
  *" api "*pulls*)
    # One open PR object, only if the case declared a head-branch match.
    if [ -n "${STUB_HEAD_PR:-}" ]; then
      emit "[{\"number\":${STUB_HEAD_PR},\"head\":{\"ref\":\"${STUB_HEAD_REF}\"}}]"
    else
      emit "[]"
    fi
    ;;
  *" pr list "*--label*)
    if [ -n "${STUB_LABEL_PR:-}" ]; then
      emit "[{\"number\":${STUB_LABEL_PR}}]"
    else
      emit "[]"
    fi
    ;;
  *" pr view "*--json*comments*)
    emit "{\"comments\":[{\"body\":\"${STUB_COMMENTS:-}\"}]}"
    ;;
  *" pr view "*--json*mergeStateStatus*)
    emit '{"mergeStateStatus":"CLEAN"}'
    ;;
  *" pr view "*--json*title,body,labels*)
    labels="[]"
    if [ -n "${STUB_LABELS:-}" ]; then
      labels="["
      IFS=,
      for l in $STUB_LABELS; do labels="$labels{\"name\":\"$l\"},"; done
      labels="${labels%,}]"
    fi
    emit "{\"title\":\"${STUB_TITLE}\",\"body\":\"${STUB_BODY}\",\"labels\":$labels}"
    ;;
  *" release list "*--json*tagName*)
    if [ -n "${STUB_TAG:-}" ]; then emit "[{\"tagName\":\"$STUB_TAG\"}]"; else emit "[]"; fi
    ;;
  *" pr comment "*)
    # comment_once redirects stdout, so the posted body is recorded to the
    # case's log file — that is what the assertions inspect.
    printf '%s\n' "${!#}" >> "${STUB_LOG:-/dev/null}"
    ;;
  *" pr merge "*)
    echo "[stub] merge armed: $*"
    ;;
  *)
    echo "stub gh got unexpected args: $*" >&2; exit 1
    ;;
esac
EOF
chmod +x "$STUB_DIR/gh"

# State is assigned on its own lines and exported: bash does not make
# same-statement prefix assignments visible to that statement's command
# substitution, so `FOO=bar out="$(...)"` would run the stub with FOO unset —
# a silently vacuous suite reporting green while testing nothing.
run_gate() { PATH="$STUB_DIR:$PATH" GITHUB_REPOSITORY="test/repo" bash "$GATE" 2>&1; }
check_log() { # $1 = case name, $2 = expected substring in the posted-comment log
  if grep -qF "$2" "${STUB_LOG:-}"; then ok "$1"; else bad "$1 — posted: $(cat "${STUB_LOG:-}")"; fi
}
BOT_BODY="This PR was generated with [Release Please](https://github.com/googleapis/release-please)"

# 1. THE RACE: visible by head branch, invisible to the label search.
export STUB_HEAD_PR=918 STUB_HEAD_REF=release-please--branches--main STUB_LABEL_PR=
export STUB_TITLE="chore(main): release 1.11.17" STUB_BODY="$BOT_BODY"
export STUB_LABELS="autorelease: pending" STUB_TAG="v1.11.16" STUB_COMMENTS=
out="$(run_gate)"
check "head-branch discovery finds the just-created PR" "gating release PR #918" "$out"
check "patch bump arms auto-merge" "patch bump ('chore(main): release 1.11.17') — arming auto-merge" "$out"
check "the armed merge is actually invoked" "[stub] merge armed" "$out"

# 2. Fallback still works when the head branch is not the default.
export STUB_HEAD_PR= STUB_LABEL_PR=919 STUB_TITLE="chore(main): release 1.11.18"
out="$(run_gate)"
check "label search remains the fallback" "gating release PR #919" "$out"

# 3. Nothing pending is a clean no-op.
export STUB_HEAD_PR= STUB_LABEL_PR=
out="$(run_gate)"
check "no pending release PR exits 0" "no pending release PR — nothing to gate" "$out"

# 4. Provenance: a hand-rolled release PR goes RED.
export STUB_HEAD_PR=920 STUB_LABEL_PR= STUB_TITLE="chore(main): release 1.11.19"
export STUB_BODY="I wrote this by hand." STUB_LABELS="autorelease: pending" STUB_TAG="v1.11.18"
export STUB_LOG="$(mktemp)"
if out="$(run_gate)"; then
  bad "hand-rolled release PR must go red — gate returned 0: $out"
else
  ok "hand-rolled release PR refused"
  check_log "refusal is explained on the PR" "release-gate: provenance reject"
fi

# 5. An unreadable body is a SKIP, never a verdict (#801).
export STUB_HEAD_PR=921 STUB_TITLE="chore(main): release 1.11.20" STUB_BODY= STUB_TAG="v1.11.19"
out="$(run_gate)"
check "empty body skips without rejecting" "body not readable yet — skipping" "$out"

# 6. A minor bump without the label is HELD, and arms nothing.
export STUB_HEAD_PR=922 STUB_TITLE="chore(main): release 1.12.0" STUB_BODY="$BOT_BODY"
export STUB_LABELS="autorelease: pending" STUB_TAG="v1.11.20"
export STUB_LOG="$(mktemp)"
out="$(run_gate)"
check "minor bump held for the owner" "HELD: 'chore(main): release 1.12.0' is a minor/major bump" "$out"
check_log "the hold explains the owner gate" "release-gate: minor hold"
if grep -qF "merge armed" <<<"$out"; then
  bad "a held minor must NOT arm a merge: $out"
else
  ok "held minor arms no merge"
fi

# 7. A labeled minor ships.
export STUB_HEAD_PR=923 STUB_TITLE="chore(main): release 1.12.0"
export STUB_LABELS="autorelease: pending,minor-approved"
out="$(run_gate)"
check "minor-approved ships" "carries 'minor-approved' — arming auto-merge" "$out"

# 8. Hold comments are marker-deduped.
export STUB_HEAD_PR=924 STUB_TITLE="chore(main): release 1.13.0"
export STUB_LABELS="autorelease: pending" STUB_TAG="v1.12.9"
export STUB_COMMENTS="release-gate: minor hold already posted"
export STUB_LOG="$(mktemp)"
out="$(run_gate)"
if [ -s "$STUB_LOG" ]; then
  bad "a hold comment was posted despite the marker: $(cat "$STUB_LOG")"
else
  ok "hold comment deduped"
fi

# 9. A PR on an unrelated head branch is not gated (the filter is exact).
export STUB_HEAD_REF=some-other-branch STUB_HEAD_PR=925 STUB_LABEL_PR=
out="$(run_gate)"
check "unrelated open PRs are not gated" "no pending release PR — nothing to gate" "$out"

echo "release-gate: $pass pass, $fail fail"
[ "$fail" -eq 0 ] || exit 1
