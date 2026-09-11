// Fleet-side trust probe for the vendored SoL-Pi extension (#425/#426 r6-r9).
//
// Runs as a pi EXTENSION (pi -e /extensions/sol-pi -e this-file) — inside the
// harness process, so module resolution and trust resolution match production
// exactly. The imports are deliberately TOP-LEVEL STATIC: pi injects its
// packages into extensions via jiti, which rewrites static imports — a
// runtime `await import("@earendil-works/...")` escapes jiti and dies on
// Node's resolver (the image ships no node_modules; r9 blocker F1 reproduced
// exactly that). A static-import failure surfaces as an extension_error in
// the probe log, which the Dockerfile gate treats as a failure.
//
// On session_start it resolves the EFFECTIVE config the same way the sol-pi
// extension does — loadSolPiConfig(ctx.cwd, getAgentDir(),
// ctx.isProjectTrusted()) — and writes one JSON line to STDERR (stdout is
// pi's RPC stream):
//
//   {"type":"sol_pi_trust_probe","trusted":<bool>,"reducer":<bool>,"actionFusion":<bool>}
//
// The Dockerfile gate parses that line twice:
//   with --no-approve → reducer must be FALSE (the shipped profile wins — the
//     fleet's invariant, since buildPiArgs always emits the flag), and
//   without it      → reducer must be TRUE  (a .pi/sol-pi.json-only workspace
//     is auto-trusted by pi 0.84.4 before defaultProjectTrust is consulted —
//     this arm proves the probe is sensitive and the flag is load-bearing).
import { loadSolPiConfig } from "/extensions/sol-pi/src/sol-pi/config.ts";
import { getAgentDir } from "@earendil-works/pi-coding-agent";

export default function solPiTrustProbe(pi) {
	pi.on("session_start", (_event, ctx) => {
		const cfg = loadSolPiConfig(ctx.cwd, getAgentDir(), ctx.isProjectTrusted());
		process.stderr.write(JSON.stringify({
			type: "sol_pi_trust_probe",
			trusted: ctx.isProjectTrusted(),
			reducer: cfg.evidencePreservingReducer,
			actionFusion: cfg.actionFusion,
		}) + "\n");
	});
}
