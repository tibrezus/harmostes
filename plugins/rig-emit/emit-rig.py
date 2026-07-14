#!/usr/bin/env python3
"""
emit-rig.py — Universal Repository Intelligence Graph generator.

Follows the RIG standard (arXiv:2601.10112): components are BUILD TARGETS
(executables, libraries), source files are listed within them. Dependencies
are build-level (linking), not code-level (imports).

Auto-detects all build systems in a repository, runs the appropriate
extractor for each, and merges results into a single RIG JSON. Validates
that every source file in the repo appears in at least one component.

Supported build systems:
  go-modules    go.mod           → go list -json
  zig-build     build.zig        → static analysis + @import tracing
  cargo         Cargo.toml       → manifest parsing + src/ scan
  npm           package.json     → workspace/bin detection + src/ scan
  pip           pyproject.toml   → package discovery + scan
  cmake         CMakeLists.txt   → add_executable/add_library + scan
  generic       (fallback)       → directory scan, group by language

Usage: emit-rig.py <output.json> [--language hint] [--no-validate]
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path, PurePosixPath


# ── Helpers ────────────────────────────────────────────────────────

ID_COUNTER = [0]
PKG_COUNTER = [0]
EVIDENCE_COUNTER = [0]
TEST_COUNTER = [0]


def next_id() -> str:
    ID_COUNTER[0] += 1
    return f"comp-{ID_COUNTER[0]}"


def next_pkg_id() -> str:
    PKG_COUNTER[0] += 1
    return f"pkg-{PKG_COUNTER[0]}"


def next_evidence_id() -> str:
    EVIDENCE_COUNTER[0] += 1
    return f"evidence-{EVIDENCE_COUNTER[0]}"


def next_test_id() -> str:
    TEST_COUNTER[0] += 1
    return f"test-{TEST_COUNTER[0]}"


def git_ref() -> str:
    if not Path(".git").exists():
        return ""
    try:
        r = subprocess.run(
            ["git", "rev-parse", "--short", "HEAD"],
            capture_output=True, text=True, timeout=5,
        )
        return r.stdout.strip()
    except Exception:
        return ""


# Source file extensions grouped by language family
SOURCE_EXTENSIONS = {
    ".zig": "zig",
    ".go": "go",
    ".rs": "rust",
    ".py": "python",
    ".ts": "typescript",
    ".tsx": "typescript",
    ".js": "javascript",
    ".jsx": "javascript",
    ".mjs": "javascript",
    ".cjs": "javascript",
    ".c": "c",
    ".h": "c",
    ".cpp": "cpp",
    ".cc": "cpp",
    ".cxx": "cpp",
    ".hpp": "cpp",
    ".hxx": "cpp",
    ".cu": "cuda",
    ".cuh": "cuda",
    ".java": "java",
    ".kt": "kotlin",
    ".swift": "swift",
    ".rb": "ruby",
    ".lua": "lua",
    ".sh": "shell",
    ".bash": "shell",
}

# Directories to always exclude
EXCLUDE_DIRS = {
    ".git", "node_modules", ".zig-cache", "zig-out", "zig_cache",
    "__pycache__", ".pytest_cache", "vendor", ".venv", "venv",
    "dist", "build", "target", ".next", ".nuxt", ".output",
    ".cache", ".turbo", "coverage", ".coverage",
}


def find_source_files(root: Path | None = None) -> dict[str, list[Path]]:
    """Find all source files, grouped by language. Returns {lang: [paths]}."""
    root = root or Path(".")
    by_lang: dict[str, list[Path]] = {}
    for p in root.rglob("*"):
        if not p.is_file():
            continue
        # Skip excluded directories
        parts = set(p.parts)
        if parts & EXCLUDE_DIRS:
            continue
        ext = p.suffix.lower()
        lang = SOURCE_EXTENSIONS.get(ext)
        if lang:
            by_lang.setdefault(lang, []).append(p)
    return by_lang


def all_source_paths() -> set[str]:
    """Return all source file paths (relative, forward-slash) in the repo."""
    paths = set()
    for lang, files in find_source_files().items():
        for f in files:
            paths.add(str(f).replace("\\", "/"))
    return paths


# ── Zig extractor ──────────────────────────────────────────────────

def trace_zig_imports(root_file: Path, seen: set[str] | None = None) -> set[str]:
    """Recursively trace @import("*.zig") from a root file. Returns all file paths."""
    if seen is None:
        seen = set()
    rel = str(root_file).replace("\\", "/")
    if rel in seen:
        return seen
    seen.add(rel)
    try:
        content = root_file.read_text(encoding="utf-8", errors="replace")
    except Exception:
        return seen
    for m in re.finditer(r'@import\s*\(\s*"([^"]+)"\s*\)', content):
        target = m.group(1)
        if not target.endswith(".zig"):
            continue
        resolved = (root_file.parent / target).resolve()
        try:
            resolved = resolved.relative_to(Path.cwd())
        except ValueError:
            continue
        trace_zig_imports(resolved, seen)
    return seen


def extract_zig() -> tuple[list[dict], list[dict], list[str], list[dict]]:
    """Extract Zig build targets. Returns (components, external_packages, entrypoints, aggregators)."""
    build_zig_path = Path("build.zig")
    if not build_zig_path.exists():
        return [], [], [], []

    build_zig = build_zig_path.read_text(encoding="utf-8", errors="replace")
    build_zon = ""
    if Path("build.zig.zon").exists():
        build_zon = Path("build.zig.zon").read_text(encoding="utf-8", errors="replace")

    components: list[dict] = []
    entrypoints: list[str] = []
    name_to_id: dict[str, str] = {}
    var_to_name: dict[str, str] = {}
    # Executable name → component id, kept SEPARATE from name_to_id so a module
    # sharing an executable's name (e.g. addModule("rhesadox") alongside
    # addExecutable(.{.name="rhesadox"})) cannot shadow it during edge resolution.
    exe_id_by_name: dict[str, str] = {}

    # ── Phase 1: Build variable → source_file map ──
    # Track ALL createModule/addModule calls so we can resolve .root_module refs
    var_to_source: dict[str, str] = {}  # variable name → root source file

    # createModule(.{ .root_source_file = b.path("..."), ... })
    for m in re.finditer(
        r'(?:const|var)\s+(\w+)\s*=\s*\S*createModule\s*\(\s*\.\{\s*\.root_source_file\s*=\s*b\.path\("([^"]+)"\)',
        build_zig, re.DOTALL,
    ):
        var_to_source[m.group(1)] = m.group(2)

    # addModule("name", .{ .root_source_file = b.path("..."), ... })
    for m in re.finditer(
        r'(?:const|var)\s+(\w+)\s*=\s*b\.addModule\s*\(\s*"?\.?(\w+)"?,?\s*\.\{\s*\.root_source_file\s*=\s*b\.path\("([^"]+)"\)',
        build_zig, re.DOTALL,
    ):
        var_to_source[m.group(1)] = m.group(3)
        var_to_name[m.group(1)] = m.group(2)

    # addModule(.{ .name = .name, .root_source_file = b.path("...") })
    for m in re.finditer(
        r'(?:const|var)\s+(\w+)\s*=\s*b\.addModule\s*\(\s*\.\{\s*\.name\s*=\s*\.(\w+).*?\.root_source_file\s*=\s*b\.path\("([^"]+)"\)',
        build_zig, re.DOTALL,
    ):
        var_to_source[m.group(1)] = m.group(3)
        var_to_name[m.group(1)] = m.group(2)

    # ── Phase 2: Executables ──
    # Pattern A: addExecutable(.{ .name = "...", .root_source_file = b.path("...") })
    for m in re.finditer(
        r'addExecutable\s*\(\s*\.\{\s*\.name\s*=\s*"([^"]+)"',
        build_zig, re.DOTALL,
    ):
        name = m.group(1)
        if name in name_to_id:
            continue
        cid = next_id()
        name_to_id[name] = cid
        exe_id_by_name[name] = cid
        # Extract the struct body (up to the first });) to avoid
        # matching fields from the NEXT addExecutable/createModule call
        raw = build_zig[m.end():m.end() + 500]
        struct_end = raw.find("})")
        after = raw[:struct_end] if struct_end != -1 else raw[:200]

        # Try .root_module = <var> FIRST (newer Zig API), then .root_source_file
        mod_match = re.search(r'\.root_module\s*=\s*(\w+)', after)
        root_file_str = None
        if mod_match:
            root_file_str = var_to_source.get(mod_match.group(1))
        if not root_file_str:
            root_match = re.search(r'\.root_source_file\s*=\s*b\.path\("([^"]+)"\)', after)
            if root_match:
                root_file_str = root_match.group(1)

        root_file = Path(root_file_str) if root_file_str else None
        source_files = sorted(trace_zig_imports(root_file)) if root_file else ([root_file_str] if root_file_str else [])

        components.append({
            "id": cid, "name": name, "type": "executable",
            "programming_language": "zig",
            "source_files": source_files,
            "external_packages_ids": [], "depends_on_ids": [],
        })
        entrypoints.append(cid)

    # Track exe variable assignments
    for m in re.finditer(
        r'(?:const|var)\s+(\w+)\s*=\s*\S*addExecutable\s*\(\s*\.\{\s*\.name\s*=\s*"([^"]+)"',
        build_zig, re.DOTALL,
    ):
        var_to_name[m.group(1)] = m.group(2)

    # ── Phase 3: Modules / libraries ──
    # addModule("name", .{ ... })
    processed_mod_names = set()
    for m in re.finditer(r'addModule\s*\(\s*"([^"]+)"', build_zig):
        name = m.group(1)
        if name in processed_mod_names:
            continue
        processed_mod_names.add(name)
        cid = next_id()
        # Use name_module to avoid collision with executable names in name_to_id
        name_to_id[f"__module__{name}"] = cid
        raw = build_zig[m.end():m.end() + 500]
        struct_end = raw.find("})")
        after = raw[:struct_end] if struct_end != -1 else raw[:200]
        root_match = re.search(r'\.root_source_file\s*=\s*b\.path\("([^"]+)"\)', after)
        root_file_str = root_match.group(1) if root_match else None
        root_file = Path(root_file_str) if root_file_str else None
        source_files = sorted(trace_zig_imports(root_file)) if root_file else []
        components.append({
            "id": cid, "name": name, "type": "package_library",
            "programming_language": "zig",
            "source_files": source_files,
            "external_packages_ids": [], "depends_on_ids": [],
        })

    # addModule(.{ .name = .name })
    for m in re.finditer(r'addModule\s*\(\s*\.\{\s*\.name\s*=\s*\.(\w+)', build_zig, re.DOTALL):
        name = m.group(1)
        if name in processed_mod_names:
            continue
        cid = next_id()
        name_to_id[name] = cid
        raw = build_zig[m.end():m.end() + 500]
        struct_end = raw.find("})")
        after = raw[:struct_end] if struct_end != -1 else raw[:200]
        root_match = re.search(r'\.root_source_file\s*=\s*b\.path\("([^"]+)"\)', after)
        root_file_str = root_match.group(1) if root_match else None
        root_file = Path(root_file_str) if root_file_str else None
        source_files = sorted(trace_zig_imports(root_file)) if root_file else []
        components.append({
            "id": cid, "name": name, "type": "package_library",
            "programming_language": "zig",
            "source_files": source_files,
            "external_packages_ids": [], "depends_on_ids": [],
        })

    # ── Phase 4: Resolve build-level dependencies (addImport) ──
    # Build a name → id map that handles both executables and modules
    # Executables are keyed by name; modules are keyed by __module__<name>
    def resolve_id(name: str) -> str | None:
        # Try module first, then executable
        return name_to_id.get(f"__module__{name}") or name_to_id.get(name)

    # Map createModule/root_module variables to component IDs so that
    # addImport calls on intermediate module vars resolve correctly.
    # Pattern: const exe = b.addExecutable(.{ .root_module = exe_mod })
    # → exe_mod should resolve to the executable's component ID.
    mod_var_to_comp_id: dict[str, str] = {}
    for m in re.finditer(
        r'(?:const|var)\s+(\w+)\s*=\s*b\.addExecutable\s*\(\s*\.\{[^}]*\.root_module\s*=\s*(\w+)',
        build_zig, re.DOTALL,
    ):
        exe_var = m.group(1)
        mod_var = m.group(2)
        exe_name = var_to_name.get(exe_var)
        if exe_name:
            # Resolve to the EXECUTABLE component, never a same-named module.
            # resolve_id() is module-first; using it here mapped exe_mod → the
            # "rhesadox" module (comp-6) instead of the executable (comp-1),
            # turning exe_mod.addImport("rhesadox", engine_mod) into a self-loop
            # (comp-6→comp-6) that was dropped — losing the exe→lib edge.
            cid = exe_id_by_name.get(exe_name) or name_to_id.get(exe_name)
            if cid:
                mod_var_to_comp_id[mod_var] = cid

    for regex in [
        re.compile(r'(\w+)\.addImport\s*\(\s*"[^"]+"\s*,\s*(\w+)\s*\)'),
        re.compile(r'(\w+)\.root_module\.addImport\s*\(\s*"[^"]+"\s*,\s*(\w+)\s*\)'),
    ]:
        for m in regex.finditer(build_zig):
            consumer_var = m.group(1)
            provider_var = m.group(2)
            # Resolve consumer: try mod_var_to_comp_id, then var_to_name
            consumer_name = var_to_name.get(consumer_var)
            provider_name = var_to_name.get(provider_var)
            # If consumer_var is a createModule var, resolve via mod_var_to_comp_id
            cid = mod_var_to_comp_id.get(consumer_var)
            if not cid and consumer_name:
                cid = resolve_id(consumer_name)
            if provider_name:
                pid = resolve_id(provider_name)
            else:
                pid = mod_var_to_comp_id.get(provider_var)
            if cid and pid and cid != pid:
                for c in components:
                    if c["id"] == cid and pid not in c["depends_on_ids"]:
                        c["depends_on_ids"].append(pid)
                        break

    # ── Phase 4b: Source-level module @import cross-check ──────────────────
    # Zig source expresses inter-module deps as @import("module-name") — a BARE
    # name, not a .zig path. build.zig addImport (Phase 4) is the authority but
    # regex-fragile; scanning each component's source for @import("<module>")
    # recovers edges the build regex missed (e.g. the name-collision self-loop
    # above) and makes the dependency code-level/evidence-backed. This is the
    # finer @import granularity: build-level linking (Phase 4) ∪ source-level
    # module imports (here), reconciled onto the build-target component graph.
    module_name_to_id: dict[str, str] = {
        mk[len("__module__"):]: mv
        for mk, mv in name_to_id.items() if mk.startswith("__module__")
    }
    if module_name_to_id:
        for c in components:
            if c.get("programming_language") != "zig":
                continue
            for sf in c.get("source_files", []):
                try:
                    txt = Path(sf).read_text(encoding="utf-8", errors="replace")
                except Exception:
                    continue
                for im in re.finditer(r'@import\s*\(\s*"([^"]+)"\s*\)', txt):
                    target = im.group(1)
                    if target.endswith(".zig"):
                        continue  # file import (intra-component closure)
                    pid = module_name_to_id.get(target)
                    if pid and pid != c["id"] and pid not in c["depends_on_ids"]:
                        c["depends_on_ids"].append(pid)

    # ── Phase 5: External packages from build.zig.zon ──
    external_packages = []
    if build_zon:
        deps_match = re.search(r'\.dependencies\s*=\s*\.\{', build_zon)
        if deps_match:
            block = extract_brace_block(build_zon, deps_match.end())
            for dm in re.finditer(r'\.(\w+)\s*=\s*\.\{', block):
                dep_name = dm.group(1)
                external_packages.append({
                    "id": next_pkg_id(), "name": dep_name,
                    "package_manager": {"name": "zig-modules", "package_name": dep_name},
                })

    return components, external_packages, entrypoints, []


def exe_var_lookup(var: str, build_zig: str) -> str | None:
    """Look up exe name from variable assignment."""
    m = re.search(
        rf'(?:const|var)\s+{re.escape(var)}\s*=\s*\S*addExecutable\s*\(\s*\.\{{\s*\.name\s*=\s*"([^"]+)"',
        build_zig, re.DOTALL,
    )
    return m.group(1) if m else None


def components_lookup(components: list[dict], cid: str, field: str) -> list:
    for c in components:
        if c["id"] == cid:
            return c.get(field, [])
    return []


def extract_brace_block(text: str, start: int) -> str:
    depth = 1
    pos = start
    while pos < len(text) and depth > 0:
        if text[pos] == '{':
            depth += 1
        elif text[pos] == '}':
            depth -= 1
        pos += 1
    return text[start:pos - 1]


# ── Go extractor ───────────────────────────────────────────────────

def extract_go() -> tuple[list[dict], list[dict], list[str], list[dict]]:
    if not Path("go.mod").exists():
        return [], [], [], []

    # Download dependencies first (needed for go list to resolve imports)
    try:
        env = dict(os.environ, GOFLAGS="-mod=mod", GOWORK="off")
        subprocess.run(
            ["go", "mod", "download"],
            capture_output=True, text=True, timeout=300, env=env,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as e:
        print(f"[emit-rig] go mod download warning: {e}", file=sys.stderr)
        pass  # continue anyway — go list -e handles missing deps

    try:
        env = dict(os.environ, GOFLAGS="-mod=mod", GOWORK="off")
        r = subprocess.run(
            ["go", "list", "-e", "-json", "./..."],
            capture_output=True, text=True, timeout=180, env=env,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as e:
        print(f"[emit-rig] go list failed: {e}", file=sys.stderr)
        # Fallback: -find discovers packages without resolving imports.
        # This gives us components (but no dependencies) when the module
        # cache is cold. Better than returning nothing.
        try:
            env = dict(os.environ, GOFLAGS="-mod=mod", GOWORK="off")
            r = subprocess.run(
                ["go", "list", "-e", "-json", "-find", "./..."],
                capture_output=True, text=True, timeout=120, env=env,
            )
            print("[emit-rig] go list -find fallback used (no dep resolution)", file=sys.stderr)
        except (FileNotFoundError, subprocess.TimeoutExpired) as e:
            print(f"[emit-rig] go list -find also failed: {e}", file=sys.stderr)
            return [], [], [], []

    if not r.stdout.strip():
        if r.stderr.strip():
            print(f"[emit-rig] go list stderr: {r.stderr[:500]}", file=sys.stderr)
        return [], [], [], []

    # Parse concatenated JSON objects
    decoder = json.JSONDecoder()
    packages = []
    text = r.stdout.strip()
    idx = 0
    while idx < len(text):
        s = text[idx:].lstrip()
        if not s:
            break
        obj, consumed = decoder.raw_decode(s)
        packages.append(obj)
        idx = len(text) - len(s) + consumed

    module_path = ""
    if packages:
        module_path = packages[0].get("Module", {}).get("Path", "")

    components = []
    comp_map = {}
    ext_map = {}
    entrypoints = []

    for pkg in packages:
        import_path = pkg.get("ImportPath", "")
        if not import_path:
            continue
        name = import_path.split("/")[-1]
        is_main = pkg.get("Name") == "main"
        cid = next_id()
        comp_map[import_path] = cid

        go_files = pkg.get("GoFiles", []) or []
        dir_path = pkg.get("Dir", "")
        src_files = [
            os.path.join(dir_path, f).replace(os.getcwd() + "/", "").replace("\\", "/")
            for f in go_files
        ]

        imports = pkg.get("Imports", []) or []
        ext_refs = []
        for imp in imports:
            if module_path and imp.startswith(module_path):
                continue
            first = imp.split("/")[0]
            if "." not in first:
                continue
            if imp not in ext_map:
                ext_map[imp] = next_pkg_id()
            ext_refs.append(ext_map[imp])

        components.append({
            "id": cid, "name": name,
            "type": "executable" if is_main else "package_library",
            "programming_language": "go",
            "source_files": src_files,
            "external_packages_ids": ext_refs,
            "depends_on_ids": [],
            "_import_path": import_path,  # temporary, for test linking
        })
        if is_main:
            entrypoints.append(cid)

    # Internal deps
    for pkg in packages:
        src = pkg.get("ImportPath", "")
        if src not in comp_map:
            continue
        for imp in pkg.get("Imports", []) or []:
            if module_path and imp.startswith(module_path) and imp != src and imp in comp_map:
                for c in components:
                    if c["id"] == comp_map[src]:
                        if comp_map[imp] not in c["depends_on_ids"]:
                            c["depends_on_ids"].append(comp_map[imp])

    external_packages = [{
        "id": eid, "name": name,
        "package_manager": {"name": "go-modules", "package_name": name},
    } for name, eid in sorted(ext_map.items())]

    return components, external_packages, entrypoints, []


# ── Cargo (Rust) extractor ─────────────────────────────────────────

def extract_cargo() -> tuple[list[dict], list[dict], list[str], list[dict]]:
    toml_path = Path("Cargo.toml")
    if not toml_path.exists():
        return [], [], [], []

    content = toml_path.read_text(encoding="utf-8", errors="replace")
    components = []
    entrypoints = []
    ext_map = {}

    # Project name
    name_match = re.search(r'^\[package\].*?name\s*=\s*"([^"]+)"', content, re.DOTALL | re.MULTILINE)
    project_name = name_match.group(1) if name_match else "rust-project"

    # [lib] section
    lib_match = re.search(r'\[lib\].*?(?=\n\[|\Z)', content, re.DOTALL)
    if lib_match:
        cid = next_id()
        src_dir = Path("src")
        source_files = sorted(str(f) for f in src_dir.rglob("*.rs")) if src_dir.exists() else []
        components.append({
            "id": cid, "name": project_name, "type": "package_library",
            "programming_language": "rust",
            "source_files": source_files,
            "external_packages_ids": [], "depends_on_ids": [],
        })

    # [[bin]] sections
    for m in re.finditer(r'\[\[bin\]\].*?(?=\n\[|\Z)', content, re.DOTALL):
        bin_name_m = re.search(r'name\s*=\s*"([^"]+)"', m.group())
        bin_path_m = re.search(r'path\s*=\s*"([^"]+)"', m.group())
        bin_name = bin_name_m.group(1) if bin_name_m else "bin"
        root_file = bin_path_m.group(1) if bin_path_m else "src/main.rs"
        cid = next_id()
        components.append({
            "id": cid, "name": bin_name, "type": "executable",
            "programming_language": "rust",
            "source_files": [root_file],
            "external_packages_ids": [], "depends_on_ids": [],
        })
        entrypoints.append(cid)

    # External deps from [dependencies]
    deps_match = re.search(r'\[dependencies\](.*?)(?=\n\[|\Z)', content, re.DOTALL)
    if deps_match:
        for dm in re.finditer(r'^(\w[\w-]*)\s*=', deps_match.group(1), re.MULTILINE):
            dep = dm.group(1)
            ext_map[dep] = next_pkg_id()

    external_packages = [{
        "id": eid, "name": name,
        "package_manager": {"name": "cargo", "package_name": name},
    } for name, eid in sorted(ext_map.items())]

    return components, external_packages, entrypoints, []


# ── npm/TypeScript extractor ───────────────────────────────────────

def extract_npm() -> tuple[list[dict], list[dict], list[str], list[dict]]:
    pkg_path = Path("package.json")
    if not pkg_path.exists():
        return [], [], [], []

    try:
        pkg = json.loads(pkg_path.read_text())
    except json.JSONDecodeError:
        return [], [], [], []

    name = pkg.get("name", "npm-package")
    components = []
    entrypoints = []

    # Collect source files
    src_dirs = [d for d in ["src", "lib", "app"] if Path(d).exists()]
    source_files = []
    for d in src_dirs:
        for ext in (".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"):
            source_files.extend(str(f) for f in Path(d).rglob(f"*{ext}"))

    cid = next_id()
    components.append({
        "id": cid, "name": name, "type": "package_library",
        "programming_language": "typescript" if Path("tsconfig.json").exists() else "javascript",
        "source_files": sorted(set(source_files)),
        "external_packages_ids": [], "depends_on_ids": [],
    })

    # Binaries
    bins = pkg.get("bin", {})
    if isinstance(bins, str):
        bins = {name: bins}
    for bin_name, bin_path in bins.items():
        bid = next_id()
        components.append({
            "id": bid, "name": bin_name, "type": "executable",
            "programming_language": "javascript",
            "source_files": [bin_path],
            "external_packages_ids": [], "depends_on_ids": [cid],
        })
        entrypoints.append(bid)

    # Workspaces
    workspaces = pkg.get("workspaces", [])
    if isinstance(workspaces, list):
        for ws_pattern in workspaces:
            for ws_dir in Path(".").glob(ws_pattern):
                ws_pkg = ws_dir / "package.json"
                if ws_pkg.exists():
                    try:
                        ws_data = json.loads(ws_pkg.read_text())
                        ws_name = ws_data.get("name", ws_dir.name)
                        ws_src = sorted(
                            str(f) for f in ws_dir.rglob("*.ts")
                        ) | sorted(str(f) for f in ws_dir.rglob("*.js"))
                        wid = next_id()
                        components.append({
                            "id": wid, "name": ws_name, "type": "package_library",
                            "programming_language": "typescript",
                            "source_files": sorted(set(ws_src)),
                            "external_packages_ids": [], "depends_on_ids": [],
                        })
                    except json.JSONDecodeError:
                        pass

    # External deps
    ext_map = {}
    for dep_section in ["dependencies", "devDependencies", "peerDependencies"]:
        for dep_name in (pkg.get(dep_section) or {}).keys():
            if dep_name not in ext_map:
                ext_map[dep_name] = next_pkg_id()

    external_packages = [{
        "id": eid, "name": name,
        "package_manager": {"name": "npm", "package_name": name},
    } for name, eid in sorted(ext_map.items())]

    return components, external_packages, entrypoints, []


# ── Python extractor ───────────────────────────────────────────────

def extract_python() -> tuple[list[dict], list[dict], list[str], list[dict]]:
    has_project = Path("pyproject.toml").exists() or Path("setup.py").exists() or Path("setup.cfg").exists()
    if not has_project:
        # Check for .py files
        py_files = list(Path(".").rglob("*.py"))
        py_files = [f for f in py_files if not any(d in f.parts for d in EXCLUDE_DIRS)]
        if len(py_files) < 3:
            return [], [], [], []
    else:
        py_files = [f for f in Path(".").rglob("*.py") if not any(d in f.parts for d in EXCLUDE_DIRS)]

    if not py_files:
        return [], [], [], []

    # Determine package name
    project_name = "python-project"
    if Path("pyproject.toml").exists():
        content = Path("pyproject.toml").read_text(errors="replace")
        m = re.search(r'name\s*=\s*"([^"]+)"', content)
        if m:
            project_name = m.group(1)
    elif Path("setup.py").exists():
        content = Path("setup.py").read_text(errors="replace")
        m = re.search(r'name\s*=\s*[\'"]([^\'"]+)[\'"]', content)
        if m:
            project_name = m.group(1)

    source_files = sorted(str(f).replace("\\", "/") for f in py_files)

    # Detect executables (scripts with shebang or __main__.py)
    entrypoints = []
    main_files = [f for f in py_files if f.name == "__main__.py" or f.name == "cli.py" or f.name == "main.py"]
    if main_files:
        for mf in main_files:
            mid = next_id()
            # Find the package directory
            pkg_dir = mf.parent
            pkg_files = sorted(str(f).replace("\\", "/") for f in pkg_dir.rglob("*.py"))
            exe_name = pkg_dir.name
            # Insert executable before library
            pass  # handled below

    cid = next_id()
    components = [{
        "id": cid, "name": project_name, "type": "package_library",
        "programming_language": "python",
        "source_files": source_files,
        "external_packages_ids": [], "depends_on_ids": [],
    }]

    # Add executable for __main__.py
    for mf in main_files:
        mid = next_id()
        components.append({
            "id": mid, "name": mf.parent.name, "type": "executable",
            "programming_language": "python",
            "source_files": [str(mf).replace("\\", "/")],
            "external_packages_ids": [], "depends_on_ids": [cid],
        })
        entrypoints.append(mid)

    # External deps
    ext_map = {}
    for req_file in ["requirements.txt", "requirements-dev.txt"]:
        if Path(req_file).exists():
            for line in Path(req_file).read_text(errors="replace").splitlines():
                line = line.strip()
                if line and not line.startswith("#"):
                    pkg_name = re.split(r"[>=<\[!]", line)[0].strip()
                    if pkg_name and pkg_name not in ext_map:
                        ext_map[pkg_name] = next_pkg_id()

    if Path("pyproject.toml").exists():
        content = Path("pyproject.toml").read_text(errors="replace")
        deps_match = re.search(r'\[project\.optional-dependencies\]|dependencies\s*=\s*\[', content)
        if deps_match:
            for dm in re.finditer(r'"([^"]+)"', content[deps_match.start():]):
                pkg_name = re.split(r"[>=<\[!]", dm.group(1))[0].strip()
                if pkg_name and pkg_name not in ext_map:
                    ext_map[pkg_name] = next_pkg_id()

    external_packages = [{
        "id": eid, "name": name,
        "package_manager": {"name": "pip", "package_name": name},
    } for name, eid in sorted(ext_map.items())]

    return components, external_packages, entrypoints, []


# ── CMake / C / C++ / CUDA extractor ───────────────────────────────

def extract_cmake() -> tuple[list[dict], list[dict], list[str], list[dict]]:
    cml_path = Path("CMakeLists.txt")
    if not cml_path.exists():
        # Check for standalone C/C++/CUDA without CMake
        return extract_standalone_c()

    content = cml_path.read_text(encoding="utf-8", errors="replace")
    components = []
    entrypoints = []

    project_match = re.search(r'project\s*\(\s*(\w+)', content, re.IGNORECASE)
    project_name = project_match.group(1) if project_match else "cmake-project"

    # add_executable
    for m in re.finditer(r'add_executable\s*\(\s*(\w+)\s+([^)]+)\)', content, re.IGNORECASE):
        name = m.group(1)
        sources_raw = m.group(2)
        source_files = [
            s.strip().strip('"') for s in re.split(r'\s+', sources_raw)
            if s.strip() and s.strip().endswith((".c", ".cpp", ".cc", ".cxx", ".cu"))
        ]
        cid = next_id()
        components.append({
            "id": cid, "name": name, "type": "executable",
            "programming_language": _detect_lang_from_files(source_files),
            "source_files": source_files,
            "external_packages_ids": [], "depends_on_ids": [],
        })
        entrypoints.append(cid)

    # add_library
    for m in re.finditer(r'add_library\s*\(\s*(\w+)\s+(?:STATIC|SHARED|MODULE|INTERFACE)?\s*([^)]+)\)', content, re.IGNORECASE):
        name = m.group(1)
        sources_raw = m.group(2)
        source_files = [
            s.strip().strip('"') for s in re.split(r'\s+', sources_raw)
            if s.strip() and s.strip().endswith((".c", ".cpp", ".cc", ".cxx", ".cu", ".h", ".hpp"))
        ]
        lib_type = "static_library"  # default
        if "SHARED" in m.group(0).upper():
            lib_type = "shared_library"
        elif "INTERFACE" in m.group(0).upper():
            lib_type = "unknown"
        cid = next_id()
        components.append({
            "id": cid, "name": name, "type": lib_type,
            "programming_language": _detect_lang_from_files(source_files),
            "source_files": source_files,
            "external_packages_ids": [], "depends_on_ids": [],
        })

    # target_link_libraries (dependencies)
    for m in re.finditer(r'target_link_libraries\s*\(\s*(\w+)[^)]*?(\w+)\s*\)', content, re.IGNORECASE):
        consumer = m.group(1)
        provider = m.group(2)
        consumer_ids = [c["id"] for c in components if c["name"] == consumer]
        provider_ids = [c["id"] for c in components if c["name"] == provider]
        if consumer_ids and provider_ids:
            for c in components:
                if c["id"] == consumer_ids[0] and provider_ids[0] not in c["depends_on_ids"]:
                    c["depends_on_ids"].append(provider_ids[0])

    # External packages (find_package)
    ext_map = {}
    for m in re.finditer(r'find_package\s*\(\s*(\w+)', content, re.IGNORECASE):
        dep = m.group(1)
        if dep not in ext_map:
            ext_map[dep] = next_pkg_id()

    external_packages = [{
        "id": eid, "name": name,
        "package_manager": {"name": "cmake", "package_name": name},
    } for name, eid in sorted(ext_map.items())]

    return components, external_packages, entrypoints, []


def extract_standalone_c() -> tuple[list[dict], list[dict], list[str], list[dict]]:
    """Extract C/C++/CUDA sources when no build system is present."""
    by_lang = find_source_files()
    components = []
    entrypoints = []

    for lang in ("c", "cpp", "cuda"):
        files = by_lang.get(lang, [])
        if not files:
            continue
        source_files = sorted(str(f).replace("\\", "/") for f in files)
        cid = next_id()
        components.append({
            "id": cid, "name": f"{lang}-sources", "type": "static_library",
            "programming_language": lang,
            "source_files": source_files,
            "external_packages_ids": [], "depends_on_ids": [],
        })

    return components, [], entrypoints, []


def _detect_lang_from_files(files: list[str]) -> str:
    for f in files:
        if f.endswith(".cu"):
            return "cuda"
        if f.endswith((".cpp", ".cc", ".cxx")):
            return "cpp"
    return "c"


# ── Generic fallback extractor ─────────────────────────────────────

def extract_generic() -> tuple[list[dict], list[dict], list[str], list[dict]]:
    """Group all source files by language into one component per language."""
    by_lang = find_source_files()
    components = []
    entrypoints = []

    for lang, files in sorted(by_lang.items()):
        source_files = sorted(str(f).replace("\\", "/") for f in files)
        cid = next_id()
        comp_type = "executable" if lang in ("shell",) else "package_library"
        components.append({
            "id": cid, "name": f"{lang}-sources", "type": comp_type,
            "programming_language": lang,
            "source_files": source_files,
            "external_packages_ids": [], "depends_on_ids": [],
        })
        if comp_type == "executable":
            entrypoints.append(cid)

    return components, [], entrypoints, []


# ── Build system detection ─────────────────────────────────────────

BUILD_SYSTEM_MARKERS = [
    ("go-modules", lambda: Path("go.mod").exists(), extract_go),
    ("zig-build", lambda: Path("build.zig").exists(), extract_zig),
    ("cargo", lambda: Path("Cargo.toml").exists(), extract_cargo),
    ("npm", lambda: Path("package.json").exists(), extract_npm),
    ("pip", lambda: Path("pyproject.toml").exists() or Path("setup.py").exists(), extract_python),
    ("cmake", lambda: Path("CMakeLists.txt").exists(), extract_cmake),
]


def detect_build_systems() -> list[tuple[str, callable]]:
    """Return list of (name, extractor_fn) for all detected build systems."""
    detected = []
    for name, check, extractor in BUILD_SYSTEM_MARKERS:
        if check():
            detected.append((name, extractor))
    return detected


# ── Zig+CUDA+C co-extraction ───────────────────────────────────────

def extract_zig_with_native() -> tuple[list[dict], list[dict], list[str], list[dict]]:
    """
    Zig projects often include CUDA (.cu) and C (.c/.h) files managed by the
    Zig build system. This extracts the Zig build targets, then adds separate
    components for CUDA and C sources, and links them via build-level deps.
    """
    components, ext_pkgs, entrypoints, aggregators = extract_zig()
    if not components:
        return components, ext_pkgs, entrypoints, aggregators

    by_lang = find_source_files()

    # CUDA sources
    cuda_files = sorted(str(f).replace("\\", "/") for f in by_lang.get("cuda", []))
    cuda_id = None
    if cuda_files:
        cuda_id = next_id()
        components.append({
            "id": cuda_id, "name": "cuda-backend", "type": "shared_library",
            "programming_language": "cuda",
            "source_files": cuda_files,
            "external_packages_ids": [], "depends_on_ids": [],
        })

    # C sources
    c_files = sorted(str(f).replace("\\", "/") for f in by_lang.get("c", []))
    c_id = None
    if c_files:
        c_id = next_id()
        components.append({
            "id": c_id, "name": "c-kernels", "type": "static_library",
            "programming_language": "c",
            "source_files": c_files,
            "external_packages_ids": [], "depends_on_ids": [],
        })

    # ── Native edges: build.zig-driven, evidence-backed (replaces filename heuristic) ──
    # build.zig is the authority for native linking. An edge consumer→native exists
    # when the consumer (a) addObjectFile()s an object whose compilation step
    # compiles a .c (→ c-kernels) or .cu (→ cuda-backend) source, or (b) linkSystem-
    # Library()s a CUDA runtime lib (→ cuda-backend). The old heuristic linked every
    # library to c-kernels and guessed cuda from a filename — it over-linked (a lib
    # with "cuda" in a filename) and missed executables' real native links.
    if (c_id or cuda_id) and Path("build.zig").exists():
        bz = Path("build.zig").read_text(encoding="utf-8", errors="replace")
        _line = lambda off: f"build.zig:{bz.count(chr(10), 0, off) + 1}"

        # var → component id, type-discriminated so an executable + module sharing
        # a name (rhesadox: exe "rhesadox" + module "rhesadox") resolve correctly.
        mod_name_id = {c["name"]: c["id"] for c in components
                       if c["type"] == "package_library" and c.get("programming_language") == "zig"}
        exe_name_id = {c["name"]: c["id"] for c in components if c["type"] == "executable"}
        var_to_comp: dict[str, str] = {}
        for m in re.finditer(r'(?:const|var)\s+(\w+)\s*=\s*b\.addModule\s*\(\s*"([^"]+)"', bz):
            if m.group(2) in mod_name_id:
                var_to_comp[m.group(1)] = mod_name_id[m.group(2)]
        for m in re.finditer(r'(?:const|var)\s+(\w+)\s*=\s*b\.addExecutable\s*\(\s*\.\{\s*\.name\s*=\s*"([^"]+)"', bz, re.DOTALL):
            if m.group(2) in exe_name_id:
                var_to_comp[m.group(1)] = exe_name_id[m.group(2)]

        # step_var → native source files (.c/.cu) it compiles
        step_srcs: dict[str, list[str]] = {}
        for m in re.finditer(r'(?:const|var)\s+(\w+)\s*=\s*b\.addSystemCommand\s*\(\s*&?\.\{(.*?)\}\s*\)', bz, re.DOTALL):
            for cm in re.finditer(r'"-c",\s*"([^"]+)"', m.group(2)):       # cc -c <file>
                step_srcs.setdefault(m.group(1), []).append(cm.group(1))
        for m in re.finditer(r'(\w+)\.addFileInput\s*\(\s*\.\{\s*\.cwd_relative\s*=\s*"([^"]+)"', bz):
            step_srcs.setdefault(m.group(1), []).append(m.group(2))

        # obj_var → step_var (const obj = step.addOutputFileArg(...))
        obj_step: dict[str, str] = {}
        for m in re.finditer(r'(?:const|var)\s+(\w+)\s*=\s*(\w+)\.addOutputFileArg\(', bz):
            obj_step[m.group(1)] = m.group(2)

        def _edge(cvar: str, nid: str, lines: list[str]) -> None:
            cid = var_to_comp.get(cvar)
            if not (cid and nid and nid != cid):
                return
            for c in components:
                if c["id"] == cid:
                    if nid not in c["depends_on_ids"]:
                        c["depends_on_ids"].append(nid)
                        c.setdefault("_native_link_evidence", []).append({"target": nid, "lines": lines})
                    return

        # (a) addObjectFile(obj) → obj→step→source(.c→c-kernels / .cu→cuda-backend)
        for m in re.finditer(r'(\w+)(?:\.root_module)?\.addObjectFile\s*\(\s*(\w+)\s*\)', bz):
            cvar, ovar, ln = m.group(1), m.group(2), _line(m.start())
            for src in step_srcs.get(obj_step.get(ovar, ""), []):
                nid = cuda_id if src.endswith(".cu") else c_id if src.endswith(".c") else None
                if nid:
                    _edge(cvar, nid, [ln])

        # (b) linkSystemLibrary("cudart"/"cublas"/"cuda*") → cuda-backend
        if cuda_id:
            for m in re.finditer(r'(\w+)(?:\.root_module)?\.linkSystemLibrary\s*\(\s*"(cuda[^"]*)"', bz):
                _edge(m.group(1), cuda_id, [_line(m.start())])

    return components, ext_pkgs, entrypoints, aggregators


# ── Evidence generation (paper: arXiv:2601.10112) ─────────────────

def extract_zig_tests(components: list[dict]) -> tuple[list[dict], list[dict]]:
    """Extract Zig test definitions by scanning source files for `test \"name\"` blocks.

    Zig tests are inline blocks (not separate files), so we scan every
    component's source files for `test \"...\" {` patterns.

    Returns (test_definitions, evidence).
    """
    test_defs: list[dict] = []
    evidence: list[dict] = []
    ev_cache: dict[str, str] = {}

    def get_ev(file_ref: str) -> str:
        if file_ref in ev_cache:
            return ev_cache[file_ref]
        eid = next_evidence_id()
        evidence.append({"id": eid, "line": [file_ref]})
        ev_cache[file_ref] = eid
        return eid

    test_pattern = re.compile(r'^\s*test\s+"([^"]+)"', re.MULTILINE)

    for comp in components:
        for sf in comp.get("source_files", []):
            try:
                content = Path(sf).read_text(encoding="utf-8", errors="replace")
            except Exception:
                continue
            matches = test_pattern.findall(content)
            if not matches:
                continue
            for i, test_name in enumerate(matches):
                ev_id = get_ev(f"{sf}:1")
                test_defs.append({
                    "id": next_test_id(),
                    "name": f"test_{test_name}",
                    "covers_ids": [comp["id"]],
                    "depends_on_ids": [comp["id"]],
                    "source_files": [sf],
                    "evidence_ids": [ev_id],
                })

    return test_defs, evidence


def _detect_build_file() -> str | None:
    """Find the primary build system file, if any."""
    for f in ("go.mod", "build.zig", "Cargo.toml", "package.json", "pyproject.toml", "CMakeLists.txt"):
        if Path(f).exists():
            return f
    return None


def generate_evidence(components: list[dict]) -> list[dict]:
    """Generate evidence entries for every component.

    Evidence = file:line references proving the component is defined by the
    build system (paper core requirement). Each component gets:
      1. A reference to the build system file (go.mod, build.zig, etc.)
      2. A reference to its first source file at line 1
    """
    evidence: list[dict] = []
    ev_cache: dict[str, str] = {}  # file_ref → evidence_id

    def get_or_create(file_ref: str) -> str:
        if file_ref in ev_cache:
            return ev_cache[file_ref]
        eid = next_evidence_id()
        evidence.append({"id": eid, "line": [file_ref]})
        ev_cache[file_ref] = eid
        return eid

    build_file = _detect_build_file()
    build_ev_id = get_or_create(f"{build_file}:1") if build_file else None

    for comp in components:
        ev_ids = []
        if build_ev_id:
            ev_ids.append(build_ev_id)

        # Source file evidence (first file defines the component)
        source_files = comp.get("source_files", [])
        if source_files:
            ev_ids.append(get_or_create(f"{source_files[0]}:1"))

        if ev_ids:
            comp["evidence_ids"] = ev_ids

    return evidence


# ── Go test extraction ─────────────────────────────────────────────

def extract_go_tests() -> tuple[list[dict], list[dict]]:
    """Extract test definitions from Go packages.

    Uses `go list -json` TestGoFiles to discover test packages, then creates
    test_definitions linking each test to the component it covers.

    Returns (test_definitions, evidence).
    """
    if not Path("go.mod").exists():
        return [], []

    # Re-run go list to get TestGoFiles (the main extractor may have discarded this)
    try:
        env = dict(os.environ, GOFLAGS="-mod=mod", GOWORK="off")
        r = subprocess.run(
            ["go", "list", "-e", "-json", "./..."],
            capture_output=True, text=True, timeout=60, env=env,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return [], []

    if not r.stdout.strip():
        return [], []

    # Parse concatenated JSON
    decoder = json.JSONDecoder()
    packages = []
    text = r.stdout.strip()
    idx = 0
    while idx < len(text):
        s = text[idx:].lstrip()
        if not s:
            break
        obj, consumed = decoder.raw_decode(s)
        packages.append(obj)
        idx = len(text) - len(s) + consumed

    # Build import_path → comp_id map from existing components
    comp_by_importpath = {}
    for comp in []:  # Will be populated by caller
        pass

    test_defs: list[dict] = []
    evidence: list[dict] = []
    ev_cache: dict[str, str] = {}

    def get_ev(file_ref: str) -> str:
        if file_ref in ev_cache:
            return ev_cache[file_ref]
        eid = next_evidence_id()
        evidence.append({"id": eid, "line": [file_ref]})
        ev_cache[file_ref] = eid
        return eid

    module_path = ""
    if packages:
        module_path = packages[0].get("Module", {}).get("Path", "")

    for pkg in packages:
        test_files = pkg.get("TestGoFiles", []) or []
        xtest_files = pkg.get("XTestGoFiles", []) or []
        all_test_files = test_files + xtest_files
        if not all_test_files:
            continue

        import_path = pkg.get("ImportPath", "")
        pkg_name = import_path.split("/")[-1] if import_path else "unknown"
        dir_path = pkg.get("Dir", "")
        cwd = os.getcwd()

        test_src_files = [
            os.path.join(dir_path, f).replace(cwd + "/", "").replace("\\", "/")
            for f in all_test_files
        ]

        # Evidence: first test file
        ev_id = get_ev(f"{test_src_files[0]}:1") if test_src_files else None

        test_defs.append({
            "id": next_test_id(),
            "name": f"test_{pkg_name}",
            "covers_ids": [],  # will be linked post-hoc by import path
            "depends_on_ids": [],
            "source_files": test_src_files,
            "evidence_ids": [ev_id] if ev_id else [],
            "_import_path": import_path,  # temporary, for linking
        })

    return test_defs, evidence


# ── Merge multiple extractors ──────────────────────────────────────

def merge_results(
    results: list[tuple[list[dict], list[dict], list[str], list[dict]]],
) -> tuple[list[dict], list[dict], list[str], list[dict]]:
    """Merge multiple extractor results into one."""
    all_components = []
    all_ext_pkgs = []
    all_entrypoints = []
    all_aggregators = []

    for components, ext_pkgs, entrypoints, aggregators in results:
        all_components.extend(components)
        all_ext_pkgs.extend(ext_pkgs)
        all_entrypoints.extend(entrypoints)
        all_aggregators.extend(aggregators)

    return all_components, all_ext_pkgs, all_entrypoints, all_aggregators


# ── Completeness validation ────────────────────────────────────────

# Files that are build configs, not source components — excluded from validation
BUILD_CONFIG_FILES = {
    "build.zig", "build.zig.zon", "go.mod", "go.sum", "Cargo.toml",
    "Cargo.lock", "package.json", "package-lock.json", "yarn.lock",
    "pnpm-lock.yaml", "pyproject.toml", "setup.py", "setup.cfg",
    "requirements.txt", "CMakeLists.txt", "Makefile", "Dockerfile",
    "tsconfig.json", "webpack.config.js", "vite.config.ts",
}


def validate_completeness(components: list[dict]) -> list[str]:
    """Check that every source file in the repo appears in at least one component.

    Build config files (build.zig, Cargo.toml, etc.) and tooling scripts
    (*.sh, *.py in tools/) are excluded — they're not build targets.
    """
    repo_files = all_source_paths()
    rig_files = set()
    for c in components:
        for sf in c.get("source_files", []):
            rig_files.add(sf.replace("\\", "/"))

    missing = []
    for f in sorted(repo_files - rig_files):
        basename = Path(f).name
        # Skip build config files
        if basename in BUILD_CONFIG_FILES:
            continue
        # Skip test files (they're in test_definitions, not components)
        if "_test.go" in basename or basename.endswith((
            "_test.py", "_test.rs", ".test.ts", ".test.tsx",
            ".spec.ts", ".spec.tsx", ".test.js", ".spec.js",
        )):
            continue
        # Skip tooling scripts (not build targets)
        if f.endswith((".sh",)) and ("tools/" in f or "scripts/" in f):
            continue
        # Skip standalone Python tooling scripts (not part of a Python package)
        if f.endswith(".py") and ("tools/" in f or "scripts/" in f):
            continue
        missing.append(f"MISSING: {f} exists in repo but not in any component")
    return missing


# ── Main ───────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(description="Universal RIG generator")
    parser.add_argument("output", help="Output JSON file path")
    parser.add_argument("--language", default=None, help="Language hint (auto-detected if omitted)")
    parser.add_argument("--no-validate", action="store_true", help="Skip completeness validation")
    args = parser.parse_args()

    # Detect build systems
    systems = detect_build_systems()

    if not systems:
        # Fallback: generic file scan
        systems = [("generic", extract_generic)]

    # Special handling: if Zig build system is detected, also extract native C/CUDA
    extractors = []
    for name, extractor in systems:
        if name == "zig-build":
            extractors.append(("zig-build+native", extract_zig_with_native))
        else:
            extractors.append((name, extractor))

    # If only generic, don't run language-specific extractors
    if len(extractors) == 1 and extractors[0][0] == "generic":
        pass
    else:
        # Remove generic if we have specific extractors
        extractors = [(n, e) for n, e in extractors if n != "generic"]

    print(f"[emit-rig] Detected build systems: {', '.join(n for n, _ in extractors)}", file=sys.stderr)

    # Run all extractors
    results = []
    for name, extractor in extractors:
        result = extractor()
        results.append(result)
        n_comp = len(result[0])
        n_files = sum(len(c.get("source_files", [])) for c in result[0])
        print(f"[emit-rig]   {name}: {n_comp} components, {n_files} source files", file=sys.stderr)

    # Merge
    components, external_packages, entrypoints, aggregators = merge_results(results)

    # Generate evidence for all components (paper: arXiv:2601.10112)
    evidence = generate_evidence(components)

    # Extract Go test definitions (if Go is present)
    test_definitions: list[dict] = []
    go_test_evidence: list[dict] = []
    if any(c.get("programming_language") == "go" for c in components):
        test_definitions, go_test_evidence = extract_go_tests()
        # Link tests to components by import path
        comp_by_import = {}
        for c in components:
            ip = c.pop("_import_path", None)
            if ip:
                comp_by_import[ip] = c["id"]
        for t in test_definitions:
            ip = t.pop("_import_path", None)
            if ip and ip in comp_by_import:
                t["covers_ids"] = [comp_by_import[ip]]
                t["depends_on_ids"] = [comp_by_import[ip]]
    else:
        # Clean up temporary fields from non-Go components
        for c in components:
            c.pop("_import_path", None)

    evidence.extend(go_test_evidence)

    # Extract Zig test definitions (if Zig is present)
    if any(c.get("programming_language") == "zig" for c in components):
        zig_tests, zig_ev = extract_zig_tests(components)
        test_definitions.extend(zig_tests)
        evidence.extend(zig_ev)
        # Zig aggregators (meta-targets)
        zig_build_ev = next_evidence_id()
        evidence.append({"id": zig_build_ev, "line": ["build.zig:1"]})
        all_exec_ids = [c["id"] for c in components if c["type"] == "executable"]
        if all_exec_ids:
            aggregators.append({
                "id": f"agg-{len(aggregators) + 1}",
                "name": "zig-build",
                "depends_on_ids": all_exec_ids,
                "evidence_ids": [zig_build_ev],
            })
        all_test_ids = [t["id"] for t in zig_tests]
        if all_test_ids:
            zig_test_ev = next_evidence_id()
            evidence.append({"id": zig_test_ev, "line": ["build.zig:1"]})
            aggregators.append({
                "id": f"agg-{len(aggregators) + 1}",
                "name": "zig-build-test",
                "depends_on_ids": all_test_ids,
                "evidence_ids": [zig_test_ev],
            })
        # Convert native-link build.zig line evidence stashed by
        # extract_zig_with_native (addObjectFile / linkSystemLibrary traces) into
        # real evidence entries on the consumer component.
        for c in components:
            for ev in c.pop("_native_link_evidence", []):
                eid = next_evidence_id()
                evidence.append({"id": eid, "line": ev["lines"]})
                c.setdefault("evidence_ids", []).append(eid)

    # Add Go aggregators (paper: meta-targets that orchestrate other targets)
    # Go doesn't have explicit aggregators, but `go build ./...` and `go test ./...`
    # are implicit meta-targets. We synthesize them.
    if any(c.get("programming_language") == "go" for c in components):
        build_ev = next_evidence_id()
        evidence.append({"id": build_ev, "line": ["go.mod:1"]})
        all_exec_ids = [c["id"] for c in components if c["type"] == "executable"]
        if all_exec_ids:
            aggregators.append({
                "id": f"agg-{len(aggregators) + 1}",
                "name": "go-build-all",
                "depends_on_ids": all_exec_ids,
                "evidence_ids": [build_ev],
            })
        all_test_ids = [t["id"] for t in test_definitions]
        if all_test_ids:
            test_ev = next_evidence_id()
            evidence.append({"id": test_ev, "line": ["go.mod:1"]})
            aggregators.append({
                "id": f"agg-{len(aggregators) + 1}",
                "name": "go-test-all",
                "depends_on_ids": all_test_ids,
                "evidence_ids": [test_ev],
            })

    # Determine primary language and build system
    lang_counts: dict[str, int] = {}
    for c in components:
        lang = c.get("programming_language", "unknown")
        lang_counts[lang] = lang_counts.get(lang, 0) + 1
    primary_lang = max(lang_counts, key=lang_counts.get) if lang_counts else "unknown"
    build_systems_str = "+".join(n for n, _ in extractors)

    # Validate completeness
    if not args.no_validate:
        missing = validate_completeness(components)
        if missing:
            print(f"[emit-rig] WARNING: {len(missing)} source file(s) not in any component:", file=sys.stderr)
            for m in missing[:10]:
                print(f"  {m}", file=sys.stderr)
            if len(missing) > 10:
                print(f"  ... and {len(missing) - 10} more", file=sys.stderr)

    # Build RIG
    rig = {
        "schema_version": "rig-1.0",
        "repository": {
            "name": Path(".").resolve().name,
            "ref": git_ref(),
            "language": primary_lang,
            "build_system": build_systems_str,
            "generator": "tibrezus/llm-wiki/.github/actions/repo-map@v1",
        },
        "evidence": evidence,
        "components": components,
        "aggregators": aggregators,
        "runners": [],
        "test_definitions": test_definitions,
        "external_packages": external_packages,
        "entrypoints": entrypoints,
    }

    with open(args.output, "w") as f:
        json.dump(rig, f, indent=2)

    total_edges = sum(len(c.get("depends_on_ids", [])) for c in components)
    print(
        f"[emit-rig] RIG: {len(components)} components, "
        f"{total_edges} dependency edges, "
        f"{len(external_packages)} external packages, "
        f"{len(entrypoints)} entrypoints, "
        f"{len(evidence)} evidence, "
        f"{len(test_definitions)} test definitions",
        file=sys.stderr,
    )


if __name__ == "__main__":
    main()
