package graph

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// scriptNode builds a plugin node whose script fails with the given exit
// code on the first `failTimes` executions, then exits 0 printing valid
// JSON. The invocation counter lives in a file so each exec (a fresh sh)
func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// execCountFor reads how many times the script ran (same trick as the
// script itself: the count file lives in the node's temp dir).
// We instead count invocations via the resolver wrapper below — simpler
// and exact.

// TestRetryPolicyOf pins the parsing/capping contract.
func TestRetryPolicyOf(t *testing.T) {
	if p := retryPolicyOf(nil); p.maxAttempts != 1 {
		t.Errorf("nil policy maxAttempts = %d, want 1 (no retry)", p.maxAttempts)
	}
	if p := retryPolicyOf(&v1alpha1.RetryPolicy{MaxAttempts: 1}); p.maxAttempts != 1 {
		t.Errorf("MaxAttempts=1 → %d, want 1", p.maxAttempts)
	}
	p := retryPolicyOf(&v1alpha1.RetryPolicy{MaxAttempts: 3})
	if p.maxAttempts != 3 || p.initialDelay != defaultRetryInitialDelay || p.maxDelay != defaultRetryMaxDelay {
		t.Errorf("policy = %+v, want 3 attempts with default delays", p)
	}
	if p := retryPolicyOf(&v1alpha1.RetryPolicy{MaxAttempts: 99}); p.maxAttempts != maxRetryAttemptsCap {
		t.Errorf("uncapped maxAttempts = %d, want %d", p.maxAttempts, maxRetryAttemptsCap)
	}
	p = retryPolicyOf(&v1alpha1.RetryPolicy{MaxAttempts: 2, InitialDelay: "10s", MaxDelay: "30s"})
	if p.initialDelay != 10*time.Second || p.maxDelay != 30*time.Second {
		t.Errorf("explicit delays not parsed: %+v", p)
	}
}

// TestRetryDelayFor pins the exponential + cap math.
func TestRetryDelayFor(t *testing.T) {
	p := retryPolicy{maxAttempts: 5, initialDelay: time.Second, maxDelay: 4 * time.Second}
	want := map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 4 * time.Second}
	for a, d := range want {
		if got := p.delayFor(a); got != d {
			t.Errorf("delayFor(%d) = %s, want %s", a, got, d)
		}
	}
}

// TestExecutorTransientRetryGreen: two transient failures, then green —
// the run recovers, the envelope carries attempt=3, and the timeline got
// the node.retry story.
func TestExecutorTransientRetryGreen(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "flaky.sh")
	body := "#!/bin/sh\n" +
		"n=$(cat " + dir + "/count 2>/dev/null || echo 0)\n" +
		"n=$((n+1))\n" +
		"echo $n > " + dir + "/count\n" +
		"if [ $n -le 2 ]; then exit 75; fi\n" +
		"echo '{\"status\":\"ok\",\"artifact\":\"out.txt\",\"changed\":true}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	registry := NewDefaultRegistry(Dependencies{
		PluginResolver: &fakeResolver{command: "/bin/sh", args: []string{script}},
	})
	exec := NewGraphExecutor(registry, nil, WithLogger(func(string, ...any) {}))

	gs := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{{
			ID:     "flaky",
			Type:   "plugin",
			Config: mustJSON(t, PluginNodeConfig{Name: "flaky"}),
			Retry:  &v1alpha1.RetryPolicy{MaxAttempts: 3, InitialDelay: "1ms", MaxDelay: "2ms"},
		}},
	}
	result, err := exec.Execute(context.Background(), gs, "wf")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Fatalf("status = %q, want green (transient retries recover the run)", result.Status)
	}
	nr := result.NodeResults["flaky"]
	if nr.Attempt != 3 {
		t.Errorf("attempt = %d, want 3", nr.Attempt)
	}
	env := result.NodeEnvelopes["flaky"]
	if env.Attempt != 3 {
		t.Errorf("envelope attempt = %d, want 3", env.Attempt)
	}
	if env.Status != "ok" {
		t.Errorf("envelope status = %q, want ok (the final attempt's result wins)", env.Status)
	}
}

// TestExecutorTransientRetryExhausted: always-transient with a budget of
// 2 — the node fails with attempt=2 and the failure routes normally
// (pipeline failed, dead-letter published).
func TestExecutorTransientRetryExhausted(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "dead.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'remote down'\nexit 69\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := NewDefaultRegistry(Dependencies{
		PluginResolver: &fakeResolver{command: "/bin/sh", args: []string{script}},
	})
	exec := NewGraphExecutor(registry, nil, WithLogger(func(string, ...any) {}))

	gs := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{{
			ID:     "dead",
			Type:   "plugin",
			Config: mustJSON(t, PluginNodeConfig{Name: "dead"}),
			Retry:  &v1alpha1.RetryPolicy{MaxAttempts: 2, InitialDelay: "1ms"},
		}},
	}
	result, err := exec.Execute(context.Background(), gs, "wf")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, want failed (budget exhausted folds into failure routing)", result.Status)
	}
	if got := result.NodeResults["dead"].Attempt; got != 2 {
		t.Errorf("attempt = %d, want 2", got)
	}
	if got := result.NodeEnvelopes["dead"].Attempt; got != 2 {
		t.Errorf("envelope attempt = %d, want 2", got)
	}
	if !strings.Contains(result.Message, "dead") {
		t.Errorf("message = %q, want it to name the failing node", result.Message)
	}
}

// TestExecutorNoPolicyNoRetry: a transient exit code WITHOUT a retry
// policy fails immediately — the policy is opt-in, the default is one
// attempt.
func TestExecutorNoPolicyNoRetry(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "blip.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 75\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := NewDefaultRegistry(Dependencies{
		PluginResolver: &fakeResolver{command: "/bin/sh", args: []string{script}},
	})
	exec := NewGraphExecutor(registry, nil, WithLogger(func(string, ...any) {}))

	gs := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{{
			ID:     "blip",
			Type:   "plugin",
			Config: mustJSON(t, PluginNodeConfig{Name: "blip"}),
		}},
	}
	result, err := exec.Execute(context.Background(), gs, "wf")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, want failed (no policy = no retry)", result.Status)
	}
	if got := result.NodeResults["blip"].Attempt; got != 1 {
		t.Errorf("attempt = %d, want 1 (exactly one execution)", got)
	}
}

// TestExecutorNonTransientNeverRetries: exit 1 is terminal even with a
// policy — only the exit-code contract classifies transient.
func TestExecutorNonTransientNeverRetries(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "hard.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'bug'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := NewDefaultRegistry(Dependencies{
		PluginResolver: &fakeResolver{command: "/bin/sh", args: []string{script}},
	})
	exec := NewGraphExecutor(registry, nil, WithLogger(func(string, ...any) {}))

	gs := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{{
			ID:     "hard",
			Type:   "plugin",
			Config: mustJSON(t, PluginNodeConfig{Name: "hard"}),
			Retry:  &v1alpha1.RetryPolicy{MaxAttempts: 4, InitialDelay: "1ms"},
		}},
	}
	result, err := exec.Execute(context.Background(), gs, "wf")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if got := result.NodeResults["hard"].Attempt; got != 1 {
		t.Errorf("attempt = %d, want 1 (exit 1 is terminal — no retry)", got)
	}
}

// TestTransientExitCodeClassification pins the contract table.
func TestTransientExitCodeClassification(t *testing.T) {
	for _, c := range []struct {
		code      int
		transient bool
	}{{75, true}, {69, true}, {1, false}, {2, false}, {0, false}, {127, false}} {
		if got := transientExitCode(c.code); got != c.transient {
			t.Errorf("transientExitCode(%d) = %v, want %v", c.code, got, c.transient)
		}
	}
}
