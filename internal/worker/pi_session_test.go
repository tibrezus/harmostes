package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tibrezus/harmostes/internal/agent"
	"github.com/tibrezus/harmostes/internal/dapr"
)

type recordingDapr struct {
	dapr.Client
	saved map[string]string
}

func (r *recordingDapr) SaveState(_ context.Context, _, key, value string) error {
	if r.saved == nil {
		r.saved = map[string]string{}
	}
	r.saved[key] = value
	return nil
}

func TestSavePiSessionRoundTripAndRedaction(t *testing.T) {
	dir := t.TempDir()
	// oldest + newest: SavePiSession must pick the newest.
	old := filepath.Join(dir, "2026-08-25T10-00-00Z_old.jsonl")
	newest := filepath.Join(dir, "2026-08-25T11-00-00Z_new.jsonl")
	secret := `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"fetched https://alice:hunter2@git.example/repo.git ok"}]}}`
	if err := os.WriteFile(old, []byte(`{"old":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newest, []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}

	dc := &recordingDapr{}
	if err := SavePiSession(context.Background(), dc, "statestore", "pr-review-x", "run-1", []string{old, newest}); err != nil {
		t.Fatal(err)
	}
	// Metadata key: tiny JSON, the O(1) availability probe's target.
	metaRaw, ok := dc.saved["pr-review-x:run-1:pi-session"]
	if !ok {
		t.Fatal("metadata key not saved")
	}
	var meta PiSessionMeta
	if err := json.Unmarshal([]byte(metaRaw), &meta); err != nil {
		t.Fatalf("metadata not JSON: %v", err)
	}
	if meta.Bytes != len(secret) {
		t.Errorf("meta.Bytes = %d, want %d", meta.Bytes, len(secret))
	}
	// Data key: the gz1 blob stored as a JSON string, never probed by
	// availability checks.
	payloadJSON, ok := dc.saved["pr-review-x:run-1:pi-session/data"]
	if !ok {
		t.Fatal("data key not saved")
	}
	var payload string
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		t.Fatalf("data value is not a JSON string (UI read path would 404): %v", err)
	}
	if !strings.HasPrefix(payload, agent.PiSessionMarker) {
		t.Fatalf("payload missing %s marker", agent.PiSessionMarker)
	}
	raw, err := agent.LoadPiSession(payload)
	if err != nil {
		t.Fatal(err)
	}
	// newest file won, and the #115 leak class is redacted
	if strings.Contains(string(raw), "alice:hunter2@") {
		t.Error("credentials survived redaction")
	}
	if !strings.Contains(string(raw), "https://git.example/repo.git") {
		t.Error("redaction mangled the URL host")
	}
	if _, err := os.Stat(newest); !os.IsNotExist(err) {
		t.Error("uploaded session file not removed")
	}
	if _, err := os.Stat(old); err != nil {
		t.Error("non-selected file must be left alone (it belongs to another run)")
	}
}

func TestLoadPiSessionRejectsUnknownMarker(t *testing.T) {
	if _, err := agent.LoadPiSession("raw:AAAA"); err == nil {
		t.Fatal("unknown marker must error, not misread")
	}
	if _, err := agent.LoadPiSession("gz1:not-base64!!"); err == nil {
		t.Fatal("invalid base64 must error")
	}
}

func TestSavePiSessionCapsSize(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "2026-08-25T11-00-00Z_big.jsonl")
	if err := os.WriteFile(big, make([]byte, maxPiSession+1), 0o644); err != nil {
		t.Fatal(err)
	}
	dc := &recordingDapr{}
	if err := SavePiSession(context.Background(), dc, "statestore", "wf", "run", []string{big}); err == nil {
		t.Fatal("oversized session must be refused")
	}
	if len(dc.saved) != 0 {
		t.Fatal("nothing should be saved for an oversized session")
	}
}

// Run wiring composition (#244 r2): RPCAgentRunner.Run must pass the files pi
// wrote to the SessionFiles hook, after pi has exited (flush-at-exit). The
// fake pi speaks just enough JSONL: it answers a prompt with agent_end and
// writes the session file pi would.
func TestRPCAgentRunnerSessionFilesWiring(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "sessions")
	_ = os.MkdirAll(root, 0o700)

	fake := filepath.Join(dir, "fake-pi")
	script := `#!/bin/sh
# write the session file pi would (any --session-dir arg wins)
prev=""
for a in "$@"; do
  if [ "$prev" = "--session-dir" ]; then echo '{"fake":"session"}' > "$a/pi.jsonl"; fi
  prev="$a"
done
# answer prompts with agent_end until stdin closes
while IFS= read -r line; do
  case "$line" in *prompt*) printf '%s\n' '{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}'
  esac
done
exit 0
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	greenGate := &fixedGate{green: true}
	var hooked []string
	hookDone := make(chan struct{})
	r := RPCAgentRunner{
		Opts: agent.RPCOptions{PiPath: fake, SessionRoot: root},
		SessionFiles: func(_ context.Context, files []string) {
			hooked = files
			close(hookDone)
		},
	}
	go func() {
		_, _ = r.Run(context.Background(), "do thing", greenGate, 1, nil)
	}()
	select {
	case <-hookDone:
	case <-time.After(10 * time.Second):
		t.Fatal("SessionFiles hook never fired")
	}
	if len(hooked) == 0 {
		t.Fatal("hook received no files — pi session never persisted")
	}
	raw, err := os.ReadFile(hooked[len(hooked)-1])
	if err != nil {
		t.Fatalf("hooked file unreadable: %v", err)
	}
	if !strings.Contains(string(raw), "fake") {
		t.Fatalf("hooked file is not the fake session: %s", raw)
	}
}

// fixedGate is an agent.Gate with a fixed outcome.
type fixedGate struct{ green bool }

func (g *fixedGate) Run(context.Context) (bool, string, error) {
	return g.green, "ok", nil
}
