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
	"encoding/json"
	"os"
	"slices"
	"strings"
)

//go:generate go run ./gen

// RenderExtensions renders the extension single-source into its two consumed
// forms (#339): the JSON artifact (extensions.json) and the generated block
// for harmostes.py — same bytes the generator writes, so go test IS the drift
// check (regenerate, compare, red on any drift; no CI surgery, no scrape).
func RenderExtensions() (jsonArtifact []byte, pyBlock []byte, err error) {
	type manifest struct {
		Extensions []string          `json:"extensions"`
		Tools      map[string]string `json:"tools"`
	}
	m := manifest{Extensions: slices.Clone(Extensions), Tools: extensionTools}
	jsonArtifact, err = json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	jsonArtifact = append(jsonArtifact, '\n')
	// The py form is the block BETWEEN the generated-extension markers in
	// harmostes.py — which sit inside main(), so every line carries the
	// four-space statement indent. The markers' own indent comes from the
	// surrounding file; this function owns everything between them.
	py, err := json.MarshalIndent(m, "", "    ")
	if err != nil {
		return nil, nil, err
	}
	pyBlock = []byte("    EXTENSIONS_MANIFEST = " + strings.ReplaceAll(string(py), "\n", "\n    ") + "\n    ")
	return jsonArtifact, pyBlock, nil
}

// harmostes.py (the standalone primitive's pi invocation). The
// TestExtensionsSingleSource test fails any drift between them — an
// extension named here but missing from an image takes down every agent
// that loads it, and one missing from PiArgs is silently unavailable
// while task contracts mandate it (#338 r9/r14).
//
// THREE failure modes govern every entry (r1 review of #425 named the
// third): (1) named here but absent from the image → pi exits at startup
// on the missing -e path — the build-time load probes + the COPY drift
// test catch it; (2) on the image but missing here → silently
// unavailable; (3) present, loaded, and INERT — an extension whose
// mechanisms are opt-in (sol-pi) loads clean with everything disabled,
// and the fleet runs exactly as if nothing was added. Mode (3) is
// covered by the shipped config file (extensions/sol-pi/sol-pi.json,
// schema-checked at image build) + the "pi extensions:" startup log line
// (LoadedExtensions, mirrored in harmostes.py) making the resolved set
// visible per run.
//
// Provenance: litellm-provider and rig-query are IN-TREE
// (extensions/<name>, COPY'd); sol-pi is a VENDORED third-party checkout
// (extensions/sol-pi — NVlabs/SoL-Pi, see its UPSTREAM.md for the source
// SHA and bump procedure). Vendoring keeps the review/update path of a
// build-time-fetched artifact inside the tree.
var Extensions = []string{
	"/extensions/litellm-provider",
	"/extensions/rig-query",
	"/extensions/sol-pi",
}

// LoadedExtensions reports the extensions that exist on THIS image — the
// same stat pre-flight buildPiArgs applies before emitting -e. ALL THREE
// agent entry points log it at startup (worker pipeline, standalone
// agent primitive, harmostes.py): per-run evidence of which extension
// set — and therefore which tool pipeline, e.g. sol-pi's wrapped
// edit/write/bash — was actually in effect (#425 r1; r3 pillar 8). An
// EMPTY result is the degrade signal: callers log it as a warning, not
// an empty list.
func LoadedExtensions() []string {
	return loadedExtensions(Extensions, os.Stat)
}

func loadedExtensions(extensions []string, stat func(string) (os.FileInfo, error)) []string {
	var out []string
	for _, ext := range extensions {
		if _, err := stat(ext); err != nil {
			continue
		}
		out = append(out, ext)
	}
	return out
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
	// sol-pi's observation-pack registers obs_recall (its recall affordance
	// for replaced large tool results). The shipped profile
	// (extensions/sol-pi/sol-pi.json, observationPack=true) is THE effective
	// runtime config on every agent run — settings.json pins
	// defaultProjectTrust=never, so no workspace .pi/sol-pi.json can
	// override it (#426 r3 finding 1) — which makes obs_recall always
	// registered and therefore always allowlist-required: without this
	// entry the --tools allowlist dropped obs_recall while the rewriting
	// stayed active, destroying review evidence with the recall affordance
	// uncallable (#426 r2 pillar 5A). litellm-provider has no entry:
	// provider-only extensions register no tools.
	"/extensions/sol-pi": "obs_recall",
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
	// --no-approve lands on EVERY pi invocation (#426 r5 blocking finding):
	// the agent's cwd is the PR checkout — untrusted content. It ignores
	// project-local files (settings, extensions, .pi/sol-pi.json) for the
	// run, so a hostile repo cannot flip the harness's own config (e.g.
	// evidencePreservingReducer=true ships repo logs to a remote reducer
	// model) even when pi's trust resolution would mark the workspace
	// trusted. Belt-and-braces over the settings.json projectTrusted=false
	// pin — the config plane and the invocation plane enforce the same rule.
	args := []string{"--skill", skill, "--model", model, "--no-approve"}
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
