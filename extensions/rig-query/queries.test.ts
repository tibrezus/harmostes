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
	return qr(params).text;
}

function qr(params: RigParams): { text: string; resolved: boolean } {
	const db = openRig(FIXTURE);
	try {
		return rigQuery(db, params);
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
		assert.throws(() => openRig(bad), /speaks schema v1/);
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
	assert.match(out, /\(8\)/);
	assert.match(out, /…more hits — refine the term/);
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

test("files finds underscore-bearing paths — the r13 HIGH bug", () => {
	// The bare-substring guard used to inspect the TRANSLATED string, so any
	// literal _ skipped the % wrap → "no files matching" for existing files.
	assert.match(q({ command: "files", target: "attempt_types" }), /attempt_types\.go/);
	assert.match(q({ command: "files", target: "attempt_types.go" }), /attempt_types\.go/);
	assert.match(q({ command: "files", target: "plugin_test.go" }), /plugin_test\.go/);
});

test("files ? operator is a single-char wildcard, not a literal underscore", () => {
	// Operators are exact-shape (LIKE semantics): "exec?go" alone requires the
	// WHOLE path to be 7 chars — an honest miss; "%exec?go" matches with a
	// prefix. And the % alias still works as documented (#338 r21 F4 / r20 d).
	const exact = q({ command: "files", target: "exec?go" });
	assert.match(exact, /no files matching/, "an exact-shape miss must say so");
	const prefixed = q({ command: "files", target: "%exec?go" });
	assert.match(prefixed, /exec\.go/, "% prefix allows preceding path");
	const alias = q({ command: "files", target: "%exec%" });
	assert.match(alias, /exec\.go/, "% alias = any run, as documented");
});

test("search by the component handle overview prints (#338 r18 F1/F2)", () => {
	// overview renders tails ("cmd/worker") — search must resolve THAT handle,
	// not only the full import path.
	const out = q({ command: "search", target: "cmd/worker" });
	// the rendered handle is what overview prints (short tail), not the full path
	assert.match(out, /\[component\] cmd\/worker \(executable\)/);
	assert.match(out, /rig component 'cmd\/worker'/);
});

test("components render outside the symbol budget (#338 r18 F3)", () => {
	// 10 helper symbols match by doc; the component candidate must still appear.
	const out = q({ command: "search", target: "helper" });
	assert.match(out, /helper0/);
});

test("files ? is one char, not substring-anywhere (#338 r21 F2)", () => {
	// "internal/?" must be exactly 10 chars before the dot — the forced wrap
	// used to widen a lone-? into substring-anywhere.
	const out = q({ command: "files", target: "internal/?" });
	assert.doesNotMatch(out, /attempt_types/);
});

test("files mixes literal % and _ with operators (#338 r18 F4 operator order)", () => {
	// literal % must stay escaped while * still acts as the run wildcard
	const out = q({ command: "files", target: "100%.txt" });
	assert.match(out, /no files matching/, "100%.txt is not in the fixture — an honest miss");
	const star = q({ command: "files", target: "internal/worker/*" });
	assert.match(star, /exec\.go/);
});

test("underscore-bearing symbol names survive LIKE escaping (#338 r7 B1)", () => {
	const exact = q({ command: "search", target: "canonical_hash" });
	assert.match(exact, /canonical_hash/, "exact snake_case name must be found");
	assert.match(exact, /internal\/model\/db\.py:439/);
	const stem = q({ command: "search", target: "write_db" });
	assert.match(stem, /write_db/, "underscore stem must be found");
	assert.doesNotMatch(stem, /no symbols matching/);
});

test("doc-only FTS path is exercised when the name-prefix arm has nothing (#338 r7 pillar 8)", () => {
	// "carrying" appears only in NodeResultEnvelope's doc — the prefix arm
	// cannot find it; the sweep must.
	const out = q({ command: "search", target: "carrying" });
	assert.match(out, /NodeResultEnvelope/);
});

test("a doc containing the marker text does not flip structured truncated (#338 r9)", () => {
	const db = openRig(FIXTURE);
	try {
		const r = rigQuery(db, { command: "search", target: "renderReport" });
		assert.match(r.text, /renderReport/);
		assert.equal(r.truncated, false, "prose must never be sniffed for truncation");
	} finally {
		db.close();
	}
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

test("resolveRigDb precedence: explicit > env > prepare extras; NO cwd-relative or bare candidates (#338 r20 S1)", async () => {
	const fs = await import("node:fs");
	const dir = `${tmpdir()}/rig-prec-${process.pid}`;
	fs.mkdirSync(dir, { recursive: true });
	const a = `${dir}/rig-a.db`, b = `${dir}/rig-b.db`, c = `${dir}/rig-extra.db`;
	// A stray rig.db in the agent's cwd — the exact shadow the walk must ignore.
	const stray = `${dir}/cwd-rig.db`;
	fs.writeFileSync(stray, "stray");
	for (const f of [a, b, c]) fs.writeFileSync(f, "x");
	try {
		process.env.RIG_DB = b;
		assert.equal(resolveRigDb(a, [c]), a, "explicit wins");
		assert.equal(resolveRigDb(undefined, [c]), b, "env beats extras");
		delete process.env.RIG_DB;
		// THE freshness ordering: prepare's SHA-exact emit outranks anything in
		// the working tree — the cwd is never consulted.
		assert.equal(resolveRigDb(undefined, [c]), c, "prepare extras win");
	} finally {
		for (const f of [a, b, c, stray]) rmSync(f, { force: true });
		rmSync(dir, { recursive: true, force: true });
		delete process.env.RIG_DB;
	}
});

test("the wrapper registers the rig tool + shutdown handler (#338 r5 B2)", async () => {
	const mod = await import("./index.ts");
	const tools: Array<{ name: string; description: string; parameters: Record<string, unknown> }> = [];
	const events: string[] = [];
	mod.default({
		on: (event: string) => events.push(event),
		registerTool: (t: { name: string; description: string; parameters: Record<string, unknown> }) => tools.push(t),
	} as never);
	assert.equal(tools.length, 1, "exactly one tool registered");
	assert.equal(tools[0].name, "rig");
	assert.match(tools[0].description, /overview/);
	assert.ok(events.includes("session_shutdown"), "session_shutdown must be registered (handle cleanup)");
	// The parameter schema must be provider-valid: {type:"string", enum:[…]} —
	// Type.Enum alone emits {"enum":[…]} with no type and fails strict
	// validation on the models the fleet mandates this tool for (#338 r8 F3).
	const command = (tools[0].parameters.properties as Record<string, { type?: string; enum?: string[] }>).command;
	assert.equal(command.type, "string", "command schema must carry type:string (StringEnum, not Type.Enum)");
	assert.deepEqual([...(command.enum ?? [])].sort(), ["component", "deps", "files", "overview", "search"]);
});

test("resolved is answered-semantics: every command sets it, refusals don't exist here (r23 P7)", () => {
	// Pinned semantics: `resolved` = "the graph answered this command" — hits,
	// structured no-match answers, and usage hints alike. It is NOT "target
	// hit" (the text says that) and not wrapper-level (absence/refusal rows
	// carry resolved:false from index.ts, covered in index.runtime.test.ts).
	assert.equal(qr({ command: "search", target: "envelope" }).resolved, true, "search: hit");
	assert.equal(qr({ command: "search", target: "zzzznomatch" }).resolved, true, "search: structured no-match");
	assert.equal(qr({ command: "search" }).resolved, true, "search: usage hint");
	assert.equal(qr({ command: "overview" }).resolved, true, "overview: even an empty graph answered");
	assert.equal(qr({ command: "component", target: "kernel" }).resolved, true, "component");
	assert.equal(qr({ command: "files", target: "internal/graph/*" }).resolved, true, "files");
	assert.equal(qr({ command: "deps", target: "kernel" }).resolved, true, "deps");
});

test("verifyProvenance: absent-refusal is reachable — an expected SHA with no stamp refuses (r23 P4)", async () => {
	const { verifyProvenance } = await import("./queries.ts");
	const dir = tmpdir();
	const stamp = `${dir}/rig-stamp-${process.pid}.sha`;
	// no stamp, nothing expected → plain absent
	assert.deepEqual(verifyProvenance(stamp, undefined), { rigSha: "absent", state: "absent" });
	const { writeFileSync } = await import("node:fs");
	// no stamp, SHA demanded → absent-refusal (the wrapper refuses on this)
	assert.deepEqual(verifyProvenance(stamp, "0123abcd"), { rigSha: "absent", state: "absent-refusal" });
	// stamp + SHA demanded → verified; different SHA → mismatch
	writeFileSync(stamp, "0123abcd");
	assert.equal(verifyProvenance(stamp, "0123abcd").state, "verified");
	assert.equal(verifyProvenance(stamp, "ffffffff").state, "mismatch");
	// malformed content is never echoed back
	assert.deepEqual(verifyProvenance(stamp, "0123abcd", () => "root:x:0:0"), { rigSha: "malformed", state: "malformed" });
	unlinkSync(stamp);
});

test("resolveRigDbCandidates: the override hatch is real — exact list, empty = suppression (r23 P8)", async () => {
	const { resolveRigDbCandidates } = await import("./queries.ts");
	// suppression: "" resolves to NOTHING, regardless of RIG_DB or extras —
	// the seam that keeps this tier green inside a worker pod.
	const prevDb = process.env.RIG_DB;
	try {
		process.env.RIG_DB = "/nonexistent/from-env.db";
		process.env.RIG_DB_CANDIDATES = "";
		assert.deepEqual(resolveRigDbCandidates(undefined, ["/workspace/rig.db"]), [], "empty override suppresses the whole walk");
		// exact list: replaces the walk, ignoring RIG_DB and extras
		process.env.RIG_DB_CANDIDATES = "/exact/one.db,/exact/two.db";
		assert.deepEqual(resolveRigDbCandidates("/explicit.db", ["/workspace/rig.db"]), ["/exact/one.db", "/exact/two.db"]);
		// the hatch outranks confinement too — that is what makes
		// execute-level provenance states testable (index.runtime.test.ts).
		process.env.RIG_EXPECTED_SHA = "0123abcd";
		assert.deepEqual(resolveRigDbCandidates(undefined, ["/workspace/rig.db"]), ["/exact/one.db", "/exact/two.db"]);
		delete process.env.RIG_EXPECTED_SHA;
		// confinement without the hatch: extras only, RIG_DB dropped
		process.env.RIG_DB_CANDIDATES = undefined;
		delete process.env.RIG_DB_CANDIDATES;
		process.env.RIG_EXPECTED_SHA = "0123abcd";
		assert.deepEqual(resolveRigDbCandidates(undefined, ["/workspace/rig.db"]), ["/workspace/rig.db"], "confinement drops env candidates");
		delete process.env.RIG_EXPECTED_SHA;
	} finally {
		if (prevDb === undefined) delete process.env.RIG_DB;
		else process.env.RIG_DB = prevDb;
		delete process.env.RIG_DB_CANDIDATES;
		delete process.env.RIG_EXPECTED_SHA;
	}
});
