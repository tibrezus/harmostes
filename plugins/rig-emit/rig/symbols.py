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

# Caps per file to keep model.c4 readable. A file with 100 exports
# would bloat the model without helping an agent find reuse targets.
_MAX_EXPORTS = 20


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

def _extract_go_exports(raw: str) -> list[tuple[int, str]]:
    """Extract exported Go symbols (capitalized func/type/var/const)."""
    exports: list[tuple[int, str]] = []
    for lineno, line in enumerate(raw.split("\n"), 1):
        stripped = line.strip()
        # func ExportedName(
        if m := re.match(r"^func\s+(?:\([^)]*\)\s+)?([A-Z][A-Za-z0-9_]*)", stripped):
            exports.append((lineno, f"func {m.group(1)}"))
        # type ExportedName struct/interface/...
        elif m := re.match(r"^type\s+([A-Z][A-Za-z0-9_]*)", stripped):
            exports.append((lineno, f"type {m.group(1)}"))
        # var/const ExportedName (block or single)
        elif m := re.match(r"^(?:var|const)\s+([A-Z][A-Za-z0-9_]*)", stripped):
            exports.append((lineno, m.group(1)))
        if len(exports) >= _MAX_EXPORTS:
            break
    return exports


def _extract_zig_exports(raw: str) -> list[tuple[int, str]]:
    """Extract exported Zig symbols (pub fn, pub const, pub var)."""
    exports: list[tuple[int, str]] = []
    for lineno, line in enumerate(raw.split("\n"), 1):
        stripped = line.strip()
        if m := re.match(r"^pub\s+fn\s+([A-Za-z0-9_]*)", stripped):
            exports.append((lineno, f"fn {m.group(1)}"))
        elif m := re.match(r"^pub\s+const\s+([A-Za-z0-9_]*)", stripped):
            # Distinguish struct/type aliases from plain constants
            if "struct" in stripped or "type" in stripped.lower():
                exports.append((lineno, f"type {m.group(1)}"))
            else:
                exports.append((lineno, m.group(1)))
        elif m := re.match(r"^pub\s+var\s+([A-Za-z0-9_]*)", stripped):
            exports.append((lineno, f"var {m.group(1)}"))
        if len(exports) >= _MAX_EXPORTS:
            break
    return exports


def _extract_python_exports(raw: str) -> list[tuple[int, str]]:
    """Extract module-level Python def/class/async def."""
    exports: list[tuple[int, str]] = []
    for lineno, line in enumerate(raw.split("\n"), 1):
        # Module-level only: no leading whitespace
        if line and not line[0].isspace():
            stripped = line.strip()
            if m := re.match(r"^(?:async\s+)?def\s+([A-Za-z0-9_]*)", stripped):
                exports.append((lineno, f"def {m.group(1)}"))
            elif m := re.match(r"^class\s+([A-Za-z0-9_]*)", stripped):
                exports.append((lineno, f"class {m.group(1)}"))
        if len(exports) >= _MAX_EXPORTS:
            break
    return exports


def _extract_c_exports(raw: str) -> list[tuple[int, str]]:
    """Extract C/CUDA function declarations (non-static, name before '(')."""
    exports: list[tuple[int, str]] = []
    for lineno, line in enumerate(raw.split("\n"), 1):
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
                    exports.append((lineno, f"fn {name}"))
        if len(exports) >= _MAX_EXPORTS:
            break
    return exports


def extract_exports(filepath: Path, language: str) -> list[str]:
    """Extract exported function/type names from a source file.

    Returns a list of strings like ['fn ParseConfig', 'type Config'].
    Empty list if the file doesn't exist or the language is unsupported.
    """
    return [sym for _line, sym in extract_export_rows(filepath, language)]


def extract_export_rows(filepath: Path, language: str) -> list[tuple[int, str]]:
    """Like extract_exports but with 1-based line numbers: [(line, "kind name")]."""
    try:
        raw = filepath.read_text(encoding="utf-8", errors="replace")
    except (OSError, UnicodeDecodeError):
        return []

    if language == "go":
        return _extract_go_exports(raw)
    if language == "zig":
        return _extract_zig_exports(raw)
    if language == "python":
        return _extract_python_exports(raw)
    if language in ("c", "cuda", "cpp", "c++"):
        return _extract_c_exports(raw)
    return []


def extract_symbols(rig: dict, source_root: Path) -> list[dict]:
    """Build the full symbol table for a RIG: one row per exported symbol
    across every component's source files.

    Rows: {file, name, kind, line, signature} — db.add_symbols consumes
    them directly. `signature` is the ``"kind name"`` display string (what
    model.c4's `// Exports:` lines show).
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
            for line, sig in extract_export_rows(path, lang):
                kind, _, name = sig.partition(" ")
                symbols.append({
                    "file": sf,
                    "name": name or sig,
                    "kind": kind,
                    "line": line,
                    "signature": sig,
                })
    return symbols
