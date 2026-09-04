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
