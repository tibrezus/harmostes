"""RIG SQLite storage — the canonical, queryable code-structure database.

The RIG standard (arXiv:2601.10112) defines the LOGICAL model: components are
build targets, every node is evidence-backed. JSON was the v1 serialization;
it collapses at scale — a 15-component repo ships a 530KB / ~135K-token file
because of key repetition, and consumers must load the whole file to answer
any question. The database fixes exactly that:

  - Queryable: `SELECT` returns the 50 tokens an agent needs, not 135K.
  - Compact: no key repetition (page-level storage).
  - FTS5: full-text symbol search without reading anything else.
  - Deterministic: fixed page size + sorted inserts + VACUUM → byte-stable
    files for identical source; `canonical_hash()` excludes volatile fields
    (generated_at, git ref) so skip logic never false-fires.

Artifact policy (module decision):
  - rig.db  — canonical artifact, committed where a durable copy is needed
              (llm-wiki instance raw/arch/). Project CI uploads it as a
              workflow artifact instead of committing (no commit loop).
  - rig.json — compat/export view for schema tests and legacy consumers.

Roundtrip guarantee: `read_db(write_db(rig)) == rig` modulo the volatile
repository fields (generated_at, ref), which the DB deliberately omits —
provenance lives in git history, and its absence keeps the file
deterministic.
"""

from __future__ import annotations

import hashlib
import json
import sqlite3
from pathlib import Path

DB_SCHEMA_VERSION = 1

# Volatile repository fields deliberately excluded from the DB (and from the
# canonical hash): they change on every run without any structural change.
VOLATILE_REPO_FIELDS = ("generated_at", "ref")

SCHEMA = """
CREATE TABLE meta(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE components(
  id TEXT PRIMARY KEY,
  seq INTEGER NOT NULL,
  name TEXT NOT NULL,
  type TEXT,
  language TEXT,
  entrypoint INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE deps(
  src TEXT NOT NULL,
  dst TEXT NOT NULL,
  PRIMARY KEY (src, dst)
);
CREATE TABLE files(
  path TEXT PRIMARY KEY,
  component_id TEXT,
  language TEXT,
  bytes INTEGER,
  lines INTEGER,
  doc TEXT
);
CREATE TABLE component_files(
  component_id TEXT NOT NULL,
  path TEXT NOT NULL,
  seq INTEGER NOT NULL,
  PRIMARY KEY (component_id, path)
);
CREATE TABLE symbols(
  seq INTEGER PRIMARY KEY,
  file TEXT NOT NULL,
  name TEXT NOT NULL,
  kind TEXT,
  line INTEGER,
  signature TEXT,
  doc TEXT
);
CREATE TABLE calls(
  caller TEXT NOT NULL,
  callee TEXT NOT NULL,
  PRIMARY KEY (caller, callee)
);
CREATE TABLE artifacts(
  component_id TEXT NOT NULL,
  name TEXT NOT NULL,
  path TEXT NOT NULL
);
CREATE TABLE packages(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  manager TEXT,
  package_name TEXT
);
CREATE TABLE component_packages(
  component_id TEXT NOT NULL,
  package_id TEXT NOT NULL,
  PRIMARY KEY (component_id, package_id)
);
CREATE TABLE evidence(
  id TEXT PRIMARY KEY,
  seq INTEGER NOT NULL,
  line TEXT NOT NULL,          -- JSON array
  call_stack TEXT NOT NULL     -- JSON array
);
CREATE TABLE component_evidence(
  component_id TEXT NOT NULL,
  evidence_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  PRIMARY KEY (component_id, evidence_id)
);
CREATE TABLE aggregators(
  id TEXT PRIMARY KEY,
  seq INTEGER NOT NULL,
  name TEXT NOT NULL
);
CREATE TABLE aggregator_deps(
  src TEXT NOT NULL,
  dst TEXT NOT NULL,
  PRIMARY KEY (src, dst)
);
CREATE TABLE aggregator_evidence(
  aggregator_id TEXT NOT NULL,
  evidence_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  PRIMARY KEY (aggregator_id, evidence_id)
);
CREATE TABLE runners(
  id TEXT PRIMARY KEY,
  seq INTEGER NOT NULL,
  name TEXT NOT NULL,
  arguments TEXT NOT NULL      -- JSON array
);
CREATE TABLE runner_deps(
  src TEXT NOT NULL,
  dst TEXT NOT NULL,
  PRIMARY KEY (src, dst)
);
CREATE TABLE runner_evidence(
  runner_id TEXT NOT NULL,
  evidence_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  PRIMARY KEY (runner_id, evidence_id)
);
CREATE TABLE tests(
  id TEXT PRIMARY KEY,
  seq INTEGER NOT NULL,
  name TEXT NOT NULL,
  framework TEXT,
  executable_id TEXT
);
CREATE TABLE test_deps(
  test_id TEXT NOT NULL,
  dep_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  PRIMARY KEY (test_id, dep_id)
);
CREATE TABLE test_covers(
  test_id TEXT NOT NULL,
  component_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  PRIMARY KEY (test_id, component_id)
);
CREATE TABLE test_files(
  test_id TEXT NOT NULL,
  path TEXT NOT NULL,
  seq INTEGER NOT NULL,
  PRIMARY KEY (test_id, path)
);
CREATE TABLE test_evidence(
  test_id TEXT NOT NULL,
  evidence_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  PRIMARY KEY (test_id, evidence_id)
);
CREATE TABLE entrypoints(
  seq INTEGER PRIMARY KEY,
  component_id TEXT NOT NULL
);
"""

FTS_SCHEMA = """
CREATE VIRTUAL TABLE symbols_fts USING fts5(
  name, signature, doc,
  content='symbols', content_rowid='seq'
);
"""


# ── Write ────────────────────────────────────────────────────────────

def write_db(rig: dict, db_path: Path, *, fts: bool = True) -> None:
    """Write a RIG dict to a deterministic SQLite file.

    Determinism: fixed page size, insertion in canonical (sorted/seq) order,
    VACUUM at the end. Identical input → byte-identical file (same sqlite
    build). Volatile fields (generated_at, ref) are never stored.
    """
    db_path = Path(db_path)
    if db_path.exists():
        db_path.unlink()
    con = sqlite3.connect(db_path)
    try:
        # 1024-byte pages: RIG rows are short strings — small pages halve the
        # artifact size vs sqlite's 4096 default (measured: 1188KB → 558KB on
        # a 15-component repo including symbols + FTS).
        con.execute("PRAGMA page_size=1024")
        con.execute("PRAGMA journal_mode=DELETE")
        con.executescript(SCHEMA)
        if fts and _fts5_available(con):
            con.executescript(FTS_SCHEMA)

        _write_meta(con, rig)
        _write_evidence(con, rig)
        _write_components(con, rig)
        _write_aggregators(con, rig)
        _write_runners(con, rig)
        _write_tests(con, rig)
        _write_packages(con, rig)
        _write_entrypoints(con, rig)

        con.commit()
        if fts and _fts5_available(con):
            con.execute("INSERT INTO symbols_fts(symbols_fts) VALUES('rebuild')")
            con.commit()
        con.execute("VACUUM")
    finally:
        con.close()


def add_symbols(db_path: Path, symbols: list[dict]) -> None:
    """Append symbol rows (dicts: file,name,kind,line,signature,doc), then
    rebuild the FTS index. Insertion order = list order (source order)."""
    con = sqlite3.connect(db_path)
    try:
        start = con.execute("SELECT COALESCE(MAX(seq), 0) FROM symbols").fetchone()[0]
        rows = [
            (start + i + 1, s["file"], s["name"], s.get("kind"),
             s.get("line"), s.get("signature"), s.get("doc"))
            for i, s in enumerate(symbols)
        ]
        con.executemany(
            "INSERT INTO symbols(seq, file, name, kind, line, signature, doc) "
            "VALUES (?,?,?,?,?,?,?)", rows)
        con.commit()
        if _fts5_available(con):
            con.execute("INSERT INTO symbols_fts(symbols_fts) VALUES('rebuild')")
            con.commit()
        con.execute("VACUUM")
    finally:
        con.close()


def add_files(db_path: Path, files: list[dict]) -> None:
    """Upsert file rows (path, component_id?, language?, bytes?, lines?, doc?)."""
    con = sqlite3.connect(db_path)
    try:
        con.executemany(
            "INSERT INTO files(path, component_id, language, bytes, lines, doc) "
            "VALUES (?,?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET "
            "component_id=excluded.component_id, language=excluded.language, "
            "bytes=excluded.bytes, lines=excluded.lines, doc=excluded.doc",
            [(f["path"], f.get("component_id"), f.get("language"),
              f.get("bytes"), f.get("lines"), f.get("doc")) for f in files])
        con.commit()
        con.execute("VACUUM")
    finally:
        con.close()


def add_archmap(db_path: Path, graph: dict) -> None:
    """Ingest an archmap graph.json (files/decls/calls) into symbols + calls.

    archmap is a native-AST extractor (compiler-grade); where present it
    replaces regex symbol extraction with deeper, more correct data.
    Keys: decls → (file, name, kind, line, signature); calls → (caller, callee)
    where endpoints are "file:name" keys.
    """
    symbols = []
    for d in graph.get("decls", []):
        symbols.append({
            "file": d.get("file", ""),
            "name": d.get("name", ""),
            "kind": d.get("kind", ""),
            "line": d.get("line"),
            "signature": d.get("signature") or d.get("name", ""),
            "doc": d.get("doc"),
        })
    add_symbols(db_path, symbols)
    con = sqlite3.connect(db_path)
    try:
        con.executemany(
            "INSERT OR IGNORE INTO calls(caller, callee) VALUES (?,?)",
            [(c.get("caller", ""), c.get("callee", ""))
             for c in graph.get("calls", [])])
        con.commit()
        con.execute("VACUUM")
    finally:
        con.close()


# ── Read ─────────────────────────────────────────────────────────────

def read_db(db_path: Path) -> dict:
    """Reconstruct the RIG dict from the DB (JSON-compat roundtrip)."""
    con = sqlite3.connect(Path(db_path))
    con.row_factory = sqlite3.Row
    try:
        meta = {r["key"]: r["value"] for r in con.execute("SELECT * FROM meta")}
        rig = {
            "schema_version": meta.get("rig_schema_version", "rig-1.0"),
            "repository": {
                "name": meta.get("repo_name", ""),
                "ref": "",
                "language": meta.get("repo_language", "unknown"),
                "build_system": meta.get("build_system", ""),
                "generated_at": "",
                "generator": meta.get("generator", ""),
            },
            "evidence": [
                {"id": r["id"], "line": json.loads(r["line"]),
                 "call_stack": json.loads(r["call_stack"])}
                for r in con.execute("SELECT * FROM evidence ORDER BY seq")
            ],
            "components": [], "aggregators": [], "runners": [],
            "test_definitions": [], "external_packages": [],
            "entrypoints": [
                r["component_id"] for r in
                con.execute("SELECT component_id FROM entrypoints ORDER BY seq")
            ],
        }

        files_by_comp: dict[str, list[str]] = {}
        for r in con.execute(
                "SELECT component_id, path FROM component_files "
                "ORDER BY component_id, seq"):
            files_by_comp.setdefault(r["component_id"], []).append(r["path"])

        for r in con.execute("SELECT * FROM components ORDER BY seq"):
            cid = r["id"]
            rig["components"].append({
                "id": cid, "name": r["name"], "type": r["type"],
                "programming_language": r["language"],
                "source_files": files_by_comp.get(cid, []),
                "depends_on_ids": [d["dst"] for d in con.execute(
                    "SELECT dst FROM deps WHERE src=? ORDER BY dst", (cid,))],
                "external_packages_ids": [p["package_id"] for p in con.execute(
                    "SELECT package_id FROM component_packages "
                    "WHERE component_id=? ORDER BY package_id", (cid,))],
                "evidence_ids": [e["evidence_id"] for e in con.execute(
                    "SELECT evidence_id FROM component_evidence "
                    "WHERE component_id=? ORDER BY seq", (cid,))],
                "artifacts": [{"name": a["name"], "relative_path": a["path"]}
                              for a in con.execute(
                                  "SELECT name, path FROM artifacts "
                                  "WHERE component_id=? ORDER BY path", (cid,))],
            })

        for r in con.execute("SELECT * FROM aggregators ORDER BY seq"):
            aid = r["id"]
            rig["aggregators"].append({
                "id": aid, "name": r["name"],
                "depends_on_ids": [d["dst"] for d in con.execute(
                    "SELECT dst FROM aggregator_deps WHERE src=? ORDER BY dst", (aid,))],
                "evidence_ids": [e["evidence_id"] for e in con.execute(
                    "SELECT evidence_id FROM aggregator_evidence "
                    "WHERE aggregator_id=? ORDER BY seq", (aid,))],
            })

        for r in con.execute("SELECT * FROM runners ORDER BY seq"):
            rid = r["id"]
            rig["runners"].append({
                "id": rid, "name": r["name"],
                "arguments": json.loads(r["arguments"]),
                "depends_on_ids": [d["dst"] for d in con.execute(
                    "SELECT dst FROM runner_deps WHERE src=? ORDER BY dst", (rid,))],
                "evidence_ids": [e["evidence_id"] for e in con.execute(
                    "SELECT evidence_id FROM runner_evidence "
                    "WHERE runner_id=? ORDER BY seq", (rid,))],
            })

        for r in con.execute("SELECT * FROM tests ORDER BY seq"):
            tid = r["id"]
            covers = [c["component_id"] for c in con.execute(
                "SELECT component_id FROM test_covers "
                "WHERE test_id=? ORDER BY seq", (tid,))]
            d = {
                "id": tid, "name": r["name"],
                "depends_on_ids": [x["dep_id"] for x in con.execute(
                    "SELECT dep_id FROM test_deps WHERE test_id=? ORDER BY seq", (tid,))],
                "components_being_tested_ids": covers,
                "source_files": [f["path"] for f in con.execute(
                    "SELECT path FROM test_files WHERE test_id=? ORDER BY seq", (tid,))],
                "evidence_ids": [e["evidence_id"] for e in con.execute(
                    "SELECT evidence_id FROM test_evidence "
                    "WHERE test_id=? ORDER BY seq", (tid,))],
            }
            if r["framework"]:
                d["test_framework"] = r["framework"]
            if r["executable_id"]:
                d["test_executable_component_id"] = r["executable_id"]
            d["covers_ids"] = covers
            rig["test_definitions"].append(d)

        rig["external_packages"] = [
            {"id": p["id"], "name": p["name"],
             "package_manager": {"name": p["manager"], "package_name": p["package_name"]}}
            for p in con.execute(
                "SELECT * FROM packages ORDER BY name")
        ]
        return rig
    finally:
        con.close()


def load_rig(path: Path) -> dict:
    """Load a RIG from .db (canonical) or .json (legacy) by suffix."""
    path = Path(path)
    if path.suffix == ".db":
        return read_db(path)
    return json.loads(path.read_text())


# ── Canonical hash ───────────────────────────────────────────────────

CANONICAL_TABLES = [
    "meta", "components", "deps", "files", "component_files", "symbols", "calls",
    "artifacts", "packages", "component_packages", "evidence", "component_evidence",
    "aggregators", "aggregator_deps", "aggregator_evidence",
    "runners", "runner_deps", "runner_evidence",
    "tests", "test_deps", "test_covers", "test_files", "test_evidence",
    "entrypoints",
]


def canonical_hash(db_path: Path) -> str:
    """Deterministic content hash of the DB, independent of page layout.

    Serializes every table row (sorted) to a stable text stream and hashes
    it. Use this for skip logic — NOT sha256 of the file bytes — so the
    hash survives sqlite version changes.
    """
    h = hashlib.sha256()
    con = sqlite3.connect(Path(db_path))
    try:
        for table in CANONICAL_TABLES:
            rows = con.execute(f"SELECT * FROM {table}").fetchall()
            for row in sorted(rows, key=lambda r: tuple(str(x) for x in r)):
                h.update(table.encode())
                h.update(b"\x1f")
                h.update("\x1f".join("" if x is None else str(x) for x in row).encode())
                h.update(b"\x1e")
    finally:
        con.close()
    return h.hexdigest()


# ── Writers (private) ────────────────────────────────────────────────

def _write_meta(con, rig: dict) -> None:
    repo = rig.get("repository", {})
    con.executemany(
        "INSERT INTO meta(key, value) VALUES (?,?)",
        [
            ("db_schema_version", str(DB_SCHEMA_VERSION)),
            ("rig_schema_version", rig.get("schema_version", "rig-1.0")),
            ("repo_name", repo.get("name", "")),
            ("repo_language", repo.get("language", "unknown")),
            ("build_system", repo.get("build_system", "")),
            ("generator", repo.get("generator", "")),
        ])


def _write_evidence(con, rig: dict) -> None:
    con.executemany(
        "INSERT INTO evidence(id, seq, line, call_stack) VALUES (?,?,?,?)",
        [(e["id"], i, json.dumps(e.get("line", [])), json.dumps(e.get("call_stack", [])))
         for i, e in enumerate(rig.get("evidence", []))])


def _write_components(con, rig: dict) -> None:
    comps = rig.get("components", [])
    entrypoints = set(rig.get("entrypoints", []))
    con.executemany(
        "INSERT INTO components(id, seq, name, type, language, entrypoint) "
        "VALUES (?,?,?,?,?,?)",
        [(c["id"], i, c["name"], c.get("type", "unknown"),
          c.get("programming_language", "unknown"),
          1 if c["id"] in entrypoints else 0)
         for i, c in enumerate(comps)])

    deps = sorted({(c["id"], d) for c in comps for d in c.get("depends_on_ids", [])})
    con.executemany("INSERT OR IGNORE INTO deps(src, dst) VALUES (?,?)", deps)

    pkgs = sorted({(c["id"], p) for c in comps for p in c.get("external_packages_ids", [])})
    con.executemany("INSERT OR IGNORE INTO component_packages VALUES (?,?)", pkgs)

    evs = [(c["id"], e, j) for c in comps
           for j, e in enumerate(c.get("evidence_ids", []))]
    con.executemany("INSERT OR IGNORE INTO component_evidence VALUES (?,?,?)", evs)

    arts = [(c["id"], a.get("name", ""), a.get("relative_path", ""))
            for c in comps for a in c.get("artifacts", [])]
    con.executemany("INSERT INTO artifacts VALUES (?,?,?)", arts)

    # Exact component→file mapping (seq preserves the dict's list order).
    # `files` holds file-level metadata; `component_files` holds the mapping.
    con.executemany(
        "INSERT OR IGNORE INTO component_files VALUES (?,?,?)",
        [(c["id"], sf, j) for c in comps
         for j, sf in enumerate(c.get("source_files", []))])
    file_rows = sorted({(sf, c["id"]) for c in comps for sf in c.get("source_files", [])})
    con.executemany("INSERT OR IGNORE INTO files(path, component_id) VALUES (?,?)",
                    [(p, cid) for p, cid in file_rows])


def _write_aggregators(con, rig: dict) -> None:
    aggs = rig.get("aggregators", [])
    con.executemany(
        "INSERT INTO aggregators(id, seq, name) VALUES (?,?,?)",
        [(a["id"], i, a["name"]) for i, a in enumerate(aggs)])
    con.executemany(
        "INSERT OR IGNORE INTO aggregator_deps VALUES (?,?)",
        sorted({(a["id"], d) for a in aggs for d in a.get("depends_on_ids", [])}))
    con.executemany(
        "INSERT OR IGNORE INTO aggregator_evidence VALUES (?,?,?)",
        [(a["id"], e, j) for a in aggs
         for j, e in enumerate(a.get("evidence_ids", []))])


def _write_runners(con, rig: dict) -> None:
    runners = rig.get("runners", [])
    con.executemany(
        "INSERT INTO runners(id, seq, name, arguments) VALUES (?,?,?,?)",
        [(r["id"], i, r["name"], json.dumps(r.get("arguments", [])))
         for i, r in enumerate(runners)])
    con.executemany(
        "INSERT OR IGNORE INTO runner_deps VALUES (?,?)",
        sorted({(r["id"], d) for r in runners for d in r.get("depends_on_ids", [])}))
    con.executemany(
        "INSERT OR IGNORE INTO runner_evidence VALUES (?,?,?)",
        [(r["id"], e, j) for r in runners
         for j, e in enumerate(r.get("evidence_ids", []))])


def _write_tests(con, rig: dict) -> None:
    tests = rig.get("test_definitions", [])
    con.executemany(
        "INSERT INTO tests(id, seq, name, framework, executable_id) VALUES (?,?,?,?,?)",
        [(t["id"], i, t["name"], t.get("test_framework", ""),
          t.get("test_executable_component_id", ""))
         for i, t in enumerate(tests)])
    con.executemany(
        "INSERT OR IGNORE INTO test_deps VALUES (?,?,?)",
        [(t["id"], d, j) for t in tests
         for j, d in enumerate(t.get("depends_on_ids", []))])
    con.executemany(
        "INSERT OR IGNORE INTO test_covers VALUES (?,?,?)",
        [(t["id"], c, j) for t in tests
         for j, c in enumerate(t.get("components_being_tested_ids", []))])
    con.executemany(
        "INSERT OR IGNORE INTO test_files VALUES (?,?,?)",
        [(t["id"], f, j) for t in tests
         for j, f in enumerate(t.get("source_files", []))])
    con.executemany(
        "INSERT OR IGNORE INTO test_evidence VALUES (?,?,?)",
        [(t["id"], e, j) for t in tests
         for j, e in enumerate(t.get("evidence_ids", []))])


def _write_packages(con, rig: dict) -> None:
    con.executemany(
        "INSERT OR IGNORE INTO packages VALUES (?,?,?,?)",
        [(p["id"], p["name"],
          p.get("package_manager", {}).get("name", ""),
          p.get("package_manager", {}).get("package_name", ""))
         for p in rig.get("external_packages", [])])


def _write_entrypoints(con, rig: dict) -> None:
    con.executemany(
        "INSERT INTO entrypoints(seq, component_id) VALUES (?,?)",
        [(i, e) for i, e in enumerate(rig.get("entrypoints", []))])


def _fts5_available(con) -> bool:
    try:
        con.execute("SELECT fts5(1)")
        return True
    except sqlite3.Error:
        return False
