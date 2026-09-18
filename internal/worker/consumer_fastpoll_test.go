package worker

// C3 follow-up — the leg-2 fast path: while a review-ready workflow holds
// an ARMED, NEVER-DISPATCHED claim, the consumer fires synthetic schedule
// triggers at the FastPoll cadence, so CI-green is detected within one
// tick instead of on the next cron sweep (owner requirement: the workflow
// starts instantly when the conditions are met).

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFastPollFiresForArmedWaitingWorkflows(t *testing.T) {
	var runs atomic.Int32
	cfg := ConsumerConfig{
		FastPoll:  10 * time.Millisecond,
		Namespace: "default",
		RunFunc: func(_ context.Context, req RunRequest) error {
			if req.Workflow == "pr-review-rhesadox" {
				runs.Add(1)
			}
			return nil
		},
		ArmedWaitingWorkflows: func(_ context.Context) []string { return []string{"pr-review-rhesadox"} },
	}
	c := NewConsumer(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	c.fastPollLoop(ctx)
	if runs.Load() == 0 {
		t.Fatalf("the fast poll must fire for an armed-waiting workflow")
	}
}

func TestFastPollIdleWithoutArmedClaims(t *testing.T) {
	var runs atomic.Int32
	cfg := ConsumerConfig{
		FastPoll:  10 * time.Millisecond,
		Namespace: "default",
		RunFunc: func(_ context.Context, _ RunRequest) error {
			runs.Add(1)
			return nil
		},
		ArmedWaitingWorkflows: func(_ context.Context) []string { return nil },
	}
	c := NewConsumer(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	c.fastPollLoop(ctx)
	if runs.Load() != 0 {
		t.Fatalf("no armed-waiting claims → no synthetic runs, got %d", runs.Load())
	}
}

func TestFastPollRequestShape(t *testing.T) {
	var got RunRequest
	done := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	cfg := ConsumerConfig{
		FastPoll:  5 * time.Millisecond,
		Namespace: "harmostes",
		RunFunc: func(_ context.Context, req RunRequest) error {
			got = req
			once.Do(func() { close(done); cancel() })
			return nil
		},
		ArmedWaitingWorkflows: func(_ context.Context) []string { return []string{"wf-x"} },
	}
	c := NewConsumer(cfg)
	go c.fastPollLoop(ctx)
	<-done
	if got.Workflow != "wf-x" || got.Namespace != "harmostes" || got.Source != "fast-poll" {
		t.Fatalf("fast-poll request shape wrong: %+v", got)
	}
}
