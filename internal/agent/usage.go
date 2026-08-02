// usage.go holds token-usage tracking for the agent loop. pi emits a
// "message_end" event for each LLM response carrying message.usage with
// input/output/cacheRead/cacheWrite token counts and cost. This file
// extracts, accumulates, and records those as OTel metrics + structured log
// output so every workflow run reports its token consumption.
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/tibrezus/harmostes/internal/observability"
)

// Usage holds token counts + cost for one or more LLM responses.
type Usage struct {
	Input      int     `json:"input_tokens"`
	Output     int     `json:"output_tokens"`
	CacheRead  int     `json:"cache_read_tokens"`
	CacheWrite int     `json:"cache_write_tokens"`
	Cost       float64 `json:"cost"`
}

// add accumulates another Usage into this one.
func (u *Usage) add(other Usage) {
	u.Input += other.Input
	u.Output += other.Output
	u.CacheRead += other.CacheRead
	u.CacheWrite += other.CacheWrite
	u.Cost += other.Cost
}

// Total returns the sum of all token categories.
func (u Usage) Total() int {
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}

// String returns a human-readable summary for log output.
func (u Usage) String() string {
	return fmt.Sprintf("%d in / %d out / %d cache / $%.4f",
		u.Input, u.Output, u.CacheRead+u.CacheWrite, u.Cost)
}

// messageEndUsage extracts token usage from a pi "message_end" event's raw
// JSON line. Returns ok=false if the message isn't an assistant message or
// carries no usage data.
func messageEndUsage(raw json.RawMessage) (Usage, bool) {
	var wrapper struct {
		Message struct {
			Role  string `json:"role"`
			Usage struct {
				Input      int `json:"input"`
				Output     int `json:"output"`
				CacheRead  int `json:"cacheRead"`
				CacheWrite int `json:"cacheWrite"`
				Cost       struct {
					Total float64 `json:"total"`
				} `json:"cost"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return Usage{}, false
	}
	if wrapper.Message.Role != "assistant" {
		return Usage{}, false
	}
	u := Usage{
		Input:      wrapper.Message.Usage.Input,
		Output:     wrapper.Message.Usage.Output,
		CacheRead:  wrapper.Message.Usage.CacheRead,
		CacheWrite: wrapper.Message.Usage.CacheWrite,
		Cost:       wrapper.Message.Usage.Cost.Total,
	}
	if u.Input == 0 && u.Output == 0 {
		return Usage{}, false
	}
	return u, true
}

// recordTokens emits OTel counters for input + output tokens consumed by the
// agent. Called once per Task (session-wide totals), not per turn.
func recordTokens(ctx context.Context, workflow string, usage Usage) {
	if usage.Input > 0 {
		c, _ := observability.Meter().Int64Counter("harmostes_agent_input_tokens_total",
			metric.WithDescription("Total input tokens consumed by the agent (prompt + context)."))
		c.Add(ctx, int64(usage.Input), metric.WithAttributes(attribute.String("workflow", workflow)))
	}
	if usage.Output > 0 {
		c, _ := observability.Meter().Int64Counter("harmostes_agent_output_tokens_total",
			metric.WithDescription("Total output tokens produced by the agent (completions)."))
		c.Add(ctx, int64(usage.Output), metric.WithAttributes(attribute.String("workflow", workflow)))
	}
	if usage.Cost > 0 {
		c, _ := observability.Meter().Float64Counter("harmostes_agent_cost_total",
			metric.WithDescription("Total estimated cost (USD) consumed by the agent."))
		c.Add(ctx, usage.Cost, metric.WithAttributes(attribute.String("workflow", workflow)))
	}
}
