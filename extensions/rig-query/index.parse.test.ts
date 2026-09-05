/**
 * Parse gate for the pi wrapper (#338 r3 blocker): index.ts is loaded by
 * PiArgs for EVERY workflow, and pi exits before creating any agent session
 * when an extension fails to load — a syntactically dead index.ts takes down
 * the fleet, while the query-layer tests (which import queries.ts only) stay
 * green.
 *
 * Three outcomes, exactly one acceptable failure mode:
 *  - resolves                → full load (typebox installed — best)
 *  - rejects, non-SyntaxError → module resolution failed (packages absent in
 *                               this env) — parse succeeded, pass
 *  - rejects, SyntaxError     → the round-3 blocker — FAIL
 */
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

test("index.ts parses + loads — the fleet-wide load must never be dead code", async () => {
	// typebox is a devDependency and `make test-extensions` installs it when
	// missing, so resolution failure here is itself a harness bug (#338 r8
	// F15: a resolution "pass" executes zero lines of index.ts).
	let outcome: "loaded" | "resolution" | "syntax";
	try {
		await import("./index.ts");
		outcome = "loaded";
	} catch (e) {
		outcome = e instanceof SyntaxError ? "syntax" : "resolution";
	}
	assert.equal(outcome, "loaded", `index.ts ${outcome === "syntax" ? "does not parse — every agent in the fleet would fail to start" : "could not load (harness missing typebox? run make test-extensions)"}`);
});

// The command enum is hand-written twice (Type.Unsafe schema in index.ts,
// RigParams union in queries.ts) — a 6th command added to one and not the
// other yields a schema that admits a value rigQuery cannot handle. Pin
// them together (r27 P3 nit).
test("the command enum agrees between the tool schema and the query layer", async () => {
	const idx = await readFile(new URL("./index.ts", import.meta.url), "utf8");
	const lib = await readFile(new URL("./queries.ts", import.meta.url), "utf8");
	const schema = idx.match(/command: Type\.Unsafe[^\n]*?enum: \[([^\]]*)\]/);
	assert.ok(schema, "the command StringEnum literal must stay findable in index.ts");
	const schemaCommands = [...schema[1].matchAll(/"([a-z]+)"/g)].map((m) => m[1]).sort();
	const union = lib.match(/command: "overview"[^;]*;/);
	assert.ok(union, "the RigParams command union must stay findable in queries.ts");
	const unionCommands = [...union[0].matchAll(/"([a-z]+)"/g)].map((m) => m[1]).sort();
	assert.deepEqual(schemaCommands, unionCommands, "schema enum and RigParams union drifted apart");
});
