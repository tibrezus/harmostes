#!/usr/bin/env python3
"""requirements conformance — the wiki-stated hard requirements are ENFORCED.

Canonical statements: the wiki, one page per workflow (owner-editable;
immutable per the 2026-10-24 ruling). Enforcement maps: requirements/
<workflow>.yaml (machine-checkable pointers). This script joins the two
by requirement ID and checks BOTH directions plus every pointer:

  1. the wiki page exists and carries the immutability header;
  2. every requirement stated in the wiki has an enforcement entry
     (an unenforced hard requirement is not hard);
  3. every enforcement entry has a requirement in the wiki (the
     machinery cannot keep enforcing a retired requirement);
  4. every pointer holds: the named file exists and contains the exact
     marker (a deleted test or reworded-away marker = drift = RED).
"""
import os
import re
import sys
import urllib.request
from pathlib import Path

import yaml

REPO = os.environ.get("REQUIREMENTS_REPO_ROOT", ".")
REQ_DIR = Path(REPO) / "requirements"
CACHE = REQ_DIR / ".wiki-cache"
RAW = "https://raw.githubusercontent.com/wiki/tibrezus/harmostes/{page}.md"
IMMUTABILITY = re.compile(r"IMMUTABLE", re.I)
HEADING = re.compile(r"^##\s+([A-Z][A-Z0-9-]*-\d+)\s+—\s+(.+?)\s*$", re.M)

fail: list[str] = []
maps = sorted(REQ_DIR.glob("*.yaml"))


def fetch_wiki(page: str) -> str:
    """Fetch the canonical wiki page; keep a cache for offline reruns."""
    CACHE.mkdir(parents=True, exist_ok=True)
    cache_file = CACHE / f"{page}.md"
    try:
        with urllib.request.urlopen(RAW.format(page=page), timeout=30) as r:
            text = r.read().decode("utf-8")
        cache_file.write_text(text)
        print(f"  wiki: fetched {page} from raw.githubusercontent.com")
        return text
    except OSError as e:
        if cache_file.exists():
            print(f"  wiki: fetch failed ({e}); using cached {cache_file}")
            return cache_file.read_text()
        raise


def check_workflow(map_file: Path) -> None:
    m = yaml.safe_load(map_file.read_text())
    wf, page = m.get("workflow"), m.get("wiki")
    if not wf or not page:
        fail.append(f"{map_file.name}: missing workflow/wiki keys")
        return
    print(f"workflow {wf} <- wiki page {page}")
    wiki_text = fetch_wiki(page)
    if not IMMUTABILITY.search(wiki_text):
        fail.append(f"{page}: immutability header missing — the page must state its change rule")
    stated = {}
    for rid, title in HEADING.findall(wiki_text):
        stated[rid] = title
    if not stated:
        fail.append(f"{page}: no '## ID — Title' requirement headings found")

    enforced = {}
    for req in m.get("requirements") or []:
        rid = req.get("id", "?")
        enforced[rid] = req.get("enforcement") or []

    for rid in stated:
        if rid not in enforced:
            fail.append(f"{rid}: stated in the wiki but has NO enforcement entry in {map_file.name}")
    for rid in enforced:
        if rid not in stated:
            fail.append(f"{rid}: enforced in {map_file.name} but NOT stated in the wiki (orphaned machinery)")

    for rid, pointers in enforced.items():
        if rid not in stated:
            continue
        if not pointers:
            fail.append(f"{rid}: NO enforcement pointers — an unenforced hard requirement is not hard")
        for p in pointers:
            path, marker = p.get("file"), p.get("contains")
            if not path or not marker:
                fail.append(f"{rid}: malformed pointer {p}")
                continue
            f = Path(REPO) / path
            try:
                content = f.read_text(encoding="utf-8")
            except OSError:
                fail.append(f"{rid}: enforcement file missing: {path}")
                continue
            if marker not in content:
                fail.append(f"{rid}: marker not found in {path}:\n    {marker!r}")


if not maps:
    print(f"FAIL: no enforcement maps in {REQ_DIR}")
    sys.exit(1)
for mf in maps:
    check_workflow(mf)

n = sum(1 for _ in maps)
print(f"requirements conformance: {n} workflow map(s) checked")
if fail:
    print(f"FAIL ({len(fail)}):")
    for f in fail:
        print(f"  - {f}")
    sys.exit(1)
print("OK — every hard requirement is stated in the wiki and carried by the machinery")
