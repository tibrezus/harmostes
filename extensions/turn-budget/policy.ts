/**
 * turn-budget policy — pure decision logic, testable without the pi runtime
 * (the litellm-provider fallbacks.ts pattern).
 *
 * v2 semantics (#484 failure forensics, attempt 57a1232749c0):
 *
 * v1 blocked everything past the cap except a `write` tool — which DOES NOT
 * EXIST in the fleet's review sessions ("Tool write not found"), so the
 * agent could never produce review.json and the run failed 4 gate attempts
 * in 10 minutes. v1 also counted BLOCKED calls, draining the budget while
 * the agent retried, and 24 was tight mid-analysis.
 *
 * v2:
 *  - only EXECUTED calls count (blocked attempts are free — they cost a
 *    model turn, which is punishment enough);
 *  - past the cap the agent gets a FINALIZE LANE, not a brick wall: bash
 *    commands touching the output artifact (default `/workspace/review.json`)
 *    and registered file-writer tools (`write`/`edit`, when a deployment
 *    has them) still execute; everything else is refused with instructions;
 *  - the lane match is inert-substring comparison — the guard compares and
 *    never executes or substitutes (the perf-lab guard posture). It is an
 *    efficiency guard, not containment: an agent that stuffs the magic
 *    substring into unrelated commands only defeats its own budget.
 */

/** Tools that produce/modify files — always allowed past the cap so the
 * agent can emit its artifact. `write` may not be registered in every
 * deployment; allowing a non-existent name here is harmless. */
export const FILE_TOOLS = new Set(["write", "edit"]);

export const DEFAULT_ALLOW = "/workspace/review.json";

export interface Decision {
	allow: boolean;
	reason?: string;
	/** This block was the one-shot mid-budget checkpoint (#487): it does not
	 * count against the budget and must not re-fire (the caller flips its
	 * `nudged` flag on seeing it). */
	nudge?: boolean;
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

/** The mid-budget checkpoint message: shape convergence at the halfway mark
 * (name the pillars the diff triggers selected, verify only those, compose),
 * because the prose budget guidance in the prompt is systematically ignored
 * — measured, #484 — while a block is obeyed. */
export function nudgeReason(count: number, budget: number, lane: string): string {
	return (
		`Budget checkpoint: ${count}/${budget} calls used, ~${budget - count} left. ` +
		`Converge NOW: name the 3-4 pillars the diff's triggers selected, verify only those, ` +
		`then compose your output artifact. bash commands touching "${lane}" stay open past ` +
		`the cap. Further exploration WILL be refused.`
	);
}

export function finalizeReason(count: number, budget: number, lane: string): string {
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

/** Checkpoint state passed by the caller (it owns the flags). */
export interface DecideOpts {
	nudgeAt?: number;
	nudged?: boolean;
}

/** The cap decision for one tool call. `count` is the number of ALREADY
 * EXECUTED calls in this session (blocked attempts excluded). */
export function decide(
	toolName: string,
	args: Record<string, unknown>,
	count: number,
	budget: number,
	allowSubstrings: string[],
	opts: DecideOpts = {},
): Decision {
	if (budget <= 0) return { allow: true }; // inert
	// One-shot mid-budget checkpoint (#487): a single block at the halfway
	// mark that does NOT count against the budget and fires exactly once.
	if (
		opts.nudgeAt !== undefined && opts.nudgeAt > 0 &&
		!opts.nudged && count === opts.nudgeAt && count < budget
	) {
		return {
			allow: false,
			nudge: true,
			reason: nudgeReason(count, budget, allowSubstrings[0] ?? DEFAULT_ALLOW),
		};
	}
	if (count <= budget) return { allow: true };
	if (FILE_TOOLS.has(toolName)) return { allow: true };
	if (toolName === "bash") {
		const cmd = typeof args.command === "string" ? args.command : "";
		if (allowSubstrings.some((s) => cmd.includes(s))) return { allow: true };
	}
	const lane = allowSubstrings[0] ?? DEFAULT_ALLOW;
	return { allow: false, reason: finalizeReason(count, budget, lane) };
}
