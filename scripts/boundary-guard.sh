#!/usr/bin/env bash
# L1 boundary guard (ADR-0011 Amendment — wiki: Configuration Contract).
#
# The kernel ships logic and documentation only. Production model ids are
# deployment decisions: they live in k8s-config HelmRelease values and — as
# resilience-contract LOGIC — in the litellm-provider fallbacks/sampling
# sources and the sol-pi reducer profile (each with its own gate). Anywhere
# else — chart values, templates, fixtures, tests, e2e specs — a production
# id means someone is re-embedding a configuration decision into the kernel
# (#777 is the cautionary tale). This guard fails that PR in CI.
set -euo pipefail
cd "$(dirname "$0")/.."

# grep --exclude-dir matches BASENAMES: 'litellm-provider' covers both the
# canonical extensions/ tree and the pinned chart/files/ copy (whose drift
# gate — test-extensions — keeps it byte-identical to the source).
ALLOWED_DIRS=(litellm-provider sol-pi golden) # golden = RENDERED output; its sources are guarded
ALLOWED_FILES=(boundary-guard.sh 'Dockerfile.worker*' piargs_test.go)
# piargs_test.go + the Dockerfiles assert the sol-pi reducer profile — the
# reducer route is LOGIC with its own profile-drift gates.
PROD_IDS=(
  'ali/anthropic/qwen3.8-flash'
  'mtplx/anthropic/qwen38-27b-optimized-speed-fp16'
  'zai/glm-4.7'
  'zai/glm-5.3-flash'
)

excludes=()
for d in "${ALLOWED_DIRS[@]}"; do excludes+=(--exclude-dir="$d"); done
for f in "${ALLOWED_FILES[@]}"; do excludes+=(--exclude="$f"); done

hits=0
for id in "${PROD_IDS[@]}"; do
  while IFS= read -r -d '' file; do
    echo "::error::boundary-guard: production model id '${id}' in ${file} — configuration is deployment-owned (ADR-0011 Amendment). Decide in k8s-config values; kernel surfaces carry obviously-test ids only."
    hits=$((hits + 1))
  done < <(grep -rlZF -e "$id" --exclude-dir={.git,.agents,node_modules,dist,bin} "${excludes[@]}" . || true)
done

if [ "$hits" -gt 0 ]; then
  echo "boundary-guard: ${hits} violation(s)" >&2
  exit 1
fi
echo "boundary-guard: clean — no production model ids outside the logic allowlist"
