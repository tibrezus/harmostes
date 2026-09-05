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

// PiArgs builds the pi --mode rpc extra args from the three values it
// actually uses. Primitives, deliberately (r28 P1): no api/v1alpha1 import —
// the "stdlib-only primitive" claim in the package doc is now literally
// true and TestPiargsIsLeaf enforces the whole invariant.
func PiArgs(skill, model string, tools []string) []string {
	return buildPiArgs(skill, model, tools, Extensions, os.Stat)
}

// buildPiArgs is PiArgs' injectable core: stat lets tests simulate images
// where an extension directory is absent (older images, rollout lag) —
// pi EXITS at startup on a missing -e path, so a missing extension must
// drop out of the args (and take its --tools entry with it) rather than
// kill every workflow that declares tools (#338 r14 B1).
func buildPiArgs(skill, model string, tools []string, extensions []string, stat func(string) (os.FileInfo, error)) []string {
	args := []string{"--skill", skill, "--model", model}
	for _, ext := range extensions {
		if _, err := stat(ext); err != nil {
			continue // not on this image — degrade quietly
		}
		args = append(args, "-e", ext)
	}
	if len(tools) > 0 {
		allow := tools
		// Keep the allowlist in sync with the -e set actually emitted: only
		// tools from extensions that survived the stat pre-flight are appended.
		for _, ext := range extensions {
			if _, err := stat(ext); err != nil {
				continue
			}
			tool, ok := extensionTools[ext]
			if !ok || slices.Contains(allow, tool) {
				continue
			}
			// Copy before append: a.Tools must never be aliased/mutated.
			allow = append(append([]string{}, allow...), tool)
		}
		args = append(args, "--tools", strings.Join(allow, ","))
	}
	return args
}
