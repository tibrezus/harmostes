#!/usr/bin/env python3
"""research_digest — serve the compounded research, don't make the round search for it.

The #2845 lineage audit found the compounding asymmetry: five rounds on
one PR PUBLISHED four research notes (review_research_publish) and read
the shared store ZERO times — every fix-push generation re-derived its
orientation from scratch (one round spent its note concluding "no new
commits", a fact the store already carried). The store link
(/workspace/research) was never enough: discovery was left to the model,
and the model went straight to the diff.

This digest folds the PR's prior-round notes into ONE workspace file the
round cannot miss: /workspace/research-prior.md, newest first, each note
verbatim under a stamped header (bounded so a long history cannot flood
the context). Deterministic, non-fatal: no store, no notes, or any error
→ no file, the round orients cold exactly as before.
"""

import glob
import json
import os
import re


def note_sort_key(path):
    """Newest first: frontmatter written_at if parsable, else mtime."""
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as f:
            head = f.read(600)
    except OSError:
        head = ""
    m = re.search(r"^written_at:\s*(\S+)", head, re.M)
    if m:
        return m.group(1)
    try:
        import datetime

        return datetime.datetime.utcfromtimestamp(os.path.getmtime(path)).strftime("%Y-%m-%dT%H:%M:%SZ")
    except OSError:
        return ""


MAX_NOTES = 4  # the most recent notes in full
MAX_NOTE_BYTES = 6000  # per-note cap — orientation, not archives


def main():
    workdir = os.environ.get("HARMOSTES_WORKDIR", "/workspace")
    store = os.environ.get("HARMOSTES_SOL_PI_STORE", "").rstrip("/")
    pr = os.environ.get("HARMOSTES_TRIGGER_PR", "").strip()
    if not store or not pr:
        return 0  # cold round by configuration — silent, non-fatal
    src = os.path.join(store, "_shared", "research")
    if not os.path.isdir(src):
        return 0
    pr_num = int(pr) if pr.isdigit() else None
    notes = []
    for p in glob.glob(os.path.join(src, "*.md")):
        base = os.path.basename(p)
        m = re.match(r"^(\d+)-", base)
        if not m:
            continue  # other workflows' repo-wide notes stay behind the link
        if pr_num is not None and int(m.group(1)) != pr_num:
            continue  # another PR's notes are that PR's context, not this round's
        notes.append(p)
    if not notes:
        return 0
    notes.sort(key=note_sort_key, reverse=True)

    parts = [
        "# Prior research for this PR (the served compound)",
        "",
        f"{len(notes)} note(s) published by earlier rounds of PR #{pr}, newest first.",
        "This is orientation earlier rounds paid for: re-derive nothing it",
        "answers. A note is a LEAD, not ground truth — verify against the",
        "tree before relying on it; a path a note names that no longer",
        "exists is expired, do not propagate it.",
        "",
    ]
    served = notes[:MAX_NOTES]
    older = notes[MAX_NOTES:]
    for p in served:
        base = os.path.basename(p)
        try:
            with open(p, "r", encoding="utf-8", errors="replace") as f:
                body = f.read()
        except OSError:
            continue
        stamped = note_sort_key(p) or "unknown date"
        parts.append(f"---\n## {base} (written {stamped})\n")
        if len(body) > MAX_NOTE_BYTES:
            body = body[:MAX_NOTE_BYTES] + "\n\n[…truncated — the full note is at /workspace/research/" + base + "]\n"
        parts.append(body.rstrip() + "\n")
    if older:
        parts.append("---\n## Older notes (index only — full text at /workspace/research/)\n")
        for p in older:
            parts.append(f"- {os.path.basename(p)} (written {note_sort_key(p) or 'unknown'})")
        parts.append("")

    out = os.path.join(workdir, "research-prior.md")
    tmp = os.path.join(workdir, f".research-prior-{os.getpid()}-{id(parts)}")
    with open(tmp, "w", encoding="utf-8") as f:
        f.write("\n".join(parts))
    os.replace(tmp, out)  # atomic — the agent never sees a half file

    # the pr-context fact block: the round reads one JSON for its
    # environment facts — tell it the compound is there (best-effort,
    # a missing file is the pre-#2845 status quo, never an error).
    ctx_path = os.path.join(workdir, "pr-context.json")
    try:
        with open(ctx_path, "r", encoding="utf-8") as f:
            ctx = json.load(f)
        ctx["research_prior"] = {"file": "research-prior.md", "notes": len(served), "older": len(older)}
        tmp = ctx_path + f".tmp-{os.getpid()}"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(ctx, f, indent=2)
        os.replace(tmp, ctx_path)
    except (OSError, ValueError):
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
