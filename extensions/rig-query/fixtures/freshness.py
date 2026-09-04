#!/usr/bin/env python3
"""Fixture freshness check (#338 r2): the committed rig.db must equal a fresh
regeneration — compared via the producer's canonical_hash (logical content,
survives sqlite version/page-layout differences; byte-diffs do not).

Run: python3 extensions/rig-query/fixtures/freshness.py
Exit 0 = fresh, 1 = stale (run generate.py and commit).
"""
from __future__ import annotations

import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(ROOT / "plugins" / "rig-emit"))

from rig import db as rig_db  # noqa: E402
import generate  # noqa: E402


def main() -> int:
    committed = HERE / "rig.db"
    h_committed = rig_db.canonical_hash(committed)
    rig, symbols, files = generate.build()
    with tempfile.TemporaryDirectory() as tmp:
        fresh = Path(tmp) / "rig.db"
        rig_db.write_db(rig, fresh)
        rig_db.add_symbols(fresh, symbols)
        rig_db.add_files(fresh, files)
        h_fresh = rig_db.canonical_hash(fresh)
    if h_committed != h_fresh:
        print(f"STALE: committed={h_committed[:16]} fresh={h_fresh[:16]}")
        return 1
    print(f"fresh: {h_committed[:16]}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
