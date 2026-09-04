package worker

// Extensions lists the pi extensions every agent load path must agree on:
// PiArgs (-e flags), both worker Dockerfiles (COPY targets), and
// harmostes.py (the standalone primitive's pi invocation). The
// TestExtensionsSingleSource test fails any drift between them — an
// extension named here but missing from an image takes down every agent
// that loads it, and one missing from PiArgs is silently unavailable
// while task contracts mandate it (#338 r9).
var Extensions = []string{
	"/extensions/litellm-provider",
	"/extensions/rig-query",
}
