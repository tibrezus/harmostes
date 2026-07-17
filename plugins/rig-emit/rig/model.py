"""RIG data model — Spade (arXiv:2601.10112) node types as dataclasses.

These mirror the paper's Pydantic schemas (github.com/Greenfuze/spade, core/schemas.py)
adapted to plain dataclasses for zero-dependency serialization.

Every node type carries an `evidence` list — the paper's core invariant is that
every RIG node is *evidence-backed* (traceable to build-system definition).
"""

from __future__ import annotations

from dataclasses import dataclass, field


@dataclass
class Evidence:
    """Build-system evidence.

    At least one of `line` or `call_stack` must be populated.  `line` is a flat
    list of ``file:line`` refs; `call_stack` is an ordered chain (leaf first)
    showing the build-system call path that defines the node.
    """
    line: list[str] = field(default_factory=list)
    call_stack: list[str] = field(default_factory=list)
    id: str = ""


@dataclass
class Artifact:
    """Build output artifact (binary, library file)."""
    name: str = ""
    relative_path: str = ""


@dataclass
class ExternalPackage:
    """Third-party dependency with package-manager metadata."""
    name: str
    manager: str = ""        # go-modules, cargo, npm, pip, zig-modules, cmake…
    package: str = ""        # resolved package name within the manager
    id: str = ""


@dataclass
class Component:
    """Build target — executable, library, or package.

    Dependencies (`depends_on`) and external packages (`external_packages`)
    are expressed as **names** during extraction; the Builder resolves them to
    IDs in `build()`.
    """
    name: str
    type: str               # executable | shared_library | static_library | package_library | vm | interpreted | unknown
    programming_language: str
    source_files: list[str] = field(default_factory=list)
    depends_on: set[str] = field(default_factory=set)
    external_packages: set[str] = field(default_factory=set)
    artifacts: list[Artifact] = field(default_factory=list)
    evidence: list[Evidence] = field(default_factory=list)
    is_entrypoint: bool = False
    id: str = ""


@dataclass
class Aggregator:
    """Meta-target that orchestrates other targets (all, build, test)."""
    name: str
    depends_on: set[str] = field(default_factory=set)
    evidence: list[Evidence] = field(default_factory=list)
    id: str = ""


@dataclass
class Runner:
    """Command executor — runs a build/test command with arguments.

    Paper: ``arguments`` is the command-line, ``args_nodes`` are RIG nodes
    referenced by those arguments.  Runners are first-class nodes (e.g.
    ``go test ./…``, ``zig build test``, ``make test``).
    """
    name: str
    arguments: list[str] = field(default_factory=list)
    depends_on: set[str] = field(default_factory=set)
    evidence: list[Evidence] = field(default_factory=list)
    id: str = ""


@dataclass
class TestDefinition:
    """Test → component link.

    Paper separates three concepts our old schema conflated:
    - ``test_executable`` — the binary/runner that *runs* the test.
    - ``components_being_tested`` — the production code under test.
    - ``depends_on`` — build-level dependencies of the test target.
    """
    name: str
    test_framework: str = ""
    test_executable: str = ""                   # name of runner/component
    components_being_tested: set[str] = field(default_factory=set)
    source_files: list[str] = field(default_factory=list)
    depends_on: set[str] = field(default_factory=set)
    evidence: list[Evidence] = field(default_factory=list)
    id: str = ""
