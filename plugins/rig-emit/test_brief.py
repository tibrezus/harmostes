#!/usr/bin/env python3
"""rig-brief's contract, exercised against a REAL rig.db (built via write_db).

The r4 forensics (#2085): the reviewer model burned 31 bash greps rediscovering
what the graph already knew, then died of [length]. The fix is prepare-time
enrichment — the briefing must therefore be correct BY CONSTRUCTION, not by
model goodwill. These tests pin:

  1. overview    — every component, its deps, entrypoint marker
  2. footprint   — changed files → owning components (the killer mapping)
  3. depth       — symbols with file:line for the touched files
  4. blast radius — reverse deps of a touched component are named
  5. fail-open   — missing db / empty graph → exit 0, NOTHING written
  6. stamp       — pr-context.json gains rig_briefing

Run: python3 plugins/rig-emit/test_brief.py  (stdlib only; exit ≠ 0 fails)
CI:  wired next to test_validator.py (make test-rig-emit).
"""
import json
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))

from rig.db import write_db, add_symbols  # noqa: E402

BRIEF = HERE / "rig-brief.py"


def rig_dict() -> dict:
    return {
        "components": [
            {"id": "kernel", "name": "Kernel", "depends_on_ids": ["statestore"],
             "external_packages_ids": [], "evidence_ids": ["e1"],
             "source_files": ["internal/kernel/run.go"]},
            {"id": "statestore", "name": "StateStore", "depends_on_ids": [],
             "external_packages_ids": [], "evidence_ids": ["e1"],
             "source_files": ["internal/statestore/client.go"]},
            {"id": "ui", "name": "UI", "depends_on_ids": ["kernel"],
             "external_packages_ids": [], "evidence_ids": ["e1"],
             "source_files": ["web/src/app.tsx"]},
        ],
        "aggregators": [], "runners": [], "test_definitions": [],
        "external_packages": [], "entrypoints": ["kernel"],
        "evidence": [{"id": "e1"}],
    }


def symbols() -> list[dict]:
    return [
        {"file": "internal/kernel/run.go", "name": "Run", "kind": "function",
         "line": 42, "signature": "func Run(ctx context.Context) error"},
        {"file": "internal/kernel/run.go", "name": "Gate", "kind": "function",
         "line": 88, "signature": "func Gate(ctx context.Context) bool"},
        {"file": "internal/statestore/client.go", "name": "Save", "kind": "method",
         "line": 17, "signature": "func (c *Client) Save(key string, v any) error"},
    ]


def run_brief(tmp: Path, db: Path, ctx: dict | None) -> tuple[int, str]:
    ctx_path = tmp / "pr-context.json"
    ctx_path.write_text(json.dumps(ctx or {}))
    proc = subprocess.run(
        [sys.executable, str(BRIEF), str(db), str(ctx_path), str(tmp)],
        capture_output=True, text=True, timeout=60)
    return proc.returncode, (tmp / "rig-briefing.md").read_text() if (tmp / "rig-briefing.md").exists() else ""


def main() -> int:
    failures: list[str] = []

    with tempfile.TemporaryDirectory() as td:
        tmp = Path(td)
        db = tmp / "rig.db"
        write_db(rig_dict(), db)
        add_symbols(db, symbols())
        ctx = {"files_changed": ["internal/kernel/run.go", "docs/new.md"],
               "repo_dir": str(tmp / "repo")}
        rc, brief = run_brief(tmp, db, ctx)

        if rc != 0:
            failures.append(f"generator exited {rc}: {brief[:200]}")
        if "rig_briefing" not in json.loads((tmp / "pr-context.json").read_text()):
            failures.append("pr-context.json was not stamped with rig_briefing")

        # 1. overview: all components, dep edges, entrypoint marker.
        for needle in ["**Kernel**", "**StateStore**", "**UI**",
                       "→ deps: StateStore", "[entrypoint]"]:
            if needle not in brief:
                failures.append(f"overview missing {needle!r}")

        # 2. footprint: changed file mapped to its owner; unmapped named.
        if "internal/kernel/run.go" not in brief or "**Kernel**" not in brief.split("This diff touches")[1][:300]:
            failures.append("footprint: changed file not mapped to its component")
        if "outside the graph" not in brief or "docs/new.md" not in brief:
            failures.append("footprint: unmapped file not reported")

        # 3. depth: symbols with file:line for the touched file.
        if "`Run` (function, line 42)" not in brief:
            failures.append("depth: symbol with line missing")
        if "func Run(ctx context.Context) error" not in brief:
            failures.append("depth: signature missing")

        # 4. blast radius: ui → kernel reverse edge named for the touched comp.
        if "these depend on it" not in brief:
            failures.append("blast radius: reverse dep of the touched component missing")
        kernel_section = brief.split("### Kernel")[1] if "### Kernel" in brief else ""
        if "UI" not in kernel_section[:400]:
            failures.append("blast radius: reverse dep of the touched component missing")

    # 5. fail-open: no rig.db → exit 0, no file.
    with tempfile.TemporaryDirectory() as td:
        tmp = Path(td)
        rc, brief = run_brief(tmp, tmp / "missing.db",
                              {"files_changed": ["a.go"]})
        if rc != 0:
            failures.append(f"fail-open: missing db must exit 0, got {rc}")
        if brief:
            failures.append("fail-open: no briefing file may be written")

    # 5b. fail-open: graph without the changed files → briefing, no crash.
    with tempfile.TemporaryDirectory() as td:
        tmp = Path(td)
        db = tmp / "rig.db"
        write_db(rig_dict(), db)
        rc, brief = run_brief(tmp, db, {"files_changed": ["x/unrelated.py"]})
        if rc != 0:
            failures.append(f"fail-open: unrelated diff must exit 0, got {rc}")

    # 5c. THE PRODUCTION SHAPE (#456 r1 CRITICAL): workspace.sh sends
    # files_changed as objects ({status, filename, additions, deletions}) —
    # the exact key set of workspace.sh:88. The original contract test fed
    # strings, so the generator shipped inert in production while CI stayed
    # green (SQL binding died on the dict and the briefing never fired).
    # The object shape must brief; a falsy entry must be skipped, not fatal.
    with tempfile.TemporaryDirectory() as td:
        tmp = Path(td)
        db = tmp / "rig.db"
        write_db(rig_dict(), db)
        add_symbols(db, symbols())
        ctx = {"files_changed": [
            {"status": "changed", "filename": "internal/kernel/run.go", "additions": 3, "deletions": 1},
            {"status": "added", "filename": "docs/new.md", "additions": 12, "deletions": 0},
            {"filename": None},  # falsy entry: skipped, never bound to SQL
        ], "repo_dir": str(tmp / "repo")}
        rc, brief = run_brief(tmp, db, ctx)
        if rc != 0:
            failures.append(f"production shape: generator exited {rc}: {brief[:200]}")
        if "rig_briefing" not in json.loads((tmp / "pr-context.json").read_text()):
            failures.append("production shape: pr-context.json not stamped")
        if "This diff touches" not in brief or "Kernel" not in brief:
            failures.append("production shape: touched-component section missing from the briefing")

    if failures:
        print("TEST FAILURES:", *failures, sep="\n  - ", file=sys.stderr)
        return 1
    print("rig-brief: all contract tests green")
    return 0


if __name__ == "__main__":
    sys.exit(main())
