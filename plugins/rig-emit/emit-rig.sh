#!/usr/bin/env bash
set -euo pipefail

# emit-rig.sh — Universal RIG generator entry point.
#
# Auto-detects all build systems in the repo and generates a complete RIG.
# Replaces the old per-language emit-<lang>.sh scripts.
#
# For backward compatibility, if a specific language is requested via $2,
# it is passed as a hint to the Python generator.
#
# Usage: emit-rig.sh <output.json> [language-hint]

OUT="${1:?Usage: emit-rig.sh <output.json> [language-hint]}"
LANG_HINT="${2:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ -n "$LANG_HINT" ]; then
    python3 "$SCRIPT_DIR/emit-rig.py" "$OUT" --language "$LANG_HINT"
else
    python3 "$SCRIPT_DIR/emit-rig.py" "$OUT"
fi
