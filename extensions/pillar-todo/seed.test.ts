/**
 * pillar-todo tests — the seed-manifest contract and the tool/state
 * behavior over a fake ExtensionAPI. node --test with type stripping,
 * same tier as rig-query/turn-budget.
 */
import assert from "node:assert/strict";
import { test } from "node:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

// Under --experimental-strip-types the .ts extension must be explicit.
const { parseSeedManifest, default: pillarTodo } = await import("./index.ts");

test("parseSeedManifest: the plugin's shape — array of strings", () => {
	assert.deepEqual(parseSeedManifest(JSON.stringify(["Correctness", "Security", "Performance"])), [
		"Correctness",
		"Security",
		"Performance",
	]);
});

test("parseSeedManifest: {text} objects accepted, whitespace trimmed", () => {
	assert.deepEqual(parseSeedManifest(`[{"text":" Boundaries (structural fit) "}]`), [
		"Boundaries (structural fit)",
	]);
});

test("parseSeedManifest: null on every non-manifest shape (never throw)", () => {
	for (const raw of [
		"",
		"not json",
		"{}",
		"42",
		"null",
		"[]", // empty: no checklist
		'["ok", 42]', // one bad entry poisons the manifest
		'["ok", {"nope":1}]',
		'["ok", null]',
		'["   "]', // blank-only entry poisons too
	]) {
		assert.equal(parseSeedManifest(raw), null, `shape must reject: ${raw}`);
	}
});

// ── tool behavior over a fake ExtensionAPI ────────────────────────────────
type Entry = { type: string; message?: { role: string; toolName?: string; details?: unknown } };

interface Harness {
	todo: (params: { action: string; text?: string; id?: number }) => Promise<any>;
	sessionStart: () => Promise<void>;
}

function harness(sessionEntries: Entry[], manifestDir?: string): Harness {
	let todoHandler: (id: string, params: any) => Promise<any> = async () => null;
	let sessionStart: ((e: unknown, ctx: unknown) => Promise<void>) | undefined;
	const api = {
		on(event: string, handler: (e: unknown, ctx: unknown) => Promise<void>) {
			if (event === "session_start") sessionStart = handler;
		},
		registerTool(spec: { name: string; execute: (id: string, params: any) => Promise<any> }) {
			if (spec.name === "todo") todoHandler = spec.execute;
		},
		registerCommand() {},
	} as unknown as import("@earendil-works/pi-coding-agent").ExtensionAPI;
	const ctx = {
		sessionManager: { getBranch: () => sessionEntries },
	} as unknown as import("@earendil-works/pi-coding-agent").ExtensionContext;
	const saved = process.env.HARMOSTES_WORKDIR;
	if (manifestDir === undefined) delete process.env.HARMOSTES_WORKDIR;
	else process.env.HARMOSTES_WORKDIR = manifestDir;
	pillarTodo(api); // the module's default installs handlers onto the fake
	const h = {
		todo: (params: { action: string; text?: string; id?: number }) => todoHandler("t1", params),
		sessionStart: async () => {
			if (!sessionStart) throw new Error("session_start handler not registered");
			await sessionStart(undefined, ctx);
		},
	};
	// restore env when the test ends (best effort: GC-free but bounded)
	process.on("test:end", () => {
		if (saved === undefined) delete process.env.HARMOSTES_WORKDIR;
		else process.env.HARMOSTES_WORKDIR = saved;
	});
	void h; // harness returned below
	return Object.assign(h, {
		[Symbol.dispose]() {},
	}) as Harness;
}

function withManifest(items: string[]): string {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pillar-todo-"));
	fs.writeFileSync(path.join(dir, "pillar-todos.json"), JSON.stringify(items));
	return dir;
}

test("fresh session + manifest: seeded unticked, ids ascending", async () => {
	const dir = withManifest(["Correctness", "Security", "Performance"]);
	const h = harness([], dir);
	await h.sessionStart();
	const list = await h.todo({ action: "list" });
	const details = list.details as { todos: { id: number; text: string; done: boolean }[] };
	assert.equal(details.todos.length, 3);
	assert.deepEqual(
		details.todos.map((t) => [t.id, t.text, t.done]),
		[
			[1, "Correctness", false],
			[2, "Security", false],
			[3, "Performance", false],
		],
	);
});

test("no manifest: empty list, tool still works (add/toggle)", async () => {
	const h = harness([], undefined);
	await h.sessionStart();
	const list = await h.todo({ action: "list" });
	assert.match(String(list.content[0].text), /No todos/);
	await h.todo({ action: "add", text: "manual" });
	const r = await h.todo({ action: "toggle", id: 1 });
	assert.match(String(r.content[0].text), /completed/);
});

test("malformed manifest: rejected by the parse contract — empty list", async () => {
	const dir = withManifest(["Correctness"]);
	fs.writeFileSync(path.join(dir, "pillar-todos.json"), '["ok", 42]'); // poisoned
	const h = harness([], dir);
	await h.sessionStart();
	const list = await h.todo({ action: "list" });
	assert.match(String(list.content[0].text), /No todos/);
});

test("resumed session: branch state wins — manifest ignored even when present", async () => {
	const dir = withManifest(["Fresh-Seed-Should-Not-Appear"]);
	const branch: Entry[] = [
		{
			type: "message",
			message: {
				role: "toolResult",
				toolName: "todo",
				details: {
					action: "toggle",
					todos: [{ id: 1, text: "Correctness", done: true }],
					nextId: 2,
				},
			},
		},
	];
	const h = harness(branch, dir);
	await h.sessionStart();
	const list = await h.todo({ action: "list" });
	const details = list.details as { todos: { text: string; done: boolean }[] };
	assert.equal(details.todos.length, 1);
	assert.equal(details.todos[0].text, "Correctness");
	assert.equal(details.todos[0].done, true); // the tick survived
});

test("ticks persist across toggles and clear resets", async () => {
	const h = harness([], withManifest(["A", "B"]));
	await h.sessionStart();
	await h.todo({ action: "toggle", id: 1 });
	const list = await h.todo({ action: "list" });
	assert.match(String(list.content[0].text), /\[x\] #1: A/);
	assert.match(String(list.content[0].text), /\[ \] #2: B/);
	await h.todo({ action: "clear" });
	const after = await h.todo({ action: "list" });
	assert.match(String(after.content[0].text), /No todos/);
	// clear also resets ids: the next add starts at #1 again
	await h.todo({ action: "add", text: "C" });
	const list2 = await h.todo({ action: "list" });
	assert.match(String(list2.content[0].text), /#1: C/);
});
