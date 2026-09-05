#!/usr/bin/env python3
"""Validator severity pin (#343-adjacent, r5): circular dependencies are a
WARNING (the graph represents the codebase as it is), while dangling refs,
duplicate IDs and missing evidence stay ERRORS. Regression: cycles used to
fail the whole emit — rhesadox#1864's review then ran with NO rig.db and
degraded to grep archaeology.

Run: python3 plugins/rig-emit/test_validator.py  (stdlib only; exit ≠ 0 fails)
CI: wired as `make test-rig-emit` (ci.yml test job).
"""
import json
import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

from rig.validator import validate_rig  # noqa: E402


def comp(cid, deps=None):
    return {"id": cid, "name": cid.upper(), "depends_on_ids": deps or [],
            "external_packages_ids": [], "evidence_ids": ["e1"], "source_files": []}


def rig_with(components):
    return {
        "components": components,
        "aggregators": [], "runners": [], "test_definitions": [],
        "external_packages": [], "entrypoints": [],
        "evidence": [{"id": "e1"}],
    }


def main() -> int:
    ev = [{"id": "e1"}]

    # 1. A dependency cycle must NOT be an error.
    cyclic = rig_with([
        {"id": "a", "name": "A", "depends_on_ids": ["b"], "external_packages_ids": [], "evidence_ids": ["e1"], "source_files": []},
        {"id": "b", "name": "B", "depends_on_ids": ["a"], "external_packages_ids": [], "evidence_ids": ["e1"], "source_files": []},
    ])
    errors, warnings = validate_rig(cyclic, check_source_existence=False)
    if errors:
        print(f"FAIL: cycle must not error, got {errors}")
        return 1
    named = [w for w in warnings if "Circular dependency detected:" in w]
    if not named:
        print(f"FAIL: cycle warning must NAME the members, got {warnings}")
        return 2
    if "a → b → a" not in named[0]:
        print(f"FAIL: cycle warning must name members in order, got {named[0]}")
        return 2

    # 2. Dangling refs stay errors.
    dangling = rig_with([
        {"id": "a", "name": "A", "depends_on_ids": ["ghost"], "external_packages_ids": [], "evidence_ids": ["e1"], "source_files": []},
    ])
    errors, _ = validate_rig(dangling, check_source_existence=False)
    if not any("Dangling" in e for e in errors):
        print(f"FAIL: dangling ref must error, got {errors}")
        return 3

    # 3. Duplicate IDs stay errors.
    dup = rig_with([
        {"id": "a", "name": "A", "depends_on_ids": [], "external_packages_ids": [], "evidence_ids": ["e1"], "source_files": []},
        {"id": "a", "name": "A2", "depends_on_ids": [], "external_packages_ids": [], "evidence_ids": ["e1"], "source_files": []},
    ])
    errors, _ = validate_rig(dup, check_source_existence=False)
    if not any("Duplicate" in e for e in errors):
        print(f"FAIL: duplicate id must error, got {errors}")
        return 4

    # 4. Missing evidence stays an error.
    noev = rig_with([
        {"id": "a", "name": "A", "depends_on_ids": [], "external_packages_ids": [], "evidence_ids": [], "source_files": []},
    ])
    errors, _ = validate_rig(noev, check_source_existence=False)
    if not any("Evidence" in e for e in errors):
        print(f"FAIL: missing evidence must error, got {errors}")
        return 5

    # 5. An acyclic, well-formed rig is fully clean.
    clean = rig_with([
        {"id": "a", "name": "A", "depends_on_ids": ["b"], "external_packages_ids": [], "evidence_ids": ["e1"], "source_files": []},
        {"id": "b", "name": "B", "depends_on_ids": [], "external_packages_ids": [], "evidence_ids": ["e1"], "source_files": []},
    ])
    errors, warnings = validate_rig(clean, check_source_existence=False)
    if errors or any("Circular" in w for w in warnings):
        print(f"FAIL: clean rig must validate cleanly, got {errors} / {warnings}")
        return 6

    # Producer wiring (r5 gate 6): the thing that actually broke was
    # "errors ⇒ rig.db written". Run the REAL emitter on a cyclic fixture
    # in a tempdir and require the DB — unremovable without a red CI.
    with tempfile.TemporaryDirectory() as td:
        tdp = Path(td)
        (tdp / "go.mod").write_text("module fixture.example/cyc\n\ngo 1.22\n")
        (tdp / "a").mkdir(); (tdp / "b").mkdir()
        (tdp / "a" / "a.go").write_text("package a\n\nimport _ \"fixture.example/cyc/b\"\n")
        (tdp / "b" / "b.go").write_text("package b\n\nimport _ \"fixture.example/cyc/a\"\n")
        out = tdp / "rig.json"
        r = subprocess.run(
            [sys.executable, str(Path(__file__).parent / "emit-rig.py"), str(out)],
            cwd=tdp, capture_output=True, text=True)
        if r.returncode != 0:
            print(f"FAIL: emit of a cyclic repo must succeed, rc={r.returncode}\n{r.stderr[-400:]}")
            return 7
        if not (tdp / "rig.db").exists():
            print("FAIL: emit must write rig.db for a cyclic repo")
            return 8
        warns = [l for l in r.stderr.splitlines() if "Circular" in l]
        if not warns:
            print(f"FAIL: emitter stderr must carry the cycle warning, got {r.stderr[-200:]}")
            return 9

    # Wrapper-level pin (r2 P4 BLOCKER): rig-emit.sh gates on the emit's
    # rc. Under set -euo pipefail an `if (…)|tee|tail` wrapper is
    # errexit-exempt — failures fell through to a green prepare. Pin the
    # file's construct AND the construct's behaviour.
    wrapper = Path(__file__).parent / "rig-emit.sh"
    wsrc = wrapper.read_text()
    if "if ( cd \"$SRC_DIR\" && bash" in wsrc:
        print("FAIL: rig-emit.sh wraps the emit in `if (…)` — errexit-exempt (r2 P4 regression)")
        return 10
    if "|| rc=$?" not in wsrc:
        print("FAIL: rig-emit.sh must capture the emit rc explicitly (|| rc=$?)")
        return 11
    r = subprocess.run(["bash", "-c", 'set -euo pipefail\nrc=0\n( exit 3 ) >/dev/null 2>&1 || rc=$?\nif [ "$rc" -ne 0 ]; then exit 1; fi\n'], capture_output=True, text=True)
    if r.returncode != 1:
        print(f"FAIL: the rc-capture gate must abort on emit failure, rc={r.returncode}")
        return 12

    print("rig-emit validator severity: OK (cycles warn NAMED; dangling/duplicate/evidence error; producer writes the db; wrapper gates on rc)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
