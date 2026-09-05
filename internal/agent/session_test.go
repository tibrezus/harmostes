package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestMessageEndContent(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "single text block",
			raw:  `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"I'll read the file."}]}}`,
			want: "I'll read the file.",
		},
		{
			name: "multiple text blocks joined with newline",
			raw:  `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"First part."},{"type":"text","text":"Second part."}]}}`,
			want: "First part.\nSecond part.",
		},
		{
			name: "text blocks mixed with thinking blocks",
			raw:  `{"type":"message_end","message":{"role":"assistant","content":[{"type":"thinking","text":"internal reasoning"},{"type":"text","text":"Visible response."}]}}`,
			want: "Visible response.",
		},
		{
			name: "non-assistant message returns empty",
			raw:  `{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
			want: "",
		},
		{
			name: "empty content array",
			raw:  `{"type":"message_end","message":{"role":"assistant","content":[]}}`,
			want: "",
		},
		{
			name: "malformed json returns empty",
			raw:  `{bad json}`,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := messageEndContent(json.RawMessage(tt.raw))
			if got != tt.want {
				t.Errorf("messageEndContent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToolEndResult(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantResult string
		wantIsErr  bool
	}{
		{
			name:       "string result",
			raw:        `{"type":"tool_execution_end","toolName":"bash","result":"hello\nworld","isError":false}`,
			wantResult: "hello\nworld",
			wantIsErr:  false,
		},
		{
			name:       "error result",
			raw:        `{"type":"tool_execution_end","toolName":"bash","result":"command failed","isError":true}`,
			wantResult: "command failed",
			wantIsErr:  true,
		},
		{
			name:       "object result pretty-printed",
			raw:        `{"type":"tool_execution_end","toolName":"read","result":{"lines":["a","b"]},"isError":false}`,
			wantResult: "{\n  \"lines\": [\n    \"a\",\n    \"b\"\n  ]\n}",
			wantIsErr:  false,
		},
		{
			name:       "null result",
			raw:        `{"type":"tool_execution_end","toolName":"write","result":null,"isError":false}`,
			wantResult: "",
			wantIsErr:  false,
		},
		{
			name:       "missing result field",
			raw:        `{"type":"tool_execution_end","toolName":"read","isError":false}`,
			wantResult: "",
			wantIsErr:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotResult, gotIsErr := toolEndResult(json.RawMessage(tt.raw))
			if gotResult != tt.wantResult {
				t.Errorf("toolEndResult() result = %q, want %q", gotResult, tt.wantResult)
			}
			if gotIsErr != tt.wantIsErr {
				t.Errorf("toolEndResult() isError = %v, want %v", gotIsErr, tt.wantIsErr)
			}
		})
	}
}

// captureSession is a PiSession fake that returns canned TurnCaptures.
type captureSession struct {
	captures []TurnCapture
	idx      int
}

func (f *captureSession) Prompt(_ context.Context, _, _ string) (Event, int, Usage, TurnCapture, error) {
	c := TurnCapture{}
	if f.idx < len(f.captures) {
		c = f.captures[f.idx]
	}
	f.idx++
	return Event{Type: "agent_end"}, len(c.Tools), Usage{}, c, nil
}

func (f *captureSession) Abort(_ context.Context) error { return nil }

// scriptedCaptureGate returns a fixed sequence of (green, output) per Run() call.
type scriptedCaptureGate struct {
	results []struct {
		green  bool
		output string
	}
	idx int
}

func (g *scriptedCaptureGate) Run(_ context.Context) (bool, string, error) {
	r := struct {
		green  bool
		output string
	}{false, "default"}
	if g.idx < len(g.results) {
		r = g.results[g.idx]
	}
	g.idx++
	return r.green, r.output, nil
}

func TestTaskAccumulatesSession(t *testing.T) {
	// Session with one task turn + one feedback turn, gate passes on attempt 2.
	sess := &captureSession{
		captures: []TurnCapture{
			{
				Response: "I'll do the task.",
				Tools: []ToolCall{
					{Name: "read", Args: map[string]any{"path": "file.txt"}, Success: boolPtr(true), Result: "contents"},
				},
			},
			{
				Response: "I fixed it.",
				Tools: []ToolCall{
					{Name: "edit", Args: map[string]any{"path": "file.txt"}, Success: boolPtr(true), Result: "ok"},
				},
			},
		},
	}
	gate := &scriptedCaptureGate{
		results: []struct {
			green  bool
			output string
		}{
			{false, "lint error: line 42"},
			{true, ""},
		},
	}

	var writtenSessions []SessionRecord
	writer := func(_ context.Context, s SessionRecord) error {
		writtenSessions = append(writtenSessions, s)
		return nil
	}

	var publishedTools []ToolCall
	publisher := func(_ context.Context, _, _ string, tool ToolCall) {
		publishedTools = append(publishedTools, tool)
	}

	result, err := Task(context.Background(), sess, gate, "do the task", 2, nil,
		WithSessionWriter(writer),
		WithToolPublisher(publisher),
		WithSessionMeta(SessionMeta{Workflow: "test-wf", RunID: "job-1", Model: "test-model", Skill: "/skills/test"}),
	)
	if err != nil {
		t.Fatalf("Task error: %v", err)
	}
	if !result.Green {
		t.Error("expected green result")
	}

	// Verify session record
	s := result.Session
	if s.Workflow != "test-wf" {
		t.Errorf("session.Workflow = %q, want test-wf", s.Workflow)
	}
	if s.RunID != "job-1" {
		t.Errorf("session.RunID = %q, want job-1", s.RunID)
	}
	if s.Model != "test-model" {
		t.Errorf("session.Model = %q, want test-model", s.Model)
	}
	if len(s.Turns) != 2 {
		t.Fatalf("len(session.Turns) = %d, want 2", len(s.Turns))
	}

	// Turn 1: initial task
	t1 := s.Turns[0]
	if t1.Label != "initial task" {
		t.Errorf("turn 1 label = %q, want 'initial task'", t1.Label)
	}
	if t1.Response != "I'll do the task." {
		t.Errorf("turn 1 response = %q", t1.Response)
	}
	if len(t1.Tools) != 1 || t1.Tools[0].Name != "read" {
		t.Errorf("turn 1 tools = %+v", t1.Tools)
	}
	if t1.Gate == nil || t1.Gate.Green {
		t.Error("turn 1 gate should be failed")
	}
	if t1.Gate.Output != "lint error: line 42" {
		t.Errorf("turn 1 gate output = %q", t1.Gate.Output)
	}

	// Turn 2: feedback #1
	t2 := s.Turns[1]
	if t2.Label != "feedback #1" {
		t.Errorf("turn 2 label = %q, want 'feedback #1'", t2.Label)
	}
	if t2.Response != "I fixed it." {
		t.Errorf("turn 2 response = %q", t2.Response)
	}
	if t2.Gate == nil || !t2.Gate.Green {
		t.Error("turn 2 gate should be green")
	}

	// Session timestamps
	if s.StartedAt.IsZero() || s.EndedAt.IsZero() {
		t.Error("session timestamps should be set")
	}
	if !s.EndedAt.After(s.StartedAt) && !s.EndedAt.Equal(s.StartedAt) {
		t.Error("EndedAt should be >= StartedAt")
	}

	// Session writer should have been called at least once (after gate evaluations)
	if len(writtenSessions) == 0 {
		t.Error("session writer was never called")
	}

	// Tool publisher should have been called for both tools
	if len(publishedTools) != 2 {
		t.Fatalf("published %d tools, want 2", len(publishedTools))
	}
	if publishedTools[0].Name != "read" || publishedTools[1].Name != "edit" {
		t.Errorf("published tools = %+v", publishedTools)
	}
}

func TestSessionRecordJSONRoundTrip(t *testing.T) {
	original := SessionRecord{
		Workflow:  "test-wf",
		RunID:     "job-1",
		Model:     "litellm/zai/glm-4.7",
		Skill:     "/skills/wiki/SKILL.md",
		StartedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		EndedAt:   time.Date(2026, 1, 1, 12, 5, 0, 0, time.UTC),
		Green:     true,
		Turns: []TurnRecord{
			{
				Label:    "initial task",
				Prompt:   "Update the docs",
				Response: "Done.",
				Tools: []ToolCall{
					{Name: "read", Args: map[string]any{"path": "README.md"}, Success: boolPtr(true), Result: "file contents"},
				},
				Gate: &GateResult{Green: true, Output: ""},
			},
		},
		TotalUsage: Usage{Input: 100, Output: 50, Cost: 0.01},
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded SessionRecord
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.Workflow != original.Workflow {
		t.Errorf("workflow mismatch: %q != %q", decoded.Workflow, original.Workflow)
	}
	if len(decoded.Turns) != 1 {
		t.Fatalf("turns mismatch: %d != 1", len(decoded.Turns))
	}
	if decoded.Turns[0].Tools[0].Name != "read" {
		t.Errorf("tool name mismatch: %q", decoded.Turns[0].Tools[0].Name)
	}
	if decoded.Turns[0].Gate == nil || !decoded.Turns[0].Gate.Green {
		t.Error("gate mismatch")
	}
}

func boolPtr(b bool) *bool { return &b }

// TestToolEndDetails pins the structured-telemetry persistence (#338 r26
// OBS-1): rig-query results carry {content, details}; the details object is
// the #336 join interface — without this capture it existed only in the
// pretty-printed result text.
func TestToolEndDetails(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]any
	}{
		{
			"rig-shaped result",
			`{"type":"tool_execution_end","result":{"content":[{"type":"text","text":"overview…"}],"details":{"command":"overview","chars":2689,"truncated":false,"resolved":true,"graph":true,"sha_state":"verified"}}}`,
			map[string]any{"command": "overview", "chars": 2689.0, "truncated": false, "resolved": true, "graph": true, "sha_state": "verified"},
		},
		{"plain string result", `{"result":"just text"}`, nil},
		{"no details key", `{"result":{"content":[]}}`, nil},
		{"empty result", `{"result":null}`, nil},
		{"garbage", `not json`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := toolEndDetails(json.RawMessage(c.raw))
			if c.want == nil {
				// r27 P8: assert nil EXPLICITLY — a length comparison alone
				// reads as vacuous for the empty-want cases.
				if got != nil {
					t.Fatalf("toolEndDetails() = %#v, want nil", got)
				}
				return
			}
			if len(got) != len(c.want) {
				t.Fatalf("toolEndDetails() = %#v, want %#v", got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Fatalf("toolEndDetails()[%q] = %#v, want %#v", k, got[k], v)
				}
			}
		})
	}
}
