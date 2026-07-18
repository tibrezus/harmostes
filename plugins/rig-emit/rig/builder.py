"""RIGBuilder — central state for RIG construction.

Accumulates nodes (components, aggregators, runners, tests, evidence,
external packages), assigns stable IDs, resolves name-references to IDs,
auto-generates evidence, and produces the final RIG JSON.

Design: extractors express dependencies as **names** (they know names at
extraction time, not IDs).  The Builder resolves names → IDs in `build()`,
so extractors never need to track ID maps themselves.
"""

from __future__ import annotations

import json
import subprocess
from pathlib import Path

from .model import (
    Aggregator, Artifact, Component, Evidence,
    ExternalPackage, Runner, TestDefinition,
)

# ── Source-file discovery (shared by all extractors) ────────────────

SOURCE_EXTENSIONS = {
    ".zig": "zig", ".go": "go", ".rs": "rust",
    ".py": "python",
    ".ts": "typescript", ".tsx": "typescript",
    ".js": "javascript", ".jsx": "javascript", ".mjs": "javascript", ".cjs": "javascript",
    ".c": "c", ".h": "c",
    ".cpp": "cpp", ".cc": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hxx": "cpp",
    ".cu": "cuda", ".cuh": "cuda",
    ".java": "java", ".kt": "kotlin", ".swift": "swift",
    ".rb": "ruby", ".lua": "lua", ".sh": "shell", ".bash": "shell",
}

EXCLUDE_DIRS = {
    ".git", "node_modules", ".zig-cache", "zig-out", "zig_cache",
    "__pycache__", ".pytest_cache", "vendor", ".venv", "venv",
    "dist", "build", "target", ".next", ".nuxt", ".output",
    ".cache", ".turbo", "coverage", ".coverage",
    # Git submodules (vendored content, not build targets)
    ".agents", ".llm-wiki",
}

BUILD_CONFIG_FILES = {
    "build.zig", "build.zig.zon", "go.mod", "go.sum", "Cargo.toml",
    "Cargo.lock", "package.json", "package-lock.json", "yarn.lock",
    "pnpm-lock.yaml", "pyproject.toml", "setup.py", "setup.cfg",
    "requirements.txt", "CMakeLists.txt", "Makefile", "Dockerfile",
    "tsconfig.json", "webpack.config.js", "vite.config.ts",
}


def find_source_files(root: Path | None = None) -> dict[str, list[Path]]:
    """Find all source files grouped by language."""
    root = root or Path(".")
    by_lang: dict[str, list[Path]] = {}
    for p in root.rglob("*"):
        if not p.is_file():
            continue
        if set(p.parts) & EXCLUDE_DIRS:
            continue
        lang = SOURCE_EXTENSIONS.get(p.suffix.lower())
        if lang:
            by_lang.setdefault(lang, []).append(p)
    return by_lang


def all_source_paths() -> set[str]:
    """All source file paths (relative, forward-slash)."""
    paths = set()
    for files in find_source_files().values():
        for f in files:
            paths.add(str(f).replace("\\", "/"))
    return paths


def _line_of(text: str, offset: int) -> str:
    """Convert a character offset in *text* to a ``file:N`` line number."""
    return str(text.count("\n", 0, offset) + 1)


class RIGBuilder:
    """Accumulator + ID assigner + name-resolver + JSON assembler."""

    def __init__(self, repo_root: Path | None = None):
        self.root = repo_root or Path(".")
        self.components: list[Component] = []
        self.aggregators: list[Aggregator] = []
        self.runners: list[Runner] = []
        self.tests: list[TestDefinition] = []
        self.evidence_list: list[Evidence] = []
        self.packages: dict[str, ExternalPackage] = {}  # name → package

        self._comp_n = self._agg_n = self._run_n = self._test_n = 0
        self._ev_n = self._pkg_n = 0
        self._name_to_id: dict[str, str] = {}        # component/runner name → id
        self._ev_cache: dict[tuple, str] = {}         # (sorted lines) → evidence id

    # ── ID assignment ────────────────────────────────────────────────

    def _cid(self):
        self._comp_n += 1
        return f"comp-{self._comp_n}"

    def _aid(self):
        self._agg_n += 1
        return f"agg-{self._agg_n}"

    def _rid(self):
        self._run_n += 1
        return f"runner-{self._run_n}"

    def _tid(self):
        self._test_n += 1
        return f"test-{self._test_n}"

    # ── Evidence ─────────────────────────────────────────────────────

    def evidence(self, *lines: str, call_stack: list[str] | None = None) -> Evidence:
        """Create and register an evidence entry. Returns the Evidence (with .id)."""
        key = (tuple(sorted(lines)), tuple(call_stack or []))
        if key in self._ev_cache:
            for ev in self.evidence_list:
                if ev.id == self._ev_cache[key]:
                    return ev
        self._ev_n += 1
        ev = Evidence(line=list(lines), call_stack=call_stack or [], id=f"evidence-{self._ev_n}")
        self.evidence_list.append(ev)
        self._ev_cache[key] = ev.id
        return ev

    def evidence_at(self, build_file: str, text: str, offset: int) -> Evidence:
        """Convenience: evidence at a specific line of a build file."""
        return self.evidence(f"{build_file}:{_line_of(text, offset)}")

    # ── External packages ────────────────────────────────────────────

    def package(self, name: str, manager: str, package: str | None = None) -> str:
        """Get or create an external package. Returns its **name** (resolved to ID in build())."""
        if name not in self.packages:
            self._pkg_n += 1
            self.packages[name] = ExternalPackage(
                name=name, manager=manager, package=package or name, id=f"pkg-{self._pkg_n}",
            )
        return name

    # ── Node registration ────────────────────────────────────────────

    def add_component(self, comp: Component) -> Component:
        """Register a component. Assigns ID, indexes name → id."""
        comp.id = self._cid()
        self.components.append(comp)
        self._name_to_id[comp.name] = comp.id
        return comp

    def add_aggregator(self, agg: Aggregator) -> Aggregator:
        agg.id = self._aid()
        self.aggregators.append(agg)
        self._name_to_id[agg.name] = agg.id
        return agg

    def add_runner(self, runner: Runner) -> Runner:
        runner.id = self._rid()
        self.runners.append(runner)
        self._name_to_id[runner.name] = runner.id
        return runner

    def add_test(self, test: TestDefinition) -> TestDefinition:
        test.id = self._tid()
        self.tests.append(test)
        self._name_to_id[test.name] = test.id
        return test

    # ── Name → ID resolution ─────────────────────────────────────────

    def resolve(self, name: str) -> str | None:
        """Resolve a component/runner name to its ID."""
        return self._name_to_id.get(name)

    # ── Auto-evidence for components without explicit evidence ────────

    def auto_evidence(self, comp: Component, build_file: str | None = None):
        """Attach build-system + source-file evidence to a component.

        If the component already has explicit evidence (from the extractor),
        this only fills in the build-file reference if missing.
        """
        if build_file:
            ev = self.evidence(f"{build_file}:1")
            if ev.id not in [e.id for e in comp.evidence]:
                comp.evidence.append(ev)
        src = comp.source_files[:1]  # first source file defines the component
        if src:
            ev = self.evidence(f"{src[0]}:1")
            if ev.id not in [e.id for e in comp.evidence]:
                comp.evidence.append(ev)

    # ── Output ───────────────────────────────────────────────────────

    def primary_language(self) -> str:
        counts: dict[str, int] = {}
        for c in self.components:
            counts[c.programming_language] = counts.get(c.programming_language, 0) + 1
        return max(counts, key=counts.get) if counts else "unknown"

    @staticmethod
    def _git_ref() -> str:
        if not Path(".git").exists():
            return ""
        try:
            r = subprocess.run(["git", "rev-parse", "--short", "HEAD"],
                               capture_output=True, text=True, timeout=5)
            return r.stdout.strip()
        except Exception:
            return ""

    def _detect_build_files(self) -> list[str]:
        return [f for f in ("go.mod", "build.zig", "Cargo.toml", "package.json",
                            "pyproject.toml", "CMakeLists.txt") if Path(f).exists()]

    def build(self, extractors: list[str]) -> dict:
        """Resolve name refs → IDs and produce the final RIG JSON dict."""
        entrypoints = [c.id for c in self.components if c.is_entrypoint]

        return {
            "schema_version": "rig-1.0",
            "repository": {
                "name": Path(".").resolve().name,
                "ref": self._git_ref(),
                "language": self.primary_language(),
                "build_system": "+".join(extractors),
                "generator": "tibrezus/llm-wiki/.github/actions/repo-map@v2",
            },
            "evidence": [{"id": e.id, "line": e.line, "call_stack": e.call_stack}
                         for e in self.evidence_list],
            "components": self._serialize_components(),
            "aggregators": self._serialize_aggregators(),
            "runners": self._serialize_runners(),
            "test_definitions": self._serialize_tests(),
            "external_packages": [{"id": p.id, "name": p.name,
                                   "package_manager": {"name": p.manager, "package_name": p.package}}
                                  for p in sorted(self.packages.values(), key=lambda x: x.name)],
            "entrypoints": entrypoints,
        }

    def _resolve_deps(self, names: set[str], own_id: str = "") -> list[str]:
        return sorted({
            self._name_to_id[d] for d in names
            if d in self._name_to_id and self._name_to_id[d] != own_id
        })

    def _resolve_packages(self, names: set[str]) -> list[str]:
        return sorted({self.packages[p].id for p in names if p in self.packages})

    def _serialize_components(self) -> list[dict]:
        out = []
        for c in self.components:
            out.append({"id": c.id, "name": c.name, "type": c.type,
                        "programming_language": c.programming_language,
                        "source_files": c.source_files,
                        "depends_on_ids": self._resolve_deps(c.depends_on, c.id),
                        "external_packages_ids": self._resolve_packages(c.external_packages),
                        "evidence_ids": [e.id for e in c.evidence],
                        "artifacts": [{"name": a.name, "relative_path": a.relative_path}
                                      for a in c.artifacts]})
        return out

    def _serialize_aggregators(self) -> list[dict]:
        return [{"id": a.id, "name": a.name,
                 "depends_on_ids": self._resolve_deps(a.depends_on),
                 "evidence_ids": [e.id for e in a.evidence]}
                for a in self.aggregators]

    def _serialize_runners(self) -> list[dict]:
        return [{"id": r.id, "name": r.name,
                 "arguments": list(r.arguments),
                 "depends_on_ids": self._resolve_deps(r.depends_on),
                 "evidence_ids": [e.id for e in r.evidence]}
                for r in self.runners]

    def _serialize_tests(self) -> list[dict]:
        out = []
        for t in self.tests:
            tested_ids = self._resolve_deps(t.components_being_tested)
            exe_id = self._name_to_id.get(t.test_executable, "")
            d = {"id": t.id, "name": t.name,
                 "depends_on_ids": self._resolve_deps(t.depends_on),
                 "components_being_tested_ids": tested_ids,
                 "source_files": t.source_files,
                 "evidence_ids": [e.id for e in t.evidence]}
            if t.test_framework:
                d["test_framework"] = t.test_framework
            if exe_id:
                d["test_executable_component_id"] = exe_id
            d["covers_ids"] = tested_ids  # backward compat
            out.append(d)
        return out
