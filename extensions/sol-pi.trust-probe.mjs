// Fleet-side trust probe for the vendored SoL-Pi extension (#425/#426 r6).
//
// Runs as a pi EXTENSION (pi -e /extensions/sol-pi -e this-file) — inside the
// harness process, so module resolution and trust resolution match production
// exactly (the r6 finding this closes: a bare `node` import of the vendored
// config.ts cannot resolve @earendil-works/* in the image, and a probe that
// passes its own allowProjectConfig value tests a parameter production never
// supplies).
//
// On session_start it resolves the EFFECTIVE config the same way the sol-pi
// extension does — loadSolPiConfig(ctx.cwd, getAgentDir(), ctx.isProjectTrusted())
// — and writes one JSON line to STDERR (stdout is pi's RPC stream):
//
//   {"type":"sol_pi_trust_probe","trusted":<bool>,"reducer":<bool>,"actionFusion":<bool>}
//
// The Dockerfile gate parses that line twice:
//   with --no-approve → reducer must be FALSE (the shipped profile wins — the
//     fleet's invariant, since buildPiArgs always emits the flag), and
//   without it      → reducer must be TRUE  (a .pi/sol-pi.json-only workspace
//     is auto-trusted by pi 0.84.4 before defaultProjectTrust is consulted —
//     this arm proves the probe is sensitive and the flag is load-bearing).
export default function solPiTrustProbe(pi) {
	pi.on("session_start", async (_event, ctx) => {
		try {
			const { loadSolPiConfig } = await import("/extensions/sol-pi/src/sol-pi/config.ts");
			const { getAgentDir } = await import("@earendil-works/pi-coding-agent");
			const cfg = loadSolPiConfig(ctx.cwd, getAgentDir(), ctx.isProjectTrusted());
			process.stderr.write(JSON.stringify({
				type: "sol_pi_trust_probe",
				trusted: ctx.isProjectTrusted(),
				reducer: cfg.evidencePreservingReducer,
				actionFusion: cfg.actionFusion,
			}) + "\n");
		} catch (err) {
			// A probe failure must be LOUD in the log the gate greps — a silent
			// catch would reproduce the inert-extension failure mode it exists
			// to detect.
			process.stderr.write(JSON.stringify({
				type: "sol_pi_trust_probe",
				error: String(err),
			}) + "\n");
		}
	});
}
