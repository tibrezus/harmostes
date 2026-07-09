// Package agent implements harmostes's core: the task → gate → feedback loop
// over a warm pi.dev RPC session. This is the Go port of the proven harmostes.py
// primitive — no Python runtime is involved; pi (Node) is spawned as a
// subprocess and driven over its JSONL protocol.
//
// The loop:
//
//	prompt(task) → gate → on failure, prompt(feedback) in the SAME session →
//	gate, up to maxFixes, then a final gate.
//
// The agent keeps context across prompts (warm session), and the orchestrator
// observes every tool call. Only a green gate counts.
package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/tibrezus/harmostes/internal/pijsonl"
)

// Event is re-exported so callers don't import the protocol package directly.
type Event = pijsonl.Event

// Logger receives every event (prompts, tool calls, gate outcomes) for
// observability. May be nil.
type Logger func(Event)

// PiSession is a pi --mode rpc session: one warm process that accepts a sequence
// of prompts. The loop depends on this interface so tests can inject a fake.
type PiSession interface {
	// Prompt sends a message and blocks until the agent finishes the resulting
	// turn (agent_end). Returns the agent_end event and the number of tool calls
	// observed during this turn.
	Prompt(ctx context.Context, message, label string) (agentEnd Event, toolCalls int, err error)
	// Abort terminates the session and releases the subprocess.
	Abort(ctx context.Context) error
}

// Gate validates the agent's work. green=true means acceptable; output is the
// text fed back to the agent when green is false.
type Gate interface {
	Run(ctx context.Context) (green bool, output string, err error)
}

// CmdGate runs a shell command; exit 0 = green, the combined stdout+stderr is
// the feedback on failure. A non-zero exit is a GATE failure, not a system
// error — only a failure to START the command (e.g. bad shell) is an error.
type CmdGate struct {
	Command string
	Dir     string
}

func (g CmdGate) Run(ctx context.Context) (bool, string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", g.Command)
	cmd.Dir = g.Dir
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	output := strings.TrimSpace(out.String())
	return err == nil, output, nil
}

// Result is the outcome of a Task run.
type Result struct {
	Green    bool // true iff the gate passed
	Attempts int  // number of gate evaluations performed
}

// Task runs the agent loop and returns whether the gate ever went green.
//
// Semantics (matching the proven harmostes.py):
//
//	prompt(task)
//	for attempt in 1..maxFixes:
//	    gate → green? return green
//	    if attempt == maxFixes: break
//	    prompt(feedback)          // same session
//	final gate → green? return green
//	return not green
//
// So with maxFixes=N and persistent failure there are N+1 gate evaluations and
// N prompts total (1 task + N-1 feedbacks).
func Task(ctx context.Context, sess PiSession, gate Gate, task string, maxFixes int, log Logger) (Result, error) {
	if maxFixes < 1 {
		maxFixes = 1
	}
	// turn 1 — the task itself
	if _, _, err := sess.Prompt(ctx, task, "initial task"); err != nil {
		return Result{}, err
	}
	attempts := 0
	for attempt := 1; attempt <= maxFixes; attempt++ {
		attempts = attempt
		green, out, err := gate.Run(ctx)
		if err != nil {
			return Result{Attempts: attempts}, err
		}
		if green {
			return Result{Green: true, Attempts: attempts}, nil
		}
		logf(log, Event{Type: "gate_failed", Message: fmt.Sprintf("pass %d/%d", attempt, maxFixes)})
		if attempt >= maxFixes {
			break
		}
		fb := buildFeedback(out, attempt)
		if _, _, err := sess.Prompt(ctx, fb, fmt.Sprintf("feedback #%d", attempt)); err != nil {
			return Result{Attempts: attempts}, err
		}
	}
	// final gate after the last fix
	attempts++
	green, _, err := gate.Run(ctx)
	if err != nil {
		return Result{Attempts: attempts}, err
	}
	return Result{Green: green, Attempts: attempts}, nil
}

// buildFeedback composes the message sent to the agent on a gate failure: the
// tail of the gate's output plus a generic instruction. It is intentionally
// domain-agnostic (no fork/wiki language) — the task template carries the domain.
func buildFeedback(gateOutput string, attempt int) string {
	tail := lastLines(gateOutput, 25)
	return fmt.Sprintf(`The validation gate just failed (attempt %d). Output:

%s

Fix it — you are still in the same working directory on the same branch. Adapt
your work to its target's current shape; do not drop what you intended. Then
stop; do not merge, open pull requests, or run further validation. The gate
will re-run.`, attempt, tail)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

func logf(log Logger, ev Event) {
	if log != nil {
		log(ev)
	}
}
