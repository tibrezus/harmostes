package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	payload, ok := dc.saved["pr-review-x:run-1:pi-session"]
	if !ok {
		t.Fatal("pi-session key not saved")
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
