import assert from "node:assert/strict";
import { test } from "node:test";
import { decide, eventArgs, parseAllow, parseBudget, DEFAULT_ALLOW, finalizeReason } from "./policy.ts";

test("no/0/unparsable budget is inert", () => {
	for (const raw of [undefined, "", "0", "-3", "abc", "  "]) {
		assert.equal(parseBudget(raw), 0, `raw=${String(raw)}`);
		assert.deepEqual(decide("bash", { command: "ls" }, 999, parseBudget(raw), []), { allow: true });
	}
	assert.equal(parseBudget("24"), 24);
	assert.equal(parseBudget(" 32 "), 32);
});

test("calls within budget always pass", () => {
	assert.deepEqual(decide("bash", { command: "grep -rn x /workspace/repo" }, 24, 24, [DEFAULT_ALLOW]), { allow: true });
	assert.deepEqual(decide("bash", { command: "grep -rn x /workspace/repo" }, 1, 24, []), { allow: true });
});

test("v1 regression: past the cap the finalize lane (bash touching review.json) stays open", () => {
	// The exact call that bricked attempt 57a1232749c0 — a python3 heredoc
	// writing review.json — must EXECUTE past the cap.
	const writeCall = "cd /workspace && python3 - <<'PY'\nimport json\nr = {\"decision\": \"REQUEST_CHANGES\"}\nopen('/workspace/review.json','w').write(json.dumps(r))\nPY";
	assert.deepEqual(decide("bash", { command: writeCall }, 25, 24, parseAllow(undefined)), { allow: true });
	assert.deepEqual(decide("bash", { command: "ls -l /workspace/review.json && cat /workspace/review.json | head -5" }, 30, 24, [DEFAULT_ALLOW]), { allow: true });
});

test("v1 regression: a phantom write tool must not be REQUIRED — but is allowed when registered", () => {
	assert.deepEqual(decide("write", { path: "/workspace/review.json" }, 25, 24, []), { allow: true });
	assert.deepEqual(decide("edit", { path: "/workspace/review.json" }, 25, 24, []), { allow: true });
});

test("past the cap, non-finalize calls are refused with the lane named", () => {
	const d = decide("bash", { command: "grep -rn codeberg /workspace/repo/chart" }, 25, 24, parseAllow(undefined));
	assert.equal(d.allow, false);
	assert.match(d.reason!, /budget exhausted \(25 calls > 24 allowed\)/);
	assert.ok(d.reason!.includes(String(DEFAULT_ALLOW)), "reason names the lane");
	assert.match(d.reason!, /finalize lane/i);
});

test("non-bash tools past the cap are refused (read/rig/web_search cannot finalize)", () => {
	for (const tool of ["read", "rig", "web_search"]) {
		const d = decide(tool, {}, 25, 24, [DEFAULT_ALLOW]);
		assert.equal(d.allow, false, tool);
	}
});

test("blocked attempts do not consume budget — counting is executed-only", () => {
	// The caller never increments on a block; simulate: 30 executed, 100
	// blocked retries, still the same decision shape.
	for (let i = 0; i < 100; i++) {
		const d = decide("bash", { command: "grep x" }, 30, 24, [DEFAULT_ALLOW]);
		assert.equal(d.allow, false);
	}
	assert.deepEqual(decide("bash", { command: "cat /workspace/review.json" }, 30, 24, [DEFAULT_ALLOW]), { allow: true });
});

test("empty allow env falls back to the fleet default — never a bricked agent", () => {
	assert.deepEqual(parseAllow(undefined), [DEFAULT_ALLOW]);
	assert.deepEqual(parseAllow(""), [DEFAULT_ALLOW]);
	assert.deepEqual(parseAllow("  "), [DEFAULT_ALLOW]);
	assert.deepEqual(parseAllow("/tmp/out.json , /workspace/review.json"), ["/tmp/out.json", "/workspace/review.json"]);
});

test("allow matching is substring-based on the command string", () => {
	assert.deepEqual(decide("bash", { command: "echo review.json && grep x repo" }, 25, 24, ["review.json"]), { allow: true });
	// near-miss: different filename does not open the lane
	assert.equal(decide("bash", { command: "cat /workspace/review.json.bak" }, 25, 24, [DEFAULT_ALLOW]).allow, true); // substring — accepted by design
	assert.equal(decide("bash", { command: "cat /workspace/reviews.json" }, 25, 24, [DEFAULT_ALLOW]).allow, false);
});

test("finalizeReason names the count, the cap, and the lane", () => {
	const msg = finalizeReason(31, 30, "/workspace/review.json");
	assert.match(msg, /31 calls > 30 allowed/);
	assert.match(msg, /\/workspace\/review\.json/);
});

test("eventArgs reads pi's `input` key (types.d.ts) with `args` fallback", () => {
	// the shape that bricked attempt 55de07e6351f: command under `input`
	assert.deepEqual(eventArgs({ input: { command: "cat > /workspace/review.json" } }), { command: "cat > /workspace/review.json" });
	assert.deepEqual(eventArgs({ args: { command: "ls" } }), { command: "ls" });
	// JSON-string form parses defensively
	assert.deepEqual(eventArgs({ input: JSON.stringify({ command: "ls" }) }), { command: "ls" });
	assert.deepEqual(eventArgs({ input: "not json" }), {});
	assert.deepEqual(eventArgs(undefined), {});
	assert.deepEqual(eventArgs({}), {});
});

test("live regression: post-cap write via input-carried command opens the lane", () => {
	// exact shape from the 55de07e6351f failure: pi delivers the command
	// under `input`; the heredoc write must be ALLOWED at 33 > 32
	const d = decide("bash", eventArgs({ input: { command: "cat > /workspace/review.json <<'EOF'\n{}\nEOF" } }), 33, 32, parseAllow("/workspace/review.json"));
	assert.deepEqual(d, { allow: true });
	// and a non-lane command still blocks
	const b = decide("bash", eventArgs({ input: { command: "grep -rn x /workspace/repo" } }), 33, 32, parseAllow("/workspace/review.json"));
	assert.equal(b.allow, false);
});
