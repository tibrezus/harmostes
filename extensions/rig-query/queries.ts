/**
 * rig-query — pure query layer over a project's rig.db (ADR-0009).
 *
 * rig.db is the RIG standard's canonical SQLite artifact (see
 * plugins/rig-emit/rig/db.py): components, dependency edges, files and
 * symbols with an FTS5 index. This module turns targeted questions —
 * "where does this symbol live", "what does this component depend on" —
 * into a few hundred tokens of precise, evidence-backed answer, replacing
 * the grep expeditions that dominated review sessions (#337: ~13 of 33
 * tool calls were code archaeology).
 *
 * No pi imports here: this module is directly drivable by node
 * (--experimental-strip-types) for smoke tests, and index.ts wraps it as
 * a pi tool.
 */
import { DatabaseSync } from "node:sqlite";
import { existsSync, readFileSync } from "node:fs";

type Row = Record<string, unknown>;

/**
 * Candidate locations for rig.db, in priority order. The library knows
 * explicit + env + cwd-relative candidates only — container paths are the
 * RUNTIME contract and live in index.ts (#338 r3: a reusable module must not
 * bake in a mount layout whose graph may be from another revision).
 */
export function resolveRigDbCandidates(explicit?: string, extra: string[] = []): string[] {
	// Hermeticity hatch (r23 P8): RIG_DB_CANDIDATES replaces the walk with an
	// exact list ("" = suppression) — the seam tests use to be filesystem-
	// independent inside a worker pod. It outranks confinement by design:
	// it is how execute-level provenance states become testable at all.
	const override = process.env.RIG_DB_CANDIDATES !== undefined
		? process.env.RIG_DB_CANDIDATES.split(",").filter(Boolean)
		: null;
	if (override !== null) return override;
	// Freshness confinement (r21 F3): with a reviewed SHA the walk is the
	// caller's sanctioned extras ONLY — pod-supplied RIG_DB may point at the
	// wiki's synced copy, an artifact reviews must not consume.
	if (process.env.RIG_EXPECTED_SHA !== undefined) return extra;
	// NO cwd-relative or bare candidates: a graph resolved out of the working
	// tree is PR content answering as authoritative architecture (#338 r20
	// S1). The default walk is explicit → RIG_DB → the caller's extras.
	return [
		explicit,
		process.env.RIG_DB,
		...extra,
	].filter((p): p is string => Boolean(p));
}

/**
 * Resolve the graph to answer from. ORDER IS THE FRESHNESS RULE (#338 r11
 * C1): prepare's SHA-exact emit (the extras) must outrank any cwd-relative
 * or bare "rig.db" — a stray graph in the workdir must never shadow the
 * reviewed checkout's emit. Callers report the full walk in details.probed.
 */
export type ProvenanceState = "verified" | "mismatch" | "unchecked" | "malformed" | "absent" | "absent-refusal";

/**
 * Verify the graph's provenance stamp against an expected reviewed SHA.
 * Pure and injectable so tests can drive every state from tmpdir fixtures
 * without container paths (#338 r19/r21).
 *
 * States: verified (exact, case-insensitive equality with the expectation),
 * mismatch (stamped but different — callers must refuse), malformed (stamp
 * content is not a sha token), unchecked (stamped, nothing to verify
 * against), absent (no stamp, nothing expected), absent-refusal (no stamp
 * but a SHA WAS expected — callers must refuse: prepare died before
 * stamping, so the graph is unverifiable, r23 P4).
 */
export function verifyProvenance(
	stampPath: string,
	expectedSha: string | undefined,
	readStamp: (p: string) => string | null = (p) => {
		try {
			return readFileSync(p, "utf8").trim();
		} catch {
			return null;
		}
	},
): { rigSha: string; state: ProvenanceState } {
	const raw = readStamp(stampPath);
	if (raw === null) {
		return expectedSha ? { rigSha: "absent", state: "absent-refusal" } : { rigSha: "absent", state: "absent" };
	}
	if (!/^[0-9a-f]{7,40}$/i.test(raw)) return { rigSha: "malformed", state: "malformed" };
	const rigSha = raw.toLowerCase();
	if (!expectedSha) return { rigSha, state: "unchecked" };
	const exp = expectedSha.toLowerCase();
	const match = rigSha.length === exp.length ? rigSha === exp : exp.startsWith(rigSha) || rigSha.startsWith(exp);
	return match ? { rigSha, state: "verified" } : { rigSha, state: "mismatch" };
}

export function resolveRigDb(explicit?: string, extra: string[] = []): string | null {
	for (const p of resolveRigDbCandidates(explicit, extra)) {
		if (existsSync(p)) return p;
	}
	return null;
}

const MAX_RESULT_CHARS = 6000;
/** Overview renders the WHOLE graph on a mid-size repo (~2.7k chars at 23 components) before any row cap applies. */
const OVERVIEW_ROWS = 64; // ~1.5k tokens — sized for the grouped overview (a 23-component repo renders ~2.2k chars; r6's edge grouping grew it)

// Truncation is COUNTED, not sniffed: builders register elided rows/edges in
// a shared stats object and render the marker from the same number — `truncated`
// can never disagree with the text (a doc containing "…+" used to flip it).
const MORE_ROWS = (n: number): string => `  …+${n} more`;
const MORE_HITS = " …more hits — refine the term";
const MAX_ROWS = 12;
/** Search returns exactly this many hits; say so when the pool was bigger. */
const SEARCH_LIMIT = 8;

function cap(text: string, stats: { more: number }): string {
	if (text.length <= MAX_RESULT_CHARS) return text;
	stats.more += 1; // char-cut is truncation too
	return (
		text.slice(0, MAX_RESULT_CHARS) +
		`\n… [output exceeded the ${MAX_RESULT_CHARS}-char budget and was cut — drill down (component/search/deps) instead of repeating this query]`
	);
}

function row(v: unknown): string {
	if (v === null || v === undefined) return "";
	if (typeof v === "object") {
		// node:sqlite rows are null-prototype objects — String() would throw.
		return Object.values(v as Record<string, unknown>).map((x) => (x == null ? "" : String(x))).join(":");
	}
	return String(v);
}

/** One-line summary of a symbol row. */
function symLine(file: unknown, line: unknown, kind: unknown, name: unknown, sig: unknown, doc: unknown): string {
	const parts = [`    ${row(file)}:${row(line)}  ${row(kind)} ${row(name)}`.replace(/  +/g, "  ")];
	const s = row(sig).replace(/\s+/g, " ");
	if (s && s !== row(name)) parts.push(`      ${s.slice(0, 140)}`);
	const d = row(doc).replace(/\s+/g, " ");
	if (d) parts.push(`      ‹${d.slice(0, 120)}›`);
	return parts.join("\n");
}

export interface RigParams {
	command: "overview" | "component" | "search" | "files" | "deps";
	target?: string;
	reverse?: boolean;
}

/**
 * Open a rig.db read-only. Asserts the producer's schema version — a silent
 * mismatch would surface as "no such column" errors indistinguishable from a
 * missing graph (#338 r1). Read-only opens also require non-WAL journals;
 * this holds because write_db sets journal_mode=DELETE — if the emitter ever
 * switches to WAL, read-only opens fail with SQLITE_READONLY_RECOVERY.
 */
export function openRig(path: string): DatabaseSync {
	const db = new DatabaseSync(path, { readOnly: true });
	let version = "";
	let failed = false;
	try {
		try {
			const r = db.prepare("SELECT value FROM meta WHERE key = 'db_schema_version'").get() as { value?: unknown } | undefined;
			version = r && r.value != null ? String(r.value) : "";
		} catch (e) {
			failed = true;
			throw new Error(`${path} is not a rig.db (meta unreadable: ${String(e)})`);
		}
		if (version !== "1") {
			failed = true;
			throw new Error(`rig.db v${version || "?"} at ${path} — this build speaks schema v1; if this run should have a usable graph, check the prepare phase logs / update the worker image`);
		}
	} finally {
		// BOTH failure branches close the handle — a wrong path must not leak an
		// fd per attempt in a long-lived RPC session (#338 r6 M1, measured:
		// 5 failed opens took /proc/self/fd 18 → 23).
		if (failed) db.close();
	}
	return db;
}

/**
 * Dispatch one rig query. Returns the agent-facing text plus a `truncated`
 * flag — row-level truncation (…+N more) must be visible in telemetry, not
 * only in prose (#338 r3 pillar 7).
 */
export function rigQuery(db: DatabaseSync, p: RigParams): { text: string; truncated: boolean; resolved: boolean } {
	// `resolved` semantics (pinned r23 P7): "the graph answered this command" —
	// true for hits, structured no-match answers, and usage hints alike (every
	// command sets it); the only false rows are wrapper-level absence and
	// provenance refusals, where no query ran. NOT "target hit" — that is what
	// the text says.
	const stats = { more: 0, resolved: false };
	const run = (fn: (s: { more: number; resolved: boolean }) => string): { text: string; truncated: boolean; resolved: boolean } => {
		const text = cap(fn(stats), stats);
		return { text, truncated: stats.more > 0 || text.length > MAX_RESULT_CHARS, resolved: stats.resolved };
	};
	// Not exposed (deliberate, revisit on demand): calls/packages/artifacts/
	// tests tables are populated by the producer but no subcommand reads them.
	switch (p.command) {
		case "overview":
			return run((s) => overview(db, s));
		case "component":
			return run((s) => component(db, p.target, s));
		case "search":
			return run((s) => search(db, p.target, s));
		case "files":
			return run((s) => files(db, p.target, s));
		case "deps":
			return run((s) => deps(db, p.target, p.reverse === true, s));
		default:
			throw new Error(`unknown rig command ${JSON.stringify((p as { command?: string }).command)}`);
	}
}

function meta(db: DatabaseSync, key: string): string {
	try {
		return row(db.prepare("SELECT value FROM meta WHERE key = ?").get(key));
	} catch {
		return "";
	}
}

function overview(db: DatabaseSync, stats: { more: number }): string {
	stats.resolved = true;
	const comps = db.prepare("SELECT id, name, type, language, entrypoint FROM components ORDER BY seq").all() as Row[];
	if (comps.length === 0) return "rig.db has no components — graph empty.";
	const fileCounts = new Map<string, number>();
	for (const r of db.prepare("SELECT component_id cid, COUNT(*) n FROM component_files GROUP BY component_id").all() as Row[]) {
		fileCounts.set(row(r.cid), Number(r.n));
	}
	const symCount = db.prepare("SELECT COUNT(*) n FROM symbols").get();
	const lines: string[] = [];
	const proj = meta(db, "repo_name") || meta(db, "project");
	lines.push(
		`graph: ${proj || "(unnamed graph — no repo_name meta)"} — ${comps.length} components, ${row(symCount?.n)} symbols`,
	);
	for (const c of comps.slice(0, OVERVIEW_ROWS)) {
		const flags: string[] = [];
		if (Number(c.entrypoint) === 1) flags.push("entry");
		lines.push(
			`- ${short(row(c.name) || row(c.id))} — ${row(c.type) || "component"}${flags.length ? ` [${flags.join(",")}]` : ""} (${fileCounts.get(row(c.id)) ?? 0} files)`,
		);
	}
	if (comps.length > OVERVIEW_ROWS) {
		stats.more += comps.length - OVERVIEW_ROWS;
		lines.push(MORE_ROWS(comps.length - OVERVIEW_ROWS));
	}
	const edges = db.prepare("SELECT src, dst FROM deps ORDER BY src, dst").all() as Row[];
	if (edges.length) {
		// Grouped by source — one line per component instead of raw pairs (a
		// 23-component repo has 63 edges; flat rendering elided 51 of them and
		// pushed agents back to grep — #338 r6 M3).
		const names = namesById(db);
		const bySrc = new Map<string, string[]>();
		for (const e of edges) {
			const s = row(e.src);
			bySrc.set(s, [...(bySrc.get(s) ?? []), nameOf(names, row(e.dst))]);
		}
		const rendered = [...bySrc.entries()].map(([s, dsts]) => `${names.get(s) ?? short(s)} → ${dsts.join(", ")}`);
		lines.push(`deps (${edges.length} edges):\n  ${rendered.slice(0, OVERVIEW_ROWS).join("\n  ")}`);
		const shownEdgeCount = shownEdges(bySrc, OVERVIEW_ROWS);
		if (edges.length > shownEdgeCount) {
			// The marker carries the UNIT (edges) and says so once — "…+51 more
			// edges", not "more more" (#338 r9/r10).
			stats.more += edges.length - shownEdgeCount;
			lines.push(`  …+${edges.length - shownEdgeCount} more edges — rig deps <component> for one in full`);
		}
	}
	// The footer renders BEFORE any truncation tail: a cap-cut must never drop
	// the pointer to the next command (#338 r9 B1).
	lines.push('drill down: rig component <name-tail> | rig search \'<term>\' | rig deps <name-tail>');
	return lines.join("\n");
}

/** Edges covered by the first `limit` grouped sources (for honest elision counts). */
function shownEdges(bySrc: Map<string, string[]>, limit: number): number {
	let shown = 0;
	let i = 0;
	for (const dsts of bySrc.values()) {
		if (i >= limit) break;
		shown += dsts.length;
		i += 1;
	}
	return shown;
}

function nameOf(names: Map<string, string>, id: string): string {
	return names.get(id) ?? short(id);
}

/** id → display name map for edge endpoints (ids are opaque comp-N). */
function namesById(db: DatabaseSync): Map<string, string> {
	const names = new Map<string, string>();
	for (const r of db.prepare("SELECT id, name FROM components").all() as Row[]) {
		names.set(row(r.id), short(row(r.name) || row(r.id)));
	}
	return names;
}

/** Component ids are usually long paths (e.g. example.com/repo/cmd/foo) — shorten to the tail. */
function short(id: string): string {
	const parts = id.split("/").filter(Boolean);
	return parts.length > 2 ? parts.slice(-2).join("/") : id;
}

function resolveComponent(db: DatabaseSync, target: string | undefined): Array<{ id: string; name: string; type: unknown }> {
	if (!target) return [];
	// The advertised syntax is a trailing-* prefix term ("worker*") and a
	// leading-* wildcard ("*worker") — both reduce to the suffix tail match
	// this function already owns (#338 r21 Finding 1).
	target = target.replace(/^\*+/, "").replace(/\*+$/, "").trim();
	if (!target) return [];
	const byId = db.prepare("SELECT id, name, type FROM components WHERE id = ?").all(target) as Row[];
	if (byId.length === 1) return [byId[0] as { id: string; name: string; type: unknown }];
	const byName = db.prepare("SELECT id, name, type FROM components WHERE name = ?").all(target) as Row[];
	if (byName.length === 1) return [byName[0] as { id: string; name: string; type: unknown }];
	// Agents address components by tail ("internal/agent", "harmostes-worker") —
	// component ids are opaque (comp-N), names are import paths. Ambiguity is an
	// answer, not a coin flip: a review tool must never print a confident blast
	// radius for the wrong component (#338 r1).
	// LIMIT 3: two matches are "2 match"; a third means the honest answer is
	// "at least 3" — never print a count derived from a capped list (#338 r2 4.3).
	const esc = target.replace(/\\/g, "\\\\").replace(/[%_]/g, "\\$&");
	return db
		.prepare(
			"SELECT id, name, type FROM components WHERE id LIKE ? ESCAPE '\\' OR name LIKE ? ESCAPE '\\' ORDER BY seq LIMIT 3",
		)
		.all(`%${esc}`, `%${esc}`) as Array<{ id: string; name: string; type: unknown }>;
}

/** Shared ambiguity renderer — a confident answer for the wrong component is worse than an error. */
function ambiguous(matches: Array<{ id: string; name: string; type: unknown }>): string | null {
	if (matches.length <= 1) return null;
	const count = matches.length >= 3 ? "at least 3" : String(matches.length);
	return (
		`ambiguous — ${count} components match, be more specific:\n` +
		matches.map((c) => `  ${short(row(c.name) || c.id)} (${row(c.type) || "component"})`).join("\n")
	);
}

function component(db: DatabaseSync, target: string | undefined, stats: { more: number }): string {
	stats.resolved = true;
	const matches = resolveComponent(db, target);
	if (matches.length === 0) return `no component matching ${JSON.stringify(target ?? "")} — use rig overview for names.`;
	const amb = ambiguous(matches);
	if (amb) return amb;
	const c = matches[0];
	const lines: string[] = [`component ${short(row(c.name) || c.id)} (${row(c.type) || "component"}, id ${c.id})`];
	const names = namesById(db);
	const label = (id: unknown): string => names.get(row(id)) ?? short(row(id));
	const fileTotal = Number(db.prepare("SELECT COUNT(*) n FROM component_files WHERE component_id = ?").get(c.id)?.n ?? 0);
	for (const f of db
		.prepare(
			"SELECT f.path, f.lines FROM component_files cf JOIN files f ON f.path = cf.path WHERE cf.component_id = ? ORDER BY cf.seq LIMIT ?",
		)
		.all(c.id, MAX_ROWS) as Row[]) {
		lines.push(`  ${row(f.path)} (${row(f.lines)}L)`);
	}
	if (fileTotal > MAX_ROWS) {
		stats.more += fileTotal - MAX_ROWS;
		lines.push(`${MORE_ROWS(fileTotal - MAX_ROWS)} files — narrow with rig files '<glob>' (* / ? wildcards)`);
	}
	const edgeTotal = (d: string): number =>
		Number(db.prepare(`SELECT COUNT(*) n FROM deps WHERE ${d} = ?`).get(c.id)?.n ?? 0);
	const out = db.prepare("SELECT dst FROM deps WHERE src = ? ORDER BY dst LIMIT ?").all(c.id, MAX_ROWS) as Row[];
	if (out.length) {
		const total = edgeTotal("src");
		if (total > MAX_ROWS) stats.more += total - MAX_ROWS;
		lines.push(`deps out: ${out.map((e) => label(e.dst)).join(", ")}${total > MAX_ROWS ? MORE_ROWS(total - MAX_ROWS) : ""}`);
	}
	const inc = db.prepare("SELECT src FROM deps WHERE dst = ? ORDER BY src LIMIT ?").all(c.id, MAX_ROWS) as Row[];
	if (inc.length) {
		const total = edgeTotal("dst");
		if (total > MAX_ROWS) stats.more += total - MAX_ROWS;
		lines.push(
			`deps in (reverse blast radius): ${inc.map((e) => label(e.src)).join(", ")}${total > MAX_ROWS ? MORE_ROWS(total - MAX_ROWS) : ""}`,
		);
	}
	return lines.join("\n");
}

function search(db: DatabaseSync, target: string | undefined, stats: { more: number }): string {
	stats.resolved = true; // same semantics as every command: the graph answered
	if (!target || !target.trim()) {
		return "search: give a symbol term (prefix match supported: 'handler*') — a bare '*' has no answer.";
	}
	// The documented syntax carries a trailing *; LIKE arms must not inherit it
	// ("executor*" → "executor*%" matches nothing) — strip it here, keep it for
	// the FTS phrase (#338 r2 4.1). Emptiness is checked AFTER the strip: a
	// bare '*' is a degenerate term, not "match everything" (#338 r8 F7).
	const term = target.trim().replace(/"/g, "").replace(/\*+$/, "");
	if (!term) {
		return "search: a bare '*' matches everything — give a symbol stem (e.g. 'executor').";
	}
	const likeTerm = term.replace(/\\/g, "\\\\").replace(/[%_]/g, "\\$&"); // $& = the matched char ($0 does not exist in JS and injected a literal "$0", hiding every _-bearing symbol — #338 r7 B1)

	// Components FIRST and OUTSIDE the symbol budget (r18 F1): overview prints
	// tails ("cmd/worker"), so search must resolve THAT handle — suffix
	// semantics, shared with resolveComponent. Displacement is impossible by
	// construction: components never consume the symbol budget.
	const comps = db
		.prepare(
			"SELECT id, name, type FROM components WHERE name LIKE '%' || ? ESCAPE '\\' ORDER BY seq LIMIT 3",
		)
		.all(likeTerm) as Row[];
	let compLines: string[] = [];
	if (comps.length > 0) {
		compLines = [`components matching "${term}":`];
		for (const c of comps) {
			compLines.push(`  [component] ${short(row(c.name) || row(c.id))} (${row(c.type) || "component"}) — rig component '${short(row(c.name))}'`);
		}
	}

	const lines: string[] = [];

	// Symbols: name-prefix arm first (camelCase-honest), then FTS, then the
	// sweep — each bounded by the REMAINING budget (r18 F3).
	const hits = new Map<string, Row>();
	let sweepExhausted = false;
	const pre = db
		.prepare("SELECT file, line, kind, name, signature, doc FROM symbols WHERE name LIKE ? || '%' ESCAPE '\\' ORDER BY seq LIMIT ?")
		.all(likeTerm, SEARCH_LIMIT) as Row[];
	for (const r of pre) hits.set(`sym:${row(r.file)}:${row(r.line)}:${row(r.name)}`, r);
	if (hits.size < SEARCH_LIMIT) {
		try {
			const rows = db
				.prepare(
					`SELECT s.file, s.line, s.kind, s.name, s.signature, s.doc
					 FROM symbols_fts f JOIN symbols s ON s.seq = f.rowid
					 WHERE symbols_fts MATCH ? ORDER BY rank LIMIT ?`,
				)
				.all(term.includes(" ") ? `"${term}"` : `${term}*`, SEARCH_LIMIT - hits.size) as Row[];
			for (const r of rows) {
				if (hits.size >= SEARCH_LIMIT) break;
				hits.set(`sym:${row(r.file)}:${row(r.line)}:${row(r.name)}`, r);
			}
		} catch {
			// FTS unavailable — the sweep below still answers.
		}
	}
	if (hits.size < SEARCH_LIMIT) {
		const like = `%${likeTerm}%`;
		const likeRows = db
			.prepare(
				`SELECT file, line, kind, name, signature, doc FROM symbols
				 WHERE name LIKE ? ESCAPE '\\' OR signature LIKE ? ESCAPE '\\' OR doc LIKE ? ESCAPE '\\' ORDER BY seq LIMIT ?`,
			)
			.all(like, like, like, SEARCH_LIMIT - hits.size + 1) as Row[];
		sweepExhausted = likeRows.length <= SEARCH_LIMIT - hits.size;
		for (const r of likeRows) {
			if (hits.size >= SEARCH_LIMIT) break;
			hits.set(`sym:${row(r.file)}:${row(r.line)}:${row(r.name)}`, r);
		}
	}

	// Honest completeness: a full symbol pool + a short sweep proves the corpus
	// is exhausted; otherwise one existence probe decides the marker. Component
	// rows never participate (#338 r18 F3).
	let more = "";
	if (hits.size >= SEARCH_LIMIT && !sweepExhausted) {
		const like = `%${likeTerm}%`;
		const probe = db
			.prepare(
				`SELECT 1 FROM symbols WHERE name LIKE ? ESCAPE '\\' OR signature LIKE ? ESCAPE '\\' OR doc LIKE ? ESCAPE '\\' LIMIT ?`,
			)
			.all(like, like, like, hits.size + 1) as Row[];
		if (probe.length > hits.size) {
			more = MORE_HITS;
			stats.more += probe.length - hits.size;
		}
	}
	if (hits.size === 0 && comps.length === 0) {
		return `no symbols or components matching "${term}" — try a shorter stem with * (rig search 'handler*').`;
	}
	const symbolLines = [...hits.values()].map((r) => symLine(r.file, r.line, r.kind, r.name, r.signature, r.doc));
	return cap([...compLines, `symbols matching "${term}" (${hits.size}):`, ...symbolLines, ...(more ? [more] : [])].join("\n"), stats);
}

function files(db: DatabaseSync, target: string | undefined, stats: { more: number }): string {
	stats.resolved = true;
	if (!target) return "files: give a path glob — * or % = any run, ? = one char (e.g. 'internal/graph/*', '*dispatch*').";
	// Glob dialect (r18 F4 / r20 driver-1): * or % = any run (the storage
	// dialect is accepted as an alias — the help text advertises both), ? = one
	// char. The alias runs FIRST; the remaining literals (\ and _) are escaped
	// before the operators translate, so a documented wildcard can never be
	// silently re-interpreted and a literal can never match.
	const glob = target.replace(/%/g, "*");
	const esc = glob.replace(/\\/g, "\\\\").replace(/_/g, "\\_");
	let like = esc.replace(/\?/g, "_").replace(/\*/g, "%");
	// The %…% convenience wrap applies ONLY to operator-free substrings — a
	// caller's explicit ? is one char, not substring-anywhere (#338 r21 F2).
	const hadOperator = /[*?%]/.test(target);
	if (!hadOperator) like = `%${like}%`;
	const total = Number(db.prepare("SELECT COUNT(*) n FROM files WHERE path LIKE ? ESCAPE '\\'").get(like)?.n ?? 0);
	const rows = db
		.prepare("SELECT path, lines, language, component_id FROM files WHERE path LIKE ? ESCAPE '\\' ORDER BY path LIMIT ?")
		.all(like, MAX_ROWS) as Row[];
	if (rows.length === 0) return `no files matching "${target}".`;
	if (total > MAX_ROWS) stats.more += total - MAX_ROWS;
	const names = namesById(db);
	return [
		`files matching "${target}":`,
		...rows.map((r) => `  ${row(r.path)} (${row(r.lines)}L${r.component_id ? `, ${names.get(row(r.component_id)) ?? short(row(r.component_id))}` : ""})`),
		...(total > MAX_ROWS ? [MORE_ROWS(total - MAX_ROWS)] : []),
	].join("\n");
}

function deps(db: DatabaseSync, target: string | undefined, reverse: boolean, stats: { more: number }): string {
	stats.resolved = true;
	const matches = resolveComponent(db, target);
	if (matches.length === 0) return `no component matching ${JSON.stringify(target ?? "")} — use rig overview for names.`;
	const amb = ambiguous(matches);
	if (amb) return amb;
	const c = matches[0];
	const names = namesById(db);
	const label = (id: unknown): string => names.get(row(id)) ?? short(row(id));
	const peerCol = reverse ? "src" : "dst"; // the column to DISPLAY
	const filterCol = reverse ? "dst" : "src"; // the column to MATCH c.id on
	const total = Number(db.prepare(`SELECT COUNT(*) n FROM deps WHERE ${filterCol} = ?`).get(c.id)?.n ?? 0);
	const rows = db
		.prepare(`SELECT ${peerCol} AS peer FROM deps WHERE ${filterCol} = ? ORDER BY ${peerCol} LIMIT ?`)
		.all(c.id, MAX_ROWS) as Row[];
	const arrow = reverse ? "←" : "→";
	if (rows.length === 0) return `${label(c.id)} — no ${reverse ? "incoming (reverse)" : "outgoing"} dependencies.`;
	if (total > MAX_ROWS) stats.more += total - MAX_ROWS;
	return [
		`${label(c.id)} ${arrow}`,
		...rows.map((r) => `  ${arrow} ${label(r.peer)}`),
		...(total > MAX_ROWS ? [MORE_ROWS(total - MAX_ROWS)] : []),
	].join("\n");
}
