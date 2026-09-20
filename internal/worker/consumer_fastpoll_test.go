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
	c.armedBackoffLoop(ctx)
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
	c.armedBackoffLoop(ctx)
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
	go c.armedBackoffLoop(ctx)
	<-done
	if got.Workflow != "wf-x" || got.Namespace != "harmostes" || got.Source != "fast-poll" {
		t.Fatalf("fast-poll request shape wrong: %+v", got)
	}
}

// #556: the loop is the kernel's reconciliation floor, not a standing poll.
// A stalled armed set (sweeps converging to no-ops) must back off
// exponentially; without the doubling the pass count in a fixed window is
// ~an order of magnitude higher. Generous margins — this asserts the SHAPE
// (grows), not exact tick math.
func TestBackoffBacksOffWhileStalled(t *testing.T) {
	var runs atomic.Int32
	cfg := ConsumerConfig{
		FastPoll:  10 * time.Millisecond,
		Namespace: "default",
		RunFunc: func(_ context.Context, _ RunRequest) error {
			runs.Add(1)
			return nil
		},
		ArmedWaitingWorkflows: func(_ context.Context) []string { return []string{"wf-x"} },
	}
	c := NewConsumer(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c.armedBackoffLoop(ctx)
	// Doubling: passes land at ~0, 10, 30, 70, 150ms → ≤ 6 in 200ms.
	// A loop that forgot to double fires ~20 times.
	if n := runs.Load(); n > 6 {
		t.Fatalf("stalled armed set must back off exponentially — got %d passes in 200ms (no doubling?)", n)
	}
}

func TestBackoffResetsToBaseOnProgress(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	var passes []time.Time // start time of each sweep pass
	var sizes []int        // armed-set size seen by that pass
	cfg := ConsumerConfig{
		FastPoll:  10 * time.Millisecond,
		Namespace: "default",
		RunFunc: func(_ context.Context, _ RunRequest) error {
			return nil
		},
		ArmedWaitingWorkflows: func(_ context.Context) []string {
			mu.Lock()
			defer mu.Unlock()
			passes = append(passes, time.Now())
			n := 1
			if calls.Add(1) <= 2 {
				n = 2 // two stalled passes at 2 claims, then a dispatch lands
			}
			sizes = append(sizes, n)
			// Same slice contents the pass iterates over.
			if n == 2 {
				return []string{"wf-a", "wf-b"}
			}
			return []string{"wf-a"}
		},
	}
	c := NewConsumer(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	c.armedBackoffLoop(ctx)
	mu.Lock()
	defer mu.Unlock()
	// Find the shrink transition. The movement is only OBSERVABLE at the
	// first pass that sees the smaller set (the wait before it was armed
	// by the previous pass's stalled decision — a ≥1-interval lag is
	// inherent to polling; the 30m cap bounds it). The reset's promise is
	// about what happens AFTER the observation: the next wait is base
	// cadence (< 15ms at base 10ms), not a doubled delay. A loop that
	// kept doubling across the movement yields ≥ 20ms here.
	shrink := -1
	for i := 1; i < len(sizes); i++ {
		if sizes[i] < sizes[i-1] {
			shrink = i
			break
		}
	}
	if shrink < 1 || shrink+1 >= len(passes) {
		t.Fatalf("shrink transition not bracketed — %d passes, sizes %v", len(sizes), sizes)
	}
	gap := passes[shrink+1].Sub(passes[shrink])
	if gap >= 15*time.Millisecond {
		t.Fatalf("the wait after observing the shrink took %v — an observed movement must reset the cadence to base", gap)
	}
}
