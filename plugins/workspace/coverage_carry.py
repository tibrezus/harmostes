#!/usr/bin/env python3
"""Coverage carry (#704): feed the per-PR coverage ledger into the round's
workspace, and emit the review delta patch.

The ledger lives in the repository-scoped shared store (the sol-pi
attachment mount — durability is that feature's whole point), one JSON
per PR:  $HARMOSTES_SOL_PI_STORE/coverage/<PR>.json

    {"version": 1, "pr": 691, "rounds": 2, "last_reviewed_sha": "...",
     "covered": [{"path": "...", "sha": "...", "round": 1, "note": "..."}],
     "frontier": [{"path": "...", "reason": "..."}],
     "findings_open": 1}

What this script does (deterministic, no decisions):
  1. Copy the ledger to $WORKDIR/coverage-ledger.json — the agent reads
     it directly. Entries covered at an older SHA get `touched: true`
     when the review delta (last_reviewed_sha..head) modifies their path:
     the frontier-first round re-verifies touched files first.
  2. Emit $WORKDIR/review-delta.patch (last_reviewed_sha..head) next to
     pr-diff.patch. The shallow clone (depth 50) may not hold
     last_reviewed_sha — a fetch is attempted, then the delta is dropped
     (WARN): the ledger still names the frontier; only the fix-diff
     convenience is lost.
  3. Stamp pr-context.json with a compact `coverage` fact block so the
     round knows at a glance that carry happened.

No store env, no ledger file, or a store that does not read → clean
no-op (the round proceeds exactly as before #704; the feature is
gated on the attachment, and absent coverage means a full-surface
round — today's behavior).

All failures are spoken on stderr and exit 0: the carry is an
optimization layered on a complete round; a broken carry must never
fail a review that would otherwise run.
"""

import json
import os
import subprocess
import sys

WORKDIR = os.environ.get("HARMOSTES_WORKDIR", "/workspace")
STORE = os.environ.get("HARMOSTES_SOL_PI_STORE", "")
HEAD_SHA = os.environ.get("HARMOSTES_TRIGGER_SHA", "")
PR_NUM = os.environ.get("HARMOSTES_TRIGGER_PR", "")
REPO_DIR = os.path.join(WORKDIR, "repo")

LEDGER_PATH = os.path.join(STORE, "coverage", "%s.json" % PR_NUM) if STORE and PR_NUM else ""
CARRY_PATH = os.path.join(WORKDIR, "coverage-ledger.json")
DELTA_PATH = os.path.join(WORKDIR, "review-delta.patch")
CTX_PATH = os.path.join(WORKDIR, "pr-context.json")


def log(msg):
    print("[coverage-carry] %s" % msg)


def warn(msg):
    print("[coverage-carry] WARN: %s" % msg, file=sys.stderr)


def git(*args):
    return subprocess.run(["git", "-C", REPO_DIR, *args],
                          capture_output=True, text=True)


def main():
    if not LEDGER_PATH or not os.path.isfile(LEDGER_PATH):
        log("no coverage ledger (%s) — full-surface round" % ("store unset" if not LEDGER_PATH else "first round"))
        return 0
    try:
        with open(LEDGER_PATH) as f:
            ledger = json.load(f)
    except (OSError, json.JSONDecodeError) as e:
        warn("ledger unreadable (%s) — full-surface round" % e)
        return 0
    if not isinstance(ledger, dict) or not ledger.get("last_reviewed_sha"):
        warn("ledger malformed (missing last_reviewed_sha) — full-surface round")
        return 0

    last_sha = ledger["last_reviewed_sha"]
    same_head = bool(last_sha == HEAD_SHA)

    # ── delta: last_reviewed_sha..head, from the checked-out clone ────────
    delta_files = []
    delta_emitted = False
    if same_head:
        log("same head as last round (%s) — thread re-check round, no delta" % last_sha[:10])
    else:
        have = git("cat-file", "-e", last_sha + "^{commit}")
        if have.returncode != 0:
            # shallow clone may predate the last-reviewed commit: one fetch,
            # then give up (the ledger's frontier still guides the round).
            git("fetch", "--quiet", "--depth", "50", "origin", last_sha)
            have = git("cat-file", "-e", last_sha + "^{commit}")
        if have.returncode == 0:
            names = git("diff", "--name-only", last_sha, HEAD_SHA)
            if names.returncode == 0:
                delta_files = [ln for ln in names.stdout.splitlines() if ln.strip()]
                patch = git("diff", "--binary", last_sha, HEAD_SHA)
                if patch.returncode == 0:
                    with open(DELTA_PATH, "w") as f:
                        f.write(patch.stdout)
                    delta_emitted = True
                    log("review delta: %d file(s) changed since %s → review-delta.patch (%d bytes)"
                        % (len(delta_files), last_sha[:10], len(patch.stdout)))
                else:
                    warn("delta patch generation failed — carry continues without it")
            else:
                warn("delta file list failed — carry continues without it")
        else:
            warn("last_reviewed_sha %s not reachable (shallow clone) — no delta; frontier still carries" % last_sha[:10])

    # ── agent-readable ledger with touched flags ──────────────────────────
    delta_set = set(delta_files)
    carried = dict(ledger)
    covered = []
    for c in ledger.get("covered") or []:
        entry = dict(c)
        entry["touched"] = (not same_head) and entry.get("path") in delta_set
        covered.append(entry)
    carried["covered"] = covered
    carried["delta_emitted"] = delta_emitted
    carried["delta_files"] = delta_files if delta_emitted else []
    with open(CARRY_PATH, "w") as f:
        json.dump(carried, f, indent=2)

    # ── compact fact block into pr-context.json ───────────────────────────
    try:
        with open(CTX_PATH) as f:
            ctx = json.load(f)
    except (OSError, json.JSONDecodeError) as e:
        warn("pr-context.json unreadable (%s) — coverage fact not stamped" % e)
        return 0
    ctx["coverage"] = {
        "last_reviewed_sha": last_sha,
        "rounds": int(ledger.get("rounds") or 0),
        "covered_total": len(covered),
        "frontier_count": len(ledger.get("frontier") or []),
        "findings_open": int(ledger.get("findings_open") or 0),
        "delta_patch": "review-delta.patch" if delta_emitted else None,
        "same_head": same_head,
    }
    with open(CTX_PATH + ".tmp", "w") as f:
        json.dump(ctx, f)
    os.replace(CTX_PATH + ".tmp", CTX_PATH)
    log("coverage carried: rounds=%d covered=%d frontier=%d findings_open=%d delta=%s"
        % (ctx["coverage"]["rounds"], ctx["coverage"]["covered_total"],
           ctx["coverage"]["frontier_count"], ctx["coverage"]["findings_open"],
           "yes" if delta_emitted else "no"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
