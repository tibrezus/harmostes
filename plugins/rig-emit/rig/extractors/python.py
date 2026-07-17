"""Python extractor — pyproject.toml / setup.py + package discovery."""

from __future__ import annotations

import re
from pathlib import Path

from ..builder import RIGBuilder
from ..model import Artifact, Component, Runner
from .base import Extractor
from ..builder import EXCLUDE_DIRS


class PythonExtractor(Extractor):
    name = "pip"
    build_file = "pyproject.toml"

    @staticmethod
    def detects() -> bool:
        return Path("pyproject.toml").exists() or Path("setup.py").exists() or Path("setup.cfg").exists()

    def extract(self, builder: RIGBuilder) -> None:
        py_files = [f for f in Path(".").rglob("*.py")
                    if not any(d in f.parts for d in EXCLUDE_DIRS)]
        if not py_files:
            return

        project_name = "python-project"
        ev_file = "pyproject.toml" if Path("pyproject.toml").exists() else "setup.py"
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
        ev_manifest = builder.evidence(f"{ev_file}:1")

        comp = Component(
            name=project_name, type="package_library", programming_language="python",
            source_files=source_files, evidence=[ev_manifest],
        )
        if source_files:
            comp.evidence.append(builder.evidence(f"{source_files[0]}:1"))
        builder.add_component(comp)

        # Executables (__main__.py, cli.py, main.py)
        main_files = [f for f in py_files if f.name in ("__main__.py", "cli.py", "main.py")]
        for mf in main_files:
            exe_comp = Component(
                name=mf.parent.name, type="executable", programming_language="python",
                source_files=[str(mf).replace("\\", "/")], is_entrypoint=True,
                depends_on={project_name},
                artifacts=[Artifact(name=mf.parent.name, relative_path=str(mf).replace("\\", "/"))],
                evidence=[ev_manifest, builder.evidence(f"{str(mf).replace('/', '_')}:1")],
            )
            builder.add_component(exe_comp)

        # External deps
        for req_file in ["requirements.txt", "requirements-dev.txt"]:
            if Path(req_file).exists():
                for line in Path(req_file).read_text(errors="replace").splitlines():
                    line = line.strip()
                    if line and not line.startswith("#"):
                        pkg_name = re.split(r"[>=<\[!]", line)[0].strip()
                        if pkg_name:
                            builder.package(pkg_name, "pip", pkg_name)
                            comp.external_packages.add(pkg_name)

        if Path("pyproject.toml").exists():
            content = Path("pyproject.toml").read_text(errors="replace")
            deps_match = re.search(r'\[project\.optional-dependencies\]|dependencies\s*=\s*\[', content)
            if deps_match:
                for dm in re.finditer(r'"([^"]+)"', content[deps_match.start():]):
                    pkg_name = re.split(r"[>=<\[!]", dm.group(1))[0].strip()
                    if pkg_name:
                        builder.package(pkg_name, "pip", pkg_name)
                        comp.external_packages.add(pkg_name)

        # Runner
        builder.add_runner(Runner(
            name="pytest", arguments=["python", "-m", "pytest"], evidence=[ev_manifest],
        ))
