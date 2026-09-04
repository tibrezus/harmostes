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
import { test } from "node:test";

test("index.ts parses + loads — the fleet-wide load must never be dead code", async () => {
	let outcome: "loaded" | "resolution" | "syntax";
	try {
		await import("./index.ts");
		outcome = "loaded";
	} catch (e) {
		outcome = e instanceof SyntaxError ? "syntax" : "resolution";
	}
	assert.notEqual(outcome, "syntax", "index.ts does not parse — every agent in the fleet would fail to start");
});
