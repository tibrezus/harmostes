// Package agentlineage hosts the PRLineage Dapr actor — the durable,
// turn-based entity that owns one PR's agent session lineage (ADR-0010).
//
// Why an actor (the building block choice): a workflow's attempt Jobs are
// ephemeral (ADR-0007), so the lineage needs an owner that outlives them.
// Dapr's actor model gives all three required properties by construction:
//   - ISOLATED: actor state is keyed per entity by the sidecar
//     (PRLineage||<repo>~<pr>||<key>) — no cross-PR reads are expressible;
//   - DURABLE: state lives in the configured state store (Valkey), not in
//     any pod; actor deactivation is lossless;
//   - CONFLICT-FREE: the sidecar serializes method calls per actor ID
//     (turn-based access), so concurrent reviews of one PR cannot
//     interleave a fetch/publish race.
//
// Contract (dapr docs: reference/api/actors_api): the hosting app serves
// GET /dapr/config and PUT /actors/{type}/{id}/method/{name}; callers
// invoke via their sidecar at PUT /v1.0/actors/{type}/{id}/method/{name}.
// The actor server reaches its own state through the SAME sidecar's
// actor-scoped state endpoints (GET/PUT /v1.0/actors/{type}/{id}/state/…).
package agentlineage

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tibrezus/harmostes/internal/agent"
	"github.com/tibrezus/harmostes/internal/dapr"
)

// ActorType is the registered entity name.
const ActorType = "PRLineage"

// Session is the actor's persisted state: the pi session JSONL (the whole
// conversation, resumable), the head it was last published against, and a
// monotonic publish counter (the ordered-growth signal — it must only
// ever increase; a regression means state was lost or two writers raced).
type Session struct {
	Session    string `json:"session"`
	LastHead   string `json:"lastHead"`
	Generation int    `json:"generation"`
	// File is the pi-side FILENAME of the live conversation (pi renames
	// sessions to "<ts>_<id>.jsonl" after the first turn); fetch must
	// materialize under the SAME name or pi cannot adopt it (r21 P4.1).
	File string `json:"file,omitempty"`
}

// ActorID builds the entity id "<sanitized-repo>~<pr>". The PR half is
// digits-only (agent.SanitizePR) — path syntax can never enter an id.
func ActorID(repo, pr string) (string, error) {
	if !agent.SanitizePR(pr) {
		return "", agent.ErrNotAPR
	}
	return agent.SanitizeRepo(repo) + "~" + pr, nil
}

// Host serves the Dapr actor contract on the pool's app port.
type Host struct {
	// Sidecar is the in-pod Dapr HTTP client used for the actor's own
	// state (actor-scoped endpoints — isolation is the sidecar's job).
	Sidecar *dapr.HTTPClient
}

// Config is the GET /dapr/config payload. Idle actors deactivate after
// 1h (state persists — durability is the store's, not the activation's).
func (h *Host) Config() map[string]any {
	return map[string]any{
		"entities":                []string{ActorType},
		"actorIdleTimeout":        "1h",
		"actorScanInterval":       "30s",
		"drainOngoingCallTimeout": "30s",
		"drainRebalancedActors":   true,
	}
}

func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/dapr/config":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(h.Config())
		return
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/actors/"):
		// /actors/{type}/{id}/method/{name}
		rest := strings.TrimPrefix(r.URL.Path, "/actors/")
		parts := strings.Split(rest, "/")
		if len(parts) != 4 || parts[0] != ActorType || parts[2] != "method" {
			http.NotFound(w, r)
			return
		}
		id, method := parts[1], parts[3]
		if !validID(id) {
			http.Error(w, "invalid actor id", http.StatusBadRequest)
			return
		}
		h.invoke(w, r, id, method)
		return
	}
	http.NotFound(w, r)
}

func validID(id string) bool {
	i := strings.LastIndex(id, "~")
	return i > 0 && agent.SanitizePR(id[i+1:])
}

func (h *Host) invoke(w http.ResponseWriter, r *http.Request, id, method string) {
	ctx := r.Context()
	switch method {
	case "fetch":
		var s Session
		b, err := h.Sidecar.GetActorState(ctx, ActorType, id, "session")
		if err == nil && len(b) > 0 {
			json.Unmarshal(b, &s)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s)
	case "publish":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var in Session
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, "bad payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Generation is server-authoritative: read-modify-write INSIDE
		// the turn (the sidecar serializes calls per id, so this is
		// race-free by construction — the property the actor buys us).
		var cur Session
		if b, err := h.Sidecar.GetActorState(ctx, ActorType, id, "session"); err == nil && len(b) > 0 {
			json.Unmarshal(b, &cur)
		}
		in.Generation = cur.Generation + 1
		if err := h.Sidecar.SaveActorState(ctx, ActorType, id, "session", in); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"generation":%d}`, in.Generation)
	default:
		http.NotFound(w, r)
	}
}
