package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The spawn contract for native session persistence (#243): session root set
// → pi gets a per-run --session-dir and a unique --session-id, and NEVER
// --no-session (which would discard the conversation). The fake pi records
// its argv and writes the session file pi would, proving SessionFiles()
// finds it.
func TestRPCSessionPersistenceArgs(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	argsOut := filepath.Join(dir, "argv")

	fake := filepath.Join(dir, "fake-pi")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsOut + "\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"--session-dir\" ]; then echo '{\"fake\":1}' > \"$2/session.jsonl\"; fi\n" +
		"  shift\ndone\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	rpc, err := NewRPC(context.Background(), RPCOptions{PiPath: fake, SessionRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Abort(context.Background())
	// give the fake a moment to write argv
	var argv []byte
	for i := 0; i < 100; i++ {
		argv, _ = os.ReadFile(argsOut)
		if len(argv) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	args := string(argv)
	if args == "" {
		t.Fatal("fake pi argv not recorded")
	}
	if strings.Contains(args, "--no-session") {
		t.Error("--no-session must never be passed when persisting sessions")
	}
	if !strings.Contains(args, "--session-dir") {
		t.Error("--session-dir missing from spawn args")
	}
	if !strings.Contains(args, "--session-id") {
		t.Error("--session-id missing from spawn args")
	}
	files := rpc.SessionFiles()
	if len(files) != 1 || !strings.HasSuffix(files[0], "session.jsonl") {
		t.Errorf("SessionFiles() = %v, want the file the fake wrote", files)
	}

	// SessionRoot empty → no session flags, SessionFiles nil.
	rpc2, err := NewRPC(context.Background(), RPCOptions{PiPath: fake})
	if err != nil {
		t.Fatal(err)
	}
	defer rpc2.Abort(context.Background())
	if got := rpc2.SessionFiles(); got != nil {
		t.Errorf("SessionFiles() without root = %v, want nil", got)
	}
}
