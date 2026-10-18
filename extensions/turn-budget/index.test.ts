/**
 * Consumer-side coverage (#695 r3 TODO): index.ts is the only reader of
 * PI_LOOP_BREAKER and the only wiring of the three conceptual layers —
 * before this file, a rename in either home (dispatch.go producer /
 * index.ts consumer) left every layer silently inert with green CI.
 */
import assert from "node:assert/strict";
import { test } from "node:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import ext from "./index.ts";

function makePi() {
	const handlers: Record<string, Function[]> = {};
	return {
		pi: {
			on: (ev: string, fn: Function) => {
				(handlers[ev] ??= []).push(fn);
			},
		} as never,
		fire: (ev: string, e: unknown) => {
			const h = (handlers[ev] ?? [])[0];
			if (!h) return "no-handler"; // the extension never armed
			return h(e);
		},
	};
}

function withEnv(vars: Record<string, string | undefined>, fn: () => void) {
	const saved: Record<string, string | undefined> = {};
	for (const [k, v] of Object.entries(vars)) {
		saved[k] = process.env[k];
		if (v === undefined) delete process.env[k];
		else process.env[k] = v;
	}
	try {
		fn();
	} finally {
		for (const [k, v] of Object.entries(saved)) {
			if (v === undefined) delete process.env[k];
			else process.env[k] = v;
		}
	}
}

test("armed review session: no numeric budget, loop breaker refuses a repeat, allows fresh work", () => {
	withEnv({
		PI_TOOL_BUDGET: undefined,
		PI_TOOL_BUDGET_ALLOW: "/workspace/review.json",
		PI_LOOP_BREAKER: "1",
		HARMOSTES_WALL_SECONDS: "7200",
		HARMOSTES_RUN_STARTED_MS: String(Date.now()),
		PI_WALL_CONVERGE_MARGIN: undefined,
		PI_WALL_FINALIZE_MARGIN: undefined,
	}, () => {
		const { pi, fire } = makePi();
		ext(pi);
		const repeat = { toolName: "read", input: { path: "/workspace/repo/src/a.zig" } };
		assert.equal(fire("tool_call", repeat), undefined); // fresh: allowed
		const d = fire("tool_call", repeat);
		assert.equal(d.block, true);
		assert.match(d.reason, /already executed this exact call/);
	});
});

test("inert without arming: no breaker env and no budget registers a no-op (no handler side effects)", () => {
	withEnv({
		PI_TOOL_BUDGET: undefined,
		PI_LOOP_BREAKER: undefined,
		HARMOSTES_WALL_SECONDS: undefined,
	}, () => {
		const { pi, fire } = makePi();
		ext(pi);
		const repeat = { toolName: "read", input: { path: "/workspace/repo/src/a.zig" } };
		assert.equal(fire("tool_call", repeat), "no-handler"); // never armed: no enforcement at all
	});
});

test("the emitted name equals the read name — the textual pin across the two homes", () => {
	const producer = readFileSync(path.join(import.meta.dirname, "../../internal/worker/dispatch.go"), "utf8");
	const consumer = readFileSync(path.join(import.meta.dirname, "index.ts"), "utf8");
	assert.ok(producer.includes('"PI_LOOP_BREAKER=1"'), "dispatch.go must emit PI_LOOP_BREAKER=1");
	assert.ok(consumer.includes('process.env.PI_LOOP_BREAKER'), "index.ts must read PI_LOOP_BREAKER");
});
