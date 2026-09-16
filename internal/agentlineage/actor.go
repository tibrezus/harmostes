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

	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/sessionstore"
)

// ActorType is the registered entity name.
const ActorType = "PRLineage"

// Session is the actor's persisted state — the shared sessionstore.Lineage
// (pi session JSONL, last head, monotonic generation, pi-side filename).
// The type moved to internal/sessionstore with the rest of the lineage
// contract (#516); the actor remains its Dapr ADAPTER (HTTP server +
// sidecar state endpoints).
type Session = sessionstore.Lineage

// ActorID builds the entity id "<sanitized-repo>~<pr>".
//
// Moved to sessionstore.ActorID with the identity scheme (#516).
func ActorID(repo, pr string) (string, error) { return sessionstore.ActorID(repo, pr) }

// Host serves the Dapr actor contract on the pool's app port.
type Host struct {
	// Sidecar is the in-pod Dapr HTTP client used for the actor's own
	// state (actor-scoped endpoints — isolation is the sidecar's job).
	Sidecar dapr.Client
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
		_ = json.NewEncoder(w).Encode(h.Config())
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

func validID(id string) bool { return sessionstore.ValidID(id) }

func (h *Host) invoke(w http.ResponseWriter, r *http.Request, id, method string) {
	ctx := r.Context()
	switch method {
	case "fetch":
		var s Session
		b, err := h.Sidecar.GetActorState(ctx, ActorType, id, "session")
		if err == nil && len(b) > 0 {
			// lenient read: a corrupt blob serves as an empty session —
			// fetch is a view, and refusing to render stale bytes beats
			// failing the read entirely
			_ = json.Unmarshal(b, &s)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s)
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
			if uerr := json.Unmarshal(b, &cur); uerr != nil {
				// corrupt stored state must not silently reset Generation
				// to 1 (the server-authoritative monotonic invariant);
				// refuse the write rather than clobber the actor state
				http.Error(w, "corrupt session state: "+uerr.Error(), http.StatusInternalServerError)
				return
			}
		}
		in.Generation = cur.Generation + 1
		if err := h.Sidecar.SaveActorState(ctx, ActorType, id, "session", in); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"generation":%d}`, in.Generation)
	default:
		http.NotFound(w, r)
	}
}
