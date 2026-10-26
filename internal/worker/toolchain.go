package worker

// The plugin-facing toolchain baseline (ADR-0011 amendment, three-tier 1b /
// #786): the KERNEL guarantees these tools on the worker image, because
// template plugins (#789/#790 — self-contained template repos with their own
// plugin payloads) may depend on them. Before this contract the image
// happened to carry what its built-ins needed; the moment plugin payloads
// leave the kernel repo, "happens to" stops being a contract and becomes a
// cross-repo guessing game.
//
// The list is ENFORCED against the Dockerfile (TestToolchainBaselineInImage):
// a tool dropped from the image fails CI with the missing name, and a tool
// added to the image without joining the contract is invisible to template
// authors (add it here — the comment in Dockerfile.worker names this
// package).
//
// Release notes carry the same list per kernel release: the materialization
// contract's promise is per-VERSION, so an image that loses a tool must do
// it in a release whose notes say so.
var ToolchainBaseline = []string{
	"git",     // every plugin's transport (clone/push/diff)
	"jq",      // JSON shaping in plugin scripts
	"yq",      // YAML shaping (charts, workflow documents)
	"fj",      // the rezuscloud Forgejo client (review threads, PR surfaces)
	"gh",      // the GitHub client (inline review threads, #374)
	"python3", // plugin scripts in any language — the contract permits them
	"curl",    // registry + webhook affordances (and the materialization init)
	"tar",     // artifact unpacking
	"node",    // the pi runtime's host (extensions are TS; harness materialization)
	"npm",     // extension authoring parity with the delivery contract
}
