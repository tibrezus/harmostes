#!/usr/bin/env python3
"""Regenerate the committed rig-query test fixture (extensions/rig-query/fixtures/rig.db).

Uses the REAL producer (plugins/rig-emit/rig/db.py) so the fixture is a
byte-faithful sample of what prepare ships — the consumer is tested against
the producer's actual output, not a hand-rolled approximation (#338 r1).

The fixture encodes the exact cases the review caught:
  - two components whose names share the tail "worker"  → ambiguity answer
  - one camelCase symbol (NodeResultEnvelope) plus 10 doc rows that all
    match a common term ("handler")  → the FTS-quota-starvation regression
  - repo_name in meta  → overview names the project

Run: python3 extensions/rig-query/fixtures/generate.py
"""
from __future__ import annotations

import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "plugins" / "rig-emit"))

from rig import db as rig_db  # noqa: E402

rig = {
    "schema_version": "rig-1.0",
    "repository": {"name": "fixture-repo", "language": "go", "build_system": "go", "generator": "fixture"},
    "evidence": [],
    "entrypoints": ["comp-1"],
    "aggregators": [],
    "runners": [],
    "test_definitions": [],
    "external_packages": [],
    "components": [
        # 13 leaf components — comp-2 depends on all of them, so the deps
        # listing (MAX_ROWS=12) exercises the …+N more marker (#338 r3 4.2).
        *[{
            "id": f"comp-{4 + i}",
            "name": f"example.com/fixture/internal/leaf{i:02d}",
            "type": "package_library",
            "programming_language": "go",
            "depends_on_ids": [],
            "external_packages_ids": [],
            "evidence_ids": [],
            "artifacts": [],
        } for i in range(13)],
        {
            "id": "comp-1",
            "name": "example.com/fixture/cmd/worker",
            "type": "executable",
            "programming_language": "go",
            "depends_on_ids": ["comp-2"],
            "external_packages_ids": [],
            "evidence_ids": [],
            "artifacts": [],
        },
        {
            "id": "comp-2",
            "name": "example.com/fixture/internal/worker",
            "type": "package_library",
            "programming_language": "go",
            "depends_on_ids": ["comp-3"] + [f"comp-{4 + i}" for i in range(13)],
            "external_packages_ids": [],
            "evidence_ids": [],
            "artifacts": [],
            "source_files": ["internal/worker/exec.go"]
                + [f"internal/worker/part{i:02d}.go" for i in range(13)],
        },
        {
            "id": "comp-3",
            "name": "example.com/fixture/internal/model",
            "type": "package_library",
            "programming_language": "go",
            "depends_on_ids": [],
            "external_packages_ids": [],
            "evidence_ids": [],
            "artifacts": [],
        },
    ],
}

symbols = [
    # The camelCase case: doc matches the common term too.
    {"file": "internal/model/types.go", "name": "NodeResultEnvelope", "kind": "type", "line": 44,
     "signature": "type NodeResultEnvelope struct", "doc": "the envelope carrying a node result"},
    # Ten doc-only "handler" matches — these used to saturate FTS rank and
    # starve the name hit (the #338 r1 search bug).
    *[{"file": f"internal/worker/h{i}.go", "name": f"helper{i}", "kind": "func", "line": 10 + i,
       "signature": f"func helper{i}()", "doc": f"handler for case {i}"} for i in range(10)],
    # The underscore class (#338 r7 B1): snake_case names must survive the
    # LIKE escaping (a bad $0-style replacement hid every _-bearing symbol).
    {"file": "internal/model/db.py", "name": "canonical_hash", "kind": "func", "line": 439,
     "signature": "def canonical_hash(db_path)", "doc": "deterministic content hash"},
    {"file": "internal/model/db.py", "name": "write_db", "kind": "func", "line": 195,
     "signature": "def write_db(rig, db_path)", "doc": ""},
    # Name-prefix material.
    {"file": "internal/worker/exec.go", "name": "Executor", "kind": "type", "line": 3,
     "signature": "type Executor struct", "doc": ""},
    {"file": "cmd/worker/main.go", "name": "ExecutorMain", "kind": "func", "line": 7,
     "signature": "func ExecutorMain()", "doc": ""},
]

files = [
    {"path": "cmd/worker/main.go", "component_id": "comp-1", "language": "go", "bytes": 100, "lines": 9, "doc": ""},
    {"path": "internal/worker/exec.go", "component_id": "comp-2", "language": "go", "bytes": 80, "lines": 6, "doc": ""},
    {"path": "internal/model/types.go", "component_id": "comp-3", "language": "go", "bytes": 120, "lines": 50, "doc": ""},
    # 13 files in comp-2 — the silent-truncation case: a component listing must
    # say "…+N more files" when MAX_ROWS caps it (#338 r2 4.2).
    *[{"path": f"internal/worker/part{i:02d}.go", "component_id": "comp-2", "language": "go",
       "bytes": 50, "lines": 10 + i, "doc": ""} for i in range(13)],
]


def build() -> tuple[dict, list[dict], list[dict]]:
    """The fixture content — also consumed by freshness.py (CI drift check)."""
    return rig, symbols, files


def main() -> None:
    out = Path(__file__).resolve().parent / "rig.db"
    r, s, f = build()
    rig_db.write_db(r, out)
    rig_db.add_symbols(out, s)
    rig_db.add_files(out, f)
    print(f"fixture written: {out} ({out.stat().st_size} bytes)")


if __name__ == "__main__":
    main()
