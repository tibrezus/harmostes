/**
 * turn-budget — pi extension enforcing a HARD cap on tool calls per session
 * (pr-review wall clock: #484).
 *
 * Measurement (attempt bd563e2e5f1e): a pr-review round spent 43m9s in 47
 * tool calls at ~55s of model latency per call — tool execution is
 * milliseconds; TURN COUNT is the whole cost. The prompt's "~12 calls"
 * guidance was ignored, so the cap lives in the HARNESS, not the prose.
 *
 * Semantics (v2 — the v1 write-tool assumption failed live, attempt
 * 57a1232749c0: "Tool write not found" bricked the run):
 *  - PI_TOOL_BUDGET (int) is the cap on EXECUTED calls; blocked attempts do
 *    not count. Absent/0/unparsable → inert (mode-3, the fleet convention).
 *  - Past the cap: the finalize LANE stays open — bash commands touching the
 *    output artifact (PI_TOOL_BUDGET_ALLOW, default /workspace/review.json)
 *    and registered file-writer tools still execute. Everything else is
 *    refused with instructions naming the lane.
 *  - The lane match is inert-substring comparison (compare, never execute).
 *    It is an efficiency guard, not containment.
 *
 * Degradation: the extension registers no tools and no providers — a fleet
 * image without it simply has no cap (the pre-#484 status quo).
 */
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { decide, eventArgs, parseAllow, parseBudget } from "./policy.ts";

export default function (pi: ExtensionAPI) {
	const budget = parseBudget(process.env.PI_TOOL_BUDGET);
	if (budget <= 0) return; // inert: no budget configured
	const allowSubstrings = parseAllow(process.env.PI_TOOL_BUDGET_ALLOW);

	let executed = 0;
	pi.on("tool_call", (event) => {
		const d = decide(event.toolName, eventArgs(event), executed, budget, allowSubstrings);
		if (d.allow) {
			executed++; // blocked attempts never reach execution — they stay free
			return;
		}
		return { block: true, reason: d.reason };
	});
}
