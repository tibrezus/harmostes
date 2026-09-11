#!/usr/bin/env bash
# Regenerate the committed Workflow Code island bundle (ADR-0012 §2, #416).
#
# The artifact in internal/ui/static/vendor/code-island/ is vendored the way
# htmx.min.js is: a prebuilt third-party artifact, committed. Development and
# CI never build JavaScript — this script exists only for version bumps, run
# by hand, and its output is reviewed like any other diff.
#
# Usage:  cd tools/code-island && npm ci && npm run build
set -euo pipefail
cd "$(dirname "$0")"

OUT=../../internal/ui/static/vendor/code-island

./node_modules/.bin/esbuild src/entry.js \
  --bundle --minify --format=iife --global-name=HarmostesCodeIsland \
  --loader:.ttf=file --asset-names='[name][ext]' \
  --outfile="$OUT/code-island.js"

# CSS imported by Monaco's ESM tree is collected into one stylesheet; the
# codicon font ships beside it (the suggest-widget icons need it). esbuild
# derives the asset name from the source path — pin it to codicon.ttf so the
# CSS reference and the file agree.
sed -i 's/codiconttf\.ttf/codicon.ttf/g' "$OUT/code-island.css"
[ -f "$OUT/codiconttf.ttf" ] && mv "$OUT/codiconttf.ttf" "$OUT/codicon.ttf"
./node_modules/.bin/esbuild src/yaml.worker.js \
  --bundle --minify --format=iife \
  --outfile="$OUT/yaml.worker.js"

./node_modules/.bin/esbuild src/editor.worker.js \
  --bundle --minify --format=iife \
  --outfile="$OUT/editor.worker.js"

cp node_modules/monaco-editor/LICENSE "$OUT/LICENSE.monaco-editor" 2>/dev/null || true
cp node_modules/monaco-yaml/LICENSE.md "$OUT/LICENSE.monaco-yaml" 2>/dev/null || true

echo "bundle written to $OUT:"
ls -la "$OUT"
