/**
 * turn-budget — pi extension enforcing a HARD cap on tool calls per session
 * (pr-review wall-clock: #484).
 *
 * Measurement (attempt bd563e2e5f1e, 2026-09-13): a pr-review round spent
 * 43m9s in 47 tool calls at ~55s of model latency per call — the tool
 * executions are milliseconds; TURN COUNT is the whole cost. The prompt's
 * "~12 calls" guidance was ignored by the model (4× repeated greps, 6×
 * re-reads of one file), so the cap lives in the HARNESS, not the prose.
 *
 * Contract:
 *  - PI_TOOL_BUDGET (int) is the cap. Absent, zero, or unparsable → the
 *    extension is INERT (mode-3 semantics, the fleet convention for
 *    opt-in mechanisms — see internal/piargs/piargs.go).
 *  - Calls within budget pass untouched.
 *  - Past the cap every call is blocked with a finalize instruction —
 *    EXCEPT `write`, which stays available so the agent can still produce
 *    its output artifact (review.json) after the cut-off. The review
 *    contract already tells the agent where that file goes; the block
 *    reason only has to stop the bleeding and point at the door.
 *  - Blocking (not terminating): the agent keeps the turn, reads the
 *    reason, and wraps up. terminate would kill the session with no
 *    output at all — strictly worse than an over-budget review.
 *
 * Degradation: the extension registers no tools and no providers — a
 * fleet image without it simply has no cap (the pre-extension status quo).
 */
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

const FINALIZE_REASON = (count: number, budget: number) =>
	`Tool budget exhausted (${count} calls > ${budget} allowed). ` +
	"Do NOT investigate further. Finalize NOW from the evidence you already have: " +
	"use the write tool to produce your output artifact, then stop. " +
	"Unverified suspicions stay unmentioned — a smaller honest review beats a bloated speculative one.";

/** Finalization tools that stay callable past the cap: the agent must be able
 * to write its output after the cut-off, or the budget becomes a kill switch. */
const PAST_BUDGET_ALLOW = new Set(["write"]);

export default function (pi: ExtensionAPI) {
	const raw = process.env.PI_TOOL_BUDGET ?? "";
	const budget = Number.parseInt(raw, 10);
	if (!Number.isFinite(budget) || budget <= 0) return; // inert: no budget configured

	let count = 0;
	pi.on("tool_call", (event) => {
		count++;
		if (count <= budget) return;
		if (PAST_BUDGET_ALLOW.has(event.toolName)) return;
		return { block: true, reason: FINALIZE_REASON(count, budget) };
	});
}
