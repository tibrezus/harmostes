/**
 * Contract canary (#486 class) — the turn-budget lane's load-bearing
 * assumption is pi's tool_call event shape, and BOTH live failures of the
 * budget extension were shape/contract assumptions that only failed in a
 * paid review round (v1: a `write` tool that isn't registered; v2: the
 * command under `input`, not `args`). This canary reads the INSTALLED
 * @earendil-works/pi-coding-agent type definitions — types are
 * authoritative over prose docs, exactly the lesson of 55de07e6351f — and
 * fails at build time when the shape our reader depends on moves.
 *
 * Skips (loudly) when the package is not installed; the Makefile installs
 * it at the Dockerfile's pinned PI_VERSION so CI always exercises this.
 */
import assert from "node:assert/strict";
import { readFileSync, existsSync } from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { dirname } from "node:path";
import { test } from "node:test";
import { eventArgs } from "./policy.ts";

function resolvePiTypes(): string | null {
	// candidates: this dir's node_modules (the Makefile-installed pinned
	// copy), then anything resolvable up-tree / globally via require
	const here = path.join(
		dirname(fileURLToPath(import.meta.url)),
		"node_modules", "@earendil-works", "pi-coding-agent",
		"dist", "core", "extensions", "types.d.ts",
	);
	if (existsSync(here)) return here;
	try {
		const require = createRequire(import.meta.url);
		const pkg = require.resolve("@earendil-works/pi-coding-agent");
		// resolve returns the main entry — walk up to the package root
		let dir = path.dirname(pkg);
		for (let i = 0; i < 6; i++) {
			const cand = path.join(dir, "dist", "core", "extensions", "types.d.ts");
			if (existsSync(cand)) return cand;
			dir = path.dirname(dir);
		}
	} catch {
		// not installed
	}
	return null;
}

test("contract canary: pi's BashToolCallEvent carries `input` — the key eventArgs reads", (t) => {
	const typesPath = resolvePiTypes();
	if (!typesPath) {
		return t.skip(
			"@earendil-works/pi-coding-agent not installed — `npm install --prefix extensions/turn-budget` (pinned to the Dockerfile's PI_VERSION) to exercise the canary",
		);
	}
	const types = readFileSync(typesPath, "utf8");
	const m = types.match(/export interface BashToolCallEvent extends ToolCallEventBase \{([^}]*)\}/);
	assert.ok(m, "BashToolCallEvent not found in pi's types.d.ts — the type layout moved; re-derive the tool_call arg key and update eventArgs()");
	assert.match(
		m![1],
		/\binput:\s*\w+/,
		"pi's BashToolCallEvent no longer carries `input` — update eventArgs() in policy.ts (this exact drift turned the v2 finalize lane inert: attempt 55de07e6351f)",
	);
	// the runtime half: our reader agrees with the type contract
	assert.equal(eventArgs({ input: { command: "cat > /workspace/review.json" } }).command, "cat > /workspace/review.json");
});
