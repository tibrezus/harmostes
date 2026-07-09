package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sync"

	"github.com/tibrezus/harmostes/internal/pijsonl"
)

// RPC implements PiSession over a `pi --mode rpc` subprocess. One RPC = one
// warm pi process; every Prompt reuses it so the agent keeps context across the
// task and its feedback turns.
type RPC struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	log     Logger
	events  chan Event
	done    chan struct{} // closed by Abort to stop the reader goroutine
	closeMu sync.Mutex
	closed  bool
}

// RPCOptions configures the pi subprocess.
type RPCOptions struct {
	PiPath  string   // path to pi; "pi" if empty
	Args    []string // extra args (--skill, --model, --tools, …)
	Workdir string   // agent working directory (the repo under work)
	Env     []string // environment (must include the model API key, e.g. ZAI_API_KEY)
	Log     Logger
}

// NewRPC starts a pi --mode rpc subprocess and begins reading its event stream.
// The caller must call Abort to release the process.
func NewRPC(ctx context.Context, opts RPCOptions) (*RPC, error) {
	pi := opts.PiPath
	if pi == "" {
		pi = "pi"
	}
	args := append([]string{"--mode", "rpc", "--no-session"}, opts.Args...)
	cmd := exec.CommandContext(ctx, pi, args...)
	cmd.Dir = opts.Workdir
	cmd.Env = opts.Env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// pi emits the JSONL protocol on stdout; keep its log lines on stderr SEPARATE
	// (drained to the logger) so they never pollute the protocol stream.
	cmd.Stderr = &lineLogWriter{log: opts.Log}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	r := &RPC{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		log:    opts.Log,
		events: make(chan Event, 128),
		done:   make(chan struct{}),
	}
	go r.readLoop()
	return r, nil
}

// readLoop parses the stdout JSONL stream and forwards events to r.events until
// the stream closes (pi exited) or Abort signals done.
func (r *RPC) readLoop() {
	reader := bufio.NewReader(r.stdout)
	for {
		line, err := reader.ReadBytes('\n')
		if ev, parseErr := parseEvent(line); parseErr == nil {
			select {
			case r.events <- ev:
			case <-r.done:
				return
			}
		}
		if err != nil {
			// EOF or read error: the stream is done. Drain any buffered event
			// source then close the channel so Prompt unblocks.
			close(r.events)
			return
		}
	}
}

// Prompt sends a prompt and blocks until agent_end (or the stream closes / the
// context is cancelled). Returns the number of tool_execution_start events seen
// during this turn.
func (r *RPC) Prompt(ctx context.Context, message, label string) (Event, int, error) {
	logf(r.log, Event{Type: "prompt", Message: label})
	if err := r.send(pijsonl.Prompt{Type: pijsonl.CmdPrompt, Message: message}); err != nil {
		return Event{}, 0, err
	}
	var tools int
	var last Event
	for {
		select {
		case <-ctx.Done():
			return last, tools, ctx.Err()
		case ev, ok := <-r.events:
			if !ok {
				// stream closed before agent_end
				return last, tools, nil
			}
			last = ev
			logf(r.log, ev)
			switch ev.Type {
			case pijsonl.EvToolStart:
				tools++
			case pijsonl.EvAgentEnd:
				return ev, tools, nil
			}
		}
	}
}

// Abort sends an abort, closes stdin, and waits for pi to exit.
func (r *RPC) Abort(ctx context.Context) error {
	r.closeMu.Lock()
	if r.closed {
		r.closeMu.Unlock()
		return nil
	}
	r.closed = true
	close(r.done)
	r.closeMu.Unlock()

	// Best effort: tell pi to stop, then close stdin so it exits.
	_ = r.sendNoLock(pijsonl.Abort{Type: pijsonl.CmdAbort})
	_ = r.stdin.Close()
	// Drain the event channel so readLoop (if still selecting on done) exits.
	go func() {
		for range r.events {
		}
	}()
	waitErr := make(chan error, 1)
	go func() { waitErr <- r.cmd.Wait() }()
	select {
	case <-ctx.Done():
		_ = r.cmd.Process.Kill()
		return ctx.Err()
	case err := <-waitErr:
		return err
	}
}

func (r *RPC) send(v any) error {
	r.closeMu.Lock()
	defer r.closeMu.Unlock()
	if r.closed {
		return errors.New("session closed")
	}
	return r.sendNoLock(v)
}

// sendNoLock writes one JSON line to pi's stdin. Caller holds closeMu (or is
// Abort, which has already flipped closed and closed done).
func (r *RPC) sendNoLock(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = r.stdin.Write(b)
	return err
}

// parseEvent parses one JSONL line into an Event. Blank lines, non-JSON lines,
// and lines without a "type" are rejected (the caller drops them).
func parseEvent(line []byte) (Event, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return Event{}, errBlankLine
	}
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return Event{}, err
	}
	if ev.Type == "" {
		return Event{}, errNoType
	}
	return ev, nil
}

var (
	errBlankLine = errors.New("blank line")
	errNoType    = errors.New("event has no type")
)

// lineLogWriter writes pi's stderr line-by-line to the logger as _pi_log events
// (observability), keeping those lines out of the stdout JSONL protocol stream.
type lineLogWriter struct {
	log Logger
	mu  sync.Mutex
	buf []byte
}

func (w *lineLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimSpace(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line != "" {
			logf(w.log, Event{Type: "_pi_log", Message: line})
		}
	}
	return len(p), nil
}
