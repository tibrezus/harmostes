"""CMake / C / C++ / CUDA extractor — add_executable/add_library + target_link_libraries."""

from __future__ import annotations

import re
from pathlib import Path

from ..builder import RIGBuilder, find_source_files
from ..model import Artifact, Component, Runner
from .base import Extractor


def _detect_lang(files: list[str]) -> str:
    for f in files:
        if f.endswith(".cu"):
            return "cuda"
        if f.endswith((".cpp", ".cc", ".cxx")):
            return "cpp"
    return "c"


class CMakeExtractor(Extractor):
    name = "cmake"
    build_file = "CMakeLists.txt"

    @staticmethod
    def detects() -> bool:
        return Path("CMakeLists.txt").exists()

    def extract(self, builder: RIGBuilder) -> None:
        content = Path("CMakeLists.txt").read_text(encoding="utf-8", errors="replace")
        ev_cml = builder.evidence("CMakeLists.txt:1")

        project_m = re.search(r'project\s*\(\s*(\w+)', content, re.IGNORECASE)
        project_name = project_m.group(1) if project_m else "cmake-project"

        # add_executable
        for m in re.finditer(r'add_executable\s*\(\s*(\w+)\s+([^)]+)\)', content, re.IGNORECASE):
            name = m.group(1)
            sources = [s.strip().strip('"') for s in re.split(r'\s+', m.group(2))
                       if s.strip() and s.strip().endswith((".c", ".cpp", ".cc", ".cxx", ".cu"))]
            comp = Component(
                name=name, type="executable", programming_language=_detect_lang(sources),
                source_files=sources, is_entrypoint=True,
                artifacts=[Artifact(name=name, relative_path=name)],
                evidence=[ev_cml],
            )
            if sources:
                comp.evidence.append(builder.evidence(f"{sources[0]}:1"))
            builder.add_component(comp)

        # add_library
        for m in re.finditer(
            r'add_library\s*\(\s*(\w+)\s+(?:STATIC|SHARED|MODULE|INTERFACE)?\s*([^)]+)\)',
            content, re.IGNORECASE,
        ):
            name = m.group(1)
            sources = [s.strip().strip('"') for s in re.split(r'\s+', m.group(2))
                       if s.strip() and s.strip().endswith((".c", ".cpp", ".cc", ".cxx", ".cu", ".h", ".hpp"))]
            lib_type = "shared_library" if "SHARED" in m.group(0).upper() else (
                "unknown" if "INTERFACE" in m.group(0).upper() else "static_library")
            comp = Component(
                name=name, type=lib_type, programming_language=_detect_lang(sources),
                source_files=sources, evidence=[ev_cml],
            )
            if sources:
                comp.evidence.append(builder.evidence(f"{sources[0]}:1"))
            builder.add_component(comp)

        # target_link_libraries
        for m in re.finditer(r'target_link_libraries\s*\(\s*(\w+)[^)]*?(\w+)\s*\)', content, re.IGNORECASE):
            consumer, provider = m.group(1), m.group(2)
            comp = next((c for c in builder.components if c.name == consumer), None)
            if comp:
                comp.depends_on.add(provider)

        # find_package → external packages
        for m in re.finditer(r'find_package\s*\(\s*(\w+)', content, re.IGNORECASE):
            dep = m.group(1)
            builder.package(dep, "cmake", dep)

        # Runner
        builder.add_runner(Runner(
            name="ctest", arguments=["ctest", "--output-on-failure"], evidence=[ev_cml],
        ))


class StandaloneCExtractor(Extractor):
    """Fallback for C/C++/CUDA sources when no build system is present."""
    name = "c-sources"

    @staticmethod
    def detects() -> bool:
        by_lang = find_source_files()
        return any(by_lang.get(lang) for lang in ("c", "cpp", "cuda"))

    def extract(self, builder: RIGBuilder) -> None:
        by_lang = find_source_files()
        for lang in ("c", "cpp", "cuda"):
            files = by_lang.get(lang, [])
            if not files:
                continue
            source_files = sorted(str(f).replace("\\", "/") for f in files)
            ev = builder.evidence(f"{source_files[0]}:1")
            builder.add_component(Component(
                name=f"{lang}-sources", type="static_library", programming_language=lang,
                source_files=source_files, evidence=[ev],
            ))
