/**
 * rig-query — pi extension exposing the project's architecture graph
 * (rig.db) as a first-class tool (ADR-0009 in the harmostes.wiki repo,
 * commit 72767fb; #337).
 *
 * rig.db is generated deterministically by rig-emit in the workflow's
 * prepare phase, SHA-exact for the checked-out revision. This extension is
 * the ONLY sanctioned query path for agents: structured subcommands with
 * token-capped results replace filesystem grep expeditions, which is where
 * review sessions lost most of their wall clock (#336 measurement: ~13 of
 * 33 calls were code archaeology).
 *
 * Degradation: no rig.db in the workspace → the tool reports its absence
 * and the agent navigates with bash as before. The tool is never
 * load-bearing for unsupported languages or missing graphs.
 */
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { readFileSync, statSync } from "node:fs";
import { openRig, resolveRigDb, rigQuery, type RigParams } from "./queries.ts";

export default function (pi: ExtensionAPI) {
	// Handles keyed by path + file identity (ino/mtime): the producer's
	// write_db unlinks and recreates the file (a NEW inode), so a resume/retry
	// re-emitting the same path must not keep serving the stale graph (#338 r2).
	const handles = new Map<string, { db: ReturnType<typeof openRig>; ino: number; mtimeMs: number }>();

	const open = (path: string): ReturnType<typeof openRig> => {
		const st = statSync(path);
		const hit = handles.get(path);
		if (hit && hit.ino === st.ino && hit.mtimeMs === st.mtimeMs) return hit.db;
		if (hit) {
			try {
				hit.db.close();
			} catch {
				/* stale handle already gone */
			}
		}
		const db = openRig(path);
		handles.set(path, { db, ino: st.ino, mtimeMs: st.mtimeMs });
		return db;
	};

	// Session-scoped resources are closed via an idempotent session_shutdown
	// handler (pi extension convention).
	pi.on("session_shutdown", async () => {
		for (const { db } of handles.values()) {
			try {
				db.close();
			} catch {
				// already closed — idempotent by contract
			}
		}
		handles.clear();
	});

	pi.registerTool({
		name: "rig",
		label: "RIG",
		description: `Query the project's architecture graph (rig.db): components as build targets, dependency edges, every file and symbol with file:line precision — a targeted question costs a few hundred tokens, not a filesystem grep.

Workflow: start with command="overview" (the whole graph in one screen — roughly 400 tokens on a mid-size repo), then locate code with command="search" (symbol names/signatures/docs; trailing * is prefix syntax: "executor*"), then read the exact file:line ranges the hits give you. drill into a single component with command="component" (its files + dependency edges) or command="deps" (reverse = blast radius). command="files" lists files by path glob ('%' wildcards).

Prefer this over find/grep for ANY "where is X" or "what uses Y" question; drop to bash only for reading the specific ranges search points at.`,
		promptSnippet: "Query the project architecture graph (rig.db): components, deps, symbol search with file:line precision",
		promptGuidelines: [
			"Use rig (command=\"search\") to locate symbols and rig (command=\"overview\") to understand component structure before grepping or listing directories.",
		],
		parameters: Type.Object({
			// Type.Enum per pi's own extension docs (union-of-literals is the anti-pattern there).
			command: Type.Enum(["overview", "component", "search", "files", "deps"] as const),
			target: Type.Optional(Type.String({ description: "component id/name, search term, or path glob" })),
			reverse: Type.Optional(Type.Boolean({ description: "deps only: incoming edges (blast radius)" })),
			db: Type.Optional(Type.String({ description: "explicit rig.db path (default: auto-discover)" })),
		}),
		async execute(_id, params, _signal, _onUpdate, ctx) {
			const p = params as RigParams & { db?: string };
			// Container paths are the runtime contract (worker image layout) — they
			// live HERE, not in the library (#338 r3 pillar 1). The FALLBACK PATH
			// CONTRACT: pr-review prepare (ops workspace.sh) writes $WORKDIR/rig.db
			// = /workspace/rig.db, matching the first fallback below. Moving the
			// emit target means moving this list — they are two halves of one
			// invariant (ADR-0009 freshness).
			const path = resolveRigDb(p.db, ctx.cwd, ["/workspace/rig.db", "/workspace/repo/rig.db"]);
			if (!path) {
				return {
					content: [
						{
							type: "text",
							text: "no rig.db in this workspace (looked at the db param, $RIG_DB, ./rig.db, /workspace/rig.db) — the graph was not generated for this run; navigate with bash.",
						},
					],
					details: { graph: false }, // structured: telemetry branches on presence without parsing text
				};
			}
			// Provenance (#338 r6 B2): prepare stamps <graph>.sha with the reviewed
			// checkout's SHA. Surfaced in details so a stale-graph review is
			// observable per session, not in hindsight.
			let rigSha: string | null = null;
			try {
				rigSha = readFileSync(`${path}.sha`, "utf8").trim() || null;
			} catch {
				/* no provenance stamp — prepare predates the convention */
			}
			let db: ReturnType<typeof openRig>;
			try {
				db = open(path); // get-or-open; the (ino, mtime) identity check lives HERE only
			} catch (e) {
				// Throw, don't return text: a corrupt/version-mismatched graph must
				// surface as a FAILED tool call in the session and telemetry, not
				// as a silent no-op indistinguishable from the tool working (#338 r1).
				// The agent cannot regenerate a graph — prepare owns emission — so
				// point at the run, not the emitter.
				throw new Error(`rig.db at ${path} is not readable as a RIG database: ${String(e)} — if this run should have a graph, check the prepare phase logs`);
			}
			try {
				const { text, truncated } = rigQuery(db, p);
				// Structured telemetry: the #336 measurement (13/33 calls were
				// archaeology) is only provable from session data. `truncated`
				// covers BOTH the char cap and row-level "…+N more" paths.
				return {
					content: [{ type: "text", text: rigSha ? `graph sha: ${rigSha}\n${text}` : text }],
					details: { db: path, command: p.command, target: p.target ?? "", chars: text.length, truncated, rig_sha: rigSha ?? undefined },
				};
			} catch (e) {
				throw new Error(`rig ${p.command} failed: ${String(e)}`);
			}
		},
	});
}
