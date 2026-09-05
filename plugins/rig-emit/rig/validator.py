"""RIG validator — enforces invariants at generation time.

Mirrors Spade's rig_validator (github.com/Greenfuze/spade) adapted to run
against the RIGBuilder's output (or a loaded JSON dict).

Severity (r5, #345): dangling refs, duplicate IDs and missing evidence are
ERRORS; circular dependencies and completeness (uncovered source files)
are WARNINGS. `check_source_existence=False` skips the completeness walk
(which reads the CWD filesystem) — the unit pin relies on that hermeticity.
The wiki-side validate-rig.py keeps its own severities; drift between the
copies is a tracked llm-wiki concern.
"""

from __future__ import annotations

from pathlib import Path

from .builder import all_source_paths, BUILD_CONFIG_FILES


def validate_rig(rig: dict, *, check_source_existence: bool = True) -> tuple[list[str], list[str]]:
    """Validate a RIG dict. Returns (errors, warnings) — both empty = valid.

    Hard errors (dangling refs, duplicate IDs, missing evidence) fail the
    build.  Completeness (uncovered source files) and CIRCULAR DEPENDENCIES
    are WARNINGs: a cycle is a fact about the codebase the graph must
    represent, not an emission failure — refusing to emit because the code
    has a cycle left reviews graph-LESS (observed live: rhesadox#1864, where
    the emit failure degraded ADR-0009 navigation to grep archaeology). The
    deps table is navigable with cycles; consumers surface the warning.
    """
    errors: list[str] = []
    warnings: list[str] = []

    errors.extend(_check_dangling_refs(rig))
    warnings.extend(_check_circular_deps(rig))
    errors.extend(_check_duplicate_ids(rig))
    errors.extend(_check_evidence(rig))
    if check_source_existence:
        warnings.extend(_check_completeness(rig))

    return errors, warnings


def _check_completeness(rig: dict) -> list[str]:
    """Every source file in the repo must appear in at least one component."""
    repo_files = all_source_paths()
    rig_files: set[str] = set()
    for c in rig.get("components", []):
        for sf in c.get("source_files", []):
            rig_files.add(sf.replace("\\", "/"))

    missing = []
    for f in sorted(repo_files - rig_files):
        basename = Path(f).name
        if basename in BUILD_CONFIG_FILES:
            continue
        # Skip test files (they're in test_definitions)
        if "_test.go" in basename or basename.endswith((
            "_test.py", "_test.rs", ".test.ts", ".test.tsx",
            ".spec.ts", ".spec.tsx", ".test.js", ".spec.js",
        )):
            continue
        # Skip tooling scripts
        if f.endswith((".sh",)) and ("tools/" in f or "scripts/" in f):
            continue
        if f.endswith(".py") and ("tools/" in f or "scripts/" in f):
            continue
        missing.append(f)

    if missing:
        return [f"Completeness: {len(missing)} source file(s) not in any component"
                + (f" (first: {missing[0]})" if missing else "")]
    return []


def _all_ids(rig: dict) -> set[str]:
    ids: set[str] = set()
    for key in ("components", "aggregators", "runners", "test_definitions", "external_packages"):
        for node in rig.get(key, []):
            if "id" in node:
                ids.add(node["id"])
    return ids


def _check_dangling_refs(rig: dict) -> list[str]:
    all_ids = _all_ids(rig)
    errors = []
    for comp in rig.get("components", []):
        for ref in comp.get("depends_on_ids", []):
            if ref not in all_ids:
                errors.append(f"Dangling ref: {comp.get('name','?')}.depends_on_ids → {ref}")
        for ref in comp.get("external_packages_ids", []):
            if ref not in all_ids:
                errors.append(f"Dangling ref: {comp.get('name','?')}.external_packages_ids → {ref}")
    for agg in rig.get("aggregators", []):
        for ref in agg.get("depends_on_ids", []):
            if ref not in all_ids:
                errors.append(f"Dangling ref: aggregator {agg.get('name','?')} → {ref}")
    for ep in rig.get("entrypoints", []):
        if ep not in all_ids:
            errors.append(f"Dangling ref: entrypoint → {ep}")
    return errors


def _check_circular_deps(rig: dict) -> list[str]:
    graph: dict[str, list[str]] = {}
    for key in ("components", "aggregators", "runners"):
        for node in rig.get(key, []):
            nid = node.get("id", "")
            graph[nid] = node.get("depends_on_ids", [])
    WHITE, GRAY, BLACK = 0, 1, 2
    color = {n: WHITE for n in graph}
    found: list[list[str]] = []

    # Iterative DFS (r3 P4): recursion is one frame per component in the
    # longest chain and runs BEFORE anything is written — a RecursionError
    # on a deep monorepo would fail prepare outright. Explicit stack; the
    # path list doubles as the cycle-member source.
    for root in graph:
        if color[root] != WHITE:
            continue
        path: list[str] = []
        stack = [(root, iter([nb for nb in graph.get(root, []) if nb in color]))]
        color[root] = GRAY
        path.append(root)
        while stack and not found:
            node, it = stack[-1]
            advanced = False
            for nb in it:
                if color.get(nb) == GRAY:
                    idx = path.index(nb)
                    found.append(path[idx:] + [nb])
                    break
                if color.get(nb) == WHITE:
                    color[nb] = GRAY
                    path.append(nb)
                    stack.append((nb, iter([x for x in graph.get(nb, []) if x in color])))
                    advanced = True
                    break
            if found:
                break
            if not advanced:
                stack.pop()
                path.pop()
                color[node] = BLACK
    if not found:
        return []
    cyc = " → ".join(found[0])
    return [f"Circular dependency detected: {cyc} (emitted as-is — the "
            "deps table represents the cycle; navigation is unaffected)"]


def _check_duplicate_ids(rig: dict) -> list[str]:
    counts: dict[str, int] = {}
    for key in ("components", "aggregators", "runners", "test_definitions", "external_packages"):
        for node in rig.get(key, []):
            nid = node.get("id")
            if nid:
                counts[nid] = counts.get(nid, 0) + 1
    return [f"Duplicate ID: {nid} ({c}×)" for nid, c in counts.items() if c > 1]


def _check_evidence(rig: dict) -> list[str]:
    """Every node MUST have at least one evidence entry (paper invariant)."""
    ev_ids = {e["id"] for e in rig.get("evidence", [])}
    missing = []
    for key in ("components", "aggregators", "runners", "test_definitions"):
        for node in rig.get(key, []):
            eids = node.get("evidence_ids", [])
            if not eids or not any(eid in ev_ids for eid in eids):
                missing.append(f"{key}/{node.get('name', '?')}")
    if missing:
        return [f"Evidence: {len(missing)} node(s) lack evidence"
                + (f" (first: {missing[0]})" if missing else "")]
    return []
