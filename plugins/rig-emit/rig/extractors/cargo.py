"""Cargo (Rust) extractor — manifest parsing + src/ scan."""

from __future__ import annotations

import re
from pathlib import Path

from ..builder import RIGBuilder
from ..model import Aggregator, Artifact, Component, Runner, TestDefinition
from .base import Extractor


class CargoExtractor(Extractor):
    name = "cargo"
    build_file = "Cargo.toml"

    @staticmethod
    def detects() -> bool:
        return Path("Cargo.toml").exists()

    def extract(self, builder: RIGBuilder) -> None:
        content = Path("Cargo.toml").read_text(encoding="utf-8", errors="replace")
        ev_manifest = builder.evidence("Cargo.toml:1")

        name_m = re.search(r'^\[package\].*?name\s*=\s*"([^"]+)"', content, re.DOTALL | re.MULTILINE)
        project_name = name_m.group(1) if name_m else "rust-project"

        # [lib]
        lib_match = re.search(r'\[lib\].*?(?=\n\[|\Z)', content, re.DOTALL)
        if lib_match:
            src_dir = Path("src")
            source_files = sorted(str(f) for f in src_dir.rglob("*.rs")) if src_dir.exists() else []
            comp = Component(
                name=project_name, type="package_library", programming_language="rust",
                source_files=source_files, evidence=[ev_manifest],
            )
            if source_files:
                comp.evidence.append(builder.evidence(f"{source_files[0]}:1"))
            builder.add_component(comp)

        # [[bin]]
        for m in re.finditer(r'\[\[bin\]\].*?(?=\n\[|\Z)', content, re.DOTALL):
            bin_name_m = re.search(r'name\s*=\s*"([^"]+)"', m.group())
            bin_path_m = re.search(r'path\s*=\s*"([^"]+)"', m.group())
            bin_name = bin_name_m.group(1) if bin_name_m else "bin"
            root_file = bin_path_m.group(1) if bin_path_m else "src/main.rs"
            comp = Component(
                name=bin_name, type="executable", programming_language="rust",
                source_files=[root_file], is_entrypoint=True,
                artifacts=[Artifact(name=bin_name, relative_path=f"target/debug/{bin_name}")],
                evidence=[ev_manifest, builder.evidence(f"{root_file}:1")],
            )
            builder.add_component(comp)

        # External deps
        deps_match = re.search(r'\[dependencies\](.*?)(?=\n\[|\Z)', content, re.DOTALL)
        if deps_match:
            for dm in re.finditer(r'^(\w[\w-]*)\s*=', deps_match.group(1), re.MULTILINE):
                dep = dm.group(1)
                builder.package(dep, "cargo", dep)

        # Aggregator + runner
        exec_names = [c.name for c in builder.components if c.type == "executable"]
        if exec_names:
            builder.add_aggregator(Aggregator(
                name="cargo-build", depends_on=set(exec_names), evidence=[ev_manifest],
            ))
        builder.add_runner(Runner(
            name="cargo-test", arguments=["cargo", "test"],
            evidence=[ev_manifest],
        ))
