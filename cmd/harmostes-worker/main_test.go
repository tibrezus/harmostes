package main

import (
	"context"
	"testing"
	"time"

	"github.com/tibrezus/harmostes/internal/observability"
)

// TestFlushTelemetryCallsShutdown: the worker's exit path flushes telemetry —
// the Phase 1 guarantee that an ephemeral Job (os.Exit in 3 places) doesn't drop
// spans/metrics. finish() calls flushTelemetry() before os.Exit; this asserts the
// flush actually invokes the configured shutdown.
func TestFlushTelemetryCallsShutdown(t *testing.T) {
	called := make(chan struct{}, 1)
	prev := obsShutdown
	t.Cleanup(func() { obsShutdown = prev })
	obsShutdown = func(context.Context) error { called <- struct{}{}; return nil }

	flushTelemetry()

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("flushTelemetry did not call obsShutdown — telemetry would be dropped on exit")
	}
}

// TestFlushTelemetryNilSafe: a disabled or failed Init (nil shutdown) must not
// panic — local dev / unit runs have no collector.
func TestFlushTelemetryNilSafe(t *testing.T) {
	prev := obsShutdown
	t.Cleanup(func() { obsShutdown = prev })
	obsShutdown = nil
	flushTelemetry()
}

// TestShutdownTimeoutBounded: the flush is time-bounded so a stuck exporter
// can't hang process termination.
func TestShutdownTimeoutBounded(t *testing.T) {
	if observability.ShutdownTimeout <= 0 || observability.ShutdownTimeout > 30*time.Second {
		t.Fatalf("ShutdownTimeout=%v is not a sane flush bound", observability.ShutdownTimeout)
	}
}
