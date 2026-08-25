// Package timeline is the evidence layer of the Canonical Orchestration
// History (ADR-0005): a time-ordered, per-Attempt event stream persisted in
// Dapr state. The Attempt CR remains the canonical index (what happened,
// authoritatively); timeline events are prunable evidence (how it happened,
// in time order).
//
// Storage shape (live-spiked against state.redis — no query API, but blind
// saves + bulk reads work):
//
//	timeline/<attempt>/subject        → Subject (one per Attempt; UI index)
//	timeline/<attempt>/<run>/<seq>    → Event (seq 0,1,2… per run)
//	timeline/gate/<workflow>/<seq>    → gate lifecycle events (transitions
//	                                    only; process-local seq, short TTL)
//
// Writes are blind appends — no index to maintain, no read-modify-write, no
// coordination. A crashed run's successor writes under its own run ID from
// seq 0; nothing to repair. Reads probe per run (the Attempt CR lists runs
// authoritatively).
package timeline

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/tibrezus/harmostes/internal/dapr"
)

// KeyPrefix namespaces all timeline state keys.
const KeyPrefix = "timeline/"

// DefaultTTL is how long attempt events live. Evidence, not audit: a week
// covers review-loop debugging while keeping the store bounded.
const DefaultTTL = 7 * 24 * time.Hour

// GateTTL is shorter for gate lifecycle events — they are diagnostic noise
// once the PR they armed has completed.
const GateTTL = 48 * time.Hour

// Event kinds. One schema; the interface does not care which seam emitted.
const (
	KindRunStarted    = "run.started"
	KindRunCompleted  = "run.completed"
	KindNodeStarted   = "node.started"
	KindNodeCompleted = "node.completed"
	KindPluginTail    = "plugin.tail"
	KindGateArmed     = "gate.armed"
	KindGateWaiting   = "gate.waiting" // transitions only — not every re-evaluation
	KindGateProceed   = "gate.proceed"
	KindGateStanddown = "gate.standdown"
	KindAgentTurn     = "agent.turn" // reference only; content lives in the SessionRecord
	KindAgentTool     = "agent.tool"
)

// Subject is the orientation of an event: what triggered it and the human
// anchor to navigate by (e.g. kind=pr, ref="tibrez/rhesadox#1566",
// title="CI-tiering: decode label-gated full pipeline"). Derived from the
// Trigger Envelope; echoed on every event so any projection can group and
// filter by it before reading payloads.
type Subject struct {
	Kind  string `json:"kind,omitempty"` // pr | release | cron | manual | webhook
	Ref   string `json:"ref,omitempty"`  // host/owner/repo#number
	Title string `json:"title,omitempty"`
	SHA   string `json:"sha,omitempty"` // head at trigger time
}

// Event is one entry in an Attempt's timeline.
type Event struct {
	At       time.Time       `json:"at"`
	Attempt  string          `json:"attempt"`
	Workflow string          `json:"workflow,omitempty"`
	Run      string          `json:"run,omitempty"`
	Node     string          `json:"node,omitempty"` // "" for run/gate-level events
	Kind     string          `json:"kind"`
	Subject  Subject         `json:"subject,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// Writer emits timeline events. Nil-safe: a nil *DaprWriter is a no-op, so
// call sites never branch on availability.
type Writer interface {
	Emit(ctx context.Context, kind, node string, payload any) error
}

// DaprWriter appends events for one (attempt, run). Not safe for concurrent
// use — one worker process owns one run, sequentially.
type DaprWriter struct {
	client         dapr.Client
	store          string
	attempt        string
	workflow       string
	run            string
	subject        Subject
	ttl            time.Duration
	seq            int
	subjectSaved   bool
	prefixOverride string // gate writer uses "gate/<workflow>" instead of the attempt name
}

// NewWriter returns a Writer appending under timeline/<attempt>/<run>/<seq>.
func NewWriter(client dapr.Client, store, attempt, workflow, run string, subject Subject) *DaprWriter {
	return &DaprWriter{
		client: client, store: store,
		attempt: attempt, workflow: workflow, run: run,
		subject: subject, ttl: DefaultTTL,
	}
}

// NewGateWriter returns a Writer for gate lifecycle events. Each wake gets
// its own keyspace under the Attempt it belongs to (keyed
// timeline/<attempt>/gate/<seq>) — one worker process per wake, so seq is
// collision-free, and the Attempt CR's run list already enables discovery.
// When the wake carries no Attempt (manual dispatch), a time-unique
// namespace is used instead (rare, diagnostic-only; not enumerated by the
// Reader).
func NewGateWriter(client dapr.Client, store, workflow, attempt string, subject Subject) *DaprWriter {
	ns := attempt
	if ns == "" {
		ns = "gate- orphan/" + workflow + "/" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return &DaprWriter{
		client: client, store: store,
		workflow: workflow, subject: subject,
		ttl: GateTTL, prefixOverride: ns + "/gate",
	}
}

// SaveSubject persists the Attempt's Subject index key (one per Attempt) so
// UI list views resolve orientation with a single bulk get.
func (w *DaprWriter) SaveSubject(ctx context.Context) error {
	if w == nil || w.client == nil || w.prefixOverride != "" {
		return nil
	}
	b, err := json.Marshal(w.subject)
	if err != nil {
		return err
	}
	return w.client.SaveStateTTL(ctx, w.store, KeyPrefix+w.attempt+"/subject", string(b), w.ttl)
}

// Emit appends one event. Payload (if non-nil) is JSON-encoded inline.
func (w *DaprWriter) Emit(ctx context.Context, kind, node string, payload any) error {
	if w == nil || w.client == nil {
		return nil
	}
	w.seq++
	ev := Event{
		At:       time.Now().UTC(),
		Attempt:  w.attempt,
		Workflow: w.workflow,
		Run:      w.run,
		Node:     node,
		Kind:     kind,
		Subject:  w.subject,
	}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		ev.Payload = b
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	owner := w.attempt
	if w.prefixOverride != "" {
		owner = w.prefixOverride
	}
	var key string
	if w.prefixOverride != "" {
		key = fmt.Sprintf("%s%s/%06d", KeyPrefix, owner, w.seq)
	} else {
		key = fmt.Sprintf("%s%s/%s/%06d", KeyPrefix, owner, w.run, w.seq)
	}
	return w.client.SaveStateTTL(ctx, w.store, key, string(b), w.ttl)
}

// Filter narrows a read. Zero value = everything.
type Filter struct {
	KindPrefix string // e.g. "gate." or "node."
	Node       string
	Limit      int // 0 = no limit (applied after sort, newest-first)
}

// Reader reads timeline evidence back.
type Reader interface {
	// Attempt returns events for one Attempt, time-ordered (oldest first),
	// probing each run's keyspace. Runs are listed by the caller (from the
	// Attempt CR) — the store holds no index to discover them.
	Attempt(ctx context.Context, attempt string, runs []string, f Filter) ([]Event, error)
	// GateEvents returns gate lifecycle events for one workflow.
	GateEvents(ctx context.Context, workflow string, f Filter) ([]Event, error)
	// Subjects resolves the Subject index for many attempts in one bulk get.
	Subjects(ctx context.Context, attempts []string) (map[string]Subject, error)
}

// DaprReader reads events via the Dapr sidecar.
type DaprReader struct {
	client dapr.Client
	store  string
}

// NewReader returns the Dapr-backed Reader.
func NewReader(client dapr.Client, store string) *DaprReader {
	return &DaprReader{client: client, store: store}
}

// probeBatch is how many keys one bulk get requests per round.
const probeBatch = 64

// Attempt implements Reader. For each run it probes seq 0..n in batches of
// probeBatch until a batch comes back empty — O(runs × n/64) round trips.
func (r *DaprReader) Attempt(ctx context.Context, attempt string, runs []string, f Filter) ([]Event, error) {
	var events []Event
	for _, run := range runs {
		for base := 0; ; base += probeBatch {
			keys := make([]string, probeBatch)
			for i := range keys {
				keys[i] = fmt.Sprintf("%s%s/%s/%06d", KeyPrefix, attempt, run, base+i)
			}
			vals, err := r.client.GetBulkState(ctx, r.store, keys)
			if err != nil {
				return nil, err
			}
			got := 0
			for i := 0; i < probeBatch; i++ {
				v, ok := vals[keys[i]]
				if !ok || v == "" {
					continue
				}
				got++
				var ev Event
				if err := json.Unmarshal([]byte(v), &ev); err == nil {
					events = append(events, ev)
				}
			}
			if got < probeBatch {
				break // frontier reached
			}
		}
	}
	events = applyFilter(events, f)
	return events, nil
}

// GateEvents implements Reader: probe timeline/<attempt>/gate/<seq> for one
// Attempt's gate cycles.
func (r *DaprReader) GateEvents(ctx context.Context, attempt string, f Filter) ([]Event, error) {
	var events []Event
	for base := 0; ; base += probeBatch {
		keys := make([]string, probeBatch)
		for i := range keys {
			keys[i] = fmt.Sprintf("%s%s/gate/%06d", KeyPrefix, attempt, base+i)
		}
		vals, err := r.client.GetBulkState(ctx, r.store, keys)
		if err != nil {
			return nil, err
		}
		got := 0
		for i := 0; i < probeBatch; i++ {
			v, ok := vals[keys[i]]
			if !ok || v == "" {
				continue
			}
			got++
			var ev Event
			if err := json.Unmarshal([]byte(v), &ev); err == nil {
				events = append(events, ev)
			}
		}
		if got < probeBatch {
			break
		}
	}
	events = applyFilter(events, f)
	return events, nil
}

// Subjects implements Reader: one bulk get of per-attempt index keys.
func (r *DaprReader) Subjects(ctx context.Context, attempts []string) (map[string]Subject, error) {
	if len(attempts) == 0 {
		return map[string]Subject{}, nil
	}
	keys := make([]string, len(attempts))
	for i, a := range attempts {
		keys[i] = KeyPrefix + a + "/subject"
	}
	vals, err := r.client.GetBulkState(ctx, r.store, keys)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Subject, len(attempts))
	for i, a := range attempts {
		if v, ok := vals[keys[i]]; ok && v != "" {
			var s Subject
			if json.Unmarshal([]byte(v), &s) == nil {
				out[a] = s
			}
		}
	}
	return out, nil
}

func applyFilter(events []Event, f Filter) []Event {
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	if f.KindPrefix == "" && f.Node == "" && f.Limit == 0 {
		return events
	}
	var out []Event
	for _, ev := range events {
		if f.KindPrefix != "" && !hasKindPrefix(ev.Kind, f.KindPrefix) {
			continue
		}
		if f.Node != "" && ev.Node != f.Node {
			continue
		}
		out = append(out, ev)
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out
}

func hasKindPrefix(kind, prefix string) bool {
	if len(kind) < len(prefix) {
		return false
	}
	return kind[:len(prefix)] == prefix
}
