package worker

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRunPluginKillsStalledGrandchild (#695 r1, the #689 zero-work class):
// a node deadline must actually RETURN when the plugin's direct child died
// but a grandchild holds the stdout pipe (`git clone … | tail`). Before the
// WaitDelay + process-group fix, cmd.Run blocked until the grandchild
// exited — the G8 timeout fired but the node never came back, and the run
// burned its whole wall with zero durable work.
func TestRunPluginKillsStalledGrandchild(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real processes")
	}
	// shell: echo lands in out, then `sleep 60 | tail -1` holds the pipe
	// from a grandchild; the shell exits immediately after forking it.
	marker := "pin-" + strings.Split(t.Name(), "/")[0]
	// the pipe structure is the point: `sleep 60 | tail -1` makes tail a
	// pipe-holding grandchild; the marker rides an exported var so pgrep
	// can identify THIS run's processes without touching the pipeline.
	script := "echo starting; PIN=" + marker + "; sleep 60 | tail -1; echo done"
	env := PluginEnv{Workdir: t.TempDir()}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	type res struct {
		err error
	}
	done := make(chan res, 1)
	go func() {
		_, _, err := RunPlugin(ctx, "bash", []string{"-c", script}, env, nil)
		done <- res{err}
	}()

	select {
	case r := <-done:
		elapsed := time.Since(start)
		if elapsed > 15*time.Second {
			t.Fatalf("RunPlugin returned after %s — the pipe-holding grandchild still blocks Wait", elapsed)
		}
		if r.err == nil {
			t.Fatal("expected a non-nil error from a killed plugin")
		}
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("ctx error should be the deadline, got %v", ctx.Err())
		}
		// The group kill is the mechanism, not an accident of WaitDelay
		// (r2 4301268805: a direct-child kill passes a WaitDelay-only
		// harness). THIS run's grandchild must be DEAD by now — matched
		// by a unique marker so unrelated host processes can't flake it.
		time.Sleep(300 * time.Millisecond) // let the kill land
		if out, err := exec.Command("pgrep", "-f", "PIN="+marker).CombinedOutput(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
			t.Fatalf("grandchild survived the context cancel — the group kill is not pinning: pgrep -f %q = %s", marker, strings.TrimSpace(string(out)))
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("RunPlugin still blocked %s after the 2s deadline — a pipe-holding grandchild escapes the node bound (the exact #689 zero-work failure)", time.Since(start).Round(time.Second))
	}
}
