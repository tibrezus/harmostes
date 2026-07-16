#!/usr/bin/env bash
# harmostes "post-review" deploy plugin (pr-review workflow) — MULTI-PLATFORM.
#
# Reads review.json + pr-context.json, posts the review to GitHub OR
# Forgejo/Codeberg, and removes the trigger label.
#
# Multi-platform: the host in pr-context.json selects the API base, auth token,
# and review-event mapping (GitHub: APPROVE; Forgejo: APPROVED).
#
# GitHub auth: HARMOSTES_GIT_TOKEN
# Forgejo auth: HARMOSTES_RZC_PASSWORD (git.rezus.cloud) or HARMOSTES_CODEBERG_TOKEN
set -euo pipefail
log() { echo "[post-review] $*"; }

WORKDIR="${HARMOSTES_WORKDIR:-/workspace}"
REVIEW="$WORKDIR/review.json"
CONTEXT="$WORKDIR/pr-context.json"
SPEC="${HARMOSTES_SPEC:-}"
LABEL="needs-review"

if [ -n "$SPEC" ]; then
  LABEL=$(echo "$SPEC" | python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('config',{}).get('label','needs-review'))" 2>/dev/null || echo "needs-review")
fi

[ -f "$REVIEW" ]   || { echo "ERROR: review.json missing" >&2; exit 1; }
[ -f "$CONTEXT" ]  || { echo "ERROR: pr-context.json missing" >&2; exit 1; }

HOST=$(python3 -c "import json;print(json.load(open('$CONTEXT'))['host'])")
REPO=$(python3 -c "import json;print(json.load(open('$CONTEXT'))['repo'])")
PR_NUM=$(python3 -c "import json;print(json.load(open('$CONTEXT'))['number'])")

# Resolve API base + token for this host
case "$HOST" in
  github.com)
    API_BASE="https://api.github.com"
    TOKEN="${HARMOSTES_GIT_TOKEN:?}"
    IS_FORGEJO="false"
    ;;
  codeberg.org)
    API_BASE="https://codeberg.org/api/v1"
    TOKEN="${HARMOSTES_CODEBERG_TOKEN:-${LLM_WIKI_CODEBERG_TOKEN:-}}"
    IS_FORGEJO="true"
    ;;
  git.rezus.cloud)
    API_BASE="https://git.rezus.cloud/api/v1"
    TOKEN="${HARMOSTES_RZC_PASSWORD:?}"
    IS_FORGEJO="true"
    ;;
  *)
    API_BASE="https://$HOST/api/v1"
    TOKEN="${HARMOSTES_FORGEJO_TOKEN:-${HARMOSTES_GIT_TOKEN:-}}"
    IS_FORGEJO="true"
    ;;
esac

export API_BASE TOKEN HOST REPO PR_NUM REVIEW LABEL IS_FORGEJO WORKDIR

# ── Build + post the review ──────────────────────────────────────────────────
log "posting review to $HOST/$REPO#$PR_NUM…"

PAYLOAD=$(python3 << 'PYEOF'
import json, os

host = os.environ["HOST"]
is_forgejo = os.environ.get("IS_FORGEJO") == "true"

with open(os.environ["REVIEW"]) as f:
    review = json.load(f)

# Map the decision to the platform-specific event name.
# GitHub: APPROVE | REQUEST_CHANGES | COMMENT
# Forgejo: APPROVED | REQUEST_CHANGES | COMMENT
decision = review["decision"]
if is_forgejo and decision == "APPROVE":
    event = "APPROVED"
elif is_forgejo and decision == "REQUEST_CHANGES":
    event = "REQUEST_CHANGES"
elif is_forgejo and decision == "COMMENT":
    event = "COMMENT"
else:
    event = decision  # GitHub uses the raw value

payload = {"body": review["body"], "event": event}

comments = review.get("comments", [])
if comments:
    if is_forgejo:
        # Forgejo review comments: path + line + body (no side field)
        payload["comments"] = [
            {"path": c["path"], "line": int(c.get("line", 1)), "body": c["body"]}
            for c in comments
        ]
    else:
        # GitHub: path + line + side + body
        payload["comments"] = [
            {"path": c["path"], "line": int(c.get("line", 1)),
             "side": c.get("side", "RIGHT"), "body": c["body"]}
            for c in comments
        ]

print(json.dumps(payload))
PYEOF
)

# Post with graceful fallbacks
post_review() {
  local payload="$1"
  curl -fsSL -X POST \
    -H "authorization: token $TOKEN" \
    -H "accept: application/json" \
    "$API_BASE/repos/$REPO/pulls/$PR_NUM/reviews" \
    -d "$payload" 2>&1
}

RESPONSE=$(post_review "$PAYLOAD") || {
  # Retry without inline comments
  log "full review failed — retrying without inline comments…"
  PAYLOAD_NOCOMMENTS=$(echo "$PAYLOAD" | python3 -c "import sys,json;p=json.load(sys.stdin);p.pop('comments',None);print(json.dumps(p))")
  RESPONSE=$(post_review "$PAYLOAD_NOCOMMENTS") || {
    # Final fallback: downgrade to COMMENT (APPROVE/REQUEST_CHANGES may be rejected)
    log "review rejected — falling back to COMMENT…"
    PAYLOAD_COMMENT=$(echo "$PAYLOAD_NOCOMMENTS" | python3 -c "
import sys,json
p=json.load(sys.stdin)
p['event']='COMMENT'
print(json.dumps(p))
")
    RESPONSE=$(post_review "$PAYLOAD_COMMENT") || {
      echo "ERROR: API rejected review" >&2
      exit 1
    }
  }
}

RID=$(echo "$RESPONSE" | python3 -c "import sys,json;print(json.load(sys.stdin).get('id','?'))" 2>/dev/null || echo "?")
DEC=$(python3 -c "import json;print(json.load(open('$REVIEW'))['decision'])")
log "review posted: id=$RID decision=$DEC"

# ── Remove the trigger label ────────────────────────────────────────────────
log "removing label '$LABEL'…"
curl -fsSL -X DELETE \
  -H "authorization: token $TOKEN" \
  -H "accept: application/json" \
  "$API_BASE/repos/$REPO/issues/$PR_NUM/labels/$LABEL" 2>/dev/null || \
  log "WARN: could not remove label '$LABEL'"

echo "{\"artifact\":\"pr-$HOST-$REPO-$PR_NUM\",\"status\":\"ok\",\"event\":{\"host\":\"$HOST\",\"repo\":\"$REPO\",\"number\":$PR_NUM,\"review_id\":\"$RID\",\"decision\":\"$DEC\"}}"
