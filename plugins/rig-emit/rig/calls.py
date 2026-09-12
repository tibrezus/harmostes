"""Call-edge extraction, regex-v1 (llm-wiki-core#17).

Populates the `calls` table from the symbol table + source bodies so the
query surface (impact / dead / trace) has real fan-in data. Endpoints are
"file:name" keys — the archmap contract — so a later archmap ingestion
(compiler-grade) replaces this data wholesale, tagged `calls_source=archmap`.

PRECISION OVER RECALL. A wrong call edge produces a wrong review verdict;
a missing edge is merely a slower review. The resolution rule is therefore
maximally conservative:

  1. same file: a unique in-file definition wins (zig module scope, C
     file-local helpers);
  2. otherwise: a GLOBALLY unique exact name wins — this is the FFI
     bridge: a zig wrapper calling `rhesadox_rec_pick_victim(...)` binds
     to the C export of that exact name (the ABI is the name);
  3. otherwise (ambiguous across files): NO edge.

Deterministic: fixed keyword set, sorted output, canonical hash covers the
table. `meta.calls_source = "regex-v1"` records provenance.
"""

from __future__ import annotations

import re
import sqlite3
from pathlib import Path

from . import db as rig_db
from .symbols import span_has_open_brace

# Identifiers that appear before '(' but are never calls. Covers C, Zig and
# Python control flow / declarations. Skipping a keyword only costs an edge
# (recall), never creates a wrong one (precision).
_CALL_KEYWORDS = frozenset({
    # C / CUDA
    "if", "else", "for", "while", "do", "switch", "case", "default",
    "return", "sizeof", "typedef", "extern", "struct", "union", "enum",
    "goto", "defined",
    # Zig
    "fn", "test", "comptime", "defer", "errdefer", "unreachable",
    "try", "catch", "orelse", "and", "or", "not", "align", "usingnamespace",
    # Python
    "def", "class", "lambda", "with", "as", "import", "from", "print",
    "yield", "raise", "del", "in", "is", "assert", "elif", "except",
    "finally", "global", "nonlocal", "async", "await",
})

_CALL_RE = re.compile(r"\b([A-Za-z_][A-Za-z0-9_]*)\s*\(")

# C-family suffixes: spans without a '{' are declarations (prototypes,
# extern decls), not definitions — never edge targets (a prototype hijacking
# same-file resolution bound the #2085-style call to a phantom node).
_C_SUFFIXES = {".c", ".h", ".cu", ".cuh", ".cpp", ".cxx", ".cc", ".hpp"}


def scan_calls(lines: list[str], start: int, end: int) -> set[str]:
    """Called identifiers in the 1-based inclusive line range [start, end]."""
    found: set[str] = set()
    for line in lines[max(0, start - 1):min(len(lines), end)]:
        for m in _CALL_RE.finditer(line):
            name = m.group(1)
            if name not in _CALL_KEYWORDS:
                found.add(name)
    return found


def build_call_edges(symbols: list[dict], source_root: Path) -> list[tuple[str, str]]:
    """Resolve called identifiers to symbol rows.

    `symbols` rows: {file, name, line, line_end} (as stored in rig.db).
    Returns sorted (caller, callee) pairs of "file:name" keys.
    """
    # name → [(file, name), ...] — definition sites only: C-family spans
    # without a '{' are prototypes/extern decls and must not capture edges.
    index: dict[str, list[tuple[str, str]]] = {}
    texts: dict[str, list[str]] = {}
    for s in symbols:
        if s["file"] not in texts:
            try:
                texts[s["file"]] = (source_root / s["file"]).read_text(
                    encoding="utf-8", errors="replace").split("\n")
            except OSError:
                texts[s["file"]] = []
        lines = texts[s["file"]]
        end = s["line_end"] or s["line"]
        if (Path(s["file"]).suffix.lower() in _C_SUFFIXES
                and lines and not span_has_open_brace(lines, s["line"], end)):
            continue  # declaration-only — not a definition
        index.setdefault(s["name"], []).append((s["file"], s["name"]))

    edges: set[tuple[str, str]] = set()
    for s in symbols:
        lines = texts.get(s["file"])
        if not lines:
            continue
        caller = f"{s['file']}:{s['name']}"
        for ident in sorted(scan_calls(lines, s["line"], s["line_end"] or s["line"])):
            if ident == s["name"]:
                continue  # own declaration line inside the span — not recursion
            cands = index.get(ident, ())
            same_file = [c for c in cands if c[0] == s["file"]]
            if len(same_file) == 1:
                target = same_file[0]
            elif len(cands) == 1:
                target = cands[0]
            else:
                continue  # ambiguous across files — no edge (precision)
            edges.add((caller, f"{target[0]}:{target[1]}"))
    return sorted(edges)


def compute_and_store(db_path: Path, source_root: Path) -> int:
    """Emit-time pass: symbols → call edges → calls table. Returns the count."""
    con = sqlite3.connect(db_path)
    try:
        symbols = [
            {"file": f, "name": n, "line": ln, "line_end": le}
            for f, n, ln, le in con.execute(
                "SELECT file, name, line, line_end FROM symbols")]
    finally:
        con.close()
    edges = build_call_edges(symbols, source_root)
    rig_db.add_call_edges(db_path, edges)
    rig_db.set_meta(db_path, "calls_source", "regex-v1")
    return len(edges)
