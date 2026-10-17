import assert from "node:assert/strict";
import { test } from "node:test";
import {
	decide, eventArgs, fingerprint, newState, parseAllow, parseBudget, parseNudge,
	parseWallSeconds, parseConvergeMargin, parseFinalizeMargin,
	DEFAULT_ALLOW, STAGNATION_THRESHOLD,
	budgetExhaustedReason, convergeReason, finalizeReason, loopReason, nudgeReason, stagnationReason,
} from "./policy.ts";

// ─── legacy numeric-cap surface (kept for workflows that set one) ───

test("no/0/unparsable budget is inert", () => {
	for (const raw of [undefined, "", "0", "-3", "abc", "  "]) {
		assert.equal(parseBudget(raw), 0, `raw=${String(raw)}`);
		const st = newState();
		assert.deepEqual(decide("bash", { command: "ls" }, st, [], { budget: parseBudget(raw) }), { allow: true });
	}
	assert.equal(parseBudget("24"), 24);
	assert.equal(parseBudget(" 32 "), 32);
});

test("numeric cap: within budget passes, past it the finalize lane stays open", () => {
	const writeCall = "cd /workspace && python3 - <<'PY'\nopen('/workspace/review.json','w').write('{}')\nPY";
	const st = newState();
	for (let i = 0; i < 24; i++) {
		assert.deepEqual(decide("bash", { command: `probe-${i}` }, st, parseAllow(undefined), { budget: 24 }), { allow: true });
	}
	// past the cap: the artifact lane still executes
	assert.deepEqual(decide("bash", { command: writeCall }, st, parseAllow(undefined), { budget: 24 }), { allow: true });
	assert.deepEqual(decide("write", { path: "/workspace/review.json" }, st, []), { allow: true });
	// non-lane work is refused with the exhaustion message
	const d = decide("bash", { command: "grep -rn x /workspace/repo" }, st, [DEFAULT_ALLOW], { budget: 24 });
	assert.equal(d.allow, false);
	assert.ok(d.reason!.startsWith("Tool budget exhausted"));
	assert.equal(d.reason, budgetExhaustedReason(26, 24, DEFAULT_ALLOW));
});

test("numeric cap: the nudge fires exactly once at nudgeAt", () => {
	const st = newState();
	const opts = { budget: 10, nudgeAt: 5 };
	for (let i = 0; i < 5; i++) decide("bash", { command: `w${i}` }, st, [], opts);
	const d = decide("bash", { command: "w5" }, st, [], opts);
	assert.equal(d.nudge, true);
	assert.equal(d.reason, nudgeReason(5, 10, DEFAULT_ALLOW));
	// one-shot
	const d2 = decide("bash", { command: "w6" }, st, [], opts);
	assert.equal(d2.nudge, undefined);
});

// ─── v3 conceptual layers (#689) ───

const RB = { loopBreaker: true, wallSeconds: 0 };

test("fingerprint: canonicalizes whitespace, keeps meaningful differences", () => {
	assert.equal(fingerprint("bash", { command: "ls   -la /w" }), fingerprint("bash", { command: "ls -la /w" }));
	assert.notEqual(fingerprint("bash", { command: "ls /a" }), fingerprint("bash", { command: "ls /b" }));
	assert.notEqual(fingerprint("read", { path: "/w/x" }), fingerprint("bash", { command: "cat /w/x" }));
});

test("loop breaker: a repeated call is refused with a redirect, and never counts as work", () => {
	const st = newState();
	const call = { command: "grep -rn TODO /workspace/repo" };
	assert.deepEqual(decide("bash", call, st, [DEFAULT_ALLOW], RB), { allow: true });
	assert.equal(st.seen.size, 1);
	const d = decide("bash", call, st, [DEFAULT_ALLOW], RB);
	assert.equal(d.allow, false);
	assert.equal(d.loop, true);
	assert.equal(d.reason, loopReason("grep -rn TODO /workspace/repo"));
	// the repeat did NOT grow the executed memory (a refusal is not work done)
	assert.equal(st.seen.size, 1);
	assert.equal(st.loopStreak, 1);
	// a DIFFERENT call resets the stagnation streak
	assert.deepEqual(decide("bash", { command: "cat /workspace/other" }, st, [DEFAULT_ALLOW], RB), { allow: true });
	assert.equal(st.loopStreak, 0);
});

test("loop breaker: the worst observed offender — 10 re-reads of one file — is refused every time", () => {
	const st = newState();
	const file = { path: "/workspace/repo/src/backend/decode_vulkan.zig" };
	assert.deepEqual(decide("read", file, st, [], RB), { allow: true });
	for (let i = 0; i < 9; i++) {
		const d = decide("read", file, st, [], RB);
		assert.equal(d.allow, false);
	}
});

test("loop breaker: the finalize lane may repeat (composing legitimately re-touches the artifact)", () => {
	const st = newState();
	const append = { command: "cd /workspace && python3 -c \"open('/workspace/review.json','a').write('{}')\"" };
	assert.deepEqual(decide("bash", append, st, [DEFAULT_ALLOW], RB), { allow: true });
	// past the cap AND repeated: the lane wins
	st.executed = 999;
	const d = decide("bash", append, st, [DEFAULT_ALLOW], { ...RB, budget: 10 });
	assert.equal(d.allow, true);
});

test("stagnation: K consecutive loop-refusals fire the checkpoint (max twice)", () => {
	const st = newState();
	const call = { command: "grep -rn x /workspace/repo" };
	decide("bash", call, st, [DEFAULT_ALLOW], RB); // fresh execution
	const d = decide("bash", call, st, [DEFAULT_ALLOW], RB); // refused (streak 1)
	assert.equal(d.loop, true);
	// streaks 2 and 3 are plain loop refusals; the 4th fires the checkpoint
	assert.equal(decide("bash", call, st, [DEFAULT_ALLOW], RB).loop, true);
	assert.equal(decide("bash", call, st, [DEFAULT_ALLOW], RB).loop, true);
	const sd = decide("bash", call, st, [DEFAULT_ALLOW], RB);
	assert.equal(sd.nudge, true);
	assert.equal(sd.reason, stagnationReason(STAGNATION_THRESHOLD, DEFAULT_ALLOW));
	// second stagnation fires after another K; a third never comes
	let fired = 0;
	for (let i = 0; i < 3 * STAGNATION_THRESHOLD; i++) {
		const x = decide("bash", call, st, [DEFAULT_ALLOW], RB);
		if (x.nudge) fired++;
	}
	assert.equal(fired, 1); // the second (final) stagnation fire
	assert.equal(st.stagnationFired, 2);
});

test("wall convergence: the checkpoint fires once at the converge margin — regardless of call count", () => {
	const st = newState();
	// 101 minutes into a 120m wall: the count cap (200) would not have fired
	// at the measured 57-calls/h pace — time does not drift (#689).
	// prime: the session's first call anchors the wall clock at real now
	decide("bash", { command: "prime" }, st, [DEFAULT_ALLOW], {
		loopBreaker: true, wallSeconds: 120 * 60, convergeMarginSeconds: 20 * 60,
	});
	const realNow = Date.now;
	Date.now = () => realNow() + 101.5 * 60 * 1000; // 18.5m left → floor 18, deterministic
	try {
		const d = decide("bash", { command: "grep -rn x /w" }, st, [DEFAULT_ALLOW], {
			loopBreaker: true, wallSeconds: 120 * 60, convergeMarginSeconds: 20 * 60,
		});
		assert.equal(d.nudge, true);
		assert.equal(d.reason, convergeReason(18, DEFAULT_ALLOW));
		// one-shot
		Date.now = () => realNow() + 102 * 60 * 1000;
		const d2 = decide("bash", { command: "grep -rn y /w" }, st, [DEFAULT_ALLOW], {
			loopBreaker: true, wallSeconds: 120 * 60, convergeMarginSeconds: 20 * 60,
		});
		assert.equal(d2.allow, true);
		assert.equal(d2.nudge, undefined);
	} finally {
		Date.now = realNow;
	}
});

test("wall finalize: past the finalize margin only the lane runs", () => {
	const st = newState();
	decide("bash", { command: "prime" }, st, [DEFAULT_ALLOW], { loopBreaker: true, wallSeconds: 120 * 60 });
	const realNow = Date.now;
	Date.now = () => realNow() + 115 * 60 * 1000; // 5m left
	try {
		const d = decide("bash", { command: "grep -rn x /w" }, st, [DEFAULT_ALLOW], {
			loopBreaker: true, wallSeconds: 120 * 60,
		});
		assert.equal(d.allow, false);
		assert.equal(d.reason, finalizeReason(DEFAULT_ALLOW));
		const lane = decide("bash", { command: "python3 -c \"open('/workspace/review.json','w')\"" }, st, [DEFAULT_ALLOW], {
			loopBreaker: true, wallSeconds: 120 * 60,
		});
		assert.equal(lane.allow, true);
	} finally {
		Date.now = realNow;
	}
});

test("wall layers are inert without the arming env (non-review workflows keep v2 behavior)", () => {
	const st = newState();
	const call = { command: "git fetch origin main" };
	assert.deepEqual(decide("bash", call, st, [], { wallSeconds: 0 }), { allow: true });
	// same call again, no loopBreaker: allowed (fork-maintenance retries after upstream fixes)
	assert.deepEqual(decide("bash", call, st, [], { wallSeconds: 0 }), { allow: true });
	// loopBreaker off even with a wall configured
	assert.deepEqual(decide("bash", call, st, [], { loopBreaker: false, wallSeconds: 7200 }), { allow: true });
});

test("margins parse: defaults and explicit values", () => {
	assert.equal(parseConvergeMargin(undefined), 20 * 60);
	assert.equal(parseConvergeMargin("600"), 600);
	assert.equal(parseFinalizeMargin(undefined), 8 * 60);
	assert.equal(parseWallSeconds("7200"), 7200);
	assert.equal(parseWallSeconds("abc"), 0);
});

test("eventArgs: reads input first, parses JSON-string shapes", () => {
	assert.deepEqual(eventArgs({ input: { command: "ls" } }), { command: "ls" });
	assert.deepEqual(eventArgs({ args: "{\"command\":\"ls\"}" }), { command: "ls" });
	assert.deepEqual(eventArgs(undefined), {});
});
