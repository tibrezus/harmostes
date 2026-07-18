#!/usr/bin/env python3
"""rig-to-c4.py — Deterministic RIG → LikeC4 model generator.

Reads a rig.json and produces a model.c4 where every element is derived
from the RIG. Source files that carry a top-of-file doc comment (Zig //!,
Go //, C /* */, Python docstrings) get the comment extracted verbatim into
the C4 component description — no LLM, no hallucination, fully reproducible.

Usage:
    rig-to-c4.py <rig.json> [--source-dir <path>] [-o <output.c4>]

If --source-dir is given (or source files exist in the CWD), doc comments
are extracted from the actual source files. Otherwise, descriptions are
generated from RIG metadata only (name, type, language, source count, test
status).
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path


# ── Doc comment extraction ────────────────────────────────────────────

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

# Caps per file to keep model.c4 readable. A file with 100 exports
# would bloat the model without helping an agent find reuse targets.
_MAX_EXPORTS = 20


def _extract_go_exports(raw: str) -> list[str]:
    """Extract exported Go symbols (capitalized func/type/var/const)."""
    exports: list[str] = []
    for line in raw.split("\n"):
        stripped = line.strip()
        # func ExportedName(
        if m := re.match(r"^func\s+(?:\([^)]*\)\s+)?([A-Z][A-Za-z0-9_]*)", stripped):
            exports.append(f"func {m.group(1)}")
        # type ExportedName struct/interface/...
        elif m := re.match(r"^type\s+([A-Z][A-Za-z0-9_]*)", stripped):
            exports.append(f"type {m.group(1)}")
        # var/const ExportedName (block or single)
        elif m := re.match(r"^(?:var|const)\s+([A-Z][A-Za-z0-9_]*)", stripped):
            exports.append(m.group(1))
        if len(exports) >= _MAX_EXPORTS:
            break
    return exports


def _extract_zig_exports(raw: str) -> list[str]:
    """Extract exported Zig symbols (pub fn, pub const, pub var)."""
    exports: list[str] = []
    for line in raw.split("\n"):
        stripped = line.strip()
        if m := re.match(r"^pub\s+fn\s+([A-Za-z0-9_]*)", stripped):
            exports.append(f"fn {m.group(1)}")
        elif m := re.match(r"^pub\s+const\s+([A-Za-z0-9_]*)", stripped):
            # Distinguish struct/type aliases from plain constants
            if "struct" in stripped or "type" in stripped.lower():
                exports.append(f"type {m.group(1)}")
            else:
                exports.append(m.group(1))
        elif m := re.match(r"^pub\s+var\s+([A-Za-z0-9_]*)", stripped):
            exports.append(f"var {m.group(1)}")
        if len(exports) >= _MAX_EXPORTS:
            break
    return exports


def _extract_python_exports(raw: str) -> list[str]:
    """Extract module-level Python def/class/async def."""
    exports: list[str] = []
    for line in raw.split("\n"):
        # Module-level only: no leading whitespace
        if line and not line[0].isspace():
            stripped = line.strip()
            if m := re.match(r"^(?:async\s+)?def\s+([A-Za-z0-9_]*)", stripped):
                exports.append(f"def {m.group(1)}")
            elif m := re.match(r"^class\s+([A-Za-z0-9_]*)", stripped):
                exports.append(f"class {m.group(1)}")
        if len(exports) >= _MAX_EXPORTS:
            break
    return exports


def _extract_c_exports(raw: str) -> list[str]:
    """Extract C/CUDA function declarations (non-static, name before '(')."""
    exports: list[str] = []
    for line in raw.split("\n"):
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
                    exports.append(f"fn {name}")
        if len(exports) >= _MAX_EXPORTS:
            break
    return exports


def extract_exports(filepath: Path, language: str) -> list[str]:
    """Extract exported function/type names from a source file.

    Returns a list of strings like ['fn ParseConfig', 'type Config'].
    Empty list if the file doesn't exist or the language is unsupported.
    """
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


def sanitize_for_c4(text: str) -> str:
    """Sanitize text for safe embedding in LikeC4 single-quoted strings.

    LikeC4 '...' strings cannot contain unescaped ' or non-ASCII chars.
    We strip/replace them to stay deterministic and always valid.
    """
    # Remove characters that break LikeC4 single-quote string parsing
    text = text.replace("'", "")      # apostrophes close the string prematurely
    text = text.replace("\\", "")     # backslashes
    text = text.replace("`", "")       # backticks
    # Replace common Unicode with ASCII equivalents
    text = text.replace("\u2014", "-").replace("\u2013", "-")  # dashes
    text = text.replace("\u00d7", "x")  # multiplication sign
    # Strip any remaining non-ASCII (deterministic fallback)
    text = text.encode("ascii", errors="ignore").decode("ascii")
    return text


def truncate(text: str, max_lines: int = 15) -> str:
    """Truncate to N lines, adding ... if cut."""
    lines = text.strip().split("\n")
    if len(lines) <= max_lines:
        return text.strip()
    return "\n".join(lines[:max_lines]) + "\n..."


# ── Identifier helpers ────────────────────────────────────────────────

def sanitize(name: str) -> str:
    """Convert a RIG name to a valid LikeC4 identifier (camelCase).

    LikeC4 identifiers are [a-zA-Z][a-zA-Z0-9]* — no underscores, no hyphens.
    We convert hyphen-separator names to camelCase: dsv2-check → dsv2Check.
    """
    # Split on non-alphanumeric, build camelCase
    parts = re.split(r"[^a-zA-Z0-9]+", name)
    if not parts:
        return "unnamed"
    # First part lowercase, rest capitalized
    ident = parts[0].lower()
    for p in parts[1:]:
        if p:
            ident += p[0].upper() + p[1:].lower()
    if ident and ident[0].isdigit():
        ident = "_" + ident
    return ident or "unnamed"


def unique_ident(name: str, used: set[str]) -> str:
    """Generate a unique identifier from a name."""
    base = sanitize(name)
    if not base[0].islower() and not base[0] == "_":
        base = base[0].lower() + base[1:]
    ident = base
    n = 2
    while ident in used:
        ident = f"{base}{n}"
        n += 1
    used.add(ident)
    return ident


# ── C4 generation ─────────────────────────────────────────────────────

def generate_c4(rig: dict, source_dir: Path | None) -> str:
    """Generate the full LikeC4 model text from a RIG."""
    used_idents: set[str] = set()
    components = rig.get("components", [])
    comp_by_id = {c["id"]: c for c in components}
    name_by_id = {c["id"]: c["name"] for c in components}
    repo = rig.get("repository", {})

    # Test coverage map
    tested_ids: set[str] = set()
    for t in rig.get("test_definitions", []):
        tested_ids.update(t.get("components_being_tested_ids", []))

    # Evidence map (for build-file line refs)
    evidence_by_id = {e["id"]: e for e in rig.get("evidence", [])}

    # Test count per component
    test_count: dict[str, int] = {}
    for t in rig.get("test_definitions", []):
        for cid in t.get("components_being_tested_ids", []):
            test_count[cid] = test_count.get(cid, 0) + 1

    # Assign C4 identifiers
    c4_ids: dict[str, str] = {}  # comp-id → c4-ident
    for c in components:
        c4_ids[c["id"]] = unique_ident(c["name"], used_idents)

    lines: list[str] = []

    # ── Header comment ────────────────────────────────────────────────
    lines.append("// LikeC4 C4 model — deterministically generated from rig.json.")
    lines.append(f"// Project: {repo.get('name', '?')} | Build: {repo.get('build_system', '?')} | "
                 f"Generated: {repo.get('generated_at', '?')[:19]}")
    lines.append(f"// {len(components)} components, "
                 f"{sum(len(c.get('depends_on_ids', [])) for c in components)} edges, "
                 f"{len(rig.get('entrypoints', []))} entrypoints, "
                 f"{len(rig.get('test_definitions', []))} test definitions.")
    lines.append("// Every element is derived from the RIG — nothing invented.")
    lines.append("")

    # ── Specification ─────────────────────────────────────────────────
    lines.append("specification {")
    lines.append("  element softwareSystem")
    lines.append("  element container")
    lines.append("  element component")
    lines.append("  relationship imports")
    lines.append("}")
    lines.append("")
    lines.append("model {")

    # ── System ────────────────────────────────────────────────────────
    sys_name = sanitize(repo.get("name", "system"))
    if sys_name[0].islower():
        sys_ident = sys_name[0].upper() + sys_name[1:]
    else:
        sys_ident = sys_name
    sys_ident = unique_ident(sys_ident, used_idents)

    entry_names = [name_by_id.get(eid, eid) for eid in rig.get("entrypoints", [])]
    runner_names = [f"{' '.join(r.get('arguments', []))}" for r in rig.get("runners", [])]

    sys_desc = (
        f"{repo.get('name', 'Project')}. "
        f"{len(components)} build-target components "
        f"({sum(1 for c in components if c['type'] == 'executable')} executables, "
        f"{sum(1 for c in components if 'library' in c['type'])} libraries). "
        f"Entrypoints: {', '.join(entry_names) if entry_names else 'none'}. "
    )
    if runner_names:
        sys_desc += f"Test runner: {', '.join(runner_names)}. "
    if rig.get("external_packages"):
        sys_desc += f"{len(rig['external_packages'])} external packages. "
    n_tests = len(rig.get("test_definitions", []))
    n_tested = len(tested_ids)
    sys_desc += f"{n_tests} test definitions covering {n_tested}/{len(components)} components."
    sys_desc = sanitize_for_c4(sys_desc)

    lines.append(f"  {sys_ident} = softwareSystem '{repo.get('name', 'System')}' {{")
    lines.append(f"    description '{sys_desc}'")
    lines.append("")

    # ── Containers (one per RIG component) ────────────────────────────
    for c in components:
        c4_id = c4_ids[c["id"]]
        c_name = c["name"]
        c_type = c.get("type", "unknown")
        c_lang = c.get("programming_language", "unknown")
        srcs = c.get("source_files", [])
        deps = c.get("depends_on_ids", [])
        dep_names = [name_by_id.get(d, d) for d in deps]
        is_entry = c["id"] in set(rig.get("entrypoints", []))
        has_tests = c["id"] in tested_ids
        n_tests_comp = test_count.get(c["id"], 0)
        artifacts = c.get("artifacts", [])

        # Build container description from RIG data
        parts = [f"RIG {c['id']}: {c_type}"]
        parts.append(f"({c_lang})")
        parts.append(f"{len(srcs)} source file{'s' if len(srcs) != 1 else ''}")
        if is_entry:
            parts.append("entrypoint")
        if has_tests:
            parts.append(f"{n_tests_comp} test{'s' if n_tests_comp != 1 else ''}")
        else:
            parts.append("no tests")
        if dep_names:
            parts.append(f"depends on: {', '.join(dep_names)}")
        if artifacts:
            art_paths = [a.get("relative_path", a.get("name", "")) for a in artifacts]
            parts.append(f"artifact: {', '.join(art_paths)}")
        container_desc = sanitize_for_c4(". ".join(parts) + ".")

        lines.append(f"    // RIG {c['id']}: {c_name} ({c_type}, {c_lang})")
        lines.append(f"    {c4_id} = container '{c_name}' {{")
        lines.append(f"      description '{container_desc}'")

        # Nested components: source files with doc comments or exports
        files_rendered = 0
        files_without_anything = 0
        for sf in srcs:
            sf_path = source_dir / sf if source_dir else Path(sf)
            comment = ""
            exports: list[str] = []
            if source_dir and sf_path.exists():
                comment = extract_doc_comment(sf_path, c_lang)
                exports = extract_exports(sf_path, c_lang)

            # Render the file as a C4 component if it has a doc comment OR exports.
            # Exports-only files are valuable: they show the API surface even when
            # the developer wrote no top-of-file doc comment.
            if comment or exports:
                sf_ident = unique_ident(Path(sf).stem, used_idents)
                lines.append(f"")
                lines.append(f"      // {sf}")
                if exports:
                    lines.append(f"      // Exports: {', '.join(exports)}")
                lines.append(f"      {sf_ident} = component '{Path(sf).name}' {{")
                if comment:
                    lines.append(f"        description '{sanitize_for_c4(truncate(comment))}'")
                else:
                    lines.append(f"        description 'No doc comment. Exports: {', '.join(exports)}'")
                lines.append(f"      }}")
                files_rendered += 1
            else:
                files_without_anything += 1

        if files_without_anything:
            lines.append(f"      // {files_without_anything} file(s) without doc comments or exports: "
                         + ", ".join(Path(sf).name for sf in srcs if not (
                             source_dir and (source_dir / sf).exists() and
                             (extract_doc_comment(source_dir / sf, c_lang)
                              or extract_exports(source_dir / sf, c_lang))
                         ))[:200])

        lines.append("    }")
        lines.append("")

    lines.append("  }")
    lines.append("")

    # ── Relationships ─────────────────────────────────────────────────
    lines.append("  // Relationships — from RIG depends_on_ids")
    for c in components:
        for dep_id in c.get("depends_on_ids", []):
            if dep_id in c4_ids:
                src = c4_ids[c["id"]]
                tgt = c4_ids[dep_id]
                lines.append(f"  {src} -> {tgt} 'imports'")

    lines.append("}")
    lines.append("")

    # ── Views ─────────────────────────────────────────────────────────
    lines.append("views {")
    all_container_ids = [c4_ids[c["id"]] for c in components]

    # Context view
    lines.append(f"  view context of {sys_ident} {{")
    lines.append(f"    title '{repo.get('name', 'System')} — System Context'")
    lines.append("    include *")
    lines.append("  }")
    lines.append("")

    # Container view
    lines.append(f"  view containers of {sys_ident} {{")
    lines.append(f"    title '{repo.get('name', 'System')} — Containers'")
    lines.append("    include *")
    lines.append("  }")
    lines.append("")

    # Component views for containers with nested components
    for c in components:
        c4_id = c4_ids[c["id"]]
        srcs = c.get("source_files", [])
        # Only create a component view if there are nested elements
        # (files with doc comments or exports).
        has_nested = False
        if source_dir:
            for sf in srcs:
                sf_path = source_dir / sf
                lang = c.get("programming_language", "")
                if sf_path.exists() and (
                    extract_doc_comment(sf_path, lang) or extract_exports(sf_path, lang)
                ):
                    has_nested = True
                    break
        if has_nested and len(srcs) <= 30:
            view_name = sanitize(c["name"])
            view_ident = unique_ident(f"view_{view_name}", used_idents)
            lines.append(f"  view {view_ident} of {c4_id} {{")
            lines.append(f"    title '{c['name']} — Components'")
            lines.append("    include *")
            lines.append("  }")
            lines.append("")

    lines.append("}")
    lines.append("")

    return "\n".join(lines)


# ── Entry point ───────────────────────────────────────────────────────

def main() -> None:
    parser = argparse.ArgumentParser(
        description="Deterministic RIG → LikeC4 model generator."
    )
    parser.add_argument("rig_json", help="Path to rig.json")
    parser.add_argument(
        "--source-dir", "-s",
        help="Path to the source repo (for extracting code comments). "
             "If omitted, uses the current directory.",
        default=None,
    )
    parser.add_argument(
        "--output", "-o",
        help="Output file (default: stdout)",
        default=None,
    )
    args = parser.parse_args()

    rig_path = Path(args.rig_json)
    if not rig_path.exists():
        print(f"Error: {rig_path} not found", file=sys.stderr)
        sys.exit(1)

    rig = json.loads(rig_path.read_text())

    source_dir = Path(args.source_dir) if args.source_dir else Path.cwd()

    # Check if source files are actually accessible
    test_files = [
        source_dir / sf
        for c in rig.get("components", [])[:3]
        for sf in c.get("source_files", [])[:1]
    ]
    if not any(f.exists() for f in test_files):
        # Source files not accessible — disable comment extraction
        source_dir = None

    c4_text = generate_c4(rig, source_dir)

    if args.output:
        Path(args.output).write_text(c4_text)
        print(f"[rig-to-c4] wrote {args.output} ({len(c4_text)} bytes)", file=sys.stderr)
    else:
        print(c4_text)


if __name__ == "__main__":
    main()
