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

Usage: emit-rig.py <output.json> [--language hint] [--no-validate]
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
from rig.validator import validate_rig
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


def main():
    parser = argparse.ArgumentParser(description="Universal RIG generator (v2)")
    parser.add_argument("output", help="Output JSON file path")
    parser.add_argument("--language", default=None, help="Language hint (auto-detected if omitted)")
    parser.add_argument("--no-validate", action="store_true", help="Skip validation")
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

    # Build the RIG JSON
    rig = builder.build(extractor_names)

    # Validate (references, cycles, evidence = errors; completeness = warning)
    if not args.no_validate:
        errors, warnings = validate_rig(rig)
        if warnings:
            for w in warnings[:10]:
                print(f"  WARN: {w}", file=sys.stderr)
        if errors:
            print(f"[emit-rig] VALIDATION FAILED ({len(errors)} error(s)):", file=sys.stderr)
            for e in errors[:20]:
                print(f"  ERROR: {e}", file=sys.stderr)
            sys.exit(1)

    # Write output
    with open(args.output, "w") as f:
        json.dump(rig, f, indent=2)

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


if __name__ == "__main__":
    main()
