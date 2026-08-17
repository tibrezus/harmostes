#!/usr/bin/env python3
"""rig-to-c4.py — Deterministic RIG → LikeC4 model generator.

Reads a rig.db (canonical) or rig.json (legacy) and produces a model.c4
where every element is derived from the RIG. Source files that carry a
top-of-file doc comment (Zig //!, Go //, C /* */, Python docstrings) get
the comment extracted verbatim into the C4 component description — no LLM,
no hallucination, fully reproducible.

When reading rig.db, file docs and exported symbols are served from the DB
(precomputed at emit time); when reading rig.json they are extracted from
source on the fly. Both paths share ONE implementation (rig/symbols.py), so
model.c4 is byte-identical either way — the golden-parity test enforces it.

Usage:
    rig-to-c4.py <rig.db | rig.json> [--source-dir <path>] [-o <output.c4>]

If --source-dir is given (or source files exist in the CWD), doc comments
are extracted from the actual source files. Otherwise, descriptions are
generated from RIG metadata only (name, type, language, source count, test
status).
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from rig import db as rig_db
from rig.symbols import extract_doc_comment, extract_exports


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

def generate_c4(rig: dict, source_dir: Path | None,
                file_data: dict[str, tuple[str, list[str]]] | None = None) -> str:
    """Generate the full LikeC4 model text from a RIG.

    ``file_data`` (path → (doc, exports)) serves precomputed per-file data
    from rig.db. When omitted and ``source_dir`` is set, extraction runs
    on the fly via rig/symbols.py — the SAME implementation, so output is
    identical either way.
    """
    def _lookup(sf: str, lang: str) -> tuple[str, list[str]]:
        if file_data is not None:
            return file_data.get(sf, ("", []))
        sf_path = source_dir / sf if source_dir else Path(sf)
        if source_dir and sf_path.exists():
            return (extract_doc_comment(sf_path, lang),
                    extract_exports(sf_path, lang))
        return ("", [])

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
    # No timestamp: model.c4 must be byte-deterministic for identical input
    # (diff-free regeneration, hash-based skips).
    lines.append("// LikeC4 C4 model — deterministically generated from the RIG.")
    lines.append(f"// Project: {repo.get('name', '?')} | Build: {repo.get('build_system', '?')}")
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
            comment, exports = _lookup(sf, c_lang)

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
                         + ", ".join(Path(sf).name for sf in srcs if not _lookup(sf, c.get("programming_language", ""))[0]
                                     and not _lookup(sf, c.get("programming_language", ""))[1])[:200])

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

    # Structure view — the complete component graph (all build targets +
    # their dependencies). For a build-system RIG there is no real
    # context/container hierarchy — all components are peers — so a single
    # overview view is more useful than two identical context/container views.
    lines.append(f"  view structure of {sys_ident} {{")
    lines.append(f"    title '{repo.get('name', 'System')} — Repository Structure'")
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
        if file_data is not None:
            for sf in srcs:
                doc, exports = file_data.get(sf, ("", []))
                if doc or exports:
                    has_nested = True
                    break
        elif source_dir:
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
    parser.add_argument("rig_input", help="Path to rig.db (canonical) or rig.json (legacy)")
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

    rig_path = Path(args.rig_input)
    if not rig_path.exists():
        print(f"Error: {rig_path} not found", file=sys.stderr)
        sys.exit(1)

    rig = rig_db.load_rig(rig_path)

    file_data: dict[str, tuple[str, list[str]]] | None = None
    if rig_path.suffix == ".db":
        # Serve docs + exports from the DB (precomputed at emit time).
        import sqlite3
        con = sqlite3.connect(rig_path)
        try:
            file_data = {
                r[0]: (r[1] or "", r[2].split(", ") if r[2] else [])
                for r in con.execute(
                    "SELECT f.path, f.doc, ("
                    "  SELECT GROUP_CONCAT(s.signature, ', ' ORDER BY s.seq) "
                    "  FROM symbols s WHERE s.file = f.path) FROM files f")
        }
        finally:
            con.close()

    source_dir = Path(args.source_dir) if args.source_dir else Path.cwd()

    # Check if source files are actually accessible (only matters when we
    # are NOT serving from the DB)
    if file_data is None:
        test_files = [
            source_dir / sf
            for c in rig.get("components", [])[:3]
            for sf in c.get("source_files", [])[:1]
        ]
        if not any(f.exists() for f in test_files):
            # Source files not accessible — disable comment extraction
            source_dir = None

    c4_text = generate_c4(rig, source_dir, file_data)

    if args.output:
        Path(args.output).write_text(c4_text)
        print(f"[rig-to-c4] wrote {args.output} ({len(c4_text)} bytes)", file=sys.stderr)
    else:
        print(c4_text)


if __name__ == "__main__":
    main()
