/**
 * Runtime tests for the pi wrapper's non-parse logic (#338 r5 B2 / r6 M4):
 * the (ino, mtime) handle identity across a producer-style unlink+recreate,
 * session_shutdown closing handles, and the container-path fallback contract.
 * Requires typebox (devDependency) so index.ts fully loads.
 */
import assert from "node:assert/strict";
import { test } from "node:test";
import { copyFileSync, readFileSync, rmSync, statSync, unlinkSync } from "node:fs";
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
	try {
		const { execute } = await makeHarness(dir);
		const first = await execute({ command: "overview" });
		assert.match(first.content[0].text, /fixture-repo/);
		assert.equal(first.details.graph, undefined, "present graph is not flagged graph:false");

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
		rmSync(dbPath, { force: true });
	}
});

test("wrapper: session_shutdown closes every handle (idempotent)", async () => {
	const dir = tmpdir();
	const dbPath = `${dir}/rig-shutdown-${process.pid}.db`;
	copyFileSync(FIXTURE, dbPath);
	try {
		const { execute, shutdown } = await makeHarness(dir);
		await execute({ command: "overview" });
		shutdown();
		shutdown(); // idempotent by contract
		// A query after shutdown must still WORK (handle reopens on demand).
		const again = await execute({ command: "overview" });
		assert.match(again.content[0].text, /fixture-repo/);
	} finally {
		rmSync(dbPath, { force: true });
	}
});

test("wrapper: the container fallback list contains prepare's emit target (#338 r6 B2)", () => {
	// The cross-repo contract, asserted so it fails loudly in CI: ops
	// workspace.sh writes $WORKDIR/rig.db and the worker WORKDIR is /workspace.
	const src = readFileSync(fileURLToPath(new URL("./index.ts", import.meta.url)), "utf8");
	assert.match(src, /"\/workspace\/rig\.db"/, "index.ts fallback must include prepare's /workspace/rig.db emit target");
});
