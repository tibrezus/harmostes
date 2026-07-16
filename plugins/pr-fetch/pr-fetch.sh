#!/usr/bin/env bash
# harmostes "pr-fetch" prepare plugin (pr-review workflow) — MULTI-PLATFORM.
#
# Polls GitHub OR Forgejo/Codeberg for open PRs carrying a trigger label.
# When found, checks out the PR repo + the project wiki, fetches PR metadata
# (title, body, diff, linked issue, milestone, CI status), writes pr-context.json.
#
# MULTI-PLATFORM: the repo format is "host/owner/repo" (e.g.
# "github.com/tibrezus/harmostes", "git.rezus.cloud/tibrez/rhesadox").
# Bare "owner/repo" defaults to github.com. The host selects the API base,
# auth token, and clone URL format automatically.
#
# Contract: the LAST stdout line is JSON:
#   { "changed": false }                     — no labeled PR found (skip)
#   { "changed": true, "artifact": "...", "event": {"host":..., "repo":..., "number":..., "head_sha":...} }
#
# Config (via HARMOSTES_SPEC JSON .config, OR plugin args):
#   repos   — list of "host/owner/repo" strings
#   label   — trigger label (default: "needs-review")
#   wiki    — optional wiki repo URL for project design context
#
# Auth env (injected by the controller):
#   HARMOSTES_GIT_TOKEN      — GitHub PAT (for github.com)
#   HARMOSTES_RZC_USERNAME   — git.rezus.cloud username
#   HARMOSTES_RZC_PASSWORD   — git.rezus.cloud token/password
#   HARMOSTES_CODEBERG_TOKEN — Codeberg access token (for codeberg.org)
set -euo pipefail
log() { echo "[pr-fetch] $*"; }

SPEC="${HARMOSTES_SPEC:-}"
WORKDIR="${HARMOSTES_WORKDIR:-/workspace}"

# ── Parse config ─────────────────────────────────────────────────────────────
LABEL="needs-review"
WIKI_URL=""
REPOS=""
if [ -n "$SPEC" ]; then
  LABEL=$(echo "$SPEC" | python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('config',{}).get('label','needs-review'))" 2>/dev/null || echo "needs-review")
  WIKI_URL=$(echo "$SPEC" | python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('config',{}).get('wiki',''))" 2>/dev/null || echo "")
  REPOS=$(echo "$SPEC" | python3 -c "import sys,json;d=json.load(sys.stdin);print('\n'.join(d.get('config',{}).get('repos',[])))" 2>/dev/null || echo "")
fi
for arg in "$@"; do
  case "$arg" in
    --repos=*)  REPOS="${arg#--repos=}";;
    --label=*)  LABEL="${arg#--label=}";;
    --wiki=*)   WIKI_URL="${arg#--wiki=}";;
  esac
done

[ -n "$REPOS" ] || { echo '{"changed":false}'; exit 0; }

export LABEL WIKI_URL WORKDIR REPOS

# ── 1. Find the oldest labeled PR across repos ──────────────────────────────
log "polling label=$LABEL"
PR_JSON=$(python3 << 'PYEOF'
import json, os, urllib.request

label = os.environ["LABEL"]
repos_raw = [r.strip() for r in os.environ["REPOS"].split("\n") if r.strip()]

def resolve_host(host):
    """Map a git host to (api_base, token, is_forgejo)."""
    if host == "github.com":
        return ("https://api.github.com", os.environ.get("HARMOSTES_GIT_TOKEN", ""), False)
    elif host == "codeberg.org":
        tok = os.environ.get("HARMOSTES_CODEBERG_TOKEN", os.environ.get("LLM_WIKI_CODEBERG_TOKEN", ""))
        return ("https://codeberg.org/api/v1", tok, True)
    elif host == "git.rezus.cloud":
        tok = os.environ.get("HARMOSTES_RZC_PASSWORD", "")
        return ("https://git.rezus.cloud/api/v1", tok, True)
    else:
        tok = os.environ.get("HARMOSTES_FORGEJO_TOKEN", os.environ.get("HARMOSTES_GIT_TOKEN", ""))
        return (f"https://{host}/api/v1", tok, True)

def api(base, path, token, accept="application/json"):
    url = base + path
    req = urllib.request.Request(url, headers={
        "authorization": f"token {token}" if token else "",
        "accept": accept,
    })
    with urllib.request.urlopen(req) as resp:
        return json.loads(resp.read())

def parse_repo(s):
    parts = s.strip().split("/")
    if len(parts) == 3:
        return parts[0], f"{parts[1]}/{parts[2]}"
    elif len(parts) == 2:
        return "github.com", f"{parts[0]}/{parts[1]}"
    raise ValueError(f"bad repo format: {s}")

best = None
for repo_str in repos_raw:
    host, repo = parse_repo(repo_str)
    base, token, is_forgejo = resolve_host(host)
    try:
        prs = api(base, f"/repos/{repo}/pulls?state=open&sort=created&direction=asc&limit=30", token)
    except Exception as e:
        import sys; print(f"WARN {host}/{repo}: {e}", file=sys.stderr)
        continue
    for pr in prs:
        pr_labels = [l.get("name", "") for l in pr.get("labels", [])]
        if label in pr_labels:
            entry = {"host": host, "repo": repo, "pr": pr, "api_base": base,
                     "token": token, "is_forgejo": is_forgejo}
            if best is None or pr["created_at"] < best["pr"]["created_at"]:
                best = entry
            break  # oldest per repo (sorted asc)

print(json.dumps(best))
PYEOF
)

if [ "$PR_JSON" = "null" ]; then
  echo '{"changed":false}'
  exit 0
fi

HOST=$(echo "$PR_JSON" | python3 -c "import sys,json;print(json.load(sys.stdin)['host'])")
REPO=$(echo "$PR_JSON" | python3 -c "import sys,json;print(json.load(sys.stdin)['repo'])")
PR_NUM=$(echo "$PR_JSON" | python3 -c "import sys,json;print(json.load(sys.stdin)['pr']['number'])")
HEAD_SHA=$(echo "$PR_JSON" | python3 -c "import sys,json;print(json.load(sys.stdin)['pr']['head']['sha'])")
HEAD_REF=$(echo "$PR_JSON" | python3 -c "import sys,json;print(json.load(sys.stdin)['pr']['head']['ref'])")
API_BASE=$(echo "$PR_JSON" | python3 -c "import sys,json;print(json.load(sys.stdin)['api_base'])")
IS_FORGEJO=$(echo "$PR_JSON" | python3 -c "import sys,json;print(json.load(sys.stdin).get('is_forgejo',False))")
log "found PR $HOST/$REPO#$PR_NUM (head=${HEAD_SHA:0:8})"

export HOST REPO PR_NUM HEAD_SHA HEAD_REF API_BASE IS_FORGEJO WIKI_URL

# ── 2. Gather full context ──────────────────────────────────────────────────
log "fetching PR metadata, diff, issue, milestone, CI…"
python3 << 'PYEOF'
import json, os, re, urllib.request

token = os.environ.get("HARMOSTES_GIT_TOKEN", "")
# Re-resolve token for this host
host = os.environ["HOST"]
if host == "github.com":
    token = os.environ.get("HARMOSTES_GIT_TOKEN", "")
elif host == "codeberg.org":
    token = os.environ.get("HARMOSTES_CODEBERG_TOKEN", os.environ.get("LLM_WIKI_CODEBERG_TOKEN", ""))
elif host == "git.rezus.cloud":
    token = os.environ.get("HARMOSTES_RZC_PASSWORD", "")

base = os.environ["API_BASE"]
repo = os.environ["REPO"]
num = int(os.environ["PR_NUM"])
sha = os.environ["HEAD_SHA"]
ref = os.environ["HEAD_REF"]
workdir = os.environ["WORKDIR"]
wiki_url = os.environ.get("WIKI_URL", "")
is_forgejo = os.environ.get("IS_FORGEJO", "False") == "True"

def api(path, accept="application/json"):
    url = base + path
    req = urllib.request.Request(url, headers={
        "authorization": f"token {token}" if token else "",
        "accept": accept,
    })
    with urllib.request.urlopen(req) as resp:
        if "diff" in accept or "text/plain" in accept:
            return resp.read().decode("utf-8", errors="replace")
        return json.loads(resp.read())

pr = api(f"/repos/{repo}/pulls/{num}")

# Files changed
files = api(f"/repos/{repo}/pulls/{num}/files?limit=100")
files_summary = [
    {"status": f["status"], "filename": f["filename"],
     "additions": f["additions"], "deletions": f["deletions"]}
    for f in files
]

# Parse issue reference
issue_num = None
m = re.search(r"(?:close[sd]?|fix(?:es|ed)?|resolve[sd]?|refs?)\s+#(\d+)", pr.get("body") or "", re.I)
if m:
    issue_num = int(m.group(1))

issue_data = None
milestone = None
if issue_num:
    try:
        issue_data = api(f"/repos/{repo}/issues/{issue_num}")
        ms = issue_data.get("milestone")
        if ms:
            milestone = {"title": ms["title"], "description": ms.get("description", ""), "state": ms["state"]}
    except Exception as e:
        import sys; print(f"WARN issue: {e}", file=sys.stderr)

# CI status
ci_status = {"status": "none"}
try:
    if is_forgejo:
        # Forgejo: combined commit statuses
        statuses = api(f"/repos/{repo}/commits/{sha}/statuses")
        if statuses:
            states = [s.get("status") for s in statuses]
            ci_status = {
                "total": len(statuses),
                "all_success": all(s == "success" for s in states),
                "states": sorted(set(states)),
            }
    else:
        # GitHub: check runs
        cr = api(f"/repos/{repo}/commits/{sha}/check-runs?per_page=30")
        runs = cr.get("check_runs", [])
        if runs:
            conclusions = [r.get("conclusion") for r in runs if r.get("conclusion")]
            ci_status = {
                "total": len(runs),
                "completed": len(conclusions),
                "all_success": bool(conclusions) and all(c == "success" for c in conclusions),
                "conclusions": sorted(set(conclusions)),
            }
except Exception as e:
    import sys; print(f"WARN ci: {e}", file=sys.stderr)

# Diff — platform-specific
try:
    if is_forgejo:
        diff = api(f"/repos/{repo}/pulls/{num}", accept="text/plain")
    else:
        diff = api(f"/repos/{repo}/pulls/{num}", accept="application/vnd.github.v3.diff")
except Exception:
    diff = ""

with open(f"{workdir}/pr-diff.patch", "w") as f:
    f.write(diff[:100000])

context = {
    "host": host, "repo": repo, "number": num,
    "title": pr["title"], "body": pr.get("body") or "",
    "user": pr.get("user", {}).get("login", "?"),
    "url": pr.get("html_url", ""), "head_sha": sha, "head_ref": ref,
    "issue_number": issue_num,
    "issue": {"title": issue_data["title"], "body": issue_data.get("body") or "",
              "labels": [l["name"] for l in issue_data.get("labels", [])]} if issue_data else None,
    "milestone": milestone,
    "ci_status": ci_status,
    "files_changed": files_summary,
    "repo_dir": f"{workdir}/repo",
    "wiki_dir": f"{workdir}/wiki" if wiki_url else "",
    "review_path": f"{workdir}/review.json",
}
with open(f"{workdir}/pr-context.json", "w") as f:
    json.dump(context, f, indent=2)
PYEOF

# ── 3. Checkout the PR repo ─────────────────────────────────────────────────
REPO_DIR="$WORKDIR/repo"
rm -rf "$REPO_DIR"
log "cloning $HOST/$REPO (ref=$HEAD_REF) → $REPO_DIR"

# Build the clone URL based on host
CLONE_URL=""
case "$HOST" in
  github.com)
    CLONE_URL="https://x-access-token:${HARMOSTES_GIT_TOKEN}@github.com/${REPO}.git";;
  git.rezus.cloud)
    CLONE_URL="https://${HARMOSTES_RZC_USERNAME}:${HARMOSTES_RZC_PASSWORD}@git.rezus.cloud/${REPO}.git";;
  codeberg.org)
    CB_TOKEN="${HARMOSTES_CODEBERG_TOKEN:-${LLM_WIKI_CODEBERG_TOKEN:-}}"
    CLONE_URL="https://${CB_TOKEN}@codeberg.org/${REPO}.git";;
  *)
    CLONE_URL="https://${REPO}.git";;
esac

git clone --quiet --depth 50 --branch "$HEAD_REF" "$CLONE_URL" "$REPO_DIR" 2>&1 | tail -1 || {
  log "branch clone failed — fetching by SHA…"
  git clone --quiet --depth 50 "$CLONE_URL" "$REPO_DIR" 2>&1 | tail -1
  git -C "$REPO_DIR" fetch --quiet --depth 50 origin "$HEAD_SHA" 2>/dev/null || true
  git -C "$REPO_DIR" checkout --quiet "$HEAD_SHA" 2>/dev/null || true
}
git -C "$REPO_DIR" checkout --quiet "$HEAD_SHA" 2>/dev/null || true
git config --global --add safe.directory '*' 2>/dev/null || true

# ── 4. Checkout wiki (optional) ─────────────────────────────────────────────
if [ -n "$WIKI_URL" ]; then
  WIKI_DIR="$WORKDIR/wiki"
  rm -rf "$WIKI_DIR"
  WC="$WIKI_URL"
  case "$WIKI_URL" in
    https://github.com/*) WC="https://x-access-token:${HARMOSTES_GIT_TOKEN}@${WIKI_URL#https://}";;
  esac
  log "cloning wiki → $WIKI_DIR"
  git clone --quiet --depth 50 "$WC" "$WIKI_DIR" 2>&1 | tail -1 || log "WARN: wiki clone failed"
fi

log "context ready → $WORKDIR/pr-context.json"
echo "{\"changed\":true,\"artifact\":\"$WORKDIR/pr-context.json\",\"status\":\"ok\",\"event\":{\"host\":\"$HOST\",\"repo\":\"$REPO\",\"number\":$PR_NUM,\"head_sha\":\"$HEAD_SHA\"}}"
