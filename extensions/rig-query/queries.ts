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

/** Candidate locations for rig.db, in priority order. */
export function resolveRigDb(explicit?: string, cwd?: string): string | null {
	const candidates = [
		explicit,
		process.env.RIG_DB,
		cwd ? resolve(cwd, "rig.db") : undefined,
		"/workspace/rig.db",
		"/workspace/repo/rig.db",
		"rig.db",
	].filter((p): p is string => Boolean(p));
	for (const p of candidates) {
		if (existsSync(p)) return p;
	}
	return null;
}

const MAX_RESULT_CHARS = 1600;
const MAX_ROWS = 12;

function cap(text: string): string {
	if (text.length <= MAX_RESULT_CHARS) return text;
	return (
		text.slice(0, MAX_RESULT_CHARS) +
		`\n… [truncated — refine the query; full answer exceeds ${MAX_RESULT_CHARS} chars]`
	);
}

function row(v: unknown): string {
	return v === null || v === undefined ? "" : String(v);
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

/** Open a rig.db read-only. Throws a readable error if the file is not a rig.db. */
export function openRig(path: string): DatabaseSync {
	return new DatabaseSync(path, { readOnly: true });
}

/** Dispatch one rig query. Returns the agent-facing text (token-capped). */
export function rigQuery(db: DatabaseSync, p: RigParams): string {
	switch (p.command) {
		case "overview":
			return cap(overview(db));
		case "component":
			return cap(component(db, p.target));
		case "search":
			return cap(search(db, p.target));
		case "files":
			return cap(files(db, p.target));
		case "deps":
			return cap(deps(db, p.target, p.reverse === true));
		default:
			return `unknown command ${JSON.stringify((p as { command?: string }).command)}`;
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
	const proj = meta(db, "project") || meta(db, "name");
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
		// Edge endpoints are ids; resolve to names (fall back to raw id) for readability.
		const names = new Map<string, string>();
		for (const c of comps) names.set(row(c.id), short(row(c.name) || row(c.id)));
		const rendered = edges.map((e) => `${nameOf(names, row(e.src))} → ${nameOf(names, row(e.dst))}`);
		lines.push(`deps (${edges.length}): ${rendered.slice(0, MAX_ROWS).join(", ")}${rendered.length > MAX_ROWS ? " …" : ""}`);
	}
	lines.push('drill down: rig component <name-tail> | rig search \'<term>\' (symbol FTS) | rig deps <name-tail>');
	return lines.join("\n");
}

function nameOf(names: Map<string, string>, id: string): string {
	return names.get(id) ?? short(id);
}

/** Component ids are usually long paths (e.g. example.com/repo/cmd/foo) — shorten to the tail. */
function short(id: string): string {
	const parts = id.split("/").filter(Boolean);
	return parts.length > 2 ? parts.slice(-2).join("/") : id;
}

function resolveComponent(db: DatabaseSync, target: string | undefined): { id: string; name: string; type: unknown } | null {
	if (!target) return null;
	const byId = db.prepare("SELECT id, name, type FROM components WHERE id = ?").get(target);
	if (byId) return byId as { id: string; name: string; type: unknown };
	const byName = db.prepare("SELECT id, name, type FROM components WHERE name = ?").get(target);
	if (byName) return byName as { id: string; name: string; type: unknown };
	// Agents address components by tail ("internal/agent", "harmostes-worker") —
	// component ids are opaque (comp-N), names are import paths.
	const byTail = db
		.prepare("SELECT id, name, type FROM components WHERE id LIKE ? OR name LIKE ? ORDER BY seq LIMIT 1")
		.get(`%${target}`, `%${target}`);
	if (byTail) return byTail as { id: string; name: string; type: unknown };
	return null;
}

function component(db: DatabaseSync, target: string | undefined): string {
	const c = resolveComponent(db, target);
	if (!c) return `no component matching ${JSON.stringify(target ?? "")} — use rig overview for names.`;
	const lines: string[] = [`component ${short(row(c.name) || c.id)} (${row(c.type) || "component"}, id ${c.id})`];
	for (const f of db
		.prepare(
			"SELECT f.path, f.lines FROM component_files cf JOIN files f ON f.path = cf.path WHERE cf.component_id = ? ORDER BY cf.seq LIMIT ?",
		)
		.all(c.id, MAX_ROWS)) {
		lines.push(`  ${row(f.path)} (${row(f.lines)}L)`);
	}
	const out = db.prepare("SELECT dst FROM deps WHERE src = ? LIMIT ?").all(c.id, MAX_ROWS) as Row[];
	if (out.length) {
		const names = new Map<string, string>();
		for (const r of db.prepare("SELECT id, name FROM components").all() as Row[]) names.set(row(r.id), short(row(r.name) || row(r.id)));
		lines.push(`deps out: ${out.map((e) => names.get(row(e.dst)) ?? short(row(e.dst))).join(", ")}`);
	}
	const inc = db.prepare("SELECT src FROM deps WHERE dst = ? LIMIT ?").all(c.id, MAX_ROWS) as Row[];
	if (inc.length) lines.push(`deps in (reverse blast radius): ${inc.map((e) => short(row(e.src))).join(", ")}`);
	return lines.join("\n");
}

function search(db: DatabaseSync, target: string | undefined): string {
	if (!target || !target.trim()) return "search: give a symbol term (FTS5 prefix match supported: 'executor*').";
	const term = target.trim().replace(/"/g, "");
	const hits = new Map<string, Record<string, unknown>>();
	try {
		const rows = db
			.prepare(
				`SELECT s.file, s.line, s.kind, s.name, s.signature, s.doc
				 FROM symbols_fts f JOIN symbols s ON s.seq = f.rowid
				 WHERE symbols_fts MATCH ? ORDER BY rank LIMIT ?`,
			)
			.all(`"${term}"*`, 8) as Array<Record<string, unknown>>;
		for (const r of rows) hits.set(`${row(r.file)}:${row(r.line)}:${row(r.name)}`, r);
	} catch {
		// FTS unavailable — the LIKE sweep below still answers.
	}
	// Always merge a substring sweep: FTS tokenizes on non-alphanumerics only, so
	// "envelope" misses camelCase names like NodeResultEnvelope — exactly the
	// case that matters when navigating unfamiliar code.
	const like = `%${term}%`;
	const likeRows = db
		.prepare(
			`SELECT file, line, kind, name, signature, doc FROM symbols
			 WHERE name LIKE ? OR signature LIKE ? OR doc LIKE ? ORDER BY seq LIMIT ?`,
		)
		.all(like, like, like, 16) as Array<Record<string, unknown>>;
	for (const r of likeRows) {
		if (hits.size >= 8) break;
		hits.set(`${row(r.file)}:${row(r.line)}:${row(r.name)}`, r);
	}
	if (hits.size === 0) return `no symbols matching "${term}" — try a shorter stem with * (rig search 'resume*').`;
	return [
		`search "${term}" — ${hits.size} hit(s):`,
		...[...hits.values()].map((r) => symLine(r.file, r.line, r.kind, r.name, r.signature, r.doc)),
	].join("\n");
}

function files(db: DatabaseSync, target: string | undefined): string {
	if (!target) return "files: give a path glob, e.g. 'internal/graph/%' or '%executor%'.";
	const like = target.replace(/\*/g, "%").replace(/\?/g, "_");
	const rows = db
		.prepare("SELECT path, lines, language, component_id FROM files WHERE path LIKE ? ORDER BY path LIMIT ?")
		.all(like, MAX_ROWS) as Row[];
	if (rows.length === 0) return `no files matching "${target}".`;
	return [
		`files matching "${target}":`,
		...rows.map((r) => `  ${row(r.path)} (${row(r.lines)}L${r.component_id ? `, ${short(row(r.component_id))}` : ""})`),
	].join("\n");
}

function deps(db: DatabaseSync, target: string | undefined, reverse: boolean): string {
	const c = resolveComponent(db, target);
	if (!c) return `no component matching ${JSON.stringify(target ?? "")} — use rig overview for names.`;
	const names = new Map<string, string>();
	for (const r of db.prepare("SELECT id, name FROM components").all() as Row[]) names.set(row(r.id), short(row(r.name) || row(r.id)));
	const label = (id: unknown): string => names.get(row(id)) ?? short(row(id));
	const rows = reverse
		? (db.prepare("SELECT src FROM deps WHERE dst = ? ORDER BY src LIMIT ?").all(c.id, MAX_ROWS) as Row[])
		: (db.prepare("SELECT dst FROM deps WHERE src = ? ORDER BY dst LIMIT ?").all(c.id, MAX_ROWS) as Row[]);
	const arrow = reverse ? "←" : "→";
	if (rows.length === 0) return `${label(c.id)} has no ${reverse ? "reverse" : ""} dependencies.`;
	return [
		`${label(c.id)} ${arrow}`,
		...rows.map((r) => `  ${arrow} ${label(reverse ? r.src : r.dst)}`),
	].join("\n");
}
