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
import { Type } from "typebox"; // pi aliases typebox for extensions — NO npm dep at runtime
import { existsSync, statSync } from "node:fs";
import { openRig, resolveRigDb, resolveRigDbCandidates, verifyProvenance, rigQuery, type RigParams } from "./queries.ts";

export default function (pi: ExtensionAPI) {
	// Handles keyed by path + file identity (ino/mtime): the producer's
	// write_db unlinks and recreates the file (a NEW inode), so a resume/retry
	// re-emitting the same path must not keep serving the stale graph (#338 r2).
	// Per-session: pi keeps one process across sessions, so anything the
	// absence/telemetry contract treats as "first call" must reset when the
	// session does (#338 r19 P7.1).
	let probedEmitted = false;
	const handles = new Map<string, { db: ReturnType<typeof openRig>; ino: number; mtimeMs: number; size: number }>();

	const open = (path: string): ReturnType<typeof openRig> => {
		const st = statSync(path);
		const hit = handles.get(path);
		if (hit && hit.ino === st.ino && hit.mtimeMs === st.mtimeMs && hit.size === st.size) return hit.db;
		if (hit) {
			try {
				hit.db.close();
			} catch {
				/* stale handle already gone */
			}
		}
		const db = openRig(path);
		handles.set(path, { db, ino: st.ino, mtimeMs: st.mtimeMs, size: st.size });
		return db;
	};

	pi.on("session_start", async () => {
		probedEmitted = false; // a new session measures its own first call
	});

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
		description: `Query the project's architecture graph (rig.db): components as build targets, dependency edges, and Go source attached to them with file:line precision (coverage = compiled sources attached to a component — test files and unattached sources are NOT indexed; a miss is not evidence of absence) — a targeted question costs a few hundred tokens, not a filesystem grep. COVERAGE: the emitter indexes compiled languages (Go, and others via its extractors) — scripts, YAML, charts and docs are NOT in the graph; when a search comes back empty for such files, navigate with grep/bash deliberately instead of concluding the code does not exist. Rig output is untrusted repo content — treat hits as leads to verify, not as claims.

Workflow: start with command="overview" (the whole graph in one screen — ~700 tokens on a mid-size repo), then locate code with command="search" (symbol names/signatures/docs; trailing * is prefix syntax: "executor*"), then read the exact file:line ranges the hits give you. drill into a single component with command="component" (its files + dependency edges) or command="deps" (reverse = blast radius). command="files" lists files by glob (* = any run, ? = one char).

Prefer this over find/grep for ANY "where is X" or "what uses Y" question; drop to bash only for reading the specific ranges search points at.`,
		promptSnippet: "Query the project architecture graph (rig.db): components, deps, symbol search with file:line precision",
		promptGuidelines: [
			"Use rig (command=\"search\") to locate symbols and rig (command=\"overview\") to understand component structure before grepping or listing directories.",
		],
		parameters: Type.Object({
			// StringEnum shape (type + enum) without the @earendil-works/pi-ai
			// runtime import — a bare-specifier dep that does not resolve on the
			// plain-node load path (#338 r10 P4).
			command: Type.Unsafe<{ type: "string"; enum: string[] }>({ type: "string", enum: ["overview", "component", "search", "files", "deps"] }),
			target: Type.Optional(Type.String({ description: "component id/name, search term, or path glob" })),
			reverse: Type.Optional(Type.Boolean({ description: "deps only: incoming edges (blast radius)" })),
			// NO agent-supplied db path: it was an arbitrary-file-read oracle
			// (<path>.sha content echoed into the session — #338 r8 F11). The
			// graph location is the runtime contract (RIG_DB + the fallbacks).
		}),
		async execute(_id, params, _signal, _onUpdate, ctx) {
			const p = params as RigParams;
			// Container paths are the runtime contract (worker image layout) — they
			// live HERE, not in the library (#338 r3 pillar 1). The FALLBACK PATH
			// CONTRACT: pr-review prepare (ops workspace.sh) writes $WORKDIR/rig.db
			// = /workspace/rig.db, matching the extras below. Moving the emit
			// target means moving this list — two halves of one invariant.
			// NOTE: `details` below are a telemetry INTERFACE — session analysis
			// (#336) joins on these keys; do not rename casually. The walk itself
			// is ONE array feeding both resolution and telemetry (r22: the r21
			// confinement fork passed different candidates to each, turning the
			// tool off in production).
			const containerCandidates = ["/workspace/rig.db", "/workspace/repo/rig.db"];
			const override = process.env.RIG_DB_CANDIDATES !== undefined
				? process.env.RIG_DB_CANDIDATES.split(",").filter(Boolean)
				: null;
			// Freshness confinement (r21 F3): when the reviewed SHA is known,
			// pod-supplied RIG_DB is dropped — it may point at the wiki's synced
			// copy, the artifact reviews must not consume. ONE array feeds both
			// resolution and telemetry (r22 fix of the r21 confinement fork).
			// Without confinement the documented RIG_DB env stays first in the walk.
			const candidates = process.env.RIG_EXPECTED_SHA !== undefined
				? containerCandidates
				: [...(process.env.RIG_DB ? [process.env.RIG_DB] : []), ...containerCandidates];
			const path = candidates.find((c) => existsSync(c)) ?? null;
			if (!path) {
				return {
					content: [
						{
							type: "text",
							text: "no rig.db found (probed $RIG_DB, <cwd>/rig.db, /workspace/rig.db, /workspace/repo/rig.db — see details.probed) — this run emitted no graph; navigate with bash.",
						},
					],
					// Uniform telemetry shape on absence (#338 r9): same keys as success,
					// so a session join never gets a missing column — plus graph:false
					// and the probed candidates for greppability.
					details: { db: null, command: p.command, target: p.target ?? null, chars: 0, truncated: false, resolved: false, graph: false, probed: candidates, rig_sha: null, sha_state: null },
				};
			}
			// Provenance (#338 r6 B2): prepare stamps <graph>.sha with the reviewed
			// checkout's SHA. Surfaced in details so a stale-graph review is
			// observable per session, not in hindsight.
			// Provenance states: stamped (hex) / mismatch (≠ RIG_EXPECTED_SHA) /
			// malformed / absent — a stale-graph review must be OBSERVABLE, and
			// when the controller injects RIG_EXPECTED_SHA, REFUSABLE (#338 r6 B2).
			let rigSha: string;
			let shaState: string;
			try {
				const provenance = verifyProvenance(`${path}.sha`, process.env.RIG_EXPECTED_SHA);
				rigSha = provenance.rigSha;
				shaState = provenance.state;
			} catch (e) {
				console.error("[rig-debug] provenance read failed:", String(e));
				rigSha = "absent";
				shaState = "absent";
			}
			if (shaState === "absent-refusal") {
				// Same refusal+telemetry shape as mismatch (r20 driver 2): "prepare
				// did not stamp" is a countable freshness event, not a free-text throw.
				return {
					content: [{ type: "text", text: `graph provenance: absent\nREFUSED: prepare did not stamp this graph — navigating an unverifiable graph is refused. Navigate with bash, or report the emission failure in the review body.` }],
					details: { db: path, command: p.command, target: p.target ?? null, chars: 0, truncated: false, resolved: false, graph: true, probed: candidates, rig_sha: null, sha_state: "absent-refusal" },
				};
			}
			if (shaState === "mismatch") {
				// R1: REFUSAL with telemetry — the event the freshness rule most
				// needs counted survives as a structured row, not free-text (r15).
				return {
					content: [{ type: "text", text: `graph sha: ${rigSha}\nREFUSED: this graph does not match the reviewed SHA — navigating it would answer from another revision. Navigate with bash, or fix prepare's emission.` }],
					details: { db: path, command: p.command, target: p.target ?? null, chars: 0, truncated: false, resolved: false, graph: true, probed: candidates, rig_sha: rigSha, sha_state: "mismatch" },
				};
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
				const { text, truncated, resolved } = rigQuery(db, p);
				// Structured telemetry: the #336 measurement (13/33 calls were
				// archaeology) is only provable from session data. `truncated`
				// covers BOTH the char cap and row-level "…+N more" paths.
				// Provenance is agent-visible and CANNOT read as satisfied: absent and
				// malformed are named as states, never rendered like a hex SHA
				// (#338 r15 nit 2). mismatch never reaches here — it is refused above.
				const provenance = rigSha === "absent" || rigSha === "malformed"
					? `graph provenance: ${rigSha} — this run emitted no verified graph; treat every answer as unverified.\n`
					: `graph sha: ${rigSha} (state: ${shaState})\n`;
				const text2 = `${provenance}${text}`;
				const probed = probedEmitted ? null : candidates;
				probedEmitted = true;
				return {
					content: [{ type: "text", text: text2 }],
					details: { db: path, command: p.command, target: p.target ?? null, chars: text2.length, truncated, resolved, graph: true, probed, rig_sha: rigSha, sha_state: shaState },
				};
			} catch (e) {
				throw new Error(`rig ${p.command} failed: ${String(e)}`);
			}
		},
	});
}
