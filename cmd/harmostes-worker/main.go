// Command harmostes-worker is the generic worker for any Workflow phase. It
// reads its assignment from the environment (the HARMOSTES_* contract — see
// docs/plugin-interface.md), resolves the Workflow CR, and runs the phase:
//
//   - prepare / deploy: invoke the phase's plugin (image or ConfigMap script)
//     with the contract env, parse the JSON result, emit the next Dapr event.
//   - agent: run the Go harmostes primitive (internal/agent) — pi --mode rpc,
//     task → gate → feedback-as-warm-session-continuation.
//
// This file is the entrypoint + arg/env wiring. Phase execution + the Dapr
// client land alongside it as the controller + worker are completed (see
// ARCHITECTURE.md §Migration). For now it validates its contract and reports.
package main

import (
	"fmt"
	"log"
	"os"
)

func main() {
	phase := os.Getenv("HARMOSTES_PHASE")
	workflow := os.Getenv("HARMOSTES_WORKFLOW")
	namespace := os.Getenv("HARMOSTES_NAMESPACE")
	if phase == "" || workflow == "" {
		fmt.Fprintln(os.Stderr, "ERROR: HARMOSTES_PHASE and HARMOSTES_WORKFLOW are required")
		os.Exit(2)
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[harmostes-worker] ")
	log.Printf("phase=%s workflow=%s/%s — phase runtime not yet wired (framework skeleton)",
		phase, namespace, workflow)
	// TODO(worker): dispatch on phase:
	//   prepare → worker.Prepare(ctx, wf)  (invoke plugin, emit work.needs-agent)
	//   agent   → worker.Agent(ctx, wf)    (run internal/agent.Task, emit work.resolved|failed)
	//   deploy  → worker.Deploy(ctx, wf)   (invoke plugin, emit work.deployed)
	os.Exit(0)
}
