/**
 * turn-budget policy — pure decision logic, testable without the pi runtime
 * (the litellm-provider fallbacks.ts pattern).
 *
 * v3 semantics (#689 — "caps should be conceptual"): the numeric call cap is
 * DEMOTED to an emergency stop for workflows that want one; review sessions
 * are managed by three conceptual layers measured against the live fleet
 * data (2026-10-16/17 transcripts):
 *
 *  1. LOOP BREAKER — a review workspace is STATIC: re-executing an identical
 *     (tool, args) returns identical bytes. The worst observed offender
 *     re-read decode_vulkan.zig TEN times (the round that wall-died); 10-12%
 *     of all calls were repeats-in-window-10. A repeated fingerprint is
 *     refused with "you already have this output in context". No budget, no
 *     count — recognition, not arithmetic.
 *
 *  2. STAGNATION CHECKPOINT — progress = FIRST executions (new information).
 *     If the last K calls were ALL loop-refusals, the session is cycling,
 *     not working: fire the converge checkpoint (the one lever the model
 *     demonstrably obeys — #487). Escalates once; then the loop breaker
 *     alone holds the line.
 *
 *  3. WALL-MARGIN CONVERGENCE — the only hard bound is TIME, the actual
 *     resource. HARMOSTES_WALL_SECONDS + the session's first-call timestamp
 *     give remaining wall; at ≤ convergeMargin fire the converge checkpoint
 *     (once), at ≤ finalizeMargin flip to finalize-lane-only. Call-rate
 *     noise made the numeric cap untrustworthy: measured 110 calls/h on one
 *     round and 57/h on another (2×), so a count calibrated to one pace
 *     binds past the wall on the other. Time does not drift.
 *
 * The numeric cap (PI_TOOL_BUDGET) keeps its v2 semantics for workflows
 * that set it; sessions without it are now MANAGED (layers 1-3) instead of
 * unmanaged.
 */

/** Tools that produce/modify files — always allowed past any stop so the
 * agent can emit its artifact. `write` may not be registered in every
 * deployment; allowing a non-existent name here is harmless. */
export const FILE_TOOLS = new Set(["write", "edit"]);

export const DEFAULT_ALLOW = "/workspace/review.json";

export interface Decision {
	allow: boolean;
	reason?: string;
	/** This block was the converge checkpoint — one-shot per trigger; the
	 * caller flips the corresponding fired-flag on seeing it. */
	nudge?: boolean;
	/** This block was a loop refusal — the caller records the fingerprint as
	 * seen AND counts it toward stagnation. */
	loop?: boolean;
}

/** Parse PI_TOOL_BUDGET: absent/0/unparsable → 0 = inert (mode-3). */
export function parseBudget(raw: string | undefined): number {
	const n = Number.parseInt((raw ?? "").trim(), 10);
	return Number.isFinite(n) && n > 0 ? n : 0;
}

/** Parse PI_TOOL_BUDGET_ALLOW: comma-separated substrings; empty env falls
 * back to the fleet's review-output path so a deployment that forgets the
 * var still gets a workable finalize lane instead of a bricked agent. */
export function parseAllow(raw: string | undefined): string[] {
	const parts = (raw ?? "")
		.split(",")
		.map((s) => s.trim())
		.filter(Boolean);
	return parts.length > 0 ? parts : [DEFAULT_ALLOW];
}

/** Parse PI_TOOL_BUDGET_NUDGE: absent → budget/2 (checkpoint ON by default —
 * the block is the one lever the model demonstrably obeys: attempt ef2b683
 * went from blocked-at-33 to artifact-written on the very next call);
 * "0" → off; n ≥ budget or garbage → off (a checkpoint at or past the cap
 * is meaningless). */
export function parseNudge(raw: string | undefined, budget: number): number {
	const t = (raw ?? "").trim();
	if (t === "") return budget > 1 ? Math.floor(budget / 2) : 0;
	if (t === "0") return 0;
	const n = Number.parseInt(t, 10);
	return Number.isFinite(n) && n > 0 && n < budget ? n : 0;
}

/** Parse HARMOSTES_WALL_SECONDS: absent/0/garbage → 0 = wall layers inert
 * (a deployment without the wall env keeps the pre-#689 behavior). */
export function parseWallSeconds(raw: string | undefined): number {
	const n = Number.parseInt((raw ?? "").trim(), 10);
	return Number.isFinite(n) && n > 0 ? n : 0;
}

/** Parse the converge margin (seconds before the wall): default 20m —
 * enough for ledger + verdict synthesis at the measured output pace
 * (~3.4K tokens/min, synthesis ≈ 8-15K tokens). "0" disables. */
export function parseConvergeMargin(raw: string | undefined): number {
	const t = (raw ?? "").trim();
	if (t === "") return 20 * 60;
	const n = Number.parseInt(t, 10);
	return Number.isFinite(n) && n >= 0 ? n : 20 * 60;
}

/** Parse the finalize margin: default 8m — past this only the artifact
 * lane runs. Must be < the converge margin to mean anything. */
export function parseFinalizeMargin(raw: string | undefined): number {
	const t = (raw ?? "").trim();
	if (t === "") return 8 * 60;
	const n = Number.parseInt(t, 10);
	return Number.isFinite(n) && n >= 0 ? n : 8 * 60;
}

/** Canonicalize a tool call into a loop-comparable fingerprint — over ALL
 * arguments, sorted (r2 4302417584: a primary-key fingerprint collided
 * chunked reads — read {path, offset=1} vs {path, offset=201} are DIFFERENT
 * calls, the documented way past the 2000-line read cap — and every grep
 * sharing a path collapsed into one). Whitespace-collapsed for shell
 * readability; volatile noise (tokens, timestamps) is NOT stripped: a
 * command that differs only in a token is usually a genuinely different
 * call, and false loop-refusals are worse than missed ones. */
export function fingerprint(toolName: string, args: Record<string, unknown>): string {
	const keys = Object.keys(args).sort();
	const canon = keys.map((k) => {
		const v = args[k];
		const str = typeof v === "string" ? v : JSON.stringify(v) ?? "";
		return `${k}=${str.replace(/\s+/g, " ").trim()}`;
	}).join("&");
	return toolName + "::" + canon;
}

/** Human-facing echo of a call for refusal messages — the primary arg when
 * it reads well, else the fingerprint tail. */
export function callEcho(toolName: string, args: Record<string, unknown>): string {
	for (const k of ["command", "path", "file_path", "pattern"]) {
		if (typeof args[k] === "string") return (args[k] as string).replace(/\s+/g, " ").slice(0, 60);
	}
	return fingerprint(toolName, args).split("::")[1].slice(0, 60);
}

/** Converge checkpoint message — time-based, not count-based (#689). */
export function convergeReason(minutesLeft: number, lane: string): string {
	return (
		`Convergence checkpoint: ~${minutesLeft}m of wall remain. ` +
		`Do NOT open new lines of investigation. Compose NOW: verify only what ` +
		`your findings already depend on, post pending verified findings, and ` +
		`write your output artifact (bash touching "${lane}" stays open to the ` +
		`wall). An honest partial review beats no review.`
	);
}

/** Finalize-only message — the last belt. */
export function finalizeReason(lane: string): string {
	return (
		`The wall is here. Do NOT investigate further. Your finalize lane is ` +
		`still open: bash commands touching "${lane}" execute normally — write ` +
		`your output artifact there now (e.g. a python3 heredoc), verify it, ` +
		`then stop. Unverified suspicions stay unmentioned — a smaller honest ` +
		`review beats a bloated speculative one.`
	);
}

/** Loop-refusal message — redirect without punishing. */
export function loopReason(cmd: string): string {
	return (
		`Loop: you already executed this exact call and its output is in your ` +
		`context ("${cmd}"). The workspace does not change under you — re-reading ` +
		`it returns the same bytes. Synthesize what you have, or make a call ` +
		`that produces NEW information.`
	);
}

/** Stagnation checkpoint — fires after a run of loop-refusals. */
export function stagnationReason(streak: number, lane: string): string {
	return (
		`Stagnation checkpoint: your last ${streak} calls were repeats of work ` +
		`you already hold in context. The session is cycling, not reviewing. ` +
		`Step back: name the pillars your findings so far support, verify only ` +
		`what a NEW claim depends on, and compose the artifact (bash touching ` +
		`"${lane}" stays open).`
	);
}

/** Legacy numeric-cap messages (kept for workflows that set a budget). */
export function nudgeReason(count: number, budget: number, lane: string): string {
	return (
		`Budget checkpoint: ${count}/${budget} calls used, ~${budget - count} left. ` +
		`Converge NOW: name the 3-4 pillars the diff's triggers selected, verify only those, ` +
		`then compose your output artifact. bash commands touching "${lane}" stay open past ` +
		`the cap. Further exploration WILL be refused.`
	);
}

export function budgetExhaustedReason(count: number, budget: number, lane: string): string {
	return (
		`Tool budget exhausted (${count} calls > ${budget} allowed). ` +
		`Do NOT investigate further. Your finalize lane is still open: bash commands ` +
		`touching "${lane}" execute normally — write your output artifact there now ` +
		`(e.g. a python3 heredoc), verify it, then stop. ` +
		`Unverified suspicions stay unmentioned — a smaller honest review beats a bloated speculative one.`
	);
}

/** Pi's tool_call event carries the arguments under `input` (types.d.ts:
 * BashToolCallEvent.input). The prose docs say `args` — read BOTH, input
 * first. The live failure (attempt 55de07e6351f) was exactly this: the lane
 * matched nothing because the command was read from the wrong key. */
export function eventArgs(event: unknown): Record<string, unknown> {
	const e = (event ?? {}) as { input?: unknown; args?: unknown };
	const raw = e.input ?? e.args;
	if (typeof raw === "string") {
		// some shapes carry the args as a JSON string — parse defensively
		try {
			const parsed = JSON.parse(raw);
			return typeof parsed === "object" && parsed !== null ? (parsed as Record<string, unknown>) : {};
			// eslint-disable-next-line @typescript-eslint/no-unused-vars
		} catch {
			return {};
		}
	}
	return typeof raw === "object" && raw !== null ? (raw as Record<string, unknown>) : {};
}

/** Mutable caller state across calls (the extension owns it). */
export interface BudgetState {
	/** Fingerprints of EXECUTED calls (loop-breaker memory). */
	seen: Set<string>;
	/** Consecutive loop-refusals since the last fresh execution. */
	loopStreak: number;
	/** Stagnation checkpoints fired (cap: 2 — after that the loop breaker
	 * alone holds the line; a third lecture is noise). */
	stagnationFired: number;
	/** Wall-converge checkpoint fired (once). */
	wallConverged: boolean;
	/** Wall-clock anchor: the session's FIRST tool call (epoch ms). The
	 * caller sets it on the first decision; the wall layers read it. */
	startedAtMs?: number;
	/** Legacy numeric-cap state. */
	executed: number;
	nudged: boolean;
}

export function newState(): BudgetState {
	return { seen: new Set(), loopStreak: 0, stagnationFired: 0, wallConverged: false, executed: 0, nudged: false };
}

/** Stagnation threshold: consecutive loop-refusals before the checkpoint
 * fires (data: healthy sessions run <1% consecutive repeats; the wall-died
 * round ran x10 on one file). */
export const STAGNATION_THRESHOLD = 4;
export const STAGNATION_MAX_FIRES = 2;

export interface DecideOpts {
	/** Legacy numeric cap (0 = no cap — the #689 default for reviews). */
	budget?: number;
	nudgeAt?: number;
	/** Conceptual layers armed? Review-only (dispatch.go): the loop breaker
	 * assumes a static workspace. */
	loopBreaker?: boolean;
	/** Wall layers: seconds of wall + the Job's start epoch ms (the ADS
	 * clock). Absent stamp → first-call anchoring (approximate). */
	wallSeconds?: number;
	runStartedAtMs?: number;
	convergeMarginSeconds?: number;
	finalizeMarginSeconds?: number;
}

/** The single decision point. Order matters:
 * loop breaker → wall finalize → wall converge → stagnation → numeric cap → allow.
 * An ALLOWED call records its fingerprint and resets the loop streak; a
 * refused one never enters memory (a blocked attempt is not work done). */
export function decide(
	toolName: string,
	args: Record<string, unknown>,
	state: BudgetState,
	allowSubstrings: string[],
	opts: DecideOpts = {},
): Decision {
	const fp = fingerprint(toolName, args);
	const lane = allowSubstrings[0] ?? DEFAULT_ALLOW;
	const inLane = FILE_TOOLS.has(toolName) ||
		(toolName === "bash" && allowSubstrings.some((s) => {
			const cmd = typeof args.command === "string" ? args.command : "";
			return cmd.includes(s);
		}));

	// Wall-clock anchor (r2 4302426988): the dispatcher stamps the Job's
	// start (HARMOSTES_RUN_STARTED_MS) — the SAME clock ADS enforces. The
	// session's first tool call would anchor LATE by the prepare prelude
	// (clone+rig, bounded 5m), firing both checkpoints late and quoting the
	// model a minutes-left number that is not the truth. First-call
	// anchoring remains the fallback for a dispatcher that predates the
	// stamp (documented approximation).
	if (opts.loopBreaker && (opts.wallSeconds ?? 0) > 0 && state.startedAtMs === undefined) {
		// Prefer the dispatcher's stamp (the ADS clock); fall back to the
		// first call (approximate — late by the prepare prelude).
		state.startedAtMs = opts.runStartedAtMs ?? Date.now();
	}

	// 1. Loop breaker — but never inside the finalize lane (composing IS the
	// point; re-reading the ledger to append is legitimate).
	if (opts.loopBreaker && state.seen.has(fp) && !inLane) {
		state.loopStreak++;
		if (state.loopStreak >= STAGNATION_THRESHOLD && state.stagnationFired < STAGNATION_MAX_FIRES) {
			state.stagnationFired++;
			state.loopStreak = 0;
			return { allow: false, nudge: true, loop: true, reason: stagnationReason(STAGNATION_THRESHOLD, lane) };
		}
		return { allow: false, loop: true, reason: loopReason(callEcho(toolName, args)) };
	}

	// 3. Wall-margin convergence — TIME is the resource (#689).
	const wall = opts.loopBreaker ? (opts.wallSeconds ?? 0) : 0;
	if (wall > 0 && state.startedAtMs !== undefined) {
		const elapsedS = (Date.now() - state.startedAtMs) / 1000;
		const remainingS = wall - elapsedS;
		const convM = opts.convergeMarginSeconds ?? 20 * 60;
		const finM = opts.finalizeMarginSeconds ?? 8 * 60;
		if (remainingS <= finM) {
			if (!inLane) {
				return { allow: false, reason: finalizeReason(lane) };
			}
		} else if (remainingS <= convM && !state.wallConverged) {
			state.wallConverged = true;
			return { allow: false, nudge: true, reason: convergeReason(Math.floor(remainingS / 60), lane) };
		}
	}

	// 2. Stagnation is handled in the loop branch above; fall through for
	// fresh calls.

	// Legacy numeric cap — emergency stop for workflows that set one.
	const budget = opts.budget ?? 0;
	if (budget > 0) {
		if (
			opts.nudgeAt !== undefined && opts.nudgeAt > 0 &&
			!state.nudged && state.executed === opts.nudgeAt && state.executed < budget
		) {
			state.nudged = true;
			return { allow: false, nudge: true, reason: nudgeReason(state.executed, budget, lane) };
		}
		if (state.executed >= budget && !inLane) {
			return { allow: false, reason: budgetExhaustedReason(state.executed, budget, lane) };
		}
	}

	// ALLOWED — record as work done.
	state.seen.add(fp);
	state.loopStreak = 0;
	state.executed++;
	return { allow: true };
}

/** Parse HARMOSTES_RUN_STARTED_MS (the Job's creation, epoch ms): absent or
 * garbage → undefined = the caller falls back to first-call anchoring (a
 * documented approximation, r2 4302426988). */
export function parseRunStartedMs(raw: string | undefined): number | undefined {
	const n = Number.parseInt((raw ?? "").trim(), 10);
	return Number.isFinite(n) && n > 0 ? n : undefined;
}
