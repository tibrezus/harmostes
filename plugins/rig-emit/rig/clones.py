"""Near-clone detection — MinHash + LSH over symbol bodies.

Stores near-clone edges in the `similar` table: pairs of exported symbols
whose bodies are near-identical by Jaccard similarity over k=5 token
shingles. Language-agnostic by construction — bodies are sliced from
source using the line ranges recorded in `symbols`, so every language the
extractors cover gets clone detection for free.

Determinism: blake2b with fixed per-permutation keys, sorted shingles,
sorted output rows. Identical input → identical `similar` table → stable
canonical hash.

Cost guards (the RIG build is the only place this runs — no extra CI job):
bodies under _MIN_BODY_LINES are skipped (short functions collide by
idiom, not by copying); LSH banding finds candidate pairs in ~O(n) instead
of O(n²); at most _MAX_MATCHES best matches per symbol are stored.
"""

from __future__ import annotations

import hashlib
import re
import sqlite3
from pathlib import Path

_MIN_BODY_LINES = 8     # skip bodies shorter than this (idiom noise)
_K = 5                  # shingle width in tokens
_PERMS = 128            # MinHash permutations
_BANDS = 16             # LSH bands …
_ROWS = 8               # … × rows per band = _PERMS
_J_CROSS = 0.80         # Jaccard threshold, cross-component pairs
_J_SAME = 0.70          # Jaccard threshold, same component or same file
_MAX_MATCHES = 3        # stored matches per symbol (either direction)

_TOKEN_RE = re.compile(r"[A-Za-z_][A-Za-z0-9_]*|\d+")


def _shingles(body: str) -> set[bytes]:
    """k-token shingles of a symbol body, whitespace-normalized."""
    toks = _TOKEN_RE.findall(body)
    if len(toks) < _K:
        return set()
    return {" ".join(toks[i:i + _K]).encode()
            for i in range(len(toks) - _K + 1)}


def _minhash(shingles: set[bytes]) -> tuple[bytes, ...]:
    """Per-permutation minimum hash. Fixed keys → deterministic."""
    return tuple(
        min(hashlib.blake2b(s, digest_size=8, key=bytes([i])).digest()
            for s in shingles)
        for i in range(_PERMS))


def _scope(file_a: str, comp_a: str | None,
           file_b: str, comp_b: str | None) -> str:
    if file_a == file_b:
        return "same-file"
    if comp_a and comp_a == comp_b:
        return "same-component"
    return "cross-component"


def _threshold(scope: str) -> float:
    return _J_SAME if scope in ("same-file", "same-component") else _J_CROSS


def compute_and_store(db_path: Path, source_root: Path) -> int:
    """Detect near-clone pairs among the DB's symbols and store them.

    Reads symbols (+ line ranges) and the file→component map from the DB,
    slices bodies from `source_root`, and writes the `similar` table.
    Returns the number of edges stored. Never raises on missing sources —
    unreadable files simply contribute no bodies (rows keep line data).
    """
    from rig import db as rig_db

    db_path = Path(db_path)
    source_root = Path(source_root)
    con = sqlite3.connect(db_path)
    con.row_factory = sqlite3.Row
    try:
        syms = con.execute(
            "SELECT s.seq, s.file, s.name, s.line, s.line_end, "
            "       f.component_id "
            "FROM symbols s LEFT JOIN files f ON f.path = s.file "
            "ORDER BY s.seq").fetchall()
    finally:
        con.close()

    # Slice bodies — one read per file.
    file_lines: dict[str, list[str]] = {}
    entries: list[dict] = []
    for s in syms:
        start, end = s["line"], s["line_end"] or s["line"]
        if start is None or end - start + 1 < _MIN_BODY_LINES:
            continue
        lines = file_lines.get(s["file"])
        if lines is None:
            path = source_root / s["file"]
            try:
                lines = path.read_text(encoding="utf-8",
                                       errors="replace").split("\n")
            except OSError:
                lines = []
            file_lines[s["file"]] = lines
        body = "\n".join(lines[start - 1:end])
        sh = _shingles(body)
        if not sh:
            continue
        entries.append({
            "key": f"{s['file']}:{s['name']}",
            "file": s["file"],
            "component_id": s["component_id"],
            "shingles": sh,
            "sig": _minhash(sh),
        })

    # LSH: bucket by (band, band-hash) — candidates share ≥ 1 band.
    buckets: dict[tuple[int, bytes], list[int]] = {}
    for idx, e in enumerate(entries):
        for b in range(_BANDS):
            band = b"\x00".join(e["sig"][b * _ROWS:(b + 1) * _ROWS])
            buckets.setdefault((b, band), []).append(idx)

    candidates: set[tuple[int, int]] = set()
    for idxs in buckets.values():
        if len(idxs) < 2:
            continue
        for i in range(len(idxs)):
            for j in range(i + 1, len(idxs)):
                if entries[i]["key"] == entries[j]["key"]:
                    continue  # same file:name mapped twice (shared file) — no self-edges
                candidates.add((min(idxs[i], idxs[j]),
                                max(idxs[i], idxs[j])))

    # Exact Jaccard on candidates; keep per-symbol top-K by (jaccard, key).
    best: dict[str, list[tuple[float, str]]] = {}
    scopes: dict[tuple[str, str], str] = {}
    for i, j in candidates:
        a, b = entries[i], entries[j]
        inter = len(a["shingles"] & b["shingles"])
        union = len(a["shingles"] | b["shingles"])
        if not union:
            continue
        jac = inter / union
        scope = _scope(a["file"], a["component_id"], b["file"], b["component_id"])
        if jac < _threshold(scope):
            continue
        scopes[(a["key"], b["key"])] = scope
        scopes[(b["key"], a["key"])] = scope
        for src, dst in ((a["key"], b["key"]), (b["key"], a["key"])):
            best.setdefault(src, []).append((-jac, dst))

    # Canonical direction: lexicographically smaller endpoint is `src`.
    rows: dict[tuple[str, str], dict] = {}
    for src, matches in best.items():
        matches.sort()
        for neg_jac, dst in matches[:_MAX_MATCHES]:
            left, right = sorted([src, dst])
            rows[(left, right)] = {
                "src": left, "dst": right,
                "jaccard": round(-neg_jac, 4),
                "scope": scopes[(src, dst)],
            }

    ordered = [rows[k] for k in sorted(rows)]
    rig_db.add_similar(db_path, ordered)
    return len(ordered)
