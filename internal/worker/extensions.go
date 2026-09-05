package worker

// Extensions lists the pi extensions every agent load path must agree on:
// PiArgs (-e flags), both worker Dockerfiles (COPY targets), and
// harmostes.py (the standalone primitive's pi invocation). The
// TestExtensionsSingleSource test fails any drift between them — an
// extension named here but missing from an image takes down every agent
// that loads it, and one missing from PiArgs is silently unavailable
// while task contracts mandate it (#338 r9/r14).
var Extensions = []string{
	"/extensions/litellm-provider",
	"/extensions/rig-query",
}

// RigGraphPath is the ONE sanctioned location of the SHA-exact review-time
// graph (ADR-0009 freshness contract, #338 r25 F6). Three halves must agree:
// the ops prepare emits it there (workspace.sh), the rig-query extension
// probes it there (its container-candidates list), and the worker pre-flights
// it there (graphPresenceLine). TestRigGraphPathSingleSource pins the
// extension's literal to this constant — the wiring claim most likely to rot.
const RigGraphPath = "/workspace/rig.db"

// extensionTools maps an extension to the TOOL it registers (pi --tools
// allowlists are over tool names, not extension paths). Extensions that
// only register providers have no entry.
var extensionTools = map[string]string{
	"/extensions/rig-query": "rig",
}
