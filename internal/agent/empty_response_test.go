package agent

import (
	"context"
	"strings"
	"testing"
)

// emptySession is the #504 incident shape: every model call "succeeds"
// instantly with an empty response and ZERO tokens (the LiteLLM key-access
// 403 / unhealthy-upstream signature).
type emptySession struct{ calls int }

func (s *emptySession) Prompt(ctx context.Context, message, label string) (Event, int, Usage, TurnCapture, error) {
	s.calls++
	return Event{Type: "agent_end"}, 0, Usage{}, TurnCapture{AssistantMessageEnd: true}, nil
}
func (s *emptySession) Abort(ctx context.Context) error { return nil }

type passGate struct{}

func (passGate) Run(ctx context.Context) (bool, string, error) { return true, "ok", nil }

// The #504 guard: empty + zero-token turns fail LOUDLY, naming the model —
// never feed empty text to the gate loop and burn silent retries.
func TestTask_EmptyCompletionFailsLoudly(t *testing.T) {
	sess := &emptySession{}
	_, err := Task(context.Background(), sess, passGate{}, "do the task", 3, func(Event) {}, WithSessionMeta(SessionMeta{
		Workflow: "pr-review-rhesadox",
		RunID:    "run-1",
		Model:    "litellm/ali/anthropic/deepseek-v4.1-flash",
	}))
	if err == nil {
		t.Fatal("empty-completion run must fail loudly, got nil")
	}
	for _, want := range []string{"EMPTY completion", "litellm/ali/anthropic/deepseek-v4.1-flash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	if sess.calls != 1 { // MUTANT: accept empty silently
		t.Errorf("calls = %d, want 1 — the guard fires on the FIRST empty turn, no silent retries", sess.calls)
	}
}
