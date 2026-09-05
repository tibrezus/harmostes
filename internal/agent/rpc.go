package agent

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/tibrezus/harmostes/internal/observability"
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

	// seenAgentMsgs counts messages already accounted by a previous
	// agent_end snapshot. Warm sessions re-send the whole conversation on
	// every agent_end; only messages beyond this index belong to this turn.
	seenAgentMsgs int

	// sessionDir is the per-RPC pi session directory (empty when SessionRoot
	// was not configured). Exactly one *.jsonl lands in it per spawn.
	sessionDir string
}

// SessionFiles returns the pi session files this RPC wrote, oldest first.
// Empty when session persistence is off or pi wrote nothing (crash, abort
// before first flush). Callers should read them after Abort.
func (r *RPC) SessionFiles() []string {
	if r.sessionDir == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(r.sessionDir, "*.jsonl"))
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	return matches
}

// RPCOptions configures the pi subprocess.
type RPCOptions struct {
	PiPath  string   // path to pi; "pi" if empty
	Args    []string // extra args (--skill, --model, --tools, …)
	Workdir string   // agent working directory (the repo under work)
	Env     []string // environment (must include the model API key, e.g. LITELLM_API_KEY)
	Log     Logger

	// SessionRoot, when set, makes every RPC persist its pi session as a
	// native session file: pi runs with --session-dir <fresh dir under
	// SessionRoot> and a unique --session-id, so the conversation survives
	// the process and can be forked later (pi --fork <file>). Empty disables
	// persistence (pi still sessions, but in its default location).
	SessionRoot string
}

// NewRPC starts a pi --mode rpc subprocess and begins reading its event stream.
// The caller must call Abort to release the process.
func NewRPC(ctx context.Context, opts RPCOptions) (*RPC, error) {
	pi := opts.PiPath
	if pi == "" {
		pi = "pi"
	}
	// Persistence is opt-in via SessionRoot (#243). Callers that don't opt
	// in keep the historical --no-session behavior: pi writes no session
	// file anywhere (cmd/harmostes-agent relies on exactly that).
	args := append([]string{"--mode", "rpc"}, opts.Args...)
	if opts.SessionRoot == "" {
		args = append(args, "--no-session")
	}
	// Opted in: a fresh session dir per RPC plus a unique session id. The id
	// guarantees a NEW session per spawn — a reused id would silently
	// continue an old conversation — and the fresh dir makes the file
	// findable without racing concurrent runs on one pod.
	sessionDir := ""
	if opts.SessionRoot != "" {
		dir, err := os.MkdirTemp(opts.SessionRoot, "run-")
		if err != nil {
			// Observable degradation (#243 r1): a failed mkdir would
			// otherwise be indistinguishable from persistence being off.
			logf(opts.Log, Event{Type: "session_dir_error", Message: err.Error()})
			// pi persists sessions by DEFAULT — without this the raw,
			// unredacted conversation lands in pi's own session dir,
			// undiscoverable by SessionFiles() and never cleaned (#244 r3).
			args = append(args, "--no-session")
		} else {
			sessionDir = dir
			args = append(args,
				"--session-dir", dir,
				"--session-id", fmt.Sprintf("harmostes-%d", time.Now().UnixNano()),
			)
		}
	}
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
		cmd:        cmd,
		stdin:      stdin,
		stdout:     stdout,
		log:        opts.Log,
		events:     make(chan Event, 128),
		done:       make(chan struct{}),
		sessionDir: sessionDir,
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
// context is cancelled). Returns the agent_end event, the number of
// tool_execution_start events seen during this turn, the token usage, and a
// TurnCapture with the full assistant response text + tool calls (args +
// complete results — Option A, no truncation).
func (r *RPC) Prompt(ctx context.Context, message, label string) (Event, int, Usage, TurnCapture, error) {
	logf(r.log, Event{Type: "prompt", Message: label})
	if err := r.send(pijsonl.Prompt{Type: pijsonl.CmdPrompt, Message: message}); err != nil {
		return Event{}, 0, Usage{}, TurnCapture{}, err
	}
	tracer := observability.Tracer()
	wf := observability.WorkflowFrom(ctx)
	var tools int
	var usage Usage
	var last Event
	var capture TurnCapture
	var toolSpan trace.Span // open tool span (pi runs tools sequentially per turn)
	endTool := func() {
		if toolSpan != nil {
			toolSpan.End()
			toolSpan = nil
		}
	}
	for {
		select {
		case <-ctx.Done():
			endTool()
			return last, tools, usage, capture, ctx.Err()
		case ev, ok := <-r.events:
			if !ok {
				// stream closed before agent_end
				endTool()
				return last, tools, usage, capture, nil
			}
			last = ev
			logf(r.log, ev)
			switch ev.Type {
			case "message_end":
				if u, ok := messageEndUsage(ev.Raw); ok {
					usage.add(u)
				}
				// Capture assistant response text (full content).
				if text := messageEndContent(ev.Raw); text != "" {
					capture.Response = text
				}
			case pijsonl.EvToolStart:
				tools++
				endTool() // close any prior (defensive; tools are sequential)
				// Capture tool call start (name + args — full content, Option A).
				capture.Tools = append(capture.Tools, ToolCall{
					Name: ev.ToolName,
					Args: ev.Args,
				})
				_, toolSpan = tracer.Start(ctx, ev.ToolName)
				toolSpan.SetAttributes(
					attribute.String("harmostes.tool", ev.ToolName),
					attribute.Int("harmostes.args_chars", argsChars(ev.Args)), // size only — never the body
				)
				recordToolCall(ctx, wf, ev.ToolName)
			case pijsonl.EvToolEnd:
				// Capture tool result (full content, Option A) for the current tool.
				if len(capture.Tools) > 0 {
					idx := len(capture.Tools) - 1
					result, isErr := toolEndResult(ev.Raw)
					success := !isErr
					capture.Tools[idx].Result = result
					capture.Tools[idx].Success = &success
					capture.Tools[idx].Details = toolEndDetails(ev.Raw)
					if toolSpan != nil {
						toolSpan.SetAttributes(attribute.Bool("harmostes.success", success))
					}
					// Freshness incidents must be countable WITHOUT a session
					// join (#338 r25 F11): a rig refusal carries a machine-
					// greppable [rig sha_state=…] token in its text; one event
					// line here makes a stale-graph run visible in the pod's
					// event stream — the class the ADR-0009 rule exists for.
					if capture.Tools[idx].Name == "rig" {
						if state := rigRefusalState(result); state != "" {
							logf(r.log, Event{Type: "rig_refused", Message: "graph refused, sha_state=" + state})
						}
					}
				}
				endTool()
			case pijsonl.EvAgentEnd:
				endTool()
				absorbAgentEnd(ev.Raw, r, &usage, &capture)
				return ev, tools, usage, capture, nil
			}
		}
	}
}

// rigRefusalRe matches the machine-greppable token every rig-query refusal
// text carries ([rig sha_state=mismatch] and friends). Non-refusal answers
// never contain it, so a match IS the incident.
var rigRefusalRe = regexp.MustCompile(`\[rig sha_state=([a-z-]+)\]`)

// rigRefusalState extracts the refusal's sha_state from a rig tool result,
// or "" when the result is not a refusal (#338 r25 F11).
func rigRefusalState(result string) string {
	if m := rigRefusalRe.FindStringSubmatch(result); m != nil {
		return m[1]
	}
	return ""
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
	ev.Raw = json.RawMessage(line)
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

// agentEndMessage is one conversation message inside pi's agent_end snapshot.
// In the spawn shape harmostes drives (exec over pipes), pi's RPC stream does
// not emit message boundary events (message_start/message_end) at all — the
// agent_end snapshot is then the ONLY carrier of per-message usage and the
// final assistant text. (Live-proven against pi 0.84.3: message_end present
// when shell-driven, absent when exec-driven; agent_end.messages identical in
// both.)
type agentEndMessage struct {
	Role  string `json:"role"`
	Usage *struct {
		Input      int `json:"input"`
		Output     int `json:"output"`
		CacheRead  int `json:"cacheRead"`
		CacheWrite int `json:"cacheWrite"`
		Cost       *struct {
			Total float64 `json:"total"`
		} `json:"cost"`
	} `json:"usage"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// absorbAgentEnd folds the agent_end conversation snapshot into the turn's
// usage and capture. It is additive-safe with the message_end path: when
// boundary events already supplied usage and response (shell-driven shape),
// the snapshot only advances the warm-session watermark. When they did not
// (exec-driven shape), the snapshot is the source of truth: per-assistant
// usage is summed over the turn's delta (messages beyond the watermark) and
// the last assistant text becomes the captured response.
func absorbAgentEnd(raw json.RawMessage, r *RPC, usage *Usage, capture *TurnCapture) {
	var payload struct {
		Messages []agentEndMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		// #239 was silent drops; make the next protocol drift visible.
		logf(r.log, Event{Type: "agent_end_unparsable", Message: err.Error()})
		return
	}
	if len(payload.Messages) < r.seenAgentMsgs {
		// Shrinking snapshot (context compaction): the conversation was
		// replaced by a shorter summary — everything after it is fresh
		// accounting, so reset the watermark instead of leaving it stale.
		r.seenAgentMsgs = 0
	}
	if len(payload.Messages) <= r.seenAgentMsgs {
		return
	}
	turn := payload.Messages[r.seenAgentMsgs:]
	if usage.Input == 0 && usage.Output == 0 {
		for _, m := range turn {
			if m.Role != "assistant" || m.Usage == nil {
				continue
			}
			cost := 0.0
			if m.Usage.Cost != nil {
				cost = m.Usage.Cost.Total
			}
			usage.add(Usage{
				Input:      m.Usage.Input,
				Output:     m.Usage.Output,
				CacheRead:  m.Usage.CacheRead,
				CacheWrite: m.Usage.CacheWrite,
				Cost:       cost,
			})
		}
	}
	if capture.Response == "" {
	loops:
		for i := len(turn) - 1; i >= 0; i-- {
			if turn[i].Role != "assistant" {
				continue
			}
			for _, c := range turn[i].Content {
				if c.Type == "text" && c.Text != "" {
					capture.Response = c.Text
					break loops
				}
			}
		}
	}
	r.seenAgentMsgs = len(payload.Messages)
}

// PiSessionMarker prefixes every persisted pi-session payload ("gz1:" =
// gzip + base64). LoadPiSession refuses unknown markers — encodings never
// silently reinterpret.
const PiSessionMarker = "gz1:"

// LoadPiSession decodes what worker.SavePiSession stored ("gz1:" marker,
// base64, gzip). Lives beside the pi spawn code it round-trips with; the
// marker check refuses unknown encodings rather than misreading them.
func LoadPiSession(payload string) ([]byte, error) {
	if !strings.HasPrefix(payload, PiSessionMarker) {
		return nil, fmt.Errorf("pi session payload has unknown encoding (prefix %q)", payload[:min(16, len(payload))])
	}
	compressed, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(payload, PiSessionMarker))
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, 20<<20))
}
