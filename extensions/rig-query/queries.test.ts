/**
 * Consumer tests for the rig-query query layer against a fixture produced by
 * the REAL emitter (fixtures/generate.py → plugins/rig-emit/rig/db.py).
 *
 * These encode the #338 r1 findings: named components everywhere, camelCase
 * symbol discovery that survives FTS-quota saturation, ambiguity as an
 * answer, and schema-version assertion. Run: make test-extensions (or
 * node --test --experimental-strip-types extensions/rig-query/).
 */
import assert from "node:assert/strict";
import { test } from "node:test";
import { DatabaseSync } from "node:sqlite";
import { unlinkSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { openRig, rigQuery, type RigParams } from "./queries.ts";

const FIXTURE = fileURLToPath(new URL("./fixtures/rig.db", import.meta.url));

function q(params: RigParams): string {
	return rigQuery(openRig(FIXTURE), params);
}

test("openRig asserts the producer schema version", () => {
	// Build a rig.db-shaped file carrying a future schema version.
	const bad = fileURLToPath(new URL("./fixtures/bad-version.db", import.meta.url));
	const badDb = new DatabaseSync(bad);
	badDb.exec("CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)");
	badDb.prepare("INSERT INTO meta VALUES ('db_schema_version', '9')").run();
	badDb.close();
	try {
		assert.throws(() => openRig(bad), /speaks v1/);
	} finally {
		unlinkSync(bad);
	}
});

test("overview names the project from repo_name meta", () => {
	const out = q({ command: "overview" });
	assert.match(out, /fixture-repo/);
	assert.match(out, /cmd\/worker/);
	assert.match(out, /3 components/);
});

test("search finds camelCase names despite doc-heavy FTS competition", () => {
	const out = q({ command: "search", target: "envelope" });
	assert.match(out, /NodeResultEnvelope/, "the name hit must survive the 10 doc-matching rows");
	assert.match(out, /internal\/model\/types\.go:44/);
});

test("search reports truncation instead of a silently partial answer", () => {
	const out = q({ command: "search", target: "handler" });
	assert.match(out, /— 8 hit\(s\): …more hits — refine the term/);
});

test("ambiguous name tails list candidates instead of guessing", () => {
	const out = q({ command: "component", target: "worker" });
	assert.match(out, /ambiguous/);
	assert.match(out, /cmd\/worker/);
	assert.match(out, /internal\/worker/);
});

test("component resolves a unique tail and names its dependency edges", () => {
	const out = q({ command: "component", target: "cmd/worker" });
	assert.match(out, /component cmd\/worker \(executable/);
	const deps = out.split("deps out:")[1] ?? "";
	assert.match(deps, /internal\/worker/);
	assert.doesNotMatch(deps, /comp-\d+/, "edge endpoints must be named, not raw ids");
});

test("deps reverse blast radius is named and id-free", () => {
	const out = q({ command: "deps", target: "internal/model", reverse: true });
	assert.match(out, /internal\/model ←/);
	assert.match(out, /← internal\/worker/);
	assert.doesNotMatch(out, /comp-\d+/);
});

test("openRig rejects a non-rig.db file with a readable error", () => {
	assert.throws(() => openRig(fileURLToPath(new URL("./queries.ts", import.meta.url))), /not a rig\.db/);
});
