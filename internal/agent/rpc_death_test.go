package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #882: pi dying mid-turn must surface a NAMED failure — the process, its
// exit status, and its last log lines (the provider's error text) — never a
// bare "write |1: broken pipe" and never a silent partial turn.

// writeDyingPi writes a fake pi that logs a provider error to stderr and
// exits non-zero on the first prompt — the live #882 shape (the endpoint
// failed TLS, pi exited, the worker's stdin write broke).
func writeDyingPi(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dyingpi")
	script := strings.Join([]string{
		"#!/usr/bin/env bash",
		"read -r line",
		"echo '[litellm-provider] failed to fetch models from https://litellm.example/v1/models: TypeError: fetch failed' >&2",
		"echo 'tlsv1 unrecognized name' >&2",
		"exit 1",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPromptNamesPiDeath(t *testing.T) {
	fake := writeDyingPi(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rpc, err := NewRPC(ctx, RPCOptions{PiPath: fake, Workdir: ".", Log: func(Event) {}})
	if err != nil {
		t.Fatalf("NewRPC: %v", err)
	}
	defer func() { _ = rpc.Abort(context.Background()) }()

	_, _, _, _, err = rpc.Prompt(ctx, "do the task", "initial task")
	if err == nil {
		t.Fatal("a mid-turn pi death must fail the turn, not return a silent partial")
	}
	msg := err.Error()
	for _, want := range []string{"pi exited mid-turn", "exit status 1", "tlsv1 unrecognized name", "litellm-provider"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("death error missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "broken pipe") {
		t.Fatalf("the bare broken pipe must be replaced by the diagnosis: %s", msg)
	}
}

// The send path: when pi dies BEFORE the prompt write lands, the broken
// stdin write must also carry the diagnosis (the live incident's exact
// symptom was "write |1: broken pipe").
func TestSendNamesPiDeath(t *testing.T) {
	fake := writeDyingPi(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rpc, err := NewRPC(ctx, RPCOptions{PiPath: fake, Workdir: ".", Log: func(Event) {}})
	if err != nil {
		t.Fatalf("NewRPC: %v", err)
	}
	defer func() { _ = rpc.Abort(context.Background()) }()

	// First prompt consumes the fake's single read; pi exits. Wait for the
	// reaper so the death is recorded, then the NEXT send breaks the pipe.
	_, _, _, _, _ = rpc.Prompt(ctx, "first", "initial task")
	deadline := time.Now().Add(5 * time.Second)
	for {
		rpc.deathMu.Lock()
		set := rpc.exitSet
		rpc.deathMu.Unlock()
		if set || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	err = rpc.send(map[string]string{"type": "prompt", "message": "second"})
	if err == nil {
		t.Skip("pipe buffer absorbed the write (platform-dependent) — the Prompt-path test covers the contract")
	}
	if !strings.Contains(err.Error(), "pi exited mid-turn") {
		t.Fatalf("send error must carry the death diagnosis, got: %v", err)
	}
}

// The death branch itself: a closed stream on an OPEN session is a death;
// on a closed (aborted) session it is the quiet pre-existing contract.
// The closed flag is flipped AFTER the send so Prompt reaches the stream
// branch (a closed session refuses the send before the stream is read).
func TestClosedStreamDeathBranch(t *testing.T) {
	for _, closed := range []bool{false, true} {
		r := feedRPC(t) // no buffered events
		r.events = make(chan Event)
		close(r.events)
		// Send succeeds (session open at send time); then mark the session
		// closed to simulate Abort having raced the stream close.
		r.closed = closed
		_, _, _, _, err := r.Prompt(context.Background(), "m", "l")
		if closed {
			// send refuses on a closed session — never a death diagnosis
			if err != nil && strings.Contains(err.Error(), "pi exited mid-turn") {
				t.Fatalf("closed session reported a death: %v", err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "pi exited mid-turn") {
			t.Fatalf("open session + closed stream must report a death, got: %v", err)
		}
	}
}
