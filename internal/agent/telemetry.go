// telemetry.go holds the LLM-ops metrics + size-only privacy helpers for the
// agent loop (Phase 3). Instruments are fetched from the global meter on each
// record so a test that sets a meter provider (manual reader) sees them; agent
// runs are rare, so the per-call fetch is negligible. With telemetry disabled
// (no Init) the meter is no-op and these are free.
package agent

import (
	"context"
	"encoding/json"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/tibrezus/harmostes/internal/observability"
)

// recordTurn increments harmostes_agent_turns_total{workflow} (one per prompt:
// the initial task + each feedback turn).
func recordTurn(ctx context.Context, workflow string) {
	c, _ := observability.Meter().Int64Counter("harmostes_agent_turns_total",
		metric.WithDescription("Agent prompts sent (task + feedback turns)."))
	c.Add(ctx, 1, metric.WithAttributes(attribute.String("workflow", workflow)))
}

// recordToolCall increments harmostes_agent_tool_calls_total{workflow,tool}.
func recordToolCall(ctx context.Context, workflow, tool string) {
	if tool == "" {
		tool = "unknown"
	}
	c, _ := observability.Meter().Int64Counter("harmostes_agent_tool_calls_total",
		metric.WithDescription("Agent tool calls invoked."))
	c.Add(ctx, 1, metric.WithAttributes(
		attribute.String("workflow", workflow),
		attribute.String("tool", tool)))
}

// recordGateAttempts records the attempts-to-green distribution (the fix-loop
// cost) on the histogram harmostes_gate_attempts{workflow}.
func recordGateAttempts(ctx context.Context, workflow string, attempts int) {
	h, _ := observability.Meter().Int64Histogram("harmostes_gate_attempts",
		metric.WithDescription("Gate evaluations until green (fix-loop cost)."))
	h.Record(ctx, int64(attempts), metric.WithAttributes(attribute.String("workflow", workflow)))
}

// recordAgentSeconds records wall-clock time spent in the agent loop per
// workflow (harmostes_agent_seconds{workflow}).
func recordAgentSeconds(ctx context.Context, workflow string, d time.Duration) {
	h, _ := observability.Meter().Float64Histogram("harmostes_agent_seconds",
		metric.WithDescription("Agent loop wall-clock duration."))
	h.Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String("workflow", workflow)))
}

// argsChars returns the JSON-serialised size of a tool call's arguments — a SIZE
// ONLY, never the body (decision #4: tool args can hold file contents or
// secrets). A nil/empty/unserialisable map reports 0.
func argsChars(args map[string]any) int {
	if len(args) == 0 {
		return 0
	}
	b, err := json.Marshal(args)
	if err != nil {
		return 0
	}
	return len(b)
}
