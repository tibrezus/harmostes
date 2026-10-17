/**
 * turn-budget — pi extension managing session economics for tool calls.
 *
 * v3 (#689 — "caps should be conceptual"): review sessions are managed by
 * three conceptual layers, not a call count (see policy.ts for the measured
 * justification):
 *
 *   1. LOOP BREAKER — a repeated (tool, args) fingerprint in a static
 *      review workspace is refused ("you already have this output"). The
 *      wall-died round re-read one file TEN times; 10-12% of calls were
 *      repeats.
 *   2. STAGNATION CHECKPOINT — K consecutive loop-refusals fire the
 *      converge directive (the one lever the model demonstrably obeys).
 *   3. WALL-MARGIN CONVERGENCE — at ≤20m of wall left: one converge
 *      checkpoint; at ≤8m: finalize-lane-only. Call-rate noise (measured
 *      2× between rounds) made any count-based calibration untrustworthy;
 *      time does not drift.
 *
 * The numeric cap (PI_TOOL_BUDGET) keeps its v2 semantics for workflows
 * that set one; review attempts set none — they are managed, not counted.
 *
 * Arming: the conceptual layers run ONLY where their assumption holds —
 * PI_LOOP_BREAKER=1 is forwarded to review attempts exclusively
 * (dispatch.go: a static workspace is a review property; fork-maintenance
 * legitimately retries identical commands after upstream fixes).
 *
 * Degradation: the extension registers no tools and no providers — a fleet
 * image without it simply has no budget management (the pre-#484 status
 * quo).
 */
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import {
	decide, eventArgs, newState, parseAllow, parseBudget, parseConvergeMargin,
	parseFinalizeMargin, parseNudge, parseRunStartedMs, parseWallSeconds,
} from "./policy.ts";

export default function (pi: ExtensionAPI) {
	const budget = parseBudget(process.env.PI_TOOL_BUDGET);
	const allowSubstrings = parseAllow(process.env.PI_TOOL_BUDGET_ALLOW);
	const nudgeAt = parseNudge(process.env.PI_TOOL_BUDGET_NUDGE, budget);
	const loopBreaker = process.env.PI_LOOP_BREAKER === "1";
	const wallSeconds = parseWallSeconds(process.env.HARMOSTES_WALL_SECONDS);
	const runStartedAtMs = parseRunStartedMs(process.env.HARMOSTES_RUN_STARTED_MS);
	const convergeMargin = parseConvergeMargin(process.env.PI_WALL_CONVERGE_MARGIN);
	const finalizeMargin = parseFinalizeMargin(process.env.PI_WALL_FINALIZE_MARGIN);

	// inert: no conceptual layers armed AND no numeric cap configured
	if (!loopBreaker && budget <= 0) return;

	const state = newState();
	let loggedShape = false;
	pi.on("tool_call", (event) => {
		if (!loggedShape) {
			loggedShape = true;
			// Contract canary in the wild (#486 class): the event-arg key is the
			// lane's load-bearing assumption — a wrong key made v2's lane match
			// an empty string and refuse every finalize write. One grep-able
			// line turns that drift into a pod-log diagnosis instead of a
			// session autopsy.
			console.warn(
				`turn-budget: tool_call event keys = ${Object.keys(event ?? {}).sort().join(",")}`,
			);
		}
		const d = decide(event.toolName, eventArgs(event), state, allowSubstrings, {
			budget,
			nudgeAt,
			loopBreaker,
			wallSeconds,
			runStartedAtMs,
			convergeMarginSeconds: convergeMargin,
			finalizeMarginSeconds: finalizeMargin,
		});
		if (d.allow) return;
		return { block: true, reason: d.reason };
	});
}
