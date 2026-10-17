package worker

import (
	"context"
	"testing"
	"time"
)

// TestGatePluginTimeoutIsError (#695 r2 4301258144): a deadline-killed gate
// must surface as an ERROR — green=false with nil error reads as gate
// feedback and burns an agent fix attempt on a timeout. The bound exists to
// fail the NODE with timeout attribution, not to lie to the fix loop.
func TestGatePluginTimeoutIsError(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real processes")
	}
	g := GatePlugin{
		Command: "bash",
		Args:    []string{"-c", "echo starting; sleep 30"},
		Env:     PluginEnv{Workdir: t.TempDir()},
		Timeout: "1s",
	}
	start := time.Now()
	green, _, err := g.Run(context.Background())
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("gate Run returned after %s — the Timeout bound did not fire", elapsed)
	}
	if err == nil {
		t.Fatal("a deadline-killed gate must return an error (green=false, err=nil is indistinguishable from a failing gate)")
	}
	if green {
		t.Fatal("a deadline-killed gate must not be green")
	}
}

// TestGatePluginGreenStaysGreen (#695 r2 4301252377): the group kill must
// not re-contract the success path — an exit-0 plugin with a background
// child holding the pipes stays GREEN (no unconditional WaitDelay).
func TestGatePluginGreenStaysGreen(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real processes")
	}
	g := GatePlugin{
		Command: "bash",
		Args:    []string{"-c", "echo ok; (sleep 8 | tail -1 &); exit 0"},
		Env:     PluginEnv{Workdir: t.TempDir()},
	}
	green, _, err := g.Run(context.Background())
	if err != nil {
		t.Fatalf("exit-0 plugin with a daemon child must stay green (happy-path re-contraction): %v", err)
	}
	if !green {
		t.Fatal("exit-0 plugin reported not-green")
	}
}
