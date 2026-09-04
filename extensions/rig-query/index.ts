/**
 * rig-query — pi extension exposing the project's architecture graph
 * (rig.db) as a first-class tool (ADR-0009, #337).
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
import { openRig, resolveRigDb, rigQuery, type RigParams } from "./queries.ts";

export default function (pi: ExtensionAPI) {
	const handles = new Map<string, ReturnType<typeof openRig>>();

	pi.registerTool({
		name: "rig",
		label: "RIG",
		description: `Query the project's architecture graph (rig.db): components as build targets, dependency edges, every file and symbol with file:line precision — a targeted question costs a few hundred tokens, not a filesystem grep.

Workflow: start with command="overview" (whole graph, ~300 tokens), then locate code with command="search" (FTS5 over symbol names/signatures/docs; prefix match: "executor*"), then read the exact file:line ranges the hits give you. drill into a single component with command="component" (its files + dependency edges) or command="deps" (reverse = blast radius). command="files" lists files by path glob.

Prefer this over find/grep for ANY "where is X" or "what uses Y" question; drop to bash only for reading the specific ranges search points at.`,
		promptSnippet: "Query the project architecture graph (rig.db): components, deps, symbol search with file:line precision",
		promptGuidelines: [
			"Use rig (command=\"search\") to locate symbols and rig (command=\"overview\") to understand component structure before grepping or listing directories.",
		],
		parameters: Type.Object({
			command: Type.Union([
				Type.Literal("overview"),
				Type.Literal("component"),
				Type.Literal("search"),
				Type.Literal("files"),
				Type.Literal("deps"),
			]),
			target: Type.Optional(Type.String({ description: "component id/name, search term, or path glob" })),
			reverse: Type.Optional(Type.Boolean({ description: "deps only: incoming edges (blast radius)" })),
			db: Type.Optional(Type.String({ description: "explicit rig.db path (default: auto-discover)" })),
		}),
		async execute(_id, params, _signal, _onUpdate, ctx) {
			const p = params as RigParams & { db?: string };
			const path = resolveRigDb(p.db, ctx?.cwd);
			if (!path) {
				return {
					content: [
						{
							type: "text",
							text: "no rig.db in this workspace (looked at $RIG_DB, ./rig.db, /workspace/rig.db) — the graph was not generated for this run; navigate with bash.",
						},
					],
					details: {},
				};
			}
			let db = handles.get(path);
			if (!db) {
				try {
					db = openRig(path);
					handles.set(path, db);
				} catch (e) {
					return {
						content: [{ type: "text", text: `rig.db at ${path} is not readable as a RIG database: ${String(e)}` }],
						details: {},
					};
				}
			}
			try {
				return { content: [{ type: "text", text: rigQuery(db, p) }], details: { db: path } };
			} catch (e) {
				return { content: [{ type: "text", text: `rig query failed: ${String(e)}` }], details: {} };
			}
		},
	});
}
