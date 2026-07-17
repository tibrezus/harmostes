"""Zig extractor — static analysis of build.zig + native C/CUDA co-extraction.

Mirrors the sophisticated build.zig parsing from the original emit-rig.py:
- Phase 1: createModule/addModule variable → source_file mapping
- Phase 2: addExecutable discovery
- Phase 3: addModule discovery
- Phase 4: addImport dependency resolution (build-level edges)
- Phase 4b: source-level @import("module") cross-check
- Phase 5: build.zig.zon external packages
- Native: CUDA/C sources + build.zig-driven native-link tracing

Key improvements over the old monolith:
- Evidence uses actual build.zig line numbers (not just :1)
- Test definitions carry test_framework="zig test"
- Runner node emitted for "zig build test"
- Component artifacts populated (output path)
"""

from __future__ import annotations

import re
from pathlib import Path

from ..builder import RIGBuilder, find_source_files
from ..model import Aggregator, Artifact, Component, Runner, TestDefinition
from .base import Extractor


def _line_of(text: str, offset: int) -> int:
    return text.count("\n", 0, offset) + 1


def trace_zig_imports(root_file: Path, seen: set[str] | None = None) -> set[str]:
    """Recursively trace @import("*.zig") from a root file."""
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


class ZigExtractor(Extractor):
    name = "zig-build"
    build_file = "build.zig"

    @staticmethod
    def detects() -> bool:
        return Path("build.zig").exists()

    def extract(self, builder: RIGBuilder) -> None:
        build_zig_path = Path("build.zig")
        if not build_zig_path.exists():
            return
        bz = build_zig_path.read_text(encoding="utf-8", errors="replace")
        build_zon = ""
        if Path("build.zig.zon").exists():
            build_zon = Path("build.zig.zon").read_text(encoding="utf-8", errors="replace")

        # ── Phase 1: createModule/addModule var → source ──────────────
        var_to_source: dict[str, str] = {}
        var_to_name: dict[str, str] = {}

        for m in re.finditer(
            r'(?:const|var)\s+(\w+)\s*=\s*\S*createModule\s*\(\s*\.\{\s*\.root_source_file\s*=\s*b\.path\("([^"]+)"\)',
            bz, re.DOTALL,
        ):
            var_to_source[m.group(1)] = m.group(2)

        for m in re.finditer(
            r'(?:const|var)\s+(\w+)\s*=\s*b\.addModule\s*\(\s*"?\.?(\w+)"?,?\s*\.\{\s*\.root_source_file\s*=\s*b\.path\("([^"]+)"\)',
            bz, re.DOTALL,
        ):
            var_to_source[m.group(1)] = m.group(3)
            var_to_name[m.group(1)] = m.group(2)

        for m in re.finditer(
            r'(?:const|var)\s+(\w+)\s*=\s*b\.addModule\s*\(\s*\.\{\s*\.name\s*=\s*\.(\w+).*?\.root_source_file\s*=\s*b\.path\("([^"]+)"\)',
            bz, re.DOTALL,
        ):
            var_to_source[m.group(1)] = m.group(3)
            var_to_name[m.group(1)] = m.group(2)

        # ── Phase 2: Executables ──────────────────────────────────────
        exe_id_by_name: dict[str, str] = {}
        for m in re.finditer(r'addExecutable\s*\(\s*\.\{\s*\.name\s*=\s*"([^"]+)"', bz, re.DOTALL):
            name = m.group(1)
            if name in [c.name for c in builder.components if c.type == "executable"]:
                continue
            ln = _line_of(bz, m.start())
            raw = bz[m.end():m.end() + 500]
            struct_end = raw.find("})")
            after = raw[:struct_end] if struct_end != -1 else raw[:200]

            mod_match = re.search(r'\.root_module\s*=\s*(\w+)', after)
            root_file_str = None
            if mod_match:
                root_file_str = var_to_source.get(mod_match.group(1))
            if not root_file_str:
                root_match = re.search(r'\.root_source_file\s*=\s*b\.path\("([^"]+)"\)', after)
                if root_match:
                    root_file_str = root_match.group(1)

            root_file = Path(root_file_str) if root_file_str else None
            source_files = sorted(trace_zig_imports(root_file)) if root_file else (
                [root_file_str] if root_file_str else [])

            ev = builder.evidence(f"build.zig:{ln}")
            comp = Component(
                name=name, type="executable", programming_language="zig",
                source_files=source_files, is_entrypoint=True,
                artifacts=[Artifact(name=name, relative_path=f"zig-out/bin/{name}")],
                evidence=[ev],
            )
            if source_files:
                comp.evidence.append(builder.evidence(f"{source_files[0]}:1"))
            builder.add_component(comp)
            exe_id_by_name[name] = comp.id
            # Track var → name for dep resolution
            var_to_name_raw = re.search(
                r'(?:const|var)\s+(\w+)\s*=\s*\S*addExecutable\s*\(\s*\.\{\s*\.name\s*=\s*"([^"]+)"',
                bz[:m.start() + 100], re.DOTALL)
            if var_to_name_raw:
                var_to_name[var_to_name_raw.group(1)] = name

        for m in re.finditer(
            r'(?:const|var)\s+(\w+)\s*=\s*\S*addExecutable\s*\(\s*\.\{\s*\.name\s*=\s*"([^"]+)"',
            bz, re.DOTALL,
        ):
            var_to_name[m.group(1)] = m.group(2)

        # ── Phase 3: Modules ──────────────────────────────────────────
        module_names: set[str] = set()
        for m in re.finditer(r'addModule\s*\(\s*"([^"]+)"', bz):
            name = m.group(1)
            if name in module_names:
                continue
            module_names.add(name)
            ln = _line_of(bz, m.start())
            raw = bz[m.end():m.end() + 500]
            struct_end = raw.find("})")
            after = raw[:struct_end] if struct_end != -1 else raw[:200]
            root_match = re.search(r'\.root_source_file\s*=\s*b\.path\("([^"]+)"\)', after)
            root_file_str = root_match.group(1) if root_match else None
            root_file = Path(root_file_str) if root_file_str else None
            source_files = sorted(trace_zig_imports(root_file)) if root_file else []
            ev = builder.evidence(f"build.zig:{ln}")
            comp = Component(
                name=name, type="package_library", programming_language="zig",
                source_files=source_files, evidence=[ev],
            )
            if source_files:
                comp.evidence.append(builder.evidence(f"{source_files[0]}:1"))
            builder.add_component(comp)

        for m in re.finditer(r'addModule\s*\(\s*\.\{\s*\.name\s*=\s*\.(\w+)', bz, re.DOTALL):
            name = m.group(1)
            if name in module_names:
                continue
            module_names.add(name)
            ln = _line_of(bz, m.start())
            raw = bz[m.end():m.end() + 500]
            struct_end = raw.find("})")
            after = raw[:struct_end] if struct_end != -1 else raw[:200]
            root_match = re.search(r'\.root_source_file\s*=\s*b\.path\("([^"]+)"\)', after)
            root_file_str = root_match.group(1) if root_match else None
            root_file = Path(root_file_str) if root_file_str else None
            source_files = sorted(trace_zig_imports(root_file)) if root_file else []
            ev = builder.evidence(f"build.zig:{ln}")
            comp = Component(
                name=name, type="package_library", programming_language="zig",
                source_files=source_files, evidence=[ev],
            )
            if source_files:
                comp.evidence.append(builder.evidence(f"{source_files[0]}:1"))
            builder.add_component(comp)

        # ── Phase 4: Build-level dependencies (addImport) ─────────────
        mod_var_to_comp_id: dict[str, str] = {}
        for m in re.finditer(
            r'(?:const|var)\s+(\w+)\s*=\s*b\.addExecutable\s*\(\s*\.\{[^}]*\.root_module\s*=\s*(\w+)',
            bz, re.DOTALL,
        ):
            exe_var, mod_var = m.group(1), m.group(2)
            exe_name = var_to_name.get(exe_var)
            if exe_name:
                cid = exe_id_by_name.get(exe_name)
                if cid:
                    mod_var_to_comp_id[mod_var] = cid

        module_name_set = module_names

        def resolve_id(name: str) -> str | None:
            return builder.resolve(name)

        for regex in [
            re.compile(r'(\w+)\.addImport\s*\(\s*"[^"]+"\s*,\s*(\w+)\s*\)'),
            re.compile(r'(\w+)\.root_module\.addImport\s*\(\s*"[^"]+"\s*,\s*(\w+)\s*\)'),
        ]:
            for m in regex.finditer(bz):
                consumer_var, provider_var = m.group(1), m.group(2)
                consumer_name = var_to_name.get(consumer_var)
                provider_name = var_to_name.get(provider_var)

                cid = mod_var_to_comp_id.get(consumer_var)
                if not cid and consumer_name:
                    cid = resolve_id(consumer_name)
                if provider_name:
                    pid = resolve_id(provider_name)
                else:
                    pid = mod_var_to_comp_id.get(provider_var)
                if cid and pid and cid != pid:
                    comp = next((c for c in builder.components if c.id == cid), None)
                    if comp:
                        comp.depends_on.add(next(
                            (c.name for c in builder.components if c.id == pid), ""))

        # ── Phase 4b: Source-level @import cross-check ────────────────
        if module_name_set:
            for c in builder.components:
                if c.programming_language != "zig":
                    continue
                for sf in c.source_files:
                    try:
                        txt = Path(sf).read_text(encoding="utf-8", errors="replace")
                    except Exception:
                        continue
                    for im in re.finditer(r'@import\s*\(\s*"([^"]+)"\s*\)', txt):
                        target = im.group(1)
                        if target.endswith(".zig"):
                            continue
                        if target in module_name_set:
                            comp = next((cc for cc in builder.components if cc.name == target), None)
                            if comp and comp.id != c.id:
                                c.depends_on.add(target)

        # ── Phase 5: External packages (build.zig.zon) ────────────────
        if build_zon:
            deps_match = re.search(r'\.dependencies\s*=\s*\.\{', build_zon)
            if deps_match:
                block = self._brace_block(build_zon, deps_match.end())
                for dm in re.finditer(r'\.(\w+)\s*=\s*\.\{', block):
                    dep_name = dm.group(1)
                    builder.package(dep_name, "zig-modules", dep_name)
                    # Link to all Zig components (conservative — they may use the dep)
                    for c in builder.components:
                        if c.programming_language == "zig":
                            c.external_packages.add(dep_name)

        # ── Native C/CUDA co-extraction ───────────────────────────────
        self._extract_native(builder, bz)

        # ── Tests ─────────────────────────────────────────────────────
        self._extract_tests(builder)

        # ── Aggregators + Runner ──────────────────────────────────────
        self._emit_meta_targets(builder)

    def _brace_block(self, text: str, start: int) -> str:
        depth, pos = 1, start
        while pos < len(text) and depth > 0:
            if text[pos] == '{':
                depth += 1
            elif text[pos] == '}':
                depth -= 1
            pos += 1
        return text[start:pos - 1]

    def _extract_native(self, builder: RIGBuilder, bz: str) -> None:
        """Extract C/CUDA sources and trace native-link edges from build.zig."""
        by_lang = find_source_files()

        cuda_files = sorted(str(f).replace("\\", "/") for f in by_lang.get("cuda", []))
        cuda_id = None
        if cuda_files:
            ev = builder.evidence(*[f"{f}:1" for f in cuda_files[:3]])
            comp = Component(
                name="cuda-backend", type="shared_library", programming_language="cuda",
                source_files=cuda_files, evidence=[ev],
            )
            builder.add_component(comp)
            cuda_id = comp.id

        c_files = sorted(str(f).replace("\\", "/") for f in by_lang.get("c", []))
        c_id = None
        if c_files:
            ev = builder.evidence(*[f"{f}:1" for f in c_files[:3]])
            comp = Component(
                name="c-kernels", type="static_library", programming_language="c",
                source_files=c_files, evidence=[ev],
            )
            builder.add_component(comp)
            c_id = comp.id

        if not (c_id or cuda_id):
            return

        # ── Build.zig-driven native edges (evidence-backed) ───────────
        _l = lambda off: f"build.zig:{_line_of(bz, off)}"

        mod_name_id = {c.name: c.id for c in builder.components
                       if c.type == "package_library" and c.programming_language == "zig"}
        exe_name_id = {c.name: c.id for c in builder.components if c.type == "executable"}
        var_to_comp: dict[str, str] = {}
        for m in re.finditer(r'(?:const|var)\s+(\w+)\s*=\s*b\.addModule\s*\(\s*"([^"]+)"', bz):
            if m.group(2) in mod_name_id:
                var_to_comp[m.group(1)] = mod_name_id[m.group(2)]
        for m in re.finditer(
            r'(?:const|var)\s+(\w+)\s*=\s*b\.addExecutable\s*\(\s*\.\{\s*\.name\s*=\s*"([^"]+)"',
            bz, re.DOTALL,
        ):
            if m.group(2) in exe_name_id:
                var_to_comp[m.group(1)] = exe_name_id[m.group(2)]

        step_srcs: dict[str, list[str]] = {}
        for m in re.finditer(
            r'(?:const|var)\s+(\w+)\s*=\s*b\.addSystemCommand\s*\(\s*&?\.\{(.*?)\}\s*\)',
            bz, re.DOTALL,
        ):
            for cm in re.finditer(r'"-c",\s*"([^"]+)"', m.group(2)):
                step_srcs.setdefault(m.group(1), []).append(cm.group(1))
        for m in re.finditer(r'(\w+)\.addFileInput\s*\(\s*\.\{\s*\.cwd_relative\s*=\s*"([^"]+)"', bz):
            step_srcs.setdefault(m.group(1), []).append(m.group(2))

        obj_step: dict[str, str] = {}
        for m in re.finditer(r'(?:const|var)\s+(\w+)\s*=\s*(\w+)\.addOutputFileArg\(', bz):
            obj_step[m.group(1)] = m.group(2)

        def _edge(cvar: str, nid: str, lines: list[str]) -> None:
            cid = var_to_comp.get(cvar)
            if not (cid and nid and nid != cid):
                return
            comp = next((c for c in builder.components if c.id == cid), None)
            if comp:
                target_name = next((c.name for c in builder.components if c.id == nid), "")
                if target_name:
                    comp.depends_on.add(target_name)
                    ev = builder.evidence(*lines)
                    comp.evidence.append(ev)

        for m in re.finditer(r'(\w+)(?:\.root_module)?\.addObjectFile\s*\(\s*(\w+)\s*\)', bz):
            cvar, ovar, ln = m.group(1), m.group(2), _l(m.start())
            for src in step_srcs.get(obj_step.get(ovar, ""), []):
                nid = cuda_id if src.endswith(".cu") else c_id if src.endswith(".c") else None
                if nid:
                    _edge(cvar, nid, [ln])

        if cuda_id:
            for m in re.finditer(r'(\w+)(?:\.root_module)?\.linkSystemLibrary\s*\(\s*"(cuda[^"]*)"', bz):
                _edge(m.group(1), cuda_id, [_l(m.start())])

    def _extract_tests(self, builder: RIGBuilder) -> None:
        """Scan Zig source files for inline `test "name" {` blocks."""
        test_pattern = re.compile(r'^\s*test\s+"([^"]+)"', re.MULTILINE)
        for comp in builder.components:
            if comp.programming_language != "zig":
                continue
            for sf in comp.source_files:
                try:
                    content = Path(sf).read_text(encoding="utf-8", errors="replace")
                except Exception:
                    continue
                matches = test_pattern.findall(content)
                if not matches:
                    continue
                for test_name in matches:
                    ev = builder.evidence(f"{sf}:1")
                    builder.add_test(TestDefinition(
                        name=f"test_{test_name}",
                        test_framework="zig test",
                        components_being_tested={comp.name},
                        depends_on={comp.name},
                        source_files=[sf],
                        evidence=[ev],
                    ))

    def _emit_meta_targets(self, builder: RIGBuilder) -> None:
        ev_build = builder.evidence("build.zig:1")
        exec_names = [c.name for c in builder.components
                      if c.type == "executable" and c.programming_language == "zig"]
        if exec_names:
            builder.add_aggregator(Aggregator(
                name="zig-build", depends_on=set(exec_names), evidence=[ev_build],
            ))

        test_names = [t.name for t in builder.tests if t.test_framework == "zig test"]
        if test_names:
            ev_test = builder.evidence("build.zig:1")
            builder.add_aggregator(Aggregator(
                name="zig-build-test", depends_on=set(test_names), evidence=[ev_test],
            ))
            builder.add_runner(Runner(
                name="zig-test", arguments=["zig", "build", "test"],
                depends_on=set(test_names), evidence=[ev_test],
            ))
