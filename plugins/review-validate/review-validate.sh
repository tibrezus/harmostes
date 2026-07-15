#!/usr/bin/env bash
# harmostes "review-validate" gate plugin (pr-review workflow).
#
# Deterministic validation of the agent's review output. Checks that
# review.json exists, is valid JSON, has a recognized decision, and a non-empty
# body. exit 0 = green (the review is well-formed and can be posted); non-zero
# = malformed, and the stderr becomes feedback the agent receives to fix it.
#
# The gate does NOT judge review quality — that is the agent's job. It only
# ensures the output is structurally valid so the deploy plugin can post it.
set -euo pipefail

REVIEW="${HARMOSTES_WORKDIR:-/workspace}/review.json"

if [ ! -f "$REVIEW" ]; then
  echo "ERROR: review.json not found at $REVIEW — the agent must write its review there" >&2
  exit 1
fi

python3 << 'PYEOF'
import json, sys, os

path = os.environ.get("HARMOSTES_WORKDIR", "/workspace") + "/review.json"
try:
    with open(path) as f:
        review = json.load(f)
except json.JSONDecodeError as e:
    print(f"ERROR: review.json is not valid JSON: {e}", file=sys.stderr)
    print("Write VALID JSON to review.json with this shape:", file=sys.stderr)
    print('  {"decision": "APPROVE|REQUEST_CHANGES|COMMENT", "body": "...", "comments": []}', file=sys.stderr)
    sys.exit(1)

decision = review.get("decision", "")
body = review.get("body", "")
comments = review.get("comments", [])

valid = {"APPROVE", "REQUEST_CHANGES", "COMMENT"}
if decision not in valid:
    print(f'ERROR: decision must be one of {valid}, got "{decision}"', file=sys.stderr)
    print("Set decision to APPROVE, REQUEST_CHANGES, or COMMENT.", file=sys.stderr)
    sys.exit(1)

if not body or not body.strip():
    print("ERROR: body is empty — write a review summary explaining your decision", file=sys.stderr)
    sys.exit(1)

if not isinstance(comments, list):
    print("ERROR: comments must be a list (use [] if no inline comments)", file=sys.stderr)
    sys.exit(1)

for i, c in enumerate(comments):
    if not isinstance(c, dict):
        print(f"ERROR: comments[{i}] must be an object {{path, line, body}}", file=sys.stderr)
        sys.exit(1)
    if not c.get("path") or not c.get("body"):
        print(f"ERROR: comments[{i}] must have path + body", file=sys.stderr)
        sys.exit(1)

print(f"review.json valid: decision={decision} comments={len(comments)}")
PYEOF

echo '{"status":"ok"}'
