"""npm / TypeScript extractor — package.json + workspace detection."""

from __future__ import annotations

import json
from pathlib import Path

from ..builder import RIGBuilder
from ..model import Aggregator, Artifact, Component, Runner
from .base import Extractor


class NpmExtractor(Extractor):
    name = "npm"
    build_file = "package.json"

    @staticmethod
    def detects() -> bool:
        return Path("package.json").exists()

    def extract(self, builder: RIGBuilder) -> None:
        pkg = json.loads(Path("package.json").read_text())
        name = pkg.get("name", "npm-package")
        ev_pkg = builder.evidence("package.json:1")
        lang = "typescript" if Path("tsconfig.json").exists() else "javascript"

        src_dirs = [d for d in ["src", "lib", "app"] if Path(d).exists()]
        source_files = []
        for d in src_dirs:
            for ext in (".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"):
                source_files.extend(str(f) for f in Path(d).rglob(f"*{ext}"))

        comp = Component(
            name=name, type="package_library", programming_language=lang,
            source_files=sorted(set(source_files)), evidence=[ev_pkg],
        )
        if source_files:
            comp.evidence.append(builder.evidence(f"{sorted(set(source_files))[0]}:1"))
        builder.add_component(comp)

        # Binaries
        bins = pkg.get("bin", {})
        if isinstance(bins, str):
            bins = {name: bins}
        for bin_name, bin_path in bins.items():
            bin_comp = Component(
                name=bin_name, type="executable", programming_language=lang,
                source_files=[bin_path], is_entrypoint=True,
                depends_on={name},
                artifacts=[Artifact(name=bin_name, relative_path=bin_path)],
                evidence=[ev_pkg, builder.evidence(f"{bin_path}:1")],
            )
            builder.add_component(bin_comp)

        # Workspaces
        workspaces = pkg.get("workspaces", [])
        if isinstance(workspaces, list):
            for ws_pattern in workspaces:
                for ws_dir in Path(".").glob(ws_pattern):
                    ws_pkg_f = ws_dir / "package.json"
                    if ws_pkg_f.exists():
                        try:
                            ws_data = json.loads(ws_pkg_f.read_text())
                            ws_name = ws_data.get("name", ws_dir.name)
                            ws_src = sorted(set(
                                str(f) for f in ws_dir.rglob("*.ts")) | set(
                                str(f) for f in ws_dir.rglob("*.js")))
                            ws_comp = Component(
                                name=ws_name, type="package_library", programming_language="typescript",
                                source_files=ws_src, evidence=[builder.evidence(f"{ws_pkg_f}:1")],
                            )
                            builder.add_component(ws_comp)
                        except json.JSONDecodeError:
                            pass

        # External deps
        for section in ["dependencies", "devDependencies", "peerDependencies"]:
            for dep_name in (pkg.get(section) or {}).keys():
                builder.package(dep_name, "npm", dep_name)
                comp.external_packages.add(dep_name)

        # Runner
        builder.add_runner(Runner(
            name="npm-test", arguments=["npm", "test"], evidence=[ev_pkg],
        ))
