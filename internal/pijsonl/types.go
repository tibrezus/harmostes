// Package pijsonl defines the pi.dev RPC JSONL protocol types used by harmostes.
//
// pi (https://pi.dev) exposes an RPC mode (`pi --mode rpc`) that speaks
// line-delimited JSON over stdin/stdout: the client sends commands, pi streams
// back events. harmostes uses a tiny slice of that protocol:
//
//   - send {"type":"prompt","message":...}   → the agent does work
//   - receive {"type":"tool_execution_start",...} (observability)
//   - receive {"type":"agent_end"}           → the agent finished this turn
//   - send {"type":"abort"}                  → terminate the session
//
// Multiple prompts to one process form a warm session (the agent retains context
// across them) — that is what makes feedback a continuation rather than a cold
// restart.
package pijsonl

// Command "type" values (outgoing).
const (
	CmdPrompt = "prompt"
	CmdAbort  = "abort"
)

// Event "type" values (incoming). Only the slice harmostes cares about is named;
// every other event type still parses (into Event.Type) and is forwarded to the
// logger.
const (
	EvToolStart = "tool_execution_start"
	EvToolEnd   = "tool_execution_end"
	EvAgentEnd  = "agent_end"
	EvResponse  = "response"
)

// Prompt sends a message to the agent. Reused for both the initial task and each
// feedback turn (same process = warm session).
type Prompt struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Abort terminates the pi session.
type Abort struct {
	Type string `json:"type"`
}

// Event is one incoming JSONL event from pi. Unknown fields are ignored; the
// logger still sees them via the Raw line captured by the RPC reader.
type Event struct {
	Type     string         `json:"type"`
	ToolName string         `json:"toolName,omitempty"`
	Args     map[string]any `json:"args,omitempty"`
	Message  string         `json:"message,omitempty"`
	Success  *bool          `json:"success,omitempty"`
}
