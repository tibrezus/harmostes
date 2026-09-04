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
import { copyFileSync, rmSync, unlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { openRig, resolveRigDb, rigQuery, type RigParams } from "./queries.ts";

const FIXTURE = fileURLToPath(new URL("./fixtures/rig.db", import.meta.url));

function q(params: RigParams): string {
	const db = openRig(FIXTURE);
	try {
		return rigQuery(db, params).text;
	} finally {
		db.close(); // these tests are the example the next extension copies
	}
}

test("openRig asserts the producer schema version", () => {
	// Build a rig.db-shaped file carrying a future schema version — in tmpdir:
	// a worker checkout is a place git-push operates; never litter it (#338 r2).
	const bad = `${tmpdir()}/rig-query-bad-version-${process.pid}-${Date.now()}.db`;
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
	assert.match(out, /16 components/);
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

test("documented trailing-* syntax works on the LIKE paths too (#338 r2 4.1)", () => {
	const starred = q({ command: "search", target: "Executor*" });
	const plain = q({ command: "search", target: "Executor" });
	assert.match(starred, /ExecutorMain/);
	assert.equal(starred, plain, "the * is syntax, not a literal — results must be identical");
});

test("search degrades gracefully when FTS5 is absent (#338 r2 4.1)", () => {
	const noFts = `${tmpdir()}/rig-query-no-fts-${process.pid}-${Date.now()}.db`;
	copyFileSync(FIXTURE, noFts);
	try {
		const db = new DatabaseSync(noFts);
		db.exec("DROP TABLE IF EXISTS symbols_fts"); // what _fts5_available() false produces
		db.close();
		const { text } = rigQuery(openRig(noFts), { command: "search", target: "Executor" });
		assert.match(text, /Executor/);
		assert.match(text, /internal\/worker\/exec\.go/);
	} finally {
		rmSync(noFts, { force: true });
	}
});

test("component file listings say …+N more instead of silently truncating (#338 r2 4.2)", () => {
	const out = q({ command: "component", target: "internal/worker" });
	assert.match(out, /…\+\d+ more files/);
	assert.match(out, /narrow with rig files/);
});

test("files lists bare prefixes and names components (#338 r2 4.5)", () => {
	const out = q({ command: "files", target: "internal/worker" });
	assert.match(out, /internal\/worker\/exec\.go/);
	assert.match(out, /internal\/worker/); // component name, not comp-N
});

test("deps blast radius says …+N more instead of silently truncating (#338 r3 4.2)", () => {
	const out = q({ command: "deps", target: "internal/worker" });
	assert.match(out, /…\+\d+ more/);
});

test("files listing says …+N more when over the row cap (#338 r3 4.2)", () => {
	const out = q({ command: "files", target: "internal/worker" });
	assert.match(out, /…\+\d+ more/);
});

test("rigQuery reports row-level truncation in its structured result (#338 r3 pillar 7)", () => {
	const db = openRig(FIXTURE);
	try {
		const r = rigQuery(db, { command: "deps", target: "internal/worker" });
		assert.equal(r.truncated, true);
		const complete = rigQuery(db, { command: "deps", target: "internal/model" });
		assert.equal(complete.truncated, false);
	} finally {
		db.close();
	}
});

test("openRig rejects a non-rig.db file with a readable error", () => {
	assert.throws(() => openRig(fileURLToPath(new URL("./queries.ts", import.meta.url))), /not a rig\.db/);
});
