// Package piargs owns the pi invocation shape: which extensions load
// (-e), which tools they register (--tools allowlists are over tool names),
// and where the SHA-exact review graph lives. It is a LEAF package — the
// fleet's smallest binary (cmd/harmostes-agent, a stdlib-only pi wrapper)
// and the pipeline worker (internal/worker) both point AT it, so the
// single source never costs a dependency edge (#338 r25 F1 / r26 ARCH-1:
// PiArgs previously lived in internal/worker, putting the agent primitive
// on the Dapr/k8s/otel closure — 960 packages vs 388 — to assemble four
// flags). TestPiargsIsLeaf keeps it that way.
package piargs

import (
	"os"
	"slices"
	"strings"

	"github.com/tibrezus/harmostes/api/v1alpha1"
)

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

// PiArgs builds the pi --mode rpc extra args from a Workflow's agent spec.
// The litellm-provider extension is always loaded so the litellm/* model prefix
// resolves (the extension registers the provider at startup from LITELLM_URL +
// LITELLM_API_KEY env vars injected by the controller). The rig-query extension
// exposes the architecture graph (rig.db, emitted by prepare when present —
// ADR-0009) as a structured tool; it degrades gracefully when no graph exists.
// --tools is an ALLOWLIST in pi (it replaces the whole set, extension tools
// included), so a workflow declaring spec.agent.tools would silently lose rig
// while the task contract still mandates it — append "rig" unless already
// present. Workflows declaring NO tools get no --tools flag at all (every
// tool available, rig merely among them — nothing is forced onto the allow-
// list). The tool itself degrades harmlessly when no graph was emitted.
func PiArgs(a v1alpha1.AgentSpec) []string {
	return buildPiArgs(a, Extensions, os.Stat)
}

// buildPiArgs is PiArgs' injectable core: stat lets tests simulate images
// where an extension directory is absent (older images, rollout lag) —
// pi EXITS at startup on a missing -e path, so a missing extension must
// drop out of the args (and take its --tools entry with it) rather than
// kill every workflow that declares tools (#338 r14 B1).
func buildPiArgs(a v1alpha1.AgentSpec, extensions []string, stat func(string) (os.FileInfo, error)) []string {
	args := []string{"--skill", a.Skill, "--model", a.Model}
	for _, ext := range extensions {
		if _, err := stat(ext); err != nil {
			continue // not on this image — degrade quietly
		}
		args = append(args, "-e", ext)
	}
	if len(a.Tools) > 0 {
		tools := a.Tools
		// Keep the allowlist in sync with the -e set actually emitted: only
		// tools from extensions that survived the stat pre-flight are appended.
		for _, ext := range extensions {
			if _, err := stat(ext); err != nil {
				continue
			}
			tool, ok := extensionTools[ext]
			if !ok || slices.Contains(tools, tool) {
				continue
			}
			// Copy before append: a.Tools must never be aliased/mutated.
			tools = append(append([]string{}, tools...), tool)
		}
		args = append(args, "--tools", strings.Join(tools, ","))
	}
	return args
}
