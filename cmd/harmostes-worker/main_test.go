package main

import (
	"context"
	"github.com/tibrezus/harmostes/internal/timeline"
	"testing"
	"time"

	"github.com/tibrezus/harmostes/internal/observability"
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
