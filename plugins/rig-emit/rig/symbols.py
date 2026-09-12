"""Symbol extraction — the single implementation shared by all consumers.

Ported verbatim from rig-to-c4.py (which previously owned these functions)
so that model.c4 generated from rig.json (on-the-fly extraction) and from
rig.db (precomputed symbols) are byte-identical — the golden-parity test
depends on this.

Each extractor returns ``"kind name"`` strings (e.g. ``"fn ParseConfig"``).
`extract_symbols` additionally records the 1-based line number, making the
result a queryable symbol table for rig.db.
"""

from __future__ import annotations

import re
from pathlib import Path

# Display cap per file to keep model.c4 readable. A file with 100 exports
# would bloat the model without helping an agent find reuse targets.
# rig.db deliberately does NOT apply it: the symbol table is the query
# surface, and truncating it hides exactly the symbols a review asks about
# (rhesadox#2085: the decisive export sat at line 1371 of a 101-function
# file — past the cap, invisible to every graph query). The cap applies to
# the model.c4 display path only; extract_symbols passes cap=None.
_MAX_EXPORTS = 20

# Max lines scanned when computing a symbol's end line (block-end
# heuristics below). Guards against runaway scans on malformed files.
_MAX_SCAN = 500

# A `pub const` whose RHS is a bare @import is a re-export alias, not a
# declaration: the symbol is defined in the imported module and merely
# re-published here (llm-wiki-core#4 — rhesadox StDtype: one shared
# safetensors reader, two converter mains re-exporting it, counted as
# spread-3 "duplication"). Covers the accessor form `@import("m").Name`,
# the whole-module form `@import("x.zig")`, and the `.*` deref form.
# Matched on a single stripped line — the re-export idiom is a one-liner;
# multi-line forms are left conservative (still extracted).
_ZIG_REEXPORT_RE = re.compile(
    r'^pub\s+const\s+[A-Za-z0-9_]+\s*=\s*@import\s*\(\s*"[^"]+"\s*\)'
    r'(?:\s*\.\s*(?:[A-Za-z0-9_]+|\*))?\s*;\s*$'
)


# ── Span helpers (end-line computation) ──────────────────────────────
#
# rig.db records where each symbol ENDS (symbols.line_end). Two consumers
# need it: rig impact maps diff hunks → symbols via line ranges, and the
# clone detector slices symbol bodies without re-parsing language syntax.
# These are sizing heuristics (strings/comments treated naively) — good
# enough for ranges, never used for identity.


def span_has_open_brace(lines: list[str], start: int, end: int) -> bool:
    """True when the 1-based inclusive line range [start, end] contains '{'.

    The definition test for C-family spans (a definition owns a body; a
    prototype or a bare call line does not). Shared by the extractor and
    by rig/calls.py, which must not bind edges to declaration rows from
    other sources (archmap).
    """
    return any("{" in l for l in lines[max(0, start - 1):min(len(lines), end)])


def _delim_block_end(lines: list[str], start: int,
                     open_ch: str = "{", close_ch: str = "}") -> int:
    """1-based end line of the open/close-delimited block that opens on or
    after line `start`. Returns `start` when no delimiter opens there
    (single-line or delimiter-less declaration)."""
    balance = 0
    opened = False
    last = start
    for i in range(start - 1, min(len(lines), start - 1 + _MAX_SCAN)):
        last = i + 1
        balance += lines[i].count(open_ch) - lines[i].count(close_ch)
        if balance > 0:
            opened = True
        elif opened:
            return i + 1
    return last if opened else start


def _stmt_end(lines: list[str], start: int) -> int:
    """1-based end of the statement starting on line `start`: the first
    line whose stripped text ends with ';'. Falls back to `start`."""
    for i in range(start - 1, min(len(lines), start - 1 + _MAX_SCAN)):
        if lines[i].rstrip().endswith(";"):
            return i + 1
    return start


def _python_block_end(lines: list[str], start: int) -> int:
    """1-based end of the indented block under the def/class on line
    `start`: the last non-blank line indented deeper than the def."""
    base = len(lines[start - 1]) - len(lines[start - 1].lstrip())
    end = start
    for i in range(start, min(len(lines), start - 1 + _MAX_SCAN)):
        if not lines[i].strip():
            continue
        indent = len(lines[i]) - len(lines[i].lstrip())
        if indent <= base:
            break
        end = i + 1
    return end


# ── Doc comments ─────────────────────────────────────────────────────

def _extract_consecutive(lines: list[str], prefix: str) -> str:
    """Extract consecutive comment lines starting from line 0."""
    out: list[str] = []
    for line in lines:
        stripped = line.strip()
        if stripped.startswith(prefix):
            text = stripped[len(prefix):].lstrip()
            out.append(text)
        elif stripped == "" and out:
            continue  # skip blank lines within a comment block
        elif stripped and not stripped.startswith(prefix):
            break  # hit code
        elif not out:
            continue  # skip leading blanks
    return "\n".join(out).strip()


def _extract_block_comment(lines: list[str]) -> str:
    """Extract a /* */ block at the top of the file."""
    for i, line in enumerate(lines):
        stripped = line.strip()
        if stripped.startswith("/*"):
            # Collect until closing */
            block: list[str] = []
            if stripped.endswith("*/") and len(stripped) > 2:
                inner = stripped[2:-2].strip()
                if inner:
                    block.append(inner)
                return "\n".join(block)
            for sub in lines[i + 1:]:
                if "*/" in sub:
                    before = sub[: sub.index("*/")].strip()
                    if before:
                        block.append(before)
                    break
                # Strip leading * (C block convention)
                cleaned = sub.strip()
                if cleaned.startswith("*"):
                    cleaned = cleaned[1:].lstrip()
                if cleaned:
                    block.append(cleaned)
            return "\n".join(block).strip()
        elif stripped and not stripped.startswith("//"):
            break  # code before any comment
    return ""


def _extract_python_docstring(lines: list[str]) -> str:
    """Extract a Python module docstring (\"\"\"...\"\"\")."""
    text = "\n".join(lines)
    m = re.search(r'"""(.*?)"""', text, re.DOTALL)
    if m:
        return m.group(1).strip()
    m = re.search(r"'''(.*?)'''", text, re.DOTALL)
    if m:
        return m.group(1).strip()
    return ""


def extract_doc_comment(filepath: Path, language: str) -> str:
    """Extract the top-of-file documentation comment from a source file.

    Returns an empty string if the file doesn't exist or has no doc comment.
    """
    try:
        raw = filepath.read_text(encoding="utf-8", errors="replace")
    except (OSError, UnicodeDecodeError):
        return ""

    lines = raw.split("\n")

    if language == "zig":
        # Zig: //! module doc comments at the top
        comment = _extract_consecutive(lines, "//!")
        if comment:
            return comment
        # Fall back to // comments (some files use these)
        return _extract_consecutive(lines, "//")

    if language == "python":
        # Python: module docstring first, then # comments
        docstring = _extract_python_docstring(lines)
        if docstring:
            return docstring
        return _extract_consecutive(lines, "#")

    if language in ("go",):
        # Go: // package comment before the package declaration
        return _extract_consecutive(lines, "//")

    if language in ("c", "cuda", "cpp", "c++"):
        # C/CUDA: /* */ block first, then // lines
        block = _extract_block_comment(lines)
        if block:
            return block
        return _extract_consecutive(lines, "//")

    # Generic fallback
    return (
        _extract_block_comment(lines)
        or _extract_consecutive(lines, "//")
        or _extract_consecutive(lines, "#")
    )


# ── Exported symbol extraction ────────────────────────────────────────

def _extract_go_export_spans(raw: str, cap: int = _MAX_EXPORTS) -> list[tuple[int, int, str]]:
    """Exported Go symbols (capitalized func/type/var/const) with end lines."""
    lines = raw.split("\n")
    spans: list[tuple[int, int, str]] = []
    for lineno, line in enumerate(lines, 1):
        stripped = line.strip()
        # func ExportedName(
        if m := re.match(r"^func\s+(?:\([^)]*\)\s+)?([A-Z][A-Za-z0-9_]*)", stripped):
            spans.append((lineno, _delim_block_end(lines, lineno), f"func {m.group(1)}"))
        # type ExportedName struct/interface/...
        elif m := re.match(r"^type\s+([A-Z][A-Za-z0-9_]*)", stripped):
            spans.append((lineno, _delim_block_end(lines, lineno), f"type {m.group(1)}"))
        # var/const ExportedName (block or single) — brace end covers
        # multi-line composite literals; scalars close at themselves
        elif m := re.match(r"^(?:var|const)\s+([A-Z][A-Za-z0-9_]*)", stripped):
            spans.append((lineno, _delim_block_end(lines, lineno), m.group(1)))
        if cap is not None and len(spans) >= cap:
            break
    return spans


def _extract_zig_export_spans(raw: str, cap: int = _MAX_EXPORTS) -> list[tuple[int, int, str]]:
    """Exported Zig symbols (pub fn/const/var) with end lines."""
    lines = raw.split("\n")
    spans: list[tuple[int, int, str]] = []
    for lineno, line in enumerate(lines, 1):
        stripped = line.strip()
        if m := re.match(r"^pub\s+fn\s+([A-Za-z0-9_]*)", stripped):
            spans.append((lineno, _delim_block_end(lines, lineno), f"fn {m.group(1)}"))
        elif m := re.match(r"^pub\s+const\s+([A-Za-z0-9_]*)", stripped):
            if _ZIG_REEXPORT_RE.match(stripped):
                continue  # re-export alias — declared in the imported module
            end = (_delim_block_end(lines, lineno)
                   if stripped.endswith("{") else _stmt_end(lines, lineno))
            # Distinguish struct/type aliases from plain constants
            if "struct" in stripped or "type" in stripped.lower():
                spans.append((lineno, end, f"type {m.group(1)}"))
            else:
                spans.append((lineno, end, m.group(1)))
        elif m := re.match(r"^pub\s+var\s+([A-Za-z0-9_]*)", stripped):
            spans.append((lineno, _stmt_end(lines, lineno), f"var {m.group(1)}"))
        if cap is not None and len(spans) >= cap:
            break
    return spans


def _extract_python_export_spans(raw: str, cap: int = _MAX_EXPORTS) -> list[tuple[int, int, str]]:
    """Module-level Python def/class with end lines (indentation walk)."""
    lines = raw.split("\n")
    spans: list[tuple[int, int, str]] = []
    for lineno, line in enumerate(lines, 1):
        # Module-level only: no leading whitespace
        if line and not line[0].isspace():
            stripped = line.strip()
            if m := re.match(r"^(?:async\s+)?def\s+([A-Za-z0-9_]*)", stripped):
                spans.append((lineno, _python_block_end(lines, lineno), f"def {m.group(1)}"))
            elif m := re.match(r"^class\s+([A-Za-z0-9_]*)", stripped):
                spans.append((lineno, _python_block_end(lines, lineno), f"class {m.group(1)}"))
        if cap is not None and len(spans) >= cap:
            break
    return spans


def _extract_c_export_spans(raw: str, cap: int = _MAX_EXPORTS) -> list[tuple[int, int, str]]:
    """C/CUDA function definitions (non-static) with end lines.

    A span is a DEFINITION only if its range contains a '{': prototypes
    and bare call lines (`cudaDeviceSynchronize(...)` inside a body) are
    declarations/references, not exports of this file (llm-wiki-core#17 —
    uncapped extraction made call-line phantoms the majority of the C
    symbol table, poisoning fan-in and dead-code reads). Same doctrine as
    the zig re-export rule (#4): re-publishing/calling is not declaring.
    """
    lines = raw.split("\n")
    spans: list[tuple[int, int, str]] = []
    for lineno, line in enumerate(lines, 1):
        stripped = line.strip()
        # Skip preprocessor, comments, static, blank lines
        if (not stripped or stripped.startswith("#") or stripped.startswith("//")
                or stripped.startswith("/*") or stripped.startswith("*")):
            continue
        # Match: return-type function-name(...  — exclude static/inline-only
        if "(" in stripped and not stripped.startswith("static"):
            # Extract the word immediately before the first '('
            if m := re.search(r"\b([A-Za-z_][A-Za-z0-9_]*)\s*\(", stripped):
                name = m.group(1)
                # Filter out C keywords that appear before '('
                if name not in ("if", "for", "while", "switch", "return",
                                "sizeof", "typedef", "extern", "struct"):
                    code = stripped.split("//")[0].rstrip()
                    if code.endswith(";"):
                        continue  # prototype/call — declaration, not an export
                    # definition: brace end beats the first ';' inside
                    # the body; multi-line headers end at their ';'
                    end = max(_stmt_end(lines, lineno),
                              _delim_block_end(lines, lineno))
                    if not span_has_open_brace(lines, lineno, end):
                        continue  # declaration without body (multi-line proto)
                    spans.append((lineno, end, f"fn {name}"))
        if cap is not None and len(spans) >= cap:
            break
    return spans


def extract_exports(filepath: Path, language: str) -> list[str]:
    """Extract exported function/type names from a source file.

    Returns a list of strings like ['fn ParseConfig', 'type Config'].
    Empty list if the file doesn't exist or the language is unsupported.
    """
    return [sym for _line, sym in extract_export_rows(filepath, language)]


def extract_export_spans(filepath: Path, language: str,
                         cap: int | None = _MAX_EXPORTS) -> list[tuple[int, int, str]]:
    """Exported symbols with (start line, end line, "kind name").

    End lines are sizing heuristics (brace/paren matching, indentation
    walk, statement scan) — never used for identity, only for diff-hunk
    mapping and body slicing. `cap=None` extracts every export (the
    rig.db symbol table); the default cap applies to display surfaces
    (model.c4) so a 100-export file cannot bloat the model.
    """
    try:
        raw = filepath.read_text(encoding="utf-8", errors="replace")
    except (OSError, UnicodeDecodeError):
        return []

    if language == "go":
        return _extract_go_export_spans(raw, cap)
    if language == "zig":
        return _extract_zig_export_spans(raw, cap)
    if language == "python":
        return _extract_python_export_spans(raw, cap)
    if language in ("c", "cuda", "cpp", "c++"):
        return _extract_c_export_spans(raw, cap)
    return []


def extract_export_rows(filepath: Path, language: str) -> list[tuple[int, str]]:
    """Like extract_exports but with 1-based line numbers: [(line, "kind name")].

    Canonical shape for model.c4 `// Exports:` lines — the golden-parity
    test depends on it staying byte-identical; span end lines never reach
    this surface.
    """
    return [(line, sig)
            for line, _end, sig in extract_export_spans(filepath, language)]


def extract_symbols(rig: dict, source_root: Path) -> list[dict]:
    """Build the full symbol table for a RIG: one row per exported symbol
    across every component's source files.

    Rows: {file, name, kind, line, line_end, signature} — db.add_symbols
    consumes them directly. `signature` is the ``"kind name"`` display
    string (what model.c4's `// Exports:` lines show).
    """
    symbols: list[dict] = []
    seen: set[Path] = set()
    for c in rig.get("components", []):
        lang = c.get("programming_language", "")
        for sf in c.get("source_files", []):
            path = source_root / sf
            key = (path, lang)
            if key in seen or not path.is_file():
                continue
            seen.add(key)
            # cap=None: the query surface must see every export — the
            # display cap exists for model.c4 only (see _MAX_EXPORTS).
            for line, end, sig in extract_export_spans(path, lang, cap=None):
                kind, _, name = sig.partition(" ")
                symbols.append({
                    "file": sf,
                    "name": name or sig,
                    "kind": kind,
                    "line": line,
                    "line_end": end,
                    "signature": sig,
                })
    return symbols
