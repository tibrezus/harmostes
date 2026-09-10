package worker

import (
	"context"
	"testing"
	"time"

	"github.com/tibrezus/harmostes/internal/agent"
)

// #350: SpawnEnv is evaluated at RUN time — before NewRPC spawns the pi
// process. The false-fire bug this wiring kills: an assembly-time check saw
// the workspace before prepare (inside the graph) had produced rig.db. The
// probe runs without a real pi binary — NewRPC fails on the bogus path, but
// the SpawnEnv side effect must already have happened.
func TestRPCAgentRunnerSpawnEnvEvaluatedAtRun(t *testing.T) {
	called := false
	r := RPCAgentRunner{
		Opts: agent.RPCOptions{PiPath: "/nonexistent-pi-binary"},
		SpawnEnv: func(env []string) []string {
			called = true
			return append(env, "PROBE_SPAWN_ENV=1")
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := r.Run(ctx, "task", nil, 0, nil)
	if err == nil {
		t.Fatal("bogus pi path must fail — the probe needs NewRPC to have been reached")
	}
	if !called {
		t.Fatal("SpawnEnv must be evaluated at Run time, before NewRPC")
	}
}
