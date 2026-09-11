package piargs

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestPiArgsLoadBuiltinExtensions(t *testing.T) {
	args := buildPiArgs("pr-review", "litellm/test-model", nil, Extensions, alwaysPresent)
	joined := strings.Join(args, " ")
	// r25 F7: iterate the REAL Extensions list — a hard-coded copy here is the
	// drift hole it exists to catch.
	for _, ext := range Extensions {
		if !strings.Contains(joined, ext) {
			t.Errorf("PiArgs must load %s, got %v", ext, args)
		}
	}
	// -e flags must pair with their path argument (flag, value alternating).
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "/extensions/") {
			t.Errorf("-e must be followed by an extension path, got %q", args[i+1])
		}
	}
	// skill/model pair must survive (position: after the extension flags).
	s, m := -1, -1
	for i, a := range args {
		if a == "--skill" {
			s = i
		}
		if a == "--model" {
			m = i
		}
	}
	if s < 0 || m != s+2 || args[s+1] != "pr-review" || args[m+1] != "litellm/test-model" {
		t.Errorf("skill/model args lost or malformed: %v", args)
	}
}

// --tools is an ALLOWLIST in pi: it replaces the whole tool set, extension
// tools included. A workflow declaring spec.agent.tools would silently lose
// rig while the task contract still mandates it — PiArgs appends it.
func TestPiArgsToolsAllowlistKeepsRig(t *testing.T) {
	args := buildPiArgs("s", "m", []string{"bash", "read"}, Extensions, alwaysPresent)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--tools bash,read,rig") {
		t.Errorf("tools allowlist must gain rig, got: %s", joined)
	}
	// Already declared → not duplicated.
	args = buildPiArgs("s", "m", []string{"bash", "rig"}, Extensions, alwaysPresent)
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "--tools bash,rig") || strings.Contains(joined, "rig,rig") {
		t.Errorf("declared rig must not be duplicated, got: %s", joined)
	}
}

// TestExtensionToolsCoversEveryLoadedExtension is the table-driven form the
// #426 r2 pillar 9A finding asked for: one case per Extensions entry naming
// the tool its image presence contributes to the --tools allowlist. The
// blind spot this closes: the map is the allowlist test's input AND subject,
// so a missing entry (sol-pi's obs_recall shipped that way) was invisible —
// the tool was registered at runtime, dropped by the allowlist, and every
// gate stayed green. A new Extensions entry MUST either land here or be
// justified in extensionTools' comment (provider-only → no entry).
func TestExtensionToolsCoversEveryLoadedExtension(t *testing.T) {
	cases := map[string]string{
		"/extensions/litellm-provider": "", // provider-only: registers no tool
		"/extensions/rig-query":        "rig",
		"/extensions/sol-pi":           "obs_recall", // observation-pack's recall affordance (#425)
	}
	for _, ext := range Extensions {
		want, known := cases[ext]
		if !known {
			t.Errorf("extension %s has no expected-tool entry in this test — add one (or a documented extensionTools omission)", ext)
			continue
		}
		args := buildPiArgs("s", "m", []string{"bash", "read"}, Extensions, alwaysPresent)
		if want == "" {
			if tool := extensionTools[ext]; tool != "" {
				t.Errorf("%s is provider-only but extensionTools registers %q — update the table", ext, tool)
			}
			continue
		}
		// Exact membership in the PARSED --tools value (r3 finding 3): a
		// substring check passes on "--tools bash,obs_recallX" while pi's
		// strict allowlist would drop the unregistered name entirely.
		tools := ""
		for i, a := range args {
			if a == "--tools" && i+1 < len(args) {
				tools = args[i+1]
			}
		}
		found := false
		for _, t := range strings.Split(tools, ",") {
			if t == want {
				found = true
			}
		}
		if !found {
			t.Errorf("parsed --tools %q must contain %q exactly (for %s)", tools, want, ext)
		}
	}
}

func alwaysPresent(string) (os.FileInfo, error) { return nil, nil }

// An image without an extension directory must drop it from the args (and
// from the --tools allowlist) instead of killing pi at startup (#338 r14 B1).
func TestPiArgsDropsMissingExtension(t *testing.T) {
	args := buildPiArgs(
		"s", "m", []string{"bash"},
		[]string{"/extensions/litellm-provider", "/extensions/rig-query"},
		func(p string) (os.FileInfo, error) {
			if strings.Contains(p, "rig-query") {
				return nil, os.ErrNotExist
			}
			return nil, nil
		},
	)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "rig-query") || strings.Contains(joined, "rig") {
		t.Errorf("missing extension must be dropped from -e AND --tools, got: %s", joined)
	}
	for i, a := range args {
		if a == "--tools" {
			// The --tools VALUE must equal exactly the user's list — a whole-
			// string Contains can pass on the -e flag alone, which is how
			// r14-B1 hid behind its neighbour (#338 r17 M10).
			if args[i+1] != "bash" {
				t.Errorf("--tools value must be exactly the user's list, got: %s", args[i+1])
			}
		}
	}
}

// The extension list is load-bearing in FOUR places: PiArgs (-e), both worker
// Dockerfiles (COPY), and harmostes.py (the standalone primitive). Drift in
// any of them is fleet-wide — a missing COPY kills every agent at pi startup,
// a missing -e silently drops the tool (#338 r9). This test pins them all.
//
// #339: harmostes.py's manifest is GENERATED (RenderExtensions → the marked
// block; extensions.json is the checked-in artifact). The drift check is the
// render itself — regenerate, compare bytes, red on any drift — replacing
// the old strings.Contains scrapes, which a reordered or reformatted dict
// literal could satisfy while the semantics drifted.
func TestExtensionsSingleSource(t *testing.T) {
	// The generated forms must match the committed bytes exactly.
	jsonArtifact, pyBlock, err := RenderExtensions()
	if err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, "extensions.json"); string(got) != string(jsonArtifact) {
		t.Errorf("extensions.json is stale — run `go generate ./internal/piargs`\nwant:\n%s\ngot:\n%s", jsonArtifact, got)
	}
	py := string(mustRead(t, "../../harmostes.py"))
	const (
		begin = "# BEGIN GENERATED EXTENSIONS (go generate ./internal/piargs — do not edit)"
		end   = "# END GENERATED EXTENSIONS"
	)
	i, j := strings.Index(py, begin), strings.Index(py, end)
	if i < 0 || j < 0 {
		t.Fatalf("harmostes.py: generated-extension markers missing — the primitive's manifest is not generated anymore")
	}
	if got := py[i+len(begin)+1 : j]; string(pyBlock) != got {
		t.Errorf("harmostes.py generated block is stale — run `go generate ./internal/piargs`\nwant:\n%s\ngot:\n%s", pyBlock, got)
	}
	// Structural scrapes stay for the Dockerfiles (COPY lines are layout,
	// not generated): a missing COPY kills every agent at pi startup.
	for _, ext := range Extensions {
		dockerfile := string(mustRead(t, "../../Dockerfile.worker"))
		release := string(mustRead(t, "../../.github/Dockerfile.worker.release"))
		// The Dockerfile line is `COPY extensions/<name> <ext>` — the DESTINATION
		// (with a leading space) is what must exist in both images.
		if !strings.Contains(dockerfile, " "+ext) {
			t.Errorf("Dockerfile.worker does not COPY %s — agents would die at pi startup", ext)
		}
		if !strings.Contains(release, " "+ext) {
			t.Errorf(".github/Dockerfile.worker.release (the published image) does not COPY %s", ext)
		}
		// (provider-only extensions legitimately have no tool entry — see the
		// extensionTools comment. Tool coverage is now the artifact's: the
		// rendered bytes above ARE extensionTools, so a tool ADDED to the map
		// is drift-checked without this test growing a copy, r25 F7.)
		args := buildPiArgs("s", "m", nil, Extensions, alwaysPresent)
		if !strings.Contains(strings.Join(args, " "), ext) {
			t.Errorf("PiArgs does not load %s", ext)
		}
	}
}

// TestSolPiProfileSingleSource (#425 r1): the shipped sol-pi profile is the
// ONE copy in the tree — extensions/sol-pi/sol-pi.json — and both images must
// install exactly that file at the path SoL-Pi reads (~/.pi/agent). Guards
// the review findings that a duplicated printf literal per Dockerfile made
// the profile undiffable and the two images able to load different
// mechanism sets with every gate green.
func TestSolPiProfileSingleSource(t *testing.T) {
	profile := string(mustRead(t, "../../extensions/sol-pi/sol-pi.json"))
	var cfg struct {
		Version                   int      `json:"version"`
		ActionFusion              bool     `json:"actionFusion"`
		ObservationPack           bool     `json:"observationPack"`
		EvidencePreservingReducer bool     `json:"evidencePreservingReducer"`
		OnlineContextCompact      bool     `json:"onlineContextCompact"`
		CacheWriteReadRatio       *float64 `json:"cacheWriteReadRatio"`
	}
	if err := json.Unmarshal([]byte(profile), &cfg); err != nil {
		t.Fatalf("shipped sol-pi.json does not parse: %v", err)
	}
	// The conservative profile is EFFECTIVE, not merely parseable: the two
	// local, model-call-free mechanisms on; the reducer (ships repo logs to
	// a reducer model) and the compact-and-continue flow OFF.
	if !cfg.ActionFusion || !cfg.ObservationPack {
		t.Errorf("conservative profile must enable actionFusion + observationPack, got %+v", cfg)
	}
	if cfg.EvidencePreservingReducer || cfg.OnlineContextCompact {
		t.Errorf("conservative profile must keep reducer/compact OFF, got %+v", cfg)
	}
	if cfg.CacheWriteReadRatio == nil || *cfg.CacheWriteReadRatio < 0 {
		t.Errorf("cacheWriteReadRatio must be present and non-negative, got %+v", cfg.CacheWriteReadRatio)
	}
	// Both images install the same in-tree file at the path SoL-Pi reads.
	const wantCopy = "COPY extensions/sol-pi/sol-pi.json /root/.pi/agent/sol-pi.json"
	// AND the shipped settings.json must pin project trust to never — the
	// single string that makes the shipped profile THE effective config
	// (r4 §2 probe: without it, a trusted workspace's .pi/sol-pi.json
	// overrides the profile, up to evidencePreservingReducer=true egress).
	const wantTrust = `defaultProjectTrust":"never"`
	for _, f := range []string{"../../Dockerfile.worker", "../../.github/Dockerfile.worker.release"} {
		if !strings.Contains(string(mustRead(t, f)), wantTrust) {
			t.Errorf("%s does not pin defaultProjectTrust=never in settings.json — workspace .pi/sol-pi.json could override the shipped profile", f)
		}
	}
	for _, f := range []string{"../../Dockerfile.worker", "../../.github/Dockerfile.worker.release"} {
		if !strings.Contains(string(mustRead(t, f)), wantCopy) {
			t.Errorf("%s does not install the shipped profile with the exact single-source COPY (%q)", f, wantCopy)
		}
	}
	// Vendored provenance: the checkout records where it came from, so a
	// bump has a protocol and an audit trail (UPSTREAM.md).
	upstream := string(mustRead(t, "../../extensions/sol-pi/UPSTREAM.md"))
	// The provenance LINE, not any 40-char hex word anywhere in the file
	// (r3 pillar 9b: a loose scan passes on unrelated hex).
	shaLine := regexp.MustCompile(`(?m)^Vendored from .*\` + "`" + `([0-9a-f]{40})\` + "`" + `?`)
	if !shaLine.MatchString(upstream) {
		t.Errorf("extensions/sol-pi/UPSTREAM.md must record the vendored upstream commit as a 40-hex SHA on its 'Vendored from' line")
	}
}

// TestLoadedExtensions mirrors buildPiArgs' stat pre-flight: the startup log
// ("pi extensions: …") must name the same set that actually gets -e'd, so a
// silent degrade is observable per run (#425 r1 pillar 8).
func TestLoadedExtensions(t *testing.T) {
	all := loadedExtensions([]string{"/a", "/b"}, func(p string) (os.FileInfo, error) {
		if p == "/b" {
			return nil, os.ErrNotExist
		}
		return nil, nil
	})
	if len(all) != 1 || all[0] != "/a" {
		t.Fatalf("loadedExtensions = %v, want [/a] — the log must match the -e set", all)
	}
}

// piShippedTypebox is the typebox version the pinned PI_VERSION ships and
// aliases at runtime (pi's extension loader injects `typebox` → its bundled
// copy). extensions/rig-query/package.json pins exactly this version so
// `node --test` validates the schema against the SAME typebox the worker
// image loads (#338 r24 D4: the pin is the whole load-gate guarantee).
// Bump PI_VERSION → check pi's bundled typebox → bump BOTH together.
const piShippedTypebox = "1.3.7"

// TestPinnedVersionsAgree pins the version pair the load gate declares
// coupled: both worker images must carry the identical ARG PI_VERSION, and
// the extension's typebox pin must equal what that pi ships (#338 r24 D4).
// A hand-bump of PI_VERSION alone previously left every gate green while
// the image loaded a different typebox than the tests validated against.
func TestPinnedVersionsAgree(t *testing.T) {
	dev := string(mustRead(t, "../../Dockerfile.worker"))
	rel := string(mustRead(t, "../../.github/Dockerfile.worker.release"))
	devPI := mustArg(t, dev)
	relPI := mustArg(t, rel)
	if devPI == "" || relPI == "" {
		t.Fatalf("ARG PI_VERSION missing (dev=%q release=%q) — the load gate has no anchor", devPI, relPI)
	}
	if devPI != relPI {
		t.Errorf("PI_VERSION drifted: Dockerfile.worker=%s release=%s — dev and published images load DIFFERENT pi runtimes", devPI, relPI)
	}
	pkg := string(mustRead(t, "../../extensions/rig-query/package.json"))
	want := `"typebox": "` + piShippedTypebox + `"`
	if !strings.Contains(pkg, want) {
		t.Errorf("extensions/rig-query/package.json typebox pin != %s (what pi %s ships) — update the pin WITH PI_VERSION, together", piShippedTypebox, devPI)
	}
	// PI_VERSION is single-sourced at Dockerfile.worker (the hand-pin): the
	// Makefile compat tier DERIVES it (no copy), and ci.yml passes the
	// derivation through (no literal). A hardcoded copy anywhere else
	// recreates the #426 r2 pillar-4 finding: a pi bump leaves the compat
	// tier validating against a stale runtime with every gate green.
	makefile := string(mustRead(t, "../../Makefile"))
	if !strings.Contains(makefile, "PI_VERSION ?= $(shell sed -n 's/^ARG PI_VERSION=//p' Dockerfile.worker") {
		t.Errorf("Makefile must derive PI_VERSION from Dockerfile.worker — a literal copy drifts from the hand-pin")
	}
	ci := string(mustRead(t, "../../.github/workflows/ci.yml"))
	for _, line := range strings.Split(ci, "\n") {
		// A hardcoded copy is a bare semver after PI_VERSION= — a shell
		// reference ($PI_VERSION) or the sed derivation is the sanctioned form.
		if strings.Contains(line, "PI_VERSION=") && regexp.MustCompile(`PI_VERSION=\d`).MatchString(line) {
			t.Errorf("ci.yml hardcodes a PI_VERSION literal (%q) — derive it from Dockerfile.worker instead", strings.TrimSpace(line))
		}
	}
}

func mustArg(t *testing.T, dockerfile string) string {
	t.Helper()
	for _, line := range strings.Split(dockerfile, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ARG PI_VERSION="); ok {
			return v
		}
	}
	return ""
}

// TestPiargsIsLeaf keeps this package a leaf (#338 r26 ARCH-1): its whole
// reason to exist is that the smallest binary can assemble the pi shape
// without the pipeline's closure. A dependency on any internal/* sibling
// (worker, agent, dapr, k8s) re-couples them — go list -deps per run.
func TestPiargsIsLeaf(t *testing.T) {
	if testing.Short() {
		t.Skip("go list exec — skipped in -short")
	}
	root := "../.."
	cmd := exec.Command("go", "list", "-deps", "./internal/piargs")
	cmd.Dir = root // the test binary's CWD is the package dir — resolve from repo root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./internal/piargs: %v", err)
	}
	self := "github.com/tibrezus/harmostes/internal/piargs"
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != self && strings.HasPrefix(line, "github.com/tibrezus/harmostes/internal/") {
			t.Fatalf("internal/piargs depends on %s — it must stay a leaf (api/* types are fine); move the shared shape to a new leaf instead", line)
		}
	}
}

// TestRigGraphPathSingleSource pins the THIRD half of the freshness wiring
// (#338 r25 F6): the extension's container-candidates list must name exactly
// RigGraphPath. The Go half (graphPresenceLine) consumes the constant; this
// keeps the TypeScript half from drifting — with workdir reassigned by
// fetchWorkspaceRepo, a path mismatch would log "graph: absent" for a graph
// the extension happily serves (and vice versa).
func TestRigGraphPathSingleSource(t *testing.T) {
	idx := string(mustRead(t, "../../extensions/rig-query/index.ts"))
	want := `resolveRigDbCandidates(undefined, ["` + RigGraphPath + `"])`
	if !strings.Contains(idx, want) {
		t.Errorf("rig-query index.ts does not probe %s — the extension's container list and the worker pre-flight have drifted apart (want %q in the candidates call)", RigGraphPath, want)
	}
}

// TestHarmostesPyHasNoStaleExtensions is the REVERSE containment half
// (r28 P2 LOW): TestExtensionsSingleSource checks Go ⊆ python; this asserts
// python ⊆ Go — a stale extra in harmostes.py (extension removed from the
// image but still mirrored) would otherwise pass every gate while the
// primitive loads a dead path.
func TestHarmostesPyHasNoStaleExtensions(t *testing.T) {
	py := string(mustRead(t, "../../harmostes.py"))
	allowed := map[string]bool{}
	for _, ext := range Extensions {
		allowed[ext] = true
	}
	for _, m := range strings.Split(py, "\n") {
		for _, tok := range strings.Split(m, "\"") {
			if strings.HasPrefix(tok, "/extensions/") && !allowed[tok] {
				t.Errorf("harmostes.py references %q which piargs.Extensions does not carry — a stale mirror entry", tok)
			}
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
