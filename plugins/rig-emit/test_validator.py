#!/usr/bin/env python3
"""Validator severity pin (#343-adjacent, r5): circular dependencies are a
WARNING (the graph represents the codebase as it is), while dangling refs,
duplicate IDs and missing evidence stay ERRORS. Regression: cycles used to
fail the whole emit — rhesadox#1864's review then ran with NO rig.db and
degraded to grep archaeology.

Run: python3 plugins/rig-emit/test_validator.py  (stdlib only; exit ≠ 0 fails)
CI: wired as `make test-rig-emit` (ci.yml test job).
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

from rig.validator import validate_rig  # noqa: E402


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
    if not any("Circular" in w for w in warnings):
        print(f"FAIL: cycle must warn, got warnings={warnings}")
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

    print("rig-emit validator severity: OK (cycles warn; dangling/duplicate/evidence error)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
