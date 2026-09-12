"""Graph fitness — deterministic modularity metrics from a rig.db.

Fitness-function substrate (evolutionary architecture, Ford/Parsons):
scalability is a graph *shape* property, not an edge property. Computed
here deterministically from the canonical graph:

  - acyclicity          (also enforced as a hard validator error)
  - fan-in hubs         (dependents per component — stability, SDP)
  - component sizes     (god-component drift)
  - cross-component symbol duplication (DRY at the graph level)

Consumers:
  - ``rig-fitness.py`` (CLI in this directory): renders the per-SHA
    "Graph Fitness" section for Architecture.md — snapshots are immutable
    per commit, so the trend is diffable in the human page. ``--json``
    emits the same numbers for machine deltas (pr-review baseline diff).
  - ``scripts/arch/rig-compliance.py``: the severe-duplication fitness
    gate — a symbol re-declared in >= SEVERE_COMPONENT_SPREAD components
    (same language) is an error; CI fails (user policy: severe
    duplication fails the check).

False-positive controls (calibrated on real graphs):
  - only "real" symbol rows count (kind fn/type/var, or a ``fn ``/
    ``type ``/``var `` signature prefix — extractors also emit rows whose
    kind is the symbol name itself);
  - generic names are blocklisted (``main``, ``init``, …);
  - ``__``-prefixed builtins never count;
  - a duplicated name must be duplicated *within one language* — a C
    symbol matching a Zig symbol name is not duplication.
"""

from __future__ import annotations

import sqlite3
from pathlib import Path

# Names too generic for name-equality to mean anything. Calibrated per
# language idiom: entrypoint mains and lifecycle hooks (Zig), per-package
# generic types and interfaces (Go — every package may define its own
# Event/Result/Config; name-equality carries no duplication signal).
# The review-side `rig search` still catches these — this list only
# governs the automated gate. Codec/find verbs (decode, encode, find)
# calibrated on rhesadox's layer split (#1850): wire-frame decode,
# tokenizer/KV-delta encode, and registry/tensor find are unrelated
# capabilities that merely share an English verb across layers.
# Accessor/parser idiom (get, parse) calibrated on the same repo's
# #4 failure: Gguf/TensorMap/Counter/Store .get and Role/Quant/Request
# .parse are universal method verbs namespaced by their types — the most
# generic name-equality noise there is. Collection verb (add) same class:
# Catalog.add / Registry.add / histogram add are unrelated containers.
BLOCKED_SYMBOL_NAMES = frozenset({
    "add", "all", "arena", "Builder", "Client", "close", "Config", "contains",
    "count", "counter", "decode", "deinit", "empty", "encode", "Entry",
    "err", "error", "Error", "eval", "Event", "Events", "Factory", "find",
    "free", "get", "hash",
    "Handler", "Info", "init", "Item", "Kind", "label", "lineOf",
    "List", "main", "Manager", "mark", "max", "memcpy", "Message",
    "Metadata", "min", "ms", "name", "new", "nullptr", "observe",
    "ok", "open", "Option", "Options", "parse", "Payload", "profile", "Record",
    "record", "Ref", "register", "Registry", "Request", "reset",
    "Response", "Result", "Results", "sample", "scales", "Spec",
    "State", "Status", "store", "stream", "String", "test", "Type",
    "usage", "Value", "vec", "version",
})

# A symbol re-declared across this many components (same language) is
# "severe" duplication: a capability scattered repo-wide. Two components
# is one refactor ticket (warn); three is systemic (error, CI fails).
SEVERE_COMPONENT_SPREAD = 3

# Symbol kinds that represent real declarations.
_REAL_KINDS = frozenset({"fn", "type", "var"})


def _is_real_symbol(kind: str, signature: str) -> bool:
    if kind in _REAL_KINDS:
        return True
    return signature.startswith(("fn ", "type ", "var "))


def read_code_map(db_path: Path) -> list[dict]:
    """All real symbol rows joined to their component and file language.

    Returns a list of dicts: name, kind, signature, file, component_id,
    component, language. Deterministic order: name, component, file.
    """
    con = sqlite3.connect(Path(db_path))
    con.row_factory = sqlite3.Row
    try:
        rows = con.execute(
            "SELECT s.name AS name, s.kind AS kind, s.signature AS signature,"
            "       s.file AS file, f.component_id AS component_id,"
            "       f.language AS language, c.name AS component"
            "  FROM symbols s"
            "  JOIN files f ON s.file = f.path"
            "  JOIN components c ON f.component_id = c.id"
        ).fetchall()
    finally:
        con.close()
    out = [
        {
            "name": r["name"], "kind": r["kind"] or "",
            "signature": r["signature"] or "", "file": r["file"],
            "component_id": r["component_id"], "component": r["component"],
            "language": r["language"] or "",
        }
        for r in rows if _is_real_symbol(r["kind"] or "", r["signature"] or "")
    ]
    out.sort(key=lambda r: (r["name"], r["component_id"], r["file"]))
    return out


def duplication_report(code_map: list[dict]) -> dict:
    """Cross-component symbol duplication (DRY) from a code map.

    A name is duplicated when declared in >= 2 distinct components within
    the same language (blocklist and ``__`` builtins excluded). Severe =
    any single (name, language) spanning >= SEVERE_COMPONENT_SPREAD
    components.
    """
    by_name_lang: dict[tuple[str, str], dict[str, dict]] = {}
    name_counts: dict[str, set] = {}
    for row in code_map:
        name_counts.setdefault(row["component"], set()).add(row["component_id"])
        name = row["name"]
        if name in BLOCKED_SYMBOL_NAMES or name.startswith("__"):
            continue
        key = (name, row["language"])
        comps = by_name_lang.setdefault(key, {})
        comps.setdefault(row["component_id"], {
            "component": row["component"], "files": set(),
        })["files"].add(row["file"])

    def _display(cid: str, cname: str) -> str:
        # component names can repeat across ids — disambiguate for humans
        return cname if len(name_counts.get(cname, ())) <= 1 else f"{cname}[{cid}]"

    duplicated: list[dict] = []
    severe: list[dict] = []
    for (name, lang), comps in sorted(by_name_lang.items()):
        if len(comps) < 2:
            continue
        entry = {
            "name": name,
            "language": lang,
            "components": [_display(cid, c["component"])
                            for cid, c in sorted(comps.items())],
            "files": sorted(f for c in comps.values() for f in c["files"]),
            "spread": len(comps),
        }
        duplicated.append(entry)
        if len(comps) >= SEVERE_COMPONENT_SPREAD:
            severe.append(entry)

    return {"duplicated": duplicated, "severe": severe}


def _find_cycles(components: dict[str, str], deps: list[tuple[str, str]]) -> list[list[str]]:
    """Dependency cycles as component-name lists (src depends on dst)."""
    graph: dict[str, list[str]] = {}
    for src, dst in deps:
        graph.setdefault(src, []).append(dst)

    color: dict[str, int] = {}  # 0=white 1=gray 2=black
    stack: list[str] = []
    cycles: list[list[str]] = []

    def dfs(node: str) -> None:
        color[node] = 1
        stack.append(node)
        for nxt in graph.get(node, []):
            if color.get(nxt, 0) == 1:
                i = stack.index(nxt)
                cycles.append([components[n] for n in stack[i:]])
            elif color.get(nxt, 0) == 0:
                dfs(nxt)
        stack.pop()
        color[node] = 2

    for node in sorted(graph):
        if color.get(node, 0) == 0:
            dfs(node)
    return cycles


def graph_metrics(db_path: Path) -> dict:
    """Whole-graph shape metrics — the fitness snapshot numbers."""
    con = sqlite3.connect(Path(db_path))
    con.row_factory = sqlite3.Row
    try:
        comps = {
            r["id"]: r["name"]
            for r in con.execute("SELECT id, name FROM components ORDER BY seq")
        }
        deps = [
            (r["src"], r["dst"])
            for r in con.execute("SELECT src, dst FROM deps ORDER BY src, dst")
        ]
        files_per_comp: dict[str, int] = {}
        for r in con.execute(
                "SELECT component_id, COUNT(*) AS n FROM files"
                " WHERE component_id IS NOT NULL GROUP BY component_id"):
            files_per_comp[r["component_id"]] = r["n"]
        syms_per_comp: dict[str, int] = {}
        for r in con.execute(
                "SELECT f.component_id AS cid, COUNT(*) AS n"
                "  FROM symbols s JOIN files f ON s.file = f.path"
                " WHERE f.component_id IS NOT NULL GROUP BY f.component_id"):
            syms_per_comp[r["cid"]] = r["n"]
        total_symbols = con.execute("SELECT COUNT(*) FROM symbols").fetchone()[0]
    finally:
        con.close()

    fan_in: dict[str, int] = {}
    for _src, dst in deps:
        fan_in[dst] = fan_in.get(dst, 0) + 1

    def _top(counter: dict[str, int], k: int = 5) -> list[dict]:
        ranked = sorted(counter.items(), key=lambda kv: (-kv[1], comps.get(kv[0], kv[0])))
        return [{"component": comps.get(cid, cid), "count": n}
                for cid, n in ranked[:k] if n > 0]

    return {
        "components": len(comps),
        "edges": len(deps),
        "cycles": _find_cycles(comps, deps),
        "max_fan_in": _top(fan_in, 1),
        "top_fan_in": _top(fan_in),
        "largest_by_files": [
            {"component": comps.get(cid, cid), "count": n}
            for cid, n in sorted(files_per_comp.items(),
                                 key=lambda kv: (-kv[1], comps.get(kv[0], kv[0])))[:5]
        ],
        "largest_by_symbols": [
            {"component": comps.get(cid, cid), "count": n}
            for cid, n in sorted(syms_per_comp.items(),
                                 key=lambda kv: (-kv[1], comps.get(kv[0], kv[0])))[:5]
        ],
        "symbols": total_symbols,
    }


def fitness_snapshot(db_path: Path) -> dict:
    """Metrics + duplication in one payload (the CLI renders this)."""
    code_map = read_code_map(db_path)
    return {
        "metrics": graph_metrics(db_path),
        "duplication": duplication_report(code_map),
    }


def render_markdown(snapshot: dict) -> str:
    """The `## Graph Fitness` section for Architecture.md.

    A per-SHA snapshot — packages are immutable, so consecutive pages
    diff into the trend (radiator, not gate: the gate lives in
    rig-compliance.py).
    """
    m = snapshot["metrics"]
    dup = snapshot["duplication"]["duplicated"]
    severe = snapshot["duplication"]["severe"]
    lines = [
        "## Graph Fitness",
        "",
        "Deterministic modularity snapshot for this SHA (source: rig.db —",
        "cycles are hard CI errors; fan-in and size show hub / god-component",
        "drift; the duplication list is DRY debt visible at the graph level).",
        "",
        "| Metric | Value |",
        "|---|---|",
        f"| Components | {m['components']} |",
        f"| Dependency edges | {m['edges']} |",
        f"| Dependency cycles | {len(m['cycles'])}"
        + (f" ({'; '.join(' -> '.join(c) + ' -> ' + c[0] for c in m['cycles'])})" if m["cycles"] else "")
        + " |",
    ]
    if m["max_fan_in"]:
        hub = m["max_fan_in"][0]
        lines.append(f"| Max fan-in (dependents) | `{hub['component']}` ({hub['count']}) |")
    if m["largest_by_files"]:
        big = m["largest_by_files"][0]
        lines.append(f"| Largest component (files) | `{big['component']}` ({big['count']}) |")
    lines += [
        f"| Symbols | {m['symbols']} |",
        "",
        "### Duplicated symbols (cross-component, same language)",
        "",
    ]
    if not dup:
        lines.append("None — no cross-component symbol duplication.")
    else:
        lines += [
            "| Symbol | Language | Components | Files |",
            "|---|---|---|---|",
        ]
        for d in dup:
            flag = " **SEVERE**" if d["spread"] >= SEVERE_COMPONENT_SPREAD else ""
            lines.append(
                f"| `{d['name']}`{flag} | {d['language']} | "
                f"{len(d['components'])} ({', '.join(d['components'])}) | "
                f"{', '.join(f'`{f}`' for f in d['files'][:4])}"
                + (" …" if len(d["files"]) > 4 else "") + " |"
            )
        note = ("Severe (>= "
                f"{SEVERE_COMPONENT_SPREAD} components) duplication FAILS CI"
                " (rig-compliance).") if severe else (
            "Two-component duplicates are advisory — extend one instead of "
            "duplicating (`rig search` before writing a near-copy).")
        lines += ["", note]
    return "\n".join(lines) + "\n"
