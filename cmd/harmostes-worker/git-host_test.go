package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tibrezus/harmostes/internal/review"
)

// git-host.sh is the bash translation of internal/review's host resolver
// (ResolveHost + TokenEnvNames) — the single mapping the image builtins
// (workspace, post-review) share since #368 finding 7. These tests pin both
// directions: the lib's own table, and the Go↔bash mirror (a drift between
// the two implementations fails CI instead of shipping divergent host maps).

const gitHostLib = "../../plugins/lib/git-host.sh"

// runGitHost sources the lib and runs a snippet, with extraEnv applied on top
// of a scrubbed environment (only PATH+HOME pass through, so chain lookups
// see exactly the sentinels the test sets).
func runGitHost(t *testing.T, env []string, snippet string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "-c", ". "+gitHostLib+"\n"+snippet)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, env...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func TestGitHostLibTable(t *testing.T) {
	cases := []struct {
		host   string
		api    string
		isFJ   string
		chainA string // first token-chain entry (sentinel must come back when set)
		chainB string // second entry (must come back when the first is unset)
		clone  string // clone URL with CHAIN_A set
	}{
		{"github.com", "https://api.github.com", "false",
			"HARMOSTES_GIT_TOKEN", "HARMOSTES_GITHUB_TOKEN",
			"https://x-access-token:V_A@github.com/o/r.git"},
		{"codeberg.org", "https://codeberg.org/api/v1", "true",
			"HARMOSTES_CODEBERG_TOKEN", "LLM_WIKI_CODEBERG_TOKEN",
			"https://V_A@codeberg.org/o/r.git"},
		{"git.rezus.cloud", "https://git.rezus.cloud/api/v1", "true",
			"HARMOSTES_FORGEJO_TOKEN", "HARMOSTES_RZC_PASSWORD",
			"https://tibrez:V_A@git.rezus.cloud/o/r.git"},
		{"git.example.com", "https://git.example.com/api/v1", "true",
			"HARMOSTES_FORGEJO_TOKEN", "HARMOSTES_GIT_TOKEN",
			"https://git.example.com/o/r.git"},
	}

	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			api, err := runGitHost(t, nil, "host::api_base "+tc.host)
			if err != nil || api != tc.api {
				t.Errorf("host::api_base %s = %q (%v), want %q", tc.host, api, err, tc.api)
			}
			isFJ, err := runGitHost(t, nil, "host::is_fj "+tc.host)
			if err != nil || isFJ != tc.isFJ {
				t.Errorf("host::is_fj %s = %q (%v), want %q", tc.host, isFJ, err, tc.isFJ)
			}

			// first chain entry set → it wins (the scrubbed env means nothing
			// else is visible, so the resolved value must be this sentinel)
			envA := []string{tc.chainA + "=V_A"}
			tok, err := runGitHost(t, envA, "host::token "+tc.host)
			if err != nil || tok != "V_A" {
				t.Errorf("host::token %s (chain head set) = %q (%v), want V_A", tc.host, tok, err)
			}
			clone, err := runGitHost(t, envA, "host::clone_url "+tc.host+" o/r")
			if err != nil || clone != tc.clone {
				t.Errorf("host::clone_url %s = %q (%v), want %q", tc.host, clone, err, tc.clone)
			}

			// chain head unset → fallback entry wins
			envB := []string{tc.chainB + "=V_B"}
			tok, err = runGitHost(t, envB, "host::token "+tc.host)
			if err != nil || tok != "V_B" {
				t.Errorf("host::token %s (fallback) = %q (%v), want V_B", tc.host, tok, err)
			}

			// all unset → optional yields empty, required fails
			tok, err = runGitHost(t, nil, "host::token "+tc.host)
			if err != nil || tok != "" {
				t.Errorf("host::token %s (no env) = %q (%v), want empty", tc.host, tok, err)
			}
			if _, err := runGitHost(t, nil, "host::token "+tc.host+" required"); err == nil {
				t.Errorf("host::token %s required with no env: want error", tc.host)
			}
		})
	}
}

// TestGitHostLibMirrorsGoResolver is the anti-drift gate: for every repo-path
// form the kernel understands, the bash lib's API base, Forgejo-ness, and
// token-chain head must agree with review.ResolveHost + Host.TokenEnvNames.
func TestGitHostLibMirrorsGoResolver(t *testing.T) {
	repos := []string{
		"github.com/o/r",
		"codeberg.org/o/r",
		"git.rezus.cloud/o/r",
		"git.example.com/o/r",
	}
	allChainVars := []string{
		"HARMOSTES_GIT_TOKEN", "HARMOSTES_GITHUB_TOKEN",
		"HARMOSTES_CODEBERG_TOKEN", "LLM_WIKI_CODEBERG_TOKEN",
		"HARMOSTES_FORGEJO_TOKEN", "HARMOSTES_RZC_PASSWORD",
	}

	for _, repo := range repos {
		h, err := review.ResolveHost(repo)
		if err != nil {
			t.Fatalf("ResolveHost(%q): %v", repo, err)
		}
		goChain := h.TokenEnvNames()
		if len(goChain) == 0 {
			t.Fatalf("TokenEnvNames(%q) empty", repo)
		}

		host := strings.SplitN(repo, "/", 2)[0]

		api, err := runGitHost(t, nil, "host::api_base "+host)
		if err != nil || api != h.APIBase {
			t.Errorf("%s: bash api_base %q != Go APIBase %q (%v)", repo, api, h.APIBase, err)
		}

		wantFJ := "false"
		if h.Kind == review.HostForgejo {
			wantFJ = "true"
		}
		isFJ, err := runGitHost(t, nil, "host::is_fj "+host)
		if err != nil || isFJ != wantFJ {
			t.Errorf("%s: bash is_fj %q != Go kind %q (want is_fj=%s)", repo, isFJ, h.Kind, wantFJ)
		}

		// the head of the bash chain must be the head of the Go chain:
		// set every chain var to a sentinel named after itself, clear none —
		// whichever sentinel comes back names the winning entry.
		env := append([]string{}, allChainVars...)
		for _, v := range allChainVars {
			env = append(env, v+"=V_"+v)
		}
		tok, err := runGitHost(t, env, "host::token "+host)
		if err != nil {
			t.Fatalf("host::token %s: %v", host, err)
		}
		if want := "V_" + goChain[0]; tok != want {
			t.Errorf("%s: bash token chain head resolves %q, Go chain head is %q (bash lib drifted from review.go TokenEnvNames)", host, tok, want)
		}
	}
}

// TestBuiltinDockerfilesShipGitHostLib extends the r131 lesson to the shared
// lib: any builtin sourcing lib/git-host.sh dies at prepare time if the lib
// is missing from the image — so BOTH Dockerfiles must ship plugins/lib/.
func TestBuiltinDockerfilesShipGitHostLib(t *testing.T) {
	sources, err := filepath.Glob("../../plugins/*/*.sh")
	if err != nil {
		t.Fatalf("glob plugins: %v", err)
	}
	usesLib := false
	for _, src := range sources {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		if strings.Contains(string(b), "../lib/") {
			usesLib = true
			break
		}
	}
	if !usesLib {
		t.Fatal("no builtin sources ../lib/ — this guard is stale, update it alongside the lib")
	}
	for _, df := range []string{"../../Dockerfile.worker", filepath.Join("../../.github", "Dockerfile.worker.release")} {
		b, err := os.ReadFile(df)
		if err != nil {
			t.Fatalf("read %s: %v", df, err)
		}
		shipped := false
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "COPY ") {
				continue
			}
			fields := strings.Fields(line)
			// builtins are COPY-ed FLAT (…/plugins/workspace.sh), so the lib
			// must land at …/lib/ — exactly what the source line
			// $(dirname "$0")/../lib/git-host.sh resolves to in-image.
			if fields[1] == "plugins/lib/" && fields[len(fields)-1] == "/usr/local/lib/harmostes/lib/" {
				shipped = true
				break
			}
		}
		if !shipped {
			t.Errorf("%s does not COPY plugins/lib/ to /usr/local/lib/harmostes/lib/ — builtins sourcing lib/git-host.sh would fail at prepare (r131 class)", df)
		}
	}
}
