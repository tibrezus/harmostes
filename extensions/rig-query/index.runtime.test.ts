/**
 * Runtime tests for the pi wrapper's non-parse logic (#338 r5 B2 / r6 M4):
 * the (ino, mtime) handle identity across a producer-style unlink+recreate,
 * session_shutdown closing handles, and the container-path fallback contract.
 * Requires typebox (devDependency) so index.ts fully loads.
 */
import assert from "node:assert/strict";
import { test } from "node:test";
import { copyFileSync, readFileSync, rmSync, statSync, unlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";

const FIXTURE = fileURLToPath(new URL("./fixtures/rig.db", import.meta.url));

interface ToolDef {
	name: string;
	execute: (id: string, params: unknown, signal: unknown, onUpdate: unknown, ctx: { cwd: string }) => Promise<{
		content: Array<{ type: string; text: string }>;
		details: Record<string, unknown>;
	}>;
}

function makeHarness(dir: string) {
	const tools: ToolDef[] = [];
	const handlers: Record<string, (() => void)[]> = {};
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	return import("./index.ts").then((mod: any) => {
		mod.default({
			on: (event: string, fn: () => void) => {
				(handlers[event] ??= []).push(fn);
			},
			registerTool: (t: ToolDef) => tools.push(t),
		});
		const execute = (params: unknown) => tools[0].execute("t1", params, undefined, undefined, { cwd: dir });
		const shutdown = () => (handlers.session_shutdown ?? []).forEach((fn) => fn());
		return { execute, shutdown };
	});
}

test("wrapper: execute answers from the graph and reports provenance + identity", async () => {
	const dir = tmpdir();
	const dbPath = `${dir}/rig-runtime-${process.pid}.db`;
	copyFileSync(FIXTURE, dbPath);
	process.env.RIG_DB = dbPath; // the documented discovery env — resolution is env-first
	try {
		const { execute } = await makeHarness(dir);
		const first = await execute({ command: "overview" });
		assert.match(first.content[0].text, /fixture-repo/);
		assert.equal(first.details.graph, true, "present graph flagged");
		// The details KEY SET is the #336 join interface — asserted in FULL on
		// both branches so a rename is a test failure, not a silent missing
		// column (#338 r17 M7).
		assert.deepEqual(Object.keys(first.details).sort(), [
			"chars", "command", "db", "graph", "probed", "resolved", "rig_sha", "sha_state", "target", "truncated",
		]);

		// Producer-style re-emit: unlink + write = a NEW inode at the same path.
		// The (ino, mtime) identity check must reopen, not serve the stale handle.
		const before = statSync(dbPath).ino;
		unlinkSync(dbPath);
		copyFileSync(FIXTURE, dbPath);
		const after = statSync(dbPath).ino;
		assert.notEqual(before, after, "test setup must produce a new inode");
		const second = await execute({ command: "overview" });
		assert.match(second.content[0].text, /fixture-repo/, "post-re-emit query still answers");
		assert.equal(second.details.db, dbPath);
	} finally {
		delete process.env.RIG_DB;
		rmSync(dbPath, { force: true });
	}
});

test("wrapper: session_shutdown closes every handle (idempotent)", async () => {
	const dir = tmpdir();
	const dbPath = `${dir}/rig-shutdown-${process.pid}.db`;
	copyFileSync(FIXTURE, dbPath);
	process.env.RIG_DB = dbPath;
	try {
		const { execute, shutdown } = await makeHarness(dir);
		await execute({ command: "overview" });
		shutdown();
		shutdown(); // idempotent by contract
		// A query after shutdown must still WORK (handle reopens on demand).
		const again = await execute({ command: "overview" });
		assert.match(again.content[0].text, /fixture-repo/);
	} finally {
		delete process.env.RIG_DB;
		rmSync(dbPath, { force: true });
	}
});

test("wrapper: the container fallback walk is real and suppressible (#338 r6 B2 / r16 C1 / r23 P8)", async () => {
	// Behaviour, not source text: with the fallback walk active and no env
	// override, resolution lands on the container path prepare emits to.
	// RIG_DB_CANDIDATES replaces the walk with an exact list ("" = full
	// suppression) — the hermeticity hatch that keeps this tier green inside
	// a worker pod, where /workspace/rig.db actually exists.
	const { resolveRigDb, resolveRigDbCandidates } = await import("./queries.ts");
	const candidates = resolveRigDbCandidates(undefined, ["/workspace/rig.db"]);
	assert.ok(candidates.includes("/workspace/rig.db"), "the container contract must stay in the walk");
	assert.ok(!candidates.includes("/workspace/repo/rig.db"), "the PR checkout is repo content — never a graph source (r20 S1 / r23 P5)");
	process.env.RIG_DB_CANDIDATES = "";
	try {
		assert.equal(resolveRigDb(undefined, ["/workspace/rig.db"]), null, "empty override suppresses the fallback walk — filesystem-independent");
	} finally {
		delete process.env.RIG_DB_CANDIDATES;
	}
	// The override is an EXACT list: it outranks both RIG_DB and confinement,
	// which is what makes execute-level provenance states testable at all.
	const exact = resolveRigDbCandidates(undefined, ["/workspace/rig.db"]);
	assert.ok(Array.isArray(exact));
});

test("sha provenance: stamped / mismatch / malformed / absent (#338 r9 B2)", async () => {
	const dir = tmpdir();
	const dbPath = `${dir}/rig-sha-${process.pid}.db`;
	copyFileSync(FIXTURE, dbPath);
	process.env.RIG_DB = dbPath;
	const shaPath = `${dbPath}.sha`;
	try {
		const { execute } = await makeHarness(dir);
		// absent
		let r = await execute({ command: "overview" });
		assert.equal(r.details.rig_sha, "absent");
		// stamped, nothing to verify against
		writeFileSync(shaPath, "0123abcd");
		r = await execute({ command: "overview" });
		assert.equal(r.details.rig_sha, "0123abcd");
		assert.equal(r.details.sha_state, "unchecked");
		// Execute-level states (r23 P8): the RIG_DB_CANDIDATES hatch injects the
		// tmp graph as an exact walk, so verified / mismatch / absent-refusal
		// are asserted through the REAL wrapper — refusal rows must be
		// reachable, countable telemetry, not dead branches.
		process.env.RIG_DB_CANDIDATES = dbPath;
		// verified: expectation matches the stamp → answered
		process.env.RIG_EXPECTED_SHA = "0123abcd";
		r = await execute({ command: "overview" });
		assert.equal(r.details.sha_state, "verified");
		assert.equal(r.details.resolved, true);
		assert.match(r.content[0].text, /fixture-repo/);
		// mismatch: REFUSED with structured telemetry — navigating another
		// revision is refused, but the event stays countable (r15).
		process.env.RIG_EXPECTED_SHA = "ffffffff";
		r = await execute({ command: "overview" });
		assert.equal(r.details.sha_state, "mismatch");
		assert.equal(r.details.resolved, false);
		assert.match(r.content[0].text, /REFUSED: this graph does not match the reviewed SHA/);
		assert.doesNotMatch(r.content[0].text, /fixture-repo/);
		// absent-refusal: expectation set but prepare never stamped → REFUSED
		// (r23 P4: the branch must be REACHABLE — an unstamped graph under a
		// demanded SHA is unverifiable, not "unverified but served").
		rmSync(shaPath, { force: true });
		r = await execute({ command: "overview" });
		assert.equal(r.details.sha_state, "absent-refusal");
		assert.equal(r.details.resolved, false);
		assert.match(r.content[0].text, /REFUSED: prepare did not stamp this graph/);
		assert.doesNotMatch(r.content[0].text, /fixture-repo/);
		delete process.env.RIG_EXPECTED_SHA;
		delete process.env.RIG_DB_CANDIDATES;
		// malformed (content never echoed — the F11 oracle class) — now REFUSED,
		// not caveat-served: an unreadable stamp establishes nothing (r24 D1).
		writeFileSync(shaPath, "root:x:0:0:secrets");
		process.env.RIG_DB_CANDIDATES = dbPath;
		r = await execute({ command: "overview" });
		assert.equal(r.details.rig_sha, "malformed");
		assert.equal(r.details.sha_state, "malformed");
		assert.equal(r.details.resolved, false);
		assert.match(r.content[0].text, /REFUSED: the graph's stamp is unreadable/);
		assert.doesNotMatch(r.content[0].text, /secrets/);
		// unchecked under RIG_REQUIRE_SHA=1 (r24 D1): a stamped graph with no
		// injected expectation must REFUSE — strictness is a per-run contract.
		writeFileSync(shaPath, "0123abcd");
		delete process.env.RIG_EXPECTED_SHA;
		process.env.RIG_REQUIRE_SHA = "1";
		r = await execute({ command: "overview" });
		assert.equal(r.details.sha_state, "unchecked");
		assert.equal(r.details.resolved, false);
		assert.match(r.content[0].text, /REFUSED: this run demands a verified graph/);
		assert.doesNotMatch(r.content[0].text, /fixture-repo/);
		delete process.env.RIG_REQUIRE_SHA;
		delete process.env.RIG_DB_CANDIDATES;
	} finally {
		delete process.env.RIG_EXPECTED_SHA;
		rmSync(shaPath, { force: true });
		rmSync(dbPath, { force: true });
	}
});

test("details key-set is an interface — uniform on success and absence (#338 r9)", async () => {
	const dir = tmpdir();
	process.env.RIG_DB = `${dir}/rig-missing-${process.pid}.db`;
	// Hermeticity hatch (r16 C1): suppress the container fallback walk so this
	// test cannot resolve a graph that exists in the pod it runs in.
	process.env.RIG_DB_CANDIDATES = "";
	try {
		const mod = await import("./index.ts");
		const tools: Array<{ execute: (id: string, params: unknown, s: unknown, u: unknown, ctx: { cwd: string }) => Promise<{ details: Record<string, unknown> }> }> = [];
		const handlers: Record<string, Array<() => void>> = {};
		mod.default({
			on: (event: string, fn: () => void) => (handlers[event] ??= []).push(fn),
			registerTool: (t: never) => tools.push(t),
		});
		const absent = await tools[0].execute("t", { command: "overview" }, undefined, undefined, { cwd: dir });
		assert.equal(absent.details.graph, false);
		assert.equal(absent.details.command, "overview", "absence keeps the telemetry shape");
		assert.equal(absent.details.truncated, false);
		assert.deepEqual(Object.keys(absent.details).sort(), [
			"chars", "command", "db", "graph", "probed", "resolved", "rig_sha", "sha_state", "target", "truncated",
		], "absence and success must expose the SAME key set");
	} finally {
		delete process.env.RIG_DB;
	}
});
