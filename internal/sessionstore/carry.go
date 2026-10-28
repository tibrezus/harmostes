package sessionstore

// The round carry (ADR-0022): the deterministic trace of a finished
// round, extracted from the pi session JSONL at publish time. The
// PRLineage actor stores THIS — bounded, human-readable, newest-first —
// never the raw transcript. The mtplx engine prefills at ~100 tok/s
// (owner measurement, 2026-10-25); a resumed lineage had reached ~162k
// window tokens on rhesadox#2833, a ~27-minute prefill that times out
// every dispatch before turn one. Resuming the transcript is not KV-cache
// economics on this engine; it is a failure mode. The trace — what the
// round DID (tools, files, models) and CONCLUDED (its closing rationale)
// — is the continuity that survives.

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxCarryBytes bounds the whole stored trace. ~24KB ≈ 6k tokens ≈
	// one minute of prefill at the measured 100 tok/s — the continuity
	// cost the design pays per round, by construction.
	MaxCarryBytes = 24 << 10
	// maxCarryPrior bounds the folded earlier-rounds block (the prior
	// carry is trimmed to this before the fresh skeleton is prepended,
	// so the total lands under MaxCarryBytes by construction).
	maxCarryPrior = MaxCarryBytes / 2
	// maxCarryFinal bounds the closing-rationale excerpt.
	maxCarryFinal = 8 << 10
	// maxCarryFiles bounds the touched-files list.
	maxCarryFiles = 40
)

// ExtractCarry renders a bounded markdown trace of one finished round
// from the pi session JSONL raw bytes, folding in the PRIOR carry
// (newest first, oldest content trimmed first). Deterministic: identical
// input renders identical output. Defensive: unparsable lines are
// skipped, never fatal — a carry is always produced (fresh next round is
// the invariant; the trace is the best-effort decoration on it).
func ExtractCarry(raw []byte, prior, head string) string {
	var b strings.Builder

	type round struct {
		first, last time.Time
		models      []string // unique, in first-seen order
		assistant   int      // assistant messages (turns)
		toolCalls   int
		tools       map[string]int
		files       []string // unique, sorted
		fileSeen    map[string]bool
		finalText   string // last non-empty assistant text
	}
	r := &round{tools: map[string]int{}, fileSeen: map[string]bool{}}
	seenModel := map[string]bool{}

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			ModelId   string    `json:"modelId"`
			Message   struct {
				Role    string `json:"role"`
				Content []struct {
					Type      string          `json:"type"`
					Text      string          `json:"text"`
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if !ev.Timestamp.IsZero() {
			if r.first.IsZero() || ev.Timestamp.Before(r.first) {
				r.first = ev.Timestamp
			}
			if ev.Timestamp.After(r.last) {
				r.last = ev.Timestamp
			}
		}
		switch ev.Type {
		case "model_change":
			if ev.ModelId != "" && !seenModel[ev.ModelId] {
				seenModel[ev.ModelId] = true
				r.models = append(r.models, ev.ModelId)
			}
		case "message":
			m := ev.Message
			switch m.Role {
			case "assistant":
				r.assistant++
				for _, c := range m.Content {
					switch c.Type {
					case "toolCall":
						r.toolCalls++
						r.tools[c.Name]++
						for _, p := range toolCallPaths(c.Name, c.Arguments) {
							if !r.fileSeen[p] {
								r.fileSeen[p] = true
								r.files = append(r.files, p)
							}
						}
					case "text":
						if t := strings.TrimSpace(c.Text); t != "" {
							r.finalText = t
						}
					}
				}
			}
		}
	}

	// ── This round's skeleton (always whole) ──
	headShort := head
	if len(headShort) > 12 {
		headShort = headShort[:12]
	}
	b.WriteString("## This round\n")
	if !r.first.IsZero() {
		b.WriteString("- span: " + r.first.UTC().Format(time.RFC3339) + " → " + r.last.UTC().Format(time.RFC3339) + "\n")
	}
	if head != "" {
		b.WriteString("- head: " + headShort + "\n")
	}
	if len(r.models) > 0 {
		b.WriteString("- models: " + strings.Join(r.models, ", ") + "\n")
	}
	b.WriteString("- turns: " + itoa(r.assistant) + " assistant messages, " + itoa(r.toolCalls) + " tool calls\n")
	if len(r.tools) > 0 {
		b.WriteString("- tools: " + renderToolHistogram(r.tools) + "\n")
	}
	if len(r.files) > 0 {
		files := r.files
		shown := ""
		if len(files) > maxCarryFiles {
			files = files[:maxCarryFiles]
			shown = " (first " + itoa(maxCarryFiles) + ")"
		}
		b.WriteString("- files touched" + shown + ": " + strings.Join(files, "; ") + "\n")
	}
	if r.finalText != "" {
		b.WriteString("\n### Closing rationale\n\n")
		b.WriteString(truncate(r.finalText, maxCarryFinal))
		b.WriteString("\n")
	}

	// ── The folded prior (trimmed to fit, oldest content falls first) ──
	if p := strings.TrimSpace(prior); p != "" {
		p = truncateHead(p, maxCarryPrior)
		b.WriteString("\n## Earlier rounds (newest first)\n\n")
		b.WriteString(p)
		b.WriteString("\n")
	}

	out := b.String()
	if len(out) > MaxCarryBytes { // final belt: the skeleton itself overran
		out = truncateHead(out, MaxCarryBytes)
	}
	return out
}

// toolCallPaths extracts touched file paths from an edit/write-class
// tool call's arguments. Only mutation-shaped tool names qualify — read
// paths are orientation, not a trace of what changed.
func toolCallPaths(name string, args json.RawMessage) []string {
	n := strings.ToLower(name)
	if !strings.Contains(n, "edit") && !strings.Contains(n, "write") && !strings.Contains(n, "patch") {
		return nil
	}
	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"path", "file_path", "notebook_path"} {
		if v, ok := a[k].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

// renderToolHistogram renders "bash×174, read×13, todo×10" — deterministic
// order (count desc, then name asc), top eight.
func renderToolHistogram(tools map[string]int) string {
	type kv struct {
		k string
		v int
	}
	var pairs []kv
	for k, v := range tools {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v != pairs[j].v {
			return pairs[i].v > pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	if len(pairs) > 8 {
		pairs = pairs[:8]
	}
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "×" + itoa(p.v)
	}
	return strings.Join(parts, ", ")
}

// truncate caps s to max bytes, marking the cut.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n\n…(truncated)"
}

// truncateHead keeps the HEAD of s (the NEWEST content, since carries are
// newest-first) and cuts the tail (the oldest), marking the loss.
func truncateHead(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n\n…(older rounds trimmed)"
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
