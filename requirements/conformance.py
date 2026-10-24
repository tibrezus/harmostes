#!/usr/bin/env python3
"""requirements conformance — the hard-requirements file is ENFORCED, not prose.

Every requirement in requirements/workflows.yaml carries enforcement
pointers (file, contains). This script verifies each pointer mechanically:
the file must exist and contain the marker (exact substring). A missing
file, a reworded-away marker, or a requirement with NO pointers is a
failure — the workflows cannot drift from the requirements file.

Immutability: the file changes only by explicit owner ruling (see the
header). This script does not judge rulings; it enforces that whatever
the file states, the machinery actually carries.
"""
import sys
import yaml

REQ = sys.argv[1] if len(sys.argv) > 1 else "requirements/workflows.yaml"

fail: list[str] = []
doc = yaml.safe_load(open(REQ))
workflows = doc.get("workflows") or {}
if not workflows:
    print(f"FAIL: {REQ} declares no workflows")
    sys.exit(1)

n_req = 0
for wf_name, wf in sorted(workflows.items()):
    reqs = (wf or {}).get("requirements") or []
    if not reqs:
        fail.append(f"{wf_name}: declares no requirements")
        continue
    for req in reqs:
        n_req += 1
        rid = req.get("id", "?")
        if not (req.get("statement") or "").strip():
            fail.append(f"{rid}: missing statement")
        enforcers = req.get("enforcement") or []
        if not enforcers:
            fail.append(f"{rid}: NO enforcement pointers — an unenforced hard requirement is not hard")
        for e in enforcers:
            path, marker = e.get("file"), e.get("contains")
            if not path or not marker:
                fail.append(f"{rid}: malformed pointer {e}")
                continue
            try:
                content = open(path, encoding="utf-8").read()
            except OSError:
                fail.append(f"{rid}: enforcement file missing: {path}")
                continue
            if marker not in content:
                fail.append(f"{rid}: marker not found in {path}:\n    {marker!r}")

print(f"requirements conformance: {len(workflows)} workflows, {n_req} requirements checked")
if fail:
    print(f"FAIL ({len(fail)}):")
    for f in fail:
        print(f"  - {f}")
    sys.exit(1)
print("OK — every hard requirement is carried by the machinery")
