// session.go defines the agent session transcript types. A SessionRecord is the
// full record of one agent.Task run: every prompt sent, every tool call (with
// full args + results), every assistant response, and every gate outcome.
//
// Unlike OTel metrics (which emit sizes only — decision #4), the SessionRecord
// carries FULL content. It is stored via Dapr state (Valkey) for the UI session
// viewer, which is owner-only behind Authentik SSO. Telemetry (SigNoz) remains
// sizes-only.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SessionRecord is the full transcript of one agent.Task run.
type SessionRecord struct {
	Workflow   string       `json:"workflow"`        // workflow CR name
	RunID      string       `json:"runId"`           // job/run name
	Model      string       `json:"model,omitempty"` // LLM model used
	Skill      string       `json:"skill,omitempty"` // skill path
	StartedAt  time.Time    `json:"startedAt"`       // session start (UTC)
	EndedAt    time.Time    `json:"endedAt"`         // session end (UTC)
	Green      bool         `json:"green"`           // final gate outcome
	Turns      []TurnRecord `json:"turns"`           // one per prompt
	TotalUsage Usage        `json:"totalUsage"`      // session-wide token totals
}

// TurnRecord is one complete turn: the prompt sent, the response received,
// the tool calls made, and the gate outcome (if a gate ran for this turn).
type TurnRecord struct {
	Label    string      `json:"label"`              // "initial task", "feedback #1"
	Prompt   string      `json:"prompt"`             // the message sent to the agent
	Response string      `json:"response,omitempty"` // assistant response text
	Tools    []ToolCall  `json:"tools"`              // tool calls in execution order
	Usage    Usage       `json:"usage"`              // token usage for this turn
	Gate     *GateResult `json:"gate,omitempty"`     // gate outcome (nil if no gate ran)
}

// ToolCall is one tool execution within a turn. Full capture (Option A) —
// args and result are never truncated.
type ToolCall struct {
	Name    string         `json:"name"`
	Args    map[string]any `json:"args"`
	Success *bool          `json:"success,omitempty"` // nil if unknown
	Result  string         `json:"result"`            // full result (stringified)
	// Details is the tool result's STRUCTURED payload when it carries one
	// (rig-query's telemetry: chars/truncated/resolved/graph/sha_state/…).
	// Persisted so the #336 measurement joins on columns instead of parsing
	// the result text (#338 r26 OBS-1: the key set was asserted as "the join
	// interface" while nothing persisted it). nil for plain-string results.
	Details map[string]any `json:"details,omitempty"`
}

// GateResult is the outcome of a gate evaluation.
type GateResult struct {
	Green  bool   `json:"green"`
	Output string `json:"output"`
}

// TurnCapture is the raw content captured from one RPC.Prompt call: the
// assistant response text and the tool calls with full args + results.
type TurnCapture struct {
	Response string     `json:"response"`
	Tools    []ToolCall `json:"tools"`
}

// SessionWriter writes the current SessionRecord to a durable store (Dapr
// state). Called after each turn completes. Best-effort: errors are logged,
// not fatal — session capture must never break a workflow run.
type SessionWriter func(ctx context.Context, session SessionRecord) error

// ToolPublisher publishes a tool call event for real-time UI updates (Dapr
// pub/sub). Called per tool execution. Best-effort.
type ToolPublisher func(ctx context.Context, workflow, runID string, tool ToolCall)

// SessionMeta carries identity metadata for the session record.
type SessionMeta struct {
	Workflow string
	RunID    string
	Model    string
	Skill    string
}

// --- Event extraction helpers ---

// messageEndContent extracts the assistant response text from a message_end
// event's raw JSON. The message.content is an array of content blocks
// (TextContent | ThinkingContent | ToolCall). Only TextContent blocks
// contribute to the response text.
func messageEndContent(raw json.RawMessage) string {
	var wrapper struct {
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return ""
	}
	if wrapper.Message.Role != "assistant" {
		return ""
	}
	var parts []string
	for _, block := range wrapper.Message.Content {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// toolEndDetails extracts a structured result's `details` object, or nil for
// plain-string results (#338 r26 OBS-1). Rig-query (and any future tool that
// returns {content, details}) gets its telemetry persisted as columns.
func toolEndDetails(raw json.RawMessage) map[string]any {
	var wrapper struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil || len(wrapper.Result) == 0 {
		return nil
	}
	var obj struct {
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(wrapper.Result, &obj); err != nil {
		return nil
	}
	return obj.Details
}

// toolEndResult extracts the tool result from a tool_execution_end event's
// raw JSON. The result field is any (string | object | array). Strings are
// used directly; non-strings are JSON-serialized for readability.
// Returns the stringified result and the isError flag.
func toolEndResult(raw json.RawMessage) (result string, isError bool) {
	var wrapper struct {
		Result  json.RawMessage `json:"result"`
		IsError bool            `json:"isError"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return "", false
	}
	if len(wrapper.Result) == 0 || string(wrapper.Result) == "null" {
		return "", wrapper.IsError
	}
	// Try string first (common case: result is a plain string).
	var s string
	if err := json.Unmarshal(wrapper.Result, &s); err == nil {
		return s, wrapper.IsError
	}
	// Fall back to pretty-printed JSON for structured results.
	var v any
	if err := json.Unmarshal(wrapper.Result, &v); err != nil {
		return string(wrapper.Result), wrapper.IsError
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v), wrapper.IsError
	}
	return string(b), wrapper.IsError
}
