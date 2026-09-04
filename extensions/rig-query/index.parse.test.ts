/**
 * Parse gate for the pi wrapper (#338 r3 blocker): index.ts is loaded by
 * PiArgs for EVERY workflow, and pi exits before creating any agent session
 * when an extension fails to load — a syntactically dead index.ts takes down
 * the fleet, while the query-layer tests (which import queries.ts only) stay
 * green. This test imports index.ts OUTSIDE pi: a SyntaxError fails the
 * test; ERR_MODULE_NOT_FOUND (the pi/typebox packages not installed here)
 * proves the module PARSED, which is the gate's job.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

test("index.ts parses — the fleet-wide load must never be dead code", async () => {
	await assert.rejects(
		() => import("./index.ts"),
		(e: unknown) => {
			assert.ok(!(e instanceof SyntaxError), `index.ts does not parse: ${String(e)}`);
			return true; // any non-syntax failure (module resolution) means the parse succeeded
		},
	);
});
