"""Extractor base class — interface for language-specific RIG extractors."""

from __future__ import annotations

from abc import ABC, abstractmethod
from pathlib import Path

from ..builder import RIGBuilder


class Extractor(ABC):
    """Base class for build-system extractors.

    Subclasses implement two methods:
    - ``detects()`` — True if this extractor's build system is present.
    - ``extract(builder)`` — populate the builder with components, tests,
      runners, aggregators, evidence, and external packages.

    The extractor expresses dependencies as **names**; the builder resolves
    names → IDs.
    """

    #: Human-readable name for logging + the ``build_system`` field.
    name: str = "unknown"

    @staticmethod
    @abstractmethod
    def detects() -> bool:
        """Return True if this build system is present in the current directory."""
        ...

    @abstractmethod
    def extract(self, builder: RIGBuilder) -> None:
        """Extract nodes into the builder."""
        ...

    @property
    def build_file(self) -> str | None:
        """Primary build file for auto-evidence (e.g. 'go.mod', 'build.zig')."""
        return None
