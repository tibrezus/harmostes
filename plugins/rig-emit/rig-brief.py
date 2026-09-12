#!/usr/bin/env python3
"""rig-brief: pre-digest the architecture graph into the agent's orientation.

The r4 forensics (#2085 fatal run) showed the failure chain: the reviewer
model ignores the "query rig first" instruction, burns 30+ bash greps
rediscovering the graph, blows its context window, and dies mid-review —
[ length ] before review.json exists. Orientation is not a model-discipline
problem; it is a context problem. So PREPARE pre-answers it.

Reads:  rig.db (the SHA-exact graph emit-rig.py just built) + pr-context.json
        (files_changed — the diff's footprint)
Writes: <workdir>/rig-briefing.md — compact markdown:
          1. graph overview (one line per component)
          2. the diff's footprint — changed files → owning components
          3. touched components in depth: key symbols (file:line), reverse
             deps (blast radius: who depends on what this diff touches)
        and stamps "rig_briefing" into pr-context.json.

Fail-open by contract (same as emit-rig.py): no graph, no changed files, or
any surprise → exit 0 with nothing written; the task prompt's fallback text
covers absence. prepare never fails here.

Run:  python3 rig-brief.py <rig.db> <pr-context.json> <workdir>
      (stdlib only — sqlite3 + json)
CI:   test_brief.py exercises the real generator against a built db.
"""
import json
import sqlite3
import sys
from pathlib import Path

MAX_SYMBOLS_PER_FILE = 8
MAX_FILES_LISTED = 12


def rows(db: sqlite3.Connection, sql: str, args=()) -> list:
    return db.execute(sql, args).fetchall()


def short_name(name: str) -> str:
    """'github.com/tibrezus/harmostes/internal/k8s' → 'internal/k8s' — the
    fully-qualified path is noise at briefing scale; the last two segments
    identify the component for a reader standing in the repo."""
    parts = (name or "").strip("/").split("/")
    return "/".join(parts[-2:]) if len(parts) >= 2 else (name or "?")


def build_briefing(db_path: str, ctx: dict) -> str:
    db = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    out: list[str] = []

    # 1. Graph overview — one line per component, deps inline.
    comps = rows(db, "SELECT id, name, type, language, entrypoint FROM components ORDER BY seq")
    name_of = {r[0]: short_name(r[1]) for r in comps}
    dep_map: dict[str, list[str]] = {}
    for src, dst in rows(db, "SELECT src, dst FROM deps"):
        dep_map.setdefault(src, []).append(dst)
    rev_map: dict[str, list[str]] = {}
    for s, ds in dep_map.items():
        for d in ds:
            rev_map.setdefault(d, []).append(s)
    counts = dict(rows(db, "SELECT component_id, COUNT(*) FROM component_files GROUP BY component_id"))

    out.append("## Architecture graph (pre-computed — do NOT grep to rediscover this)\n")
    for cid, name, ctype, lang, entry in comps:
        mark = " [entrypoint]" if entry else ""
        deps = ",".join(sorted(name_of.get(d, d) for d in dep_map.get(cid, []))) or "-"
        out.append(f"- **{short_name(name)}** ({ctype or 'component'}, {lang or '?'}, {counts.get(cid, 0)} files) → deps: {deps}{mark}")

    # 2. The diff's footprint — changed file → owning component(s).
    # workspace.sh sends entries as OBJECTS ({status, filename, additions,
    # deletions}) — accept the plain-string shape too (older contexts,
    # tests), skip falsy: a shape mismatch here used to die mid-SQL binding
    # and fail open with no briefing at all (the #456 r1 finding — the
    # generator's own test was the only place strings ever shipped).
    changed = ctx.get("files_changed") or []
    if isinstance(changed, dict):
        changed = list(changed.keys())
    paths: list[str] = []
    for entry in changed:
        if isinstance(entry, dict):
            entry = entry.get("filename")
        if entry:
            paths.append(entry)
    changed = paths
    touched: dict[str, list[str]] = {}
    unmapped: list[str] = []
    for path in changed[:60]:
        owners = [r[0] for r in rows(
            db, "SELECT component_id FROM component_files WHERE path = ?", (path,))]
        if owners:
            for o in owners:
                touched.setdefault(o, []).append(path)
        else:
            unmapped.append(path)

    out.append("\n## This diff touches\n")
    for cid, paths in touched.items():
        out.append(f"- **{name_of.get(cid, cid)}** (`{cid}`): {', '.join(paths[:6])}"
                   + (f" (+{len(paths) - 6} more)" if len(paths) > 6 else ""))
    if unmapped:
        out.append(f"- (outside the graph): {', '.join(unmapped[:6])}"
                   + (f" (+{len(unmapped) - 6} more)" if len(unmapped) > 6 else ""))

    # 3. Touched components in depth — symbols + blast radius.
    out.append("\n## Touched components in depth\n")
    for cid, paths in touched.items():
        rev = sorted(set(rev_map.get(cid, [])))
        out.append(f"### {name_of.get(cid, cid)}")
        if rev:
            out.append(f"- **Blast radius** (these depend on it — a change here can break them): {', '.join(name_of.get(r, r) for r in rev)}")
        else:
            out.append("- **Blast radius**: nothing in the graph depends on it")
        shown = 0
        for path in paths[:MAX_FILES_LISTED]:
            syms = rows(db,
                        "SELECT name, kind, line, signature FROM symbols WHERE file = ? "
                        "ORDER BY seq LIMIT ?", (path, MAX_SYMBOLS_PER_FILE))
            if not syms:
                continue
            out.append(f"- `{path}`:")
            for name, kind, line, sig in syms:
                sig = (sig or "").strip()
                sig = (sig[:90] + "…") if len(sig) > 90 else sig
                out.append(f"  - `{name}` ({kind or 'sym'}, line {line}){(' — `' + sig + '`') if sig else ''}")
                shown += 1
            if shown >= MAX_SYMBOLS_PER_FILE * 4:
                break

    out.append("\nUse `rig search` ONLY for symbols this briefing does not answer; "
               "`read` whole files; grep/find/ls expeditions are forbidden — "
               "everything above was already known before your first call.\n")
    db.close()
    return "\n".join(out)


def main() -> int:
    if len(sys.argv) != 4:
        print("usage: rig-brief.py <rig.db> <pr-context.json> <workdir>", file=sys.stderr)
        return 2
    db_path, ctx_path, workdir = sys.argv[1], sys.argv[2], sys.argv[3]
    try:
        ctx = json.loads(Path(ctx_path).read_text())
        briefing = build_briefing(db_path, ctx)
        if len(briefing.strip()) < 40:
            raise ValueError("briefing came out empty — graph has no components?")
    except Exception as e:  # fail-open: absence is covered by the prompt
        print(f"[rig-brief] skipped: {e}", file=sys.stderr)
        return 0
    dest = Path(workdir) / "rig-briefing.md"
    dest.write_text(briefing)
    # Stamp the path so the prompt + telemetry can reference it.
    tmp = Path(str(ctx_path) + ".tmp")
    tmp.write_text(json.dumps({**ctx, "rig_briefing": str(dest)}))
    tmp.replace(ctx_path)
    print(f"[rig-brief] wrote {dest} ({len(briefing)} bytes)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
