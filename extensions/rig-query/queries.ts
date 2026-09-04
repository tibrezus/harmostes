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
import { existsSync } from "node:fs";
import { resolve } from "node:path";

type Row = Record<string, unknown>;

/**
 * Candidate locations for rig.db, in priority order. The library knows
 * explicit + env + cwd-relative candidates only — container paths are the
 * RUNTIME contract and live in index.ts (#338 r3: a reusable module must not
 * bake in a mount layout whose graph may be from another revision).
 */
export function resolveRigDb(explicit?: string, cwd?: string, extra: string[] = []): string | null {
	const candidates = [
		explicit,
		process.env.RIG_DB,
		cwd ? resolve(cwd, "rig.db") : undefined,
		...extra,
		"rig.db",
	].filter((p): p is string => Boolean(p));
	for (const p of candidates) {
		if (existsSync(p)) return p;
	}
	return null;
}

const MAX_RESULT_CHARS = 4000; // ~1k tokens — sized for a full one-screen overview (a 23-component repo renders ~1.7k chars)

// Truncation markers, single-sourced: builders render them, rigQuery detects
// them for the `truncated` telemetry flag. Rewording one must reword both —
// a regex over prose was how the partial-answer signal could silently die
// (#338 r3 pillar 7).
const MORE_ROWS = (n: number): string => `  …+${n} more`;
const MORE_HITS = " …more hits — refine the term";
const rowTruncated = (text: string): boolean => text.includes("…+") || text.includes(MORE_HITS);
const MAX_ROWS = 12;
/** Search returns exactly this many hits; say so when the pool was bigger. */
const SEARCH_LIMIT = 8;

function cap(text: string): string {
	if (text.length <= MAX_RESULT_CHARS) return text;
	return (
		text.slice(0, MAX_RESULT_CHARS) +
		`\n… [truncated — refine the query; full answer exceeds ${MAX_RESULT_CHARS} chars]`
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
			throw new Error(`rig.db v${version || "?"} at ${path} — this extension speaks v1; regenerate the graph (emit-rig.py)`);
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
export function rigQuery(db: DatabaseSync, p: RigParams): { text: string; truncated: boolean } {
	const run = (fn: () => string): { text: string; truncated: boolean } => {
		const text = cap(fn());
		return { text, truncated: rowTruncated(text) || text.length >= MAX_RESULT_CHARS };
	};
	switch (p.command) {
		case "overview":
			return run(() => overview(db));
		case "component":
			return run(() => component(db, p.target));
		case "search":
			return run(() => search(db, p.target));
		case "files":
			return run(() => files(db, p.target));
		case "deps":
			return run(() => deps(db, p.target, p.reverse === true));
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

function overview(db: DatabaseSync): string {
	const comps = db.prepare("SELECT id, name, type, language, entrypoint FROM components ORDER BY seq").all() as Row[];
	if (comps.length === 0) return "rig.db has no components — graph empty.";
	const fileCounts = new Map<string, number>();
	for (const r of db.prepare("SELECT component_id cid, COUNT(*) n FROM component_files GROUP BY component_id").all() as Row[]) {
		fileCounts.set(row(r.cid), Number(r.n));
	}
	const symCount = db.prepare("SELECT COUNT(*) n FROM symbols").get();
	const lines: string[] = [];
	const proj = meta(db, "repo_name") || meta(db, "project");
	lines.push(`graph: ${proj || "project"} — ${comps.length} components, ${row(symCount?.n)} symbols`);
	for (const c of comps) {
		const flags: string[] = [];
		if (Number(c.entrypoint) === 1) flags.push("entry");
		lines.push(
			`- ${short(row(c.name) || row(c.id))} — ${row(c.type) || "component"}${flags.length ? ` [${flags.join(",")}]` : ""} (${fileCounts.get(row(c.id)) ?? 0} files)`,
		);
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
		lines.push(`deps (${edges.length} edges):\n  ${rendered.slice(0, MAX_ROWS).join("\n  ")}${rendered.length > MAX_ROWS ? MORE_ROWS(rendered.length - MAX_ROWS) : ""}`);
	}
	lines.push('drill down: rig component <name-tail> | rig search \'<term>\' (symbol FTS) | rig deps <name-tail>');
	return lines.join("\n");
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
	return db
		.prepare("SELECT id, name, type FROM components WHERE id LIKE ? OR name LIKE ? ORDER BY seq LIMIT 3")
		.all(`%${target}`, `%${target}`) as Array<{ id: string; name: string; type: unknown }>;
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

function component(db: DatabaseSync, target: string | undefined): string {
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
	if (fileTotal > MAX_ROWS) lines.push(`${MORE_ROWS(fileTotal - MAX_ROWS)} files — narrow with rig files '<glob>'`);
	const edgeTotal = (d: string): number =>
		Number(db.prepare(`SELECT COUNT(*) n FROM deps WHERE ${d} = ?`).get(c.id)?.n ?? 0);
	const out = db.prepare("SELECT dst FROM deps WHERE src = ? LIMIT ?").all(c.id, MAX_ROWS) as Row[];
	if (out.length) {
		const total = edgeTotal("src");
		lines.push(`deps out: ${out.map((e) => label(e.dst)).join(", ")}${total > MAX_ROWS ? MORE_ROWS(total - MAX_ROWS) : ""}`);
	}
	const inc = db.prepare("SELECT src FROM deps WHERE dst = ? LIMIT ?").all(c.id, MAX_ROWS) as Row[];
	if (inc.length) {
		const total = edgeTotal("dst");
		lines.push(
			`deps in (reverse blast radius): ${inc.map((e) => label(e.src)).join(", ")}${total > MAX_ROWS ? MORE_ROWS(total - MAX_ROWS) : ""}`,
		);
	}
	return lines.join("\n");
}

function search(db: DatabaseSync, target: string | undefined): string {
	if (!target || !target.trim()) return "search: give a symbol term (prefix match supported: 'executor*').";
	// The documented syntax carries a trailing *; LIKE arms must not inherit it
	// ("executor*" → "executor*%" matches nothing) — strip it here, keep it for
	// the FTS phrase (#338 r2 4.1: the documented syntax worked only when FTS
	// was available, i.e. graceful degradation failed on its own syntax).
	const term = target.trim().replace(/"/g, "").replace(/\*+$/, "");
	// Unquoted FTS tokens prefix-match; quoted phrases do NOT — quote only when
	// the term spans words (else the documented prefix syntax never reaches FTS).
	const ftsTerm = term.includes(" ") ? `"${term}"` : `${term}*`;
	const hits = new Map<string, Row>();
	// 1. Name-prefix first: the agent's term is almost always a name stem
	// ("executor", "envelope"). LIKE gives camelCase-honest matching that FTS
	// tokenization cannot ("envelope" must find NodeResultEnvelope).
	const likeTerm = term.replace(/[%_]/g, "\\$0"); // literal %/_ in a target must not widen the match
	const pre = db
		.prepare("SELECT file, line, kind, name, signature, doc FROM symbols WHERE name LIKE ? || '%' ESCAPE '\\' ORDER BY seq LIMIT ?")
		.all(likeTerm, SEARCH_LIMIT) as Row[];
	for (const r of pre) hits.set(`${row(r.file)}:${row(r.line)}:${row(r.name)}`, r);
	let ftsFilled = false;
	// 2. FTS (rank over name/signature/doc) fills the rest.
	if (hits.size < SEARCH_LIMIT) {
		try {
			const rows = db
				.prepare(
					`SELECT s.file, s.line, s.kind, s.name, s.signature, s.doc
					 FROM symbols_fts f JOIN symbols s ON s.seq = f.rowid
					 WHERE symbols_fts MATCH ? ORDER BY rank LIMIT ?`,
				)
				.all(`"${ftsTerm}"`, SEARCH_LIMIT) as Row[];
			for (const r of rows) {
				if (hits.size >= SEARCH_LIMIT) break;
				hits.set(`${row(r.file)}:${row(r.line)}:${row(r.name)}`, r);
			}
			ftsFilled = rows.length >= SEARCH_LIMIT;
		} catch {
			// FTS unavailable — the sweep below still answers.
		}
	}
	// 3. Broad substring sweep fills what is left (doc/signature matches).
	let sweptBeyondLimit = false;
	if (hits.size < SEARCH_LIMIT) {
		const like = `%${likeTerm}%`;
		const likeRows = db
			.prepare(
				`SELECT file, line, kind, name, signature, doc FROM symbols
				 WHERE name LIKE ? ESCAPE '\\' OR signature LIKE ? ESCAPE '\\' OR doc LIKE ? ESCAPE '\\' ORDER BY seq LIMIT ?`,
			)
			.all(like, like, like, SEARCH_LIMIT + 1) as Row[];
		for (const r of likeRows) {
			if (hits.size >= SEARCH_LIMIT) {
				sweptBeyondLimit = true;
				break;
			}
			hits.set(`${row(r.file)}:${row(r.line)}:${row(r.name)}`, r);
		}
	}
	if (hits.size === 0) return `no symbols matching "${term}" — try a shorter stem with * (rig search 'handler*').`;
	// Honest truncation: when the pool filled, count the real total — a partial
	// answer presented as complete is how review findings get lost (#338 r1).
	let more = "";
	if (hits.size >= SEARCH_LIMIT && (pre.length >= SEARCH_LIMIT || ftsFilled || sweptBeyondLimit)) {
		const like = `%${likeTerm}%`; // escaped — must agree with the arms that produced the hits
		const total = Number(
			db
				.prepare("SELECT COUNT(*) n FROM symbols WHERE name LIKE ? OR signature LIKE ? OR doc LIKE ?")
				.get(like, like, like)?.n ?? 0,
		);
		if (total > hits.size) more = " …more hits — refine the term";
	}
	return [
		`search "${term}" — ${hits.size} hit(s):${more}`,
		...[...hits.values()].map((r) => symLine(r.file, r.line, r.kind, r.name, r.signature, r.doc)),
	].join("\n");
}

function files(db: DatabaseSync, target: string | undefined): string {
	if (!target) return "files: give a path glob, e.g. 'internal/graph/%' or '%executor%'.";
	let like = target.replace(/\*/g, "%").replace(/\?/g, "_");
	if (!like.includes("%") && !like.includes("_")) like = `%${like}%`; // bare prefix — don't answer nothing for it
	const total = Number(db.prepare("SELECT COUNT(*) n FROM files WHERE path LIKE ?").get(like)?.n ?? 0);
	const rows = db
		.prepare("SELECT path, lines, language, component_id FROM files WHERE path LIKE ? ORDER BY path LIMIT ?")
		.all(like, MAX_ROWS) as Row[];
	if (rows.length === 0) return `no files matching "${target}".`;
	const names = namesById(db);
	return [
		`files matching "${target}":`,
		...rows.map((r) => `  ${row(r.path)} (${row(r.lines)}L${r.component_id ? `, ${names.get(row(r.component_id)) ?? short(row(r.component_id))}` : ""})`),
		...(total > MAX_ROWS ? [MORE_ROWS(total - MAX_ROWS)] : []),
	].join("\n");
}

function deps(db: DatabaseSync, target: string | undefined, reverse: boolean): string {
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
	return [
		`${label(c.id)} ${arrow}`,
		...rows.map((r) => `  ${arrow} ${label(r.peer)}`),
		...(total > MAX_ROWS ? [MORE_ROWS(total - MAX_ROWS)] : []),
	].join("\n");
}
