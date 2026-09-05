package piargs

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/tibrezus/harmostes/api/v1alpha1"
)

func TestPiArgsLoadBuiltinExtensions(t *testing.T) {
	args := buildPiArgs(v1alpha1.AgentSpec{Skill: "pr-review", Model: "litellm/test-model"}, Extensions, alwaysPresent)
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
	args := buildPiArgs(v1alpha1.AgentSpec{Skill: "s", Model: "m", Tools: []string{"bash", "read"}}, Extensions, alwaysPresent)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--tools bash,read,rig") {
		t.Errorf("tools allowlist must gain rig, got: %s", joined)
	}
	// Already declared → not duplicated.
	args = buildPiArgs(v1alpha1.AgentSpec{Skill: "s", Model: "m", Tools: []string{"bash", "rig"}}, Extensions, alwaysPresent)
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "--tools bash,rig") || strings.Contains(joined, "rig,rig") {
		t.Errorf("declared rig must not be duplicated, got: %s", joined)
	}
}

func alwaysPresent(string) (os.FileInfo, error) { return nil, nil }

// An image without an extension directory must drop it from the args (and
// from the --tools allowlist) instead of killing pi at startup (#338 r14 B1).
func TestPiArgsDropsMissingExtension(t *testing.T) {
	args := buildPiArgs(
		v1alpha1.AgentSpec{Skill: "s", Model: "m", Tools: []string{"bash"}},
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
func TestExtensionsSingleSource(t *testing.T) {
	py := string(mustRead(t, "../../harmostes.py"))
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
		if !strings.Contains(py, "\""+ext+"\"") {
			t.Errorf("harmostes.py does not load %s — the standalone primitive drifts from the worker", ext)
		}
		// The tool the extension registers must match the allowlist the Python
		// path builds — value drift is the r17 M4 class (a path can load while
		// the tool it registers never enters --tools). r25 F7: iterate the REAL
		// extensionTools map, not a re-literalized copy — the guard must not
		// itself be a duplicate of the value it guards.
		if tool, ok := extensionTools[ext]; ok {
			entry := "\"" + ext + "\": \"" + tool + "\""
			if !strings.Contains(py, entry) {
				t.Errorf("harmostes.py extension_tools is missing %s", entry)
			}
		}
		// (provider-only extensions legitimately have no entry — see the
		// extensionTools comment; iterating the REAL map means a tool ADDED
		// there is checked without this test growing a copy, r25 F7.)
		args := buildPiArgs(v1alpha1.AgentSpec{Skill: "s", Model: "m"}, Extensions, alwaysPresent)
		if !strings.Contains(strings.Join(args, " "), ext) {
			t.Errorf("PiArgs does not load %s", ext)
		}
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

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
