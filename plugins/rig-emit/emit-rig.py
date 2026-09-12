#!/usr/bin/env python3
"""
emit-rig.py — Universal Repository Intelligence Graph generator (v2).

Modular architecture:
  rig/model.py       — Spade data types (Component, Runner, TestDefinition, …)
  rig/builder.py     — RIGBuilder (ID assignment, evidence, name→ID resolution)
  rig/validator.py   — generation-time validation (completeness as ERROR)
  rig/extractors/    — one module per build system (Go, Zig, Cargo, npm, …)

Follows the RIG standard (arXiv:2601.10112, github.com/Greenfuze/Spade):
components are BUILD TARGETS, evidence is build-system-backed, every node
MUST have evidence.

The output is the canonical rig.db (SQLite + FTS5). Query it with
rig-query.py — never load the whole graph into context.

Usage: emit-rig.py <output.db> [--language hint] [--no-validate]
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

# Make the rig/ package importable regardless of CWD
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from rig.builder import RIGBuilder
from rig.model import Runner
from rig.validator import validate_rig
from rig import db as rig_db
from rig import symbols as rig_symbols
from rig.extractors.base import Extractor
from rig.extractors.go import GoExtractor
from rig.extractors.zig import ZigExtractor
from rig.extractors.cargo import CargoExtractor
from rig.extractors.npm import NpmExtractor
from rig.extractors.python import PythonExtractor
from rig.extractors.cmake import CMakeExtractor, StandaloneCExtractor
from rig.extractors.generic import GenericExtractor

# Ordered extraction pipeline.  Multiple extractors can be active (e.g. Zig + C).
# StandaloneCExtractor only fires when no CMake is present but C/CUDA files exist.
EXTRACTOR_CLASSES: list[type[Extractor]] = [
    GoExtractor,
    ZigExtractor,
    CargoExtractor,
    NpmExtractor,
    PythonExtractor,
    CMakeExtractor,
    StandaloneCExtractor,
    GenericExtractor,  # always last — fallback
]


def _add_script_runners(builder: RIGBuilder, root: Path) -> None:
    """Declare script-based CI runners from an optional .rig-runners manifest.

    The file-graph cannot see shell-script CI (GPU validation scripts, parity
    probes, serve checks) — without this, coverage views claim those components
    are untested. Shape (.rig-runners.json, or .rig-runners.yaml when pyyaml
    is installed):

        {"runners": [{"name": "gpu-integration",
                      "command": ["bash", "tools/integration.sh"],
                      "script":  "tools/integration.sh",   # must exist -> evidence
                      "covers":  ["cuda-backend", "c-kernels"]}]}

    `covers` are component names; unknown names are warned and skipped (never
    guessed), a missing script file is an error (evidence must be truthful).
    """
    jpath, ypath = root / ".rig-runners.json", root / ".rig-runners.yaml"
    if jpath.exists():
        entries = json.loads(jpath.read_text()).get("runners", [])
    elif ypath.exists():
        try:
            import yaml
        except ImportError:
            sys.exit(f"[emit-rig] {ypath}: pyyaml not available — use .rig-runners.json (stdlib)")
        entries = (yaml.safe_load(ypath.read_text()) or {}).get("runners", [])
    else:
        return

    for e in entries:
        name = str(e.get("name", "")).strip()
        script = str(e.get("script", "")).strip()
        command = e.get("command") or (["bash", script] if script else [])
        covers = list(e.get("covers") or [])
        if not name or not script:
            sys.exit(f"[emit-rig] .rig-runners: every runner needs at least name and script: {e}")
        if builder.resolve(name) is not None:
            sys.exit(f"[emit-rig] .rig-runners: '{name}' collides with an existing graph node")
        if not (root / script).exists():
            sys.exit(f"[emit-rig] .rig-runners: {name}: script not found: {script} (evidence must be truthful)")
        resolved = {c for c in covers if builder.resolve(c) is not None}
        for u in covers:
            if u not in resolved:
                print(f"  WARN: .rig-runners: {name}: cover '{u}' matches no component — skipped",
                      file=sys.stderr)
        builder.add_runner(Runner(
            name=name,
            arguments=[str(a) for a in command],
            depends_on=resolved,
            evidence=[builder.evidence(f"{script}:1")],
        ))
        print(f"[emit-rig] runner {name}: {script} covers {len(resolved)} component(s)",
              file=sys.stderr)


def main():
    parser = argparse.ArgumentParser(description="Universal RIG generator (v2)")
    parser.add_argument("output", help="Output rig.db path")
    parser.add_argument("--language", default=None, help="Language hint (auto-detected if omitted)")
    parser.add_argument("--no-validate", action="store_true", help="Skip validation")
    parser.add_argument("--source-sha", default=None,
                        help="Source commit the graph was emitted at (stored in "
                             "meta.source_sha; rig brief uses it for freshness)")
    args = parser.parse_args()

    builder = RIGBuilder()

    # Detect active extractors
    active: list[type[Extractor]] = []
    has_specific = any(E.detects() for E in EXTRACTOR_CLASSES if E is not GenericExtractor)
    for E in EXTRACTOR_CLASSES:
        if E is GenericExtractor:
            if not has_specific:
                active.append(E)
            continue
        if E is StandaloneCExtractor:
            # Only fire if CMake is NOT present but C/CUDA files exist
            if not CMakeExtractor.detects() and E.detects():
                active.append(E)
            continue
        if E.detects():
            active.append(E)

    extractor_names = [E.name for E in active]
    print(f"[emit-rig] Active extractors: {', '.join(extractor_names)}", file=sys.stderr)

    # Run extractors
    for E in active:
        extractor = E()
        n_before = len(builder.components)
        extractor.extract(builder)
        n_comp = len(builder.components) - n_before
        n_files = sum(len(c.source_files) for c in builder.components[n_before:])
        print(f"[emit-rig]   {E.name}: +{n_comp} components, {n_files} source files",
              file=sys.stderr)

    # Script-runner evidence (optional .rig-runners manifest) — after the
    # extractors, so covers resolve against the extracted component names.
    _add_script_runners(builder, Path("."))

    # Build the RIG JSON
    rig = builder.build(extractor_names)

    # Validate (references, cycles, evidence = errors; completeness = warning)
    if not args.no_validate:
        errors, warnings = validate_rig(rig)
        if warnings:
            rig["warnings"] = warnings  # VENDOR-PATCH: persisted via meta.warnings
            for w in warnings:
                print(f"  WARN: {w}", file=sys.stderr)

        if errors:
            print(f"[emit-rig] VALIDATION FAILED ({len(errors)} error(s)):", file=sys.stderr)
            for e in errors[:20]:
                print(f"  ERROR: {e}", file=sys.stderr)
            sys.exit(1)

    # Write the canonical SQLite database — the ONLY graph artifact.
    # Queryable + compact (FTS5 symbol search, per-component queries);
    # consumers answer structural questions with targeted queries instead
    # of loading the graph into context.
    db_path = Path(args.output)
    if db_path.suffix != ".db":
        db_path = db_path.with_suffix(".db")
    rig_db.write_db(rig, db_path)
    if args.source_sha:
        rig_db.set_meta(db_path, "source_sha", args.source_sha)
    source_root = Path(".").resolve()
    syms = rig_symbols.extract_symbols(rig, source_root)
    rig_db.add_symbols(db_path, syms)
    # File metadata (language, bytes, lines, doc comment) for the files table.
    from rig.builder import SOURCE_EXTENSIONS
    file_rows = []
    seen_paths: set[str] = set()
    for c in rig["components"]:
        lang = c.get("programming_language", "")
        for sf in c.get("source_files", []):
            if sf in seen_paths:
                continue
            p = source_root / sf
            if not p.is_file():
                continue
            seen_paths.add(sf)
            try:
                text = p.read_text(encoding="utf-8", errors="replace")
                file_rows.append({
                    "path": sf,
                    "component_id": c["id"],
                    "language": SOURCE_EXTENSIONS.get(p.suffix.lower(), ""),
                    "bytes": p.stat().st_size,
                    "lines": text.count("\n") + 1,
                    "doc": rig_symbols.extract_doc_comment(p, lang),
                })
            except OSError:
                continue
    rig_db.add_files(db_path, file_rows)

    # Near-clone edges (MinHash+LSH over symbol bodies) — same pass, no
    # extra CI job; deterministic, so the canonical hash covers it.
    from rig.clones import compute_and_store
    n_similar = compute_and_store(db_path, source_root)

    # Call edges, regex-v1 (symbols → bodies → resolved edges; meta tag
    # records provenance). Same single emit pass; archmap supersedes it
    # where present. This is what gives impact/dead/trace their fan-in.
    from rig.calls import compute_and_store as compute_calls
    n_calls = compute_calls(db_path, source_root)

    n_components = len(rig["components"])
    n_files = sum(len(c.get("source_files", [])) for c in rig["components"])
    print(f"[emit-rig] rig.db: {db_path} "
          f"({n_components} components, {n_files} files, {len(syms)} symbols, "
          f"{n_calls} call edges, {n_similar} clone edges)",
          file=sys.stderr)

    total_edges = sum(len(c.get("depends_on_ids", [])) for c in rig["components"])
    print(
        f"[emit-rig] RIG: {len(rig['components'])} components, "
        f"{total_edges} dependency edges, "
        f"{len(rig['external_packages'])} external packages, "
        f"{len(rig['entrypoints'])} entrypoints, "
        f"{len(rig['evidence'])} evidence, "
        f"{len(rig['test_definitions'])} test definitions, "
        f"{len(rig['runners'])} runners",
        file=sys.stderr,
    )
    print(
        f"[emit-rig] DB: {db_path} ({db_path.stat().st_size} bytes, "
        f"{len(syms)} symbols, hash {rig_db.canonical_hash(db_path)[:12]})",
        file=sys.stderr,
    )


if __name__ == "__main__":
    main()
