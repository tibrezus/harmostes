"""Go extractor — uses ``go list -json`` for authoritative component graph."""

from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

from ..builder import RIGBuilder
from ..model import Component, Evidence
from .base import Extractor


class GoExtractor(Extractor):
    name = "go-modules"
    build_file = "go.mod"

    @staticmethod
    def detects() -> bool:
        return Path("go.mod").exists()

    def extract(self, builder: RIGBuilder) -> None:
        packages = self._go_list()
        if not packages:
            return

        module_path = packages[0].get("Module", {}).get("Path", "") if packages else ""

        for pkg in packages:
            import_path = pkg.get("ImportPath", "")
            if not import_path:
                continue
            pkg_name = import_path.split("/")[-1]
            is_main = pkg.get("Name") == "main"

            go_files = pkg.get("GoFiles", []) or []
            dir_path = pkg.get("Dir", "")
            cwd = os.getcwd()
            src_files = [
                os.path.join(dir_path, f).replace(cwd + "/", "").replace("\\", "/")
                for f in go_files
            ]

            # External packages
            ext_refs: set[str] = set()
            for imp in pkg.get("Imports", []) or []:
                if module_path and imp.startswith(module_path):
                    continue
                first = imp.split("/")[0]
                if "." not in first:
                    continue
                ext_refs.add(builder.package(imp, "go-modules", imp))

            comp = Component(
                name=import_path,  # full import path for uniqueness
                type="executable" if is_main else "package_library",
                programming_language="go",
                source_files=src_files,
                external_packages=ext_refs,
                is_entrypoint=is_main,
            )
            # Evidence: the module file + this package's directory
            comp.evidence.append(builder.evidence("go.mod:1"))
            if src_files:
                comp.evidence.append(builder.evidence(f"{src_files[0]}:1"))
            builder.add_component(comp)

            # Internal deps (resolved by name after all components are added)
            comp.depends_on = {
                imp for imp in pkg.get("Imports", []) or []
                if module_path and imp.startswith(module_path) and imp != import_path
            }

        # ── Tests ─────────────────────────────────────────────────────
        self._extract_tests(packages, builder)

        # ── Aggregators + Runner ──────────────────────────────────────
        self._emit_meta_targets(builder)

    def _go_list(self) -> list[dict]:
        env = dict(os.environ, GOFLAGS="-mod=mod", GOWORK="off")
        try:
            subprocess.run(["go", "mod", "download"],
                           capture_output=True, text=True, timeout=300, env=env)
        except (FileNotFoundError, subprocess.TimeoutExpired):
            pass

        try:
            r = subprocess.run(["go", "list", "-e", "-json", "./..."],
                               capture_output=True, text=True, timeout=180, env=env)
        except (FileNotFoundError, subprocess.TimeoutExpired) as e:
            print(f"[go] go list failed: {e}", file=sys.stderr)
            try:
                r = subprocess.run(["go", "list", "-e", "-json", "-find", "./..."],
                                   capture_output=True, text=True, timeout=120, env=env)
            except (FileNotFoundError, subprocess.TimeoutExpired):
                return []
        if not r.stdout.strip():
            return []

        decoder = json.JSONDecoder()
        packages, text, idx = [], r.stdout.strip(), 0
        while idx < len(text):
            s = text[idx:].lstrip()
            if not s:
                break
            obj, consumed = decoder.raw_decode(s)
            packages.append(obj)
            idx = len(text) - len(s) + consumed
        return packages

    def _extract_tests(self, packages: list[dict], builder: RIGBuilder) -> None:
        from ..model import TestDefinition

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
            test_src = [
                os.path.join(dir_path, f).replace(cwd + "/", "").replace("\\", "/")
                for f in all_test_files
            ]

            # The Go test binary is an implicit executable compiled from the
            # test files + the package under test.  We model it as a test
            # executable referencing the production package.
            ev = builder.evidence(f"{test_src[0]}:1") if test_src else None
            builder.add_test(TestDefinition(
                name=f"test_{pkg_name}",
                test_framework="go test",
                test_executable="",  # no separate component — go run test
                components_being_tested={import_path} if import_path else set(),
                depends_on={import_path} if import_path else set(),
                source_files=test_src,
                evidence=[ev] if ev else [],
            ))

    def _emit_meta_targets(self, builder: RIGBuilder) -> None:
        from ..model import Aggregator, Runner

        ev_mod = builder.evidence("go.mod:1")
        exec_ids = [c.name for c in builder.components
                    if c.type == "executable" and c.programming_language == "go"]
        if exec_ids:
            builder.add_aggregator(Aggregator(
                name="go-build-all", depends_on=set(exec_ids), evidence=[ev_mod],
            ))

        test_names = [t.name for t in builder.tests if t.test_framework == "go test"]
        if test_names:
            ev_test = builder.evidence("go.mod:1")
            builder.add_aggregator(Aggregator(
                name="go-test-all", depends_on=set(test_names), evidence=[ev_test],
            ))
            builder.add_runner(Runner(
                name="go-test", arguments=["go", "test", "./..."],
                depends_on=set(test_names), evidence=[ev_test],
            ))
