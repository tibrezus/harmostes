#!/usr/bin/env python3
"""Vendor patch: apply harmostes' deliberate deviations to the pinned
upstream repo-map tree (tibrezus/llm-wiki-core, see VENDOR.sha).

The vendored plugins/rig-emit/ tree is byte-equivalent to upstream EXCEPT
for the deviations below — each is a reviewed harmostes decision, kept as
code so the CI vendor-sync step can apply them to a fresh upstream checkout
and byte-compare. Any anchor drift fails loudly: upstream moved, re-derive.

Deviations (the ONLY allowed differences):
  1-3. validator severity pin (rhesadox#1864): cycles WARN, not ERROR —
       refusing to emit over a fact about the codebase left reviews
       graph-less; completeness gated behind check_source_existence so the
       unit pin runs hermetically (test_validator.py relies on it).
  4.   cycle reporting: upstream's detector stops at a boolean; the vendored
       detector (r3 P4, r4 P4.2) names the members, rotates the cycle to a
       canonical form, counts distinct cycles, and runs iteratively (no
       RecursionError on deep monorepos). Upstream adoption candidate —
       when llm-wiki-core takes this body, this patch shrinks to 1-3 + 5-6.
  5-6. warnings travel with the artifact (#346 r2 P7): upstream prints
       validator warnings to stderr and drops them; the vendored pipeline
       records them in meta.warnings — the graph is honest about the cycles
       it represents.

Usage:
    python3 plugins/rig-emit/vendor-patch.py <upstream-repo-map-dir> <out-dir>

Out-dir receives the full patched tree (emit-rig.py + rig/); the CI sync
step byte-compares it against plugins/rig-emit/ (excluding harmostes-owned
files, listed in VENDOR.sha).
"""
from __future__ import annotations

import shutil
import sys
from pathlib import Path

# Files copied verbatim (byte-parity, no deviations).
VERBATIM = [
    "rig/builder.py", "rig/calls.py", "rig/clones.py", "rig/fitness.py",
    "rig/__init__.py", "rig/model.py", "rig/symbols.py",
    "rig/extractors/base.py", "rig/extractors/cargo.py",
    "rig/extractors/cmake.py", "rig/extractors/generic.py",
    "rig/extractors/go.py", "rig/extractors/__init__.py",
    "rig/extractors/npm.py", "rig/extractors/python.py",
    "rig/extractors/zig.py",
]

def _apply(name, src, patches):
    for anchor, repl in patches:
        n = src.count(anchor)
        if n != 1:
            sys.exit(f"vendor-patch: {name} anchor matched {n}x (want 1) — "
                     f"upstream moved; re-derive:\n---\n{anchor[:200]}")
        src = src.replace(anchor, repl)
    return src

def _patched_validator(src):
    return _apply("validator", src, (
        ('    Hard errors (dangling refs, cycles, duplicate IDs, missing evidence) fail\n    the build.  Completeness (uncovered source files) is a WARNING — repos with\n    multiple languages or tooling scripts may legitimately have files outside\n    any build target.', '    Hard errors (dangling refs, duplicate IDs, missing evidence) fail the\n    build.  CIRCULAR DEPENDENCIES are a WARNING (VENDOR-PATCH, rhesadox#1864):\n    a cycle is a fact about the codebase the graph must represent, not an\n    emission failure — refusing to emit left reviews graph-less. Completeness\n    (uncovered source files) is a WARNING, gated behind check_source_existence\n    (VENDOR-PATCH: the unit pin relies on that hermeticity).'),
        ("    errors.extend(_check_circular_deps(rig))",
         "    warnings.extend(_check_circular_deps(rig))  # VENDOR-PATCH (rhesadox#1864)"),
        ("    warnings.extend(_check_completeness(rig))",
         "    if check_source_existence:  # VENDOR-PATCH: hermetic unit pin\n"
         "        warnings.extend(_check_completeness(rig))"),
        ('def _check_circular_deps(rig: dict) -> list[str]:\n    graph: dict[str, list[str]] = {}\n    for key in ("components", "aggregators", "runners"):\n        for node in rig.get(key, []):\n            nid = node.get("id", "")\n            graph[nid] = node.get("depends_on_ids", [])\n    WHITE, GRAY, BLACK = 0, 1, 2\n    color = {n: WHITE for n in graph}\n    found = [False]\n\n    def _dfs(node):\n        color[node] = GRAY\n        for nb in graph.get(node, []):\n            if nb not in color:\n                continue\n            if color[nb] == GRAY:\n                found[0] = True\n                return\n            if color[nb] == WHITE:\n                _dfs(nb)\n                if found[0]:\n                    return\n        color[node] = BLACK\n\n    for n in graph:\n        if color[n] == WHITE:\n            _dfs(n)\n            if found[0]:\n                break\n    return ["Circular dependency detected"] if found[0] else []\n', 'def _check_circular_deps(rig: dict) -> list[str]:\n    graph: dict[str, list[str]] = {}\n    for key in ("components", "aggregators", "runners"):\n        for node in rig.get(key, []):\n            nid = node.get("id", "")\n            graph[nid] = node.get("depends_on_ids", [])\n    WHITE, GRAY, BLACK = 0, 1, 2\n    color = {n: WHITE for n in graph}\n    found: list[list[str]] = []\n\n    # Iterative DFS (r3 P4): recursion is one frame per component in the\n    # longest chain and runs BEFORE anything is written — a RecursionError\n    # on a deep monorepo would fail prepare outright. Explicit stack; the\n    # path list doubles as the cycle-member source.\n    for root in graph:\n        if color[root] != WHITE:\n            continue\n        path: list[str] = []\n        stack = [(root, iter([nb for nb in graph.get(root, []) if nb in color]))]\n        color[root] = GRAY\n        path.append(root)\n        while stack:\n            node, it = stack[-1]\n            advanced = False\n            for nb in it:\n                if color.get(nb) == GRAY:\n                    idx = path.index(nb)\n                    found.append(path[idx:] + [nb])\n                elif color.get(nb) == WHITE:\n                    color[nb] = GRAY\n                    path.append(nb)\n                    stack.append((nb, iter([x for x in graph.get(nb, []) if x in color])))\n                    advanced = True\n                    break\n            if not advanced:\n                stack.pop()\n                path.pop()\n                color[node] = BLACK\n    if not found:\n        return []\n    # Canonical form (r4 P4.2): the warning (and the canonical hash derived\n    # from it) must be a function of the GRAPH, not of DFS entry order —\n    # rotate the cycle to start at its smallest member NAME. Count ALL\n    # cycles: the traversal runs to completion and each GRAY back-edge is\n    # one cycle (self-loops and distinct cycles both count).\n    names = {n.get("id",""): n.get("name", n.get("id","")) for key in\n             ("components", "aggregators", "runners") for n in rig.get(key, [])}\n    cyc_nodes = found[0]\n    names_seq = [names.get(x, x) for x in cyc_nodes]\n    k = names_seq.index(min(names_seq))\n    names_seq = names_seq[k:-1] + names_seq[:k] + [names_seq[k]]\n    cyc = " → ".join(names_seq)\n    msg = [f"Circular dependency detected: {cyc} (emitted as-is — the "\n           "deps table represents the cycle; navigation is unaffected)"]\n    if len(found) > 1:\n        msg[0] += f"; +{len(found) - 1} more distinct cycle(s)"\n    return msg\n'),
    ))

DB_ANCHOR = '    con.executemany(\n        "INSERT INTO meta(key, value) VALUES (?,?)",\n        [\n            ("db_schema_version", str(DB_SCHEMA_VERSION)),\n            ("rig_schema_version", rig.get("schema_version", "rig-1.0")),\n            ("repo_name", repo.get("name", "")),\n            ("repo_language", repo.get("language", "unknown")),\n            ("build_system", repo.get("build_system", "")),\n            ("generator", repo.get("generator", "")),\n        ])'
DB_REPL = '    rows = [\n        ("db_schema_version", str(DB_SCHEMA_VERSION)),\n        ("rig_schema_version", rig.get("schema_version", "rig-1.0")),\n        ("repo_name", repo.get("name", "")),\n        ("repo_language", repo.get("language", "unknown")),\n        ("build_system", repo.get("build_system", "")),\n        ("generator", repo.get("generator", "")),\n    ]\n    # VENDOR-PATCH (#346 r2 P7): generation warnings travel WITH the artifact.\n    warnings = rig.get("warnings") or []\n    if warnings:\n        rows.append(("warnings", json.dumps(warnings)))\n    con.executemany("INSERT INTO meta(key, value) VALUES (?,?)", rows)'

def _patched_db(src):
    return _apply("db.py", src, [(DB_ANCHOR, DB_REPL)])

EMIT_ANCHOR = '        errors, warnings = validate_rig(rig)\n        if warnings:\n            for w in warnings[:10]:\n                print(f"  WARN: {w}", file=sys.stderr)'
EMIT_REPL = '        errors, warnings = validate_rig(rig)\n        if warnings:\n            rig["warnings"] = warnings  # VENDOR-PATCH: persisted via meta.warnings\n            for w in warnings:\n                print(f"  WARN: {w}", file=sys.stderr)\n'

def _patched_emit(src):
    return _apply("emit-rig.py", src, [(EMIT_ANCHOR, EMIT_REPL)])

PATCHED = {
    "rig/validator.py": _patched_validator,
    "rig/db.py": _patched_db,
    "emit-rig.py": _patched_emit,
}

def main() -> None:
    if len(sys.argv) != 3:
        sys.exit("usage: vendor-patch.py <upstream-repo-map-dir> <out-dir>")
    src_dir, out_dir = Path(sys.argv[1]), Path(sys.argv[2])
    (out_dir / "rig/extractors").mkdir(parents=True, exist_ok=True)
    for rel in VERBATIM:
        shutil.copyfile(src_dir / rel, out_dir / rel)
    for rel, fn in PATCHED.items():
        (out_dir / rel).write_text(fn((src_dir / rel).read_text(encoding="utf-8")),
                                   encoding="utf-8")
    print(f"vendored tree written to {out_dir} "
          f"({len(VERBATIM)} verbatim + {len(PATCHED)} patched)")

if __name__ == "__main__":
    main()
