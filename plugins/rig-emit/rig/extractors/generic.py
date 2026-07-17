"""Generic fallback extractor — groups source files by language."""

from __future__ import annotations

from ..builder import RIGBuilder, find_source_files
from ..model import Component
from .base import Extractor


class GenericExtractor(Extractor):
    name = "generic"

    @staticmethod
    def detects() -> bool:
        return True  # always available as last resort

    def extract(self, builder: RIGBuilder) -> None:
        by_lang = find_source_files()
        for lang, files in sorted(by_lang.items()):
            source_files = sorted(str(f).replace("\\", "/") for f in files)
            ev = builder.evidence(f"{source_files[0]}:1")
            comp = Component(
                name=f"{lang}-sources",
                type="executable" if lang in ("shell",) else "package_library",
                programming_language=lang,
                source_files=source_files,
                is_entrypoint=lang in ("shell",),
                evidence=[ev],
            )
            builder.add_component(comp)
