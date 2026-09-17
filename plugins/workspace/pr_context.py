#!/usr/bin/env python3
"""pr_context.py — the pr-review workspace's reviewer-context builder (#428).

Split out of workspace.sh so every fact the reviewing agent relies on is
derived HERE, testable hermetically (the Go suite runs the real file against
a fake forge + a fixture git repo — see internal/worker/workspace_plugin_test.go).

Two phases, both env-driven (the envelope workspace.sh already exports):

  meta     Fetch the PR object once; persist its metadata (title, body,
           refs, MERGE STATE) to .pr-meta.json and the head ref to .head_ref.
           Runs BEFORE the clone (the clone wants the branch name).

  context  Runs AFTER the clone + checkout at the dispatched HEAD_SHA.
           Derives pr-diff.patch, files_changed and diff_stats from ONE
           git tree — merge-base(base_ref, HEAD_SHA)..HEAD_SHA — so all
           three describe the exact reviewed commit, never the forge's
           current-PR state, and can never disagree (#428 finding 2).
           The patch is written IN FULL — no truncation cap (#428 finding
           3: a head-first 100 KB slice of a vendored-first diff omitted
           the entire Go delta; the workaround became the de-facto path).
           API fallback (paginated files + .diff endpoint) only when git
           derivation fails, and the ctx records which source produced
           the diff (diff_source: "git" | "api") so a reviewer can weigh
           provenance. Merge state and the gate's verified contexts are
           stated as facts from the API/envelope (#428 findings 1 and 4:
           the agent narrated a merged PR that was open, and named gates
           that do not exist).

Stdlib only; exit ≠ 0 fails prepare loudly (a silent skip would orphan the
label — the exact stuck state the envelope check guards against).
"""
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

# git status letter → forge-API vocabulary (consumers compare against the
# API's words; git's own letters would leak an implementation detail).
GIT_STATUS = {"A": "added", "C": "copied", "D": "deleted", "M": "modified",
              "T": "changed", "R": "renamed", "U": "modified"}


def api(base, path, token, accept="application/json"):
    """Bounded retry (3 attempts, 2s/4s backoff) around every fetch (#267):
    one dropped DNS UDP response must not fail the whole prepare step.
    Retries ONLY transport-level failures (URLError/socket), never
    HTTPError: a 401/403/404 is a deterministic answer, not weather."""
    req = urllib.request.Request(
        base + path,
        headers={"authorization": f"token {token}" if token else "", "accept": accept})
    last = None
    for attempt in range(3):
        try:
            with urllib.request.urlopen(req) as resp:
                body = resp.read()
                if "diff" in accept or "text/plain" in accept:
                    return body.decode("utf-8", "replace")
                return json.loads(body)
        except urllib.error.HTTPError:
            raise
        except (urllib.error.URLError, OSError) as e:
            last = e
            if attempt < 2:
                time.sleep(2 * (attempt + 1))
    raise last


def git(repo_dir, *args):
    return subprocess.run(["git", "-C", repo_dir, *args],
                          capture_output=True, text=True)


def derive_from_git(repo_dir, base_ref, sha):
    """The #428 core: diff + files + stats from one tree pair, in the
    reviewer's checkout, at the dispatched SHA. Returns None when the
    shallow clone cannot see the merge base (caller falls back to API)."""
    base = ""
    for candidate in (f"origin/{base_ref}" if base_ref else "", "FETCH_HEAD"):
        if candidate and git(repo_dir, "rev-parse", "--verify", "--quiet",
                             candidate + "^{commit}").returncode == 0:
            base = git(repo_dir, "rev-parse", candidate + "^{commit}").stdout.strip()
            break
    if not base:
        return None
    mb = git(repo_dir, "merge-base", base, sha)
    if mb.returncode != 0 or not mb.stdout.strip():
        return None
    mb = mb.stdout.strip()
    diff = git(repo_dir, "diff", mb, sha)
    if diff.returncode != 0:
        return None
    files = []
    numstat = git(repo_dir, "diff", "--numstat", mb, sha).stdout.splitlines()
    namestat = git(repo_dir, "diff", "--name-status", mb, sha).stdout.splitlines()
    for ns in namestat:
        parts = ns.split("\t")
        if len(parts) < 2:
            continue
        status = GIT_STATUS.get(parts[0][:1], "modified")
        path = parts[-1]
        add = dele = 0
        for ln in numstat:
            n = ln.split("\t")
            if len(n) == 3 and n[2] == path:
                add = int(n[0]) if n[0].isdigit() else 0   # binary → "-"
                dele = int(n[1]) if n[1].isdigit() else 0
                break
        files.append({"status": status, "filename": path,
                      "additions": add, "deletions": dele})
    return {"diff": diff.stdout, "files": files, "source": "git",
            "merge_base": mb}


def derive_from_api(base, repo, num, token, is_fj):
    """Fallback when git derivation is impossible: paginate the files list
    to completeness (the old single ?limit=100 call silently truncated —
    #428 finding 2's 30-vs-68 disagreement) and take the forge's .diff
    surface in full."""
    files = []
    page = 1
    while True:
        batch = api(base, f"/repos/{repo}/pulls/{num}/files?limit=300&page={page}", token)
        files.extend(batch)
        if len(batch) < 300:
            break
        page += 1
        if page > 20:  # 6000 files: beyond any reviewable PR; stop, facts stay sane
            break
    diff = ""
    try:
        if is_fj:
            # Forgejo does NOT honor Accept content negotiation on /pulls/N
            # (it returns the API JSON object — observed: pr-diff.patch with
            # zero 'diff --git' lines). The .diff suffix is the supported
            # surface.
            diff = api(base, f"/repos/{repo}/pulls/{num}.diff", token,
                       accept="text/plain")
        else:
            diff = api(base, f"/repos/{repo}/pulls/{num}", token,
                       accept="application/vnd.github.v3.diff")
    except Exception:
        pass
    fs = [{"status": f["status"], "filename": f["filename"],
           "additions": f["additions"], "deletions": f["deletions"]}
          for f in files]
    return {"diff": diff, "files": fs, "source": "api", "merge_base": None}


def phase_meta():
    base, repo = os.environ["API_BASE"], os.environ["REPO"]
    num, token = os.environ["PR_NUM"], os.environ.get("GIT_HOST_TOKEN", "")
    workdir = os.environ["WORKDIR"]
    pr = api(base, f"/repos/{repo}/pulls/{num}", token)
    meta = {
        "title": pr.get("title", ""),
        "body": pr.get("body") or "",
        "user": pr.get("user", {}).get("login", "?"),
        "url": pr.get("html_url", ""),
        "head_ref": pr.get("head", {}).get("ref", ""),
        "base_ref": pr.get("base", {}).get("ref", ""),
        # Merge state as FACT from the API (#428 finding 1: the context let
        # the agent narrate an open PR as merged into its own head).
        "state": pr.get("state", ""),
        "merged": bool(pr.get("merged")),
        "merged_at": pr.get("merged_at"),
        "base_sha": pr.get("base", {}).get("sha", ""),
    }
    with open(f"{workdir}/.head_ref", "w") as f:
        f.write(meta["head_ref"])
    with open(f"{workdir}/.pr-meta.json", "w") as f:
        json.dump(meta, f, indent=2)


def phase_context():
    base, repo = os.environ["API_BASE"], os.environ["REPO"]
    num = int(os.environ["PR_NUM"])
    sha = os.environ["HEAD_SHA"]
    workdir = os.environ["WORKDIR"]
    repo_dir = f"{workdir}/repo"
    token = os.environ.get("GIT_HOST_TOKEN", "")
    is_fj = os.environ.get("IS_FJ", "false") == "true"
    wiki_url = os.environ.get("WIKI_URL", "")
    with open(f"{workdir}/.pr-meta.json") as f:
        meta = json.load(f)
    base_ref = os.environ.get("TRIG_BASE", "") or meta.get("base_ref", "")

    # ── diff + files + stats: git first, API fallback, source stamped ──────
    derived = derive_from_git(repo_dir, base_ref, sha)
    if derived is None:
        derived = derive_from_api(base, repo, num, token, is_fj)
    diff, fs = derived["diff"], derived["files"]
    # IN FULL — the patch is the contract most runs trust; the manifest
    # (files_changed + diff_stats) is how a huge diff is navigated, not a
    # cap (#428 finding 3).
    with open(f"{workdir}/pr-diff.patch", "w") as f:
        f.write(diff)
    # Deterministic scope signal (#30): facts in prepare, interpretation in
    # the task. Counts from the same tree the agent will read.
    dfiles = len(set(re.findall(r"^diff --git (\S+)", diff, re.M)))
    dins = sum(1 for l in diff.splitlines() if l.startswith("+") and not l.startswith("+++"))
    ddel = sum(1 for l in diff.splitlines() if l.startswith("-") and not l.startswith("---"))
    dlines = dins + ddel
    scope = "huge" if dlines > 2000 or dfiles > 40 else ("large" if dlines > 400 or dfiles > 10 else "focused")

    # ── linked issue + milestone (unchanged behavior) ──────────────────────
    issue_num = None
    m = re.search(r"(?:close[sd]?|fix(?:es|ed)?|resolve[sd]?|refs?)\s+#(\d+)",
                  meta["body"], re.I)
    if m:
        issue_num = int(m.group(1))
    issue_data = None
    milestone = None
    if issue_num:
        try:
            issue_data = api(base, f"/repos/{repo}/issues/{issue_num}", token)
            ms = issue_data.get("milestone")
            if ms:
                milestone = {"title": ms["title"],
                             "description": ms.get("description", ""),
                             "state": ms["state"]}
        except Exception as e:
            print(f"WARN issue: {e}", file=sys.stderr)

    # ── CI at the dispatched SHA (unchanged behavior) ──────────────────────
    ci = {"status": "none"}
    try:
        if is_fj:
            statuses = api(base, f"/repos/{repo}/commits/{sha}/statuses", token)
            # First-wins per context (list is newest-first): mirrors the
            # kernel gate evaluator (internal/review/review.go ContextStates
            # dedupe) — superseded attempts must not clobber the freshest
            # state.
            states = {}
            for s in statuses:
                states.setdefault(s.get("context"), s.get("status"))
            if states:
                vals = list(states.values())
                ci = {"total": len(vals),
                      "all_success": all(v == "success" for v in vals),
                      "states": sorted(set(vals))}
        else:
            cr = api(base, f"/repos/{repo}/commits/{sha}/check-runs?per_page=30", token)
            runs = cr.get("check_runs", [])
            if runs:
                c = [r.get("conclusion") for r in runs if r.get("conclusion")]
                ci = {"total": len(runs), "completed": len(c),
                      "all_success": bool(c) and all(x == "success" for x in c),
                      "conclusions": sorted(set(c))}
    except Exception as e:
        print(f"WARN ci: {e}", file=sys.stderr)

    # ── the gate's verified contexts, as facts (#428 finding 4: narratives
    # named gates that do not exist — the envelope KNOWS the real ones) ────
    gate = {}
    try:
        env = json.loads(os.environ.get("TRIG_CONTEXTS", "") or "{}")
        gate = {"required_contexts": env.get("requiredContexts", []),
                "green_contexts": env.get("greenContexts", [])}
    except (ValueError, TypeError):
        gate = {"required_contexts": [], "green_contexts": []}

    if meta["merged"]:
        merge_note = f"MERGED at {meta['merged_at']} into {meta['base_ref']} — a verdict here reviews history, not an open change"
    elif meta["state"] == "closed":
        merge_note = "closed WITHOUT merging"
    else:
        merge_note = "open (not merged)"

    ctx = {
        "host": os.environ["HOST"], "repo": repo, "number": num,
        "title": meta["title"], "body": meta["body"], "user": meta["user"],
        "url": meta["url"],
        "pr_state": meta["state"], "merged": meta["merged"],
        "merged_at": meta["merged_at"], "merge_note": merge_note,
        "head_sha": sha, "head_ref": meta["head_ref"],
        "base": os.environ.get("TRIG_BASE", ""),
        "merge_base": derived["merge_base"],
        "diff_source": derived["source"],
        "issue_number": issue_num,
        "issue": {"title": issue_data["title"], "body": issue_data.get("body") or "",
                  "labels": [l["name"] for l in issue_data.get("labels", [])]}
                  if issue_data else None,
        "milestone": milestone, "ci_status": ci, "gate": gate,
        "files_changed": fs, "repo_dir": repo_dir,
        "diff_stats": {"files": dfiles, "insertions": dins, "deletions": ddel},
        "scope": scope,
        "wiki_dir": f"{workdir}/wiki" if wiki_url else "",
        "review_path": f"{workdir}/review.json",
    }
    with open(f"{workdir}/pr-context.json", "w") as f:
        json.dump(ctx, f, indent=2)


if __name__ == "__main__":
    if len(sys.argv) != 2 or sys.argv[1] not in ("meta", "context"):
        print("usage: pr_context.py {meta|context}", file=sys.stderr)
        sys.exit(2)
    if sys.argv[1] == "meta":
        phase_meta()
    else:
        phase_context()
