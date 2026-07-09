package agent

import (
	"context"
	"strings"
	"testing"
)

// fakeSession records the prompts it receives and optionally reports tool calls.
type fakeSession struct {
	prompts   []string
	toolCalls []int // per-prompt tool-call count (cycled if short)
	aborted   bool
	idx       int
}

func (f *fakeSession) Prompt(_ context.Context, message, _ string) (Event, int, error) {
	f.prompts = append(f.prompts, message)
	tools := 0
	if f.idx < len(f.toolCalls) {
		tools = f.toolCalls[f.idx]
	}
	f.idx++
	return Event{Type: "agent_end"}, tools, nil
}

func (f *fakeSession) Abort(_ context.Context) error { f.aborted = true; return nil }

// scriptedGate returns a fixed sequence of (green, output) per Run() call.
type scriptedGate struct {
	greens  []bool
	outputs []string
	n       int
}

func (g *scriptedGate) Run(_ context.Context) (bool, string, error) {
	green := false
	out := ""
	if g.n < len(g.greens) {
		green = g.greens[g.n]
	}
	if g.n < len(g.outputs) {
		out = g.outputs[g.n]
	}
	g.n++
	return green, out, nil
}

func TestGreenOnFirstPass(t *testing.T) {
	sess := &fakeSession{}
	gate := &scriptedGate{greens: []bool{true}}
	res, err := Task(context.Background(), sess, gate, "do thing", 3, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Green {
		t.Fatal("expected green")
	}
	if res.Attempts != 1 {
		t.Fatalf("expected 1 attempt, got %d", res.Attempts)
	}
	if len(sess.prompts) != 1 {
		t.Fatalf("expected 1 prompt (the task only), got %d", len(sess.prompts))
	}
	// Task does not own Abort; the caller (CLI/worker) does. So we do not assert it here.
}

func TestGreenAfterOneFeedback(t *testing.T) {
	sess := &fakeSession{}
	gate := &scriptedGate{
		greens:  []bool{false, true}, // fail the task gate, pass after one feedback
		outputs: []string{"lint error: MD001", ""},
	}
	res, err := Task(context.Background(), sess, gate, "do thing", 3, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Green {
		t.Fatal("expected green")
	}
	if res.Attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", res.Attempts)
	}
	if len(sess.prompts) != 2 {
		t.Fatalf("expected 2 prompts (task + feedback#1), got %d", len(sess.prompts))
	}
	// the feedback must carry the gate's output
	if !strings.Contains(sess.prompts[1], "lint error: MD001") {
		t.Fatalf("feedback prompt must include gate output; got:\n%s", sess.prompts[1])
	}
}

// TestExhaustedThenFinal: with maxFixes=3, persistent failure yields exactly
// 3 prompts (task + 2 feedbacks) and 4 gate evaluations (3 in-loop + 1 final).
// This matches the proven harmostes.py semantics.
func TestExhaustedThenFinal(t *testing.T) {
	for _, maxFixes := range []int{1, 2, 3, 5} {
		sess := &fakeSession{}
		// every gate fails
		gate := &scriptedGate{}
		res, err := Task(context.Background(), sess, gate, "do thing", maxFixes, nil)
		if err != nil {
			t.Fatalf("maxFixes=%d: unexpected error: %v", maxFixes, err)
		}
		if res.Green {
			t.Fatalf("maxFixes=%d: expected not green", maxFixes)
		}
		wantPrompts := maxFixes // task + (maxFixes-1) feedbacks
		if len(sess.prompts) != wantPrompts {
			t.Fatalf("maxFixes=%d: expected %d prompts, got %d (%v)", maxFixes, wantPrompts, len(sess.prompts), sess.prompts)
		}
		wantGates := maxFixes + 1 // in-loop + final
		if gate.n != wantGates {
			t.Fatalf("maxFixes=%d: expected %d gate runs, got %d", maxFixes, wantGates, gate.n)
		}
		if res.Attempts != wantGates {
			t.Fatalf("maxFixes=%d: expected %d attempts, got %d", maxFixes, wantGates, res.Attempts)
		}
	}
}

func TestFeedbackContainsTailAndIsGeneric(t *testing.T) {
	sess := &fakeSession{}
	long := strings.Repeat("line\n", 40) // 40 lines → feedback keeps last 25
	gate := &scriptedGate{greens: []bool{false, true}, outputs: []string{long, ""}}
	if _, err := Task(context.Background(), sess, gate, "do thing", 3, nil); err != nil {
		t.Fatal(err)
	}
	fb := sess.prompts[1]
	// tail kept (last 25 lines)
	lines := strings.Count(fb, "line")
	if lines != 25 {
		t.Fatalf("expected feedback to keep 25 lines, got %d", lines)
	}
	// generic — no fork-specific language leaked from the proven Python version
	for _, bad := range []string{"upstream", "customization", "fork"} {
		if strings.Contains(strings.ToLower(fb), bad) {
			t.Fatalf("feedback must be domain-agnostic; contains %q", bad)
		}
	}
}

func TestCmdGateGreenAndFail(t *testing.T) {
	dir := t.TempDir()
	g := CmdGate{Command: "true", Dir: dir}
	if green, _, err := g.Run(context.Background()); err != nil || !green {
		t.Fatalf("exit 0 must be green (err=%v green=%v)", err, green)
	}
	g = CmdGate{Command: "echo 'boom on stderr' 1>&2; exit 3", Dir: dir}
	green, out, err := g.Run(context.Background())
	if err != nil {
		t.Fatalf("non-zero exit is a gate failure, not a system error: %v", err)
	}
	if green {
		t.Fatal("non-zero exit must not be green")
	}
	if !strings.Contains(out, "boom on stderr") {
		t.Fatalf("gate output must capture stderr: %q", out)
	}
}
