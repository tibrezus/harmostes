package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tibrezus/harmostes/internal/observability"
	"github.com/tibrezus/harmostes/internal/timeline"
)

// TestFlushTelemetryCallsShutdown: the worker's exit path flushes telemetry —
// the Phase 1 guarantee that an ephemeral Job (os.Exit in 3 places) doesn't drop
// spans/metrics. finish() calls flushTelemetry() before os.Exit; this asserts the
// flush actually invokes the configured shutdown.
func TestFlushTelemetryCallsShutdown(t *testing.T) {
	called := make(chan struct{}, 1)
	prev := obsShutdown
	t.Cleanup(func() { obsShutdown = prev })
	obsShutdown = func(context.Context) error { called <- struct{}{}; return nil }

	flushTelemetry()

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("flushTelemetry did not call obsShutdown — telemetry would be dropped on exit")
	}
}

// TestFlushTelemetryNilSafe: a disabled or failed Init (nil shutdown) must not
// panic — local dev / unit runs have no collector.
func TestFlushTelemetryNilSafe(t *testing.T) {
	prev := obsShutdown
	t.Cleanup(func() { obsShutdown = prev })
	obsShutdown = nil
	flushTelemetry()
}

// TestShutdownTimeoutBounded: the flush is time-bounded so a stuck exporter
// can't hang process termination.
func TestShutdownTimeoutBounded(t *testing.T) {
	if observability.ShutdownTimeout <= 0 || observability.ShutdownTimeout > 30*time.Second {
		t.Fatalf("ShutdownTimeout=%v is not a sane flush bound", observability.ShutdownTimeout)
	}
}

// TestRedactStripsCredentialsFromCleanURL: a standalone basic-auth URL is fully
// redacted — the user:token@ segment is removed, the rest preserved.
func TestRedactStripsCredentialsFromCleanURL(t *testing.T) {
	in := "https://tibrez:d0a0352e7384a7ebb812196f88749fa2efe63a78@git.rezus.cloud/tibrez/rhesadox.git"
	want := "https://git.rezus.cloud/tibrez/rhesadox.git"
	if got := redact(in); got != want {
		t.Errorf("redact clean URL:\n got %q\nwant %q", got, want)
	}
}

// TestRedactStripsEmbeddedCredentialsInPluginOutput: the credential leak this
// fix targets — the token is buried in a multi-line error string (rig-emit
// plugin output captured into a pipeline result message).
func TestRedactStripsEmbeddedCredentialsInPluginOutput(t *testing.T) {
	in := "[rig-emit] cloning source https://tibrez:d0a0352e7384a7ebb812196f88749fa2efe63a78@git.rezus.cloud/tibrez/rhesadox.git (main) …\n" +
		"fatal: Authentication failed for 'https://git.rezus.cloud/tibrez/rhesadox.git/'"
	want := "[rig-emit] cloning source https://git.rezus.cloud/tibrez/rhesadox.git (main) …\n" +
		"fatal: Authentication failed for 'https://git.rezus.cloud/tibrez/rhesadox.git/'"
	if got := redact(in); got != want {
		t.Errorf("redact embedded URL:\n got %q\nwant %q", got, want)
	}
}

// TestRedactPreservesURLWithoutCredentials: a normal URL (no basic-auth) and
// arbitrary text are returned unchanged.
func TestRedactPreservesURLWithoutCredentials(t *testing.T) {
	cases := map[string]string{
		"https://git.rezus.cloud/tibrez/rhesadox.git":     "https://git.rezus.cloud/tibrez/rhesadox.git",
		"https://git.rezus.cloud:443/tibrez/rhesadox.git": "https://git.rezus.cloud:443/tibrez/rhesadox.git", // port, no creds
		"user@host: no scheme here":                       "user@host: no scheme here",
		"not a url at all":                                "not a url at all",
		"":                                                "",
	}
	for in, want := range cases {
		if got := redact(in); got != want {
			t.Errorf("redact(%q):\n got %q\nwant %q", in, got, want)
		}
	}
}

// TestRedactStripsMultipleCredentials: a string with several credentialed URLs
// (e.g. source + fork) is fully cleaned.
func TestRedactStripsMultipleCredentials(t *testing.T) {
	in := "src https://u:p@git.rezus.cloud/a.git fork https://x:y@github.com/b.git"
	want := "src https://git.rezus.cloud/a.git fork https://github.com/b.git"
	if got := redact(in); got != want {
		t.Errorf("redact multiple:\n got %q\nwant %q", got, want)
	}
}

func TestSubjectFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want timeline.Subject
	}{
		{
			name: "pointer form (annotation fallback / bare env)",
			env:  map[string]string{"HARMOSTES_TRIGGER_PR": "git.rezus.cloud/tibrez/rhesadox#1566", "HARMOSTES_TRIGGER_TITLE": "CI-tiering"},
			want: timeline.Subject{Kind: "pr", Ref: "git.rezus.cloud/tibrez/rhesadox#1566", Title: "CI-tiering"},
		},
		{
			name: "number + repo (gate envelope exports)",
			env: map[string]string{
				"HARMOSTES_TRIGGER_PR":   "1566",
				"HARMOSTES_TRIGGER_REPO": "git.rezus.cloud/tibrez/rhesadox",
				"HARMOSTES_TRIGGER_SHA":  "4c01cc4f",
			},
			want: timeline.Subject{Kind: "pr", Ref: "git.rezus.cloud/tibrez/rhesadox#1566", SHA: "4c01cc4f"},
		},
		{
			name: "sha falls back to wake revision",
			env: map[string]string{
				"HARMOSTES_TRIGGER_PR":       "1566",
				"HARMOSTES_TRIGGER_REPO":     "git.rezus.cloud/tibrez/rhesadox",
				"HARMOSTES_TRIGGER_REVISION": "ebdbcb32",
			},
			want: timeline.Subject{Kind: "pr", Ref: "git.rezus.cloud/tibrez/rhesadox#1566", SHA: "ebdbcb32"},
		},
		{
			name: "no trigger",
			env:  map[string]string{},
			want: timeline.Subject{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Hermetic: blank every trigger var first — inside a worker pod
			// the real env carries them and would leak into "no trigger".
			for _, k := range []string{
				"HARMOSTES_TRIGGER_PR", "HARMOSTES_TRIGGER_REPO",
				"HARMOSTES_TRIGGER_SHA", "HARMOSTES_TRIGGER_REVISION",
				"HARMOSTES_TRIGGER_TITLE",
			} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got := subjectFromEnv()
			if got != tc.want {
				t.Fatalf("subjectFromEnv() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestGraphPresenceLine pins the run-level degradation signal (#338 r24 D5):
// with the reviewed SHA armed, a missing graph or missing stamp must be a
// greppable pod-log line — the archaeology cost must not return invisibly.
func TestGraphPresenceLine(t *testing.T) {
	dir := t.TempDir()
	graph := filepath.Join(dir, "rig.db")

	// no graph at all
	line, ok := graphPresenceLine(graph)
	if !ok || !strings.Contains(line, "graph: absent") {
		t.Fatalf("missing graph: want graph: absent line, got ok=%v %q", ok, line)
	}
	// graph without stamp
	if err := os.WriteFile(graph, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	line, ok = graphPresenceLine(graph)
	if !ok || !strings.Contains(line, "graph: unstamped") {
		t.Fatalf("unstamped graph: want graph: unstamped line, got ok=%v %q", ok, line)
	}
	// both halves present → silent
	if err := os.WriteFile(graph+".sha", []byte("0123abcd"), 0o644); err != nil {
		t.Fatal(err)
	}
	if line, ok := graphPresenceLine(graph); ok {
		t.Fatalf("healthy graph must stay silent, got %q", line)
	}
}

// TestWakeFromEnvPrecedence (#357 P8 defect 2): the run command's boundary
// threading was untested — a precedence flip here (SHA before REVISION) is
// exactly the silent, compile-clean, CI-green bug class #357 exists to kill.
func TestWakeFromEnvPrecedence(t *testing.T) {
	cases := []struct {
		name          string
		pr, action    string
		sha, revision string
		repo          string
		wantPR        string
		wantAction    string
		wantRevision  string
	}{
		{"sha wins over revision", "#99", "labeled", "deadbeefsha", "deadbeefrev", "", "#99", "labeled", "deadbeefsha"},
		{"revision used when sha empty", "#99", "labeled", "", "deadbeefrev", "", "#99", "labeled", "deadbeefrev"},
		{"no event at all", "", "", "", "", "", "", "", ""},
		// dispatchEnv's shape: BARE number + separate repo (r16 2.1 — the
		// boundary must parse both shapes its own package exports, or the
		// wake dies as unparseable: the exact silent no-op #349 filed).
		{"bare number + repo composes", "99", "labeled", "deadbeefsha", "", "git.rezus.cloud/tibrez/rhesadox", "git.rezus.cloud/tibrez/rhesadox#99", "labeled", "deadbeefsha"},
		{"bare number without repo stays bare (gate logs the reject)", "99", "labeled", "", "", "", "99", "labeled", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HARMOSTES_TRIGGER_PR", tc.pr)
			t.Setenv("HARMOSTES_TRIGGER_ACTION", tc.action)
			t.Setenv("HARMOSTES_TRIGGER_SHA", tc.sha)
			t.Setenv("HARMOSTES_TRIGGER_REVISION", tc.revision)
			t.Setenv("HARMOSTES_TRIGGER_REPO", tc.repo)
			w := wakeFromEnv()
			if w.PR != tc.wantPR || w.Action != tc.wantAction || w.Revision != tc.wantRevision {
				t.Fatalf("want (PR=%q Action=%q Rev=%q), got (PR=%q Action=%q Rev=%q)",
					tc.wantPR, tc.wantAction, tc.wantRevision, w.PR, w.Action, w.Revision)
			}
		})
	}
}

// TestBuiltinPluginsParity — the three-way parity guard (r1 review, P1/P8):
// builtinPlugins() ↔ Dockerfile.worker COPY ↔ plugins/ on disk. Both slips
// this PR made (workspace missing from one leg, divergence-track claimed but
// never shipped) were invisible because nothing compared the three sources.
// Test lives in-package to call the unexported builtinPlugins(); repo-root
// artifacts are reached via ../../.
func TestBuiltinPluginsParity(t *testing.T) {
	builtins := builtinPlugins()
	if len(builtins) == 0 {
		t.Fatal("builtinPlugins() is empty")
	}

	// BOTH worker Dockerfiles — the dev one AND the GoReleaser release one —
	// must EACH copy every builtin. The r131 incident shipped an image whose
	// binary registered the ADR-0011 builtins while
	// .github/Dockerfile.worker.release (the one the release pipeline actually
	// builds) omitted them: prepare fork/exec ENOENT fleet-wide. Per-file
	// sets, not a union — a union cannot see a file missing from only one
	// Dockerfile (mutation-probed both ways).
	for _, df := range []string{"Dockerfile.worker", filepath.Join(".github", "Dockerfile.worker.release")} {
		dfBytes, err := os.ReadFile(filepath.Join("..", "..", df))
		if err != nil {
			t.Fatalf("read %s: %v", df, err)
		}
		set := map[string]bool{}
		for _, line := range strings.Split(string(dfBytes), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "COPY ") {
				continue
			}
			fields := strings.Fields(line)
			dest := fields[len(fields)-1]
			if strings.HasPrefix(dest, "/usr/local/lib/harmostes/plugins/") {
				set[dest] = true
			}
		}
		for name, path := range builtins {
			if !set[path] {
				t.Errorf("builtin %q → %s is NOT COPY-ed by %s — that image would resolve it but not contain it", name, path, df)
			}
		}
	}

	// plugins/ on disk: every <name>/<name>.sh pair.
	onDisk := map[string]string{} // name → relative path
	dirs, err := filepath.Glob(filepath.Join("..", "..", "plugins", "*", "*.sh"))
	if err != nil {
		t.Fatalf("glob plugins: %v", err)
	}
	for _, p := range dirs {
		name := filepath.Base(filepath.Dir(p))
		if filepath.Base(p) == name+".sh" {
			onDisk[name] = p
		}
	}

	for name := range builtins {
		disk, ok := onDisk[name]
		if !ok {
			t.Errorf("builtin %q has no plugins/%s/%s.sh source file", name, name, name)
			continue
		}
		if _, err := os.Stat(disk); err != nil {
			t.Errorf("builtin %q source missing: %v", name, err)
		}
	}
	for name := range onDisk {
		if _, ok := builtins[name]; !ok {
			t.Errorf("plugins/%s/%s.sh exists on disk but is NOT registered in builtinPlugins() — a silent third resolution path (the divergence-track slip)", name, name)
		}
	}
}
