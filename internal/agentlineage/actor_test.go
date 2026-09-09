package agentlineage

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tibrezus/harmostes/internal/dapr"
)

// fakeSidecar stands in for the Dapr sidecar: actor-scoped state keyed by
// the URL's actor id — per-entity isolation is visible as distinct maps.
func fakeSidecar(t *testing.T) (*httptest.Server, *map[string]Session) {
	t.Helper()
	store := map[string]Session{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /v1.0/actors/PRLineage/{id}/state/session
		if !strings.HasPrefix(r.URL.Path, "/v1.0/actors/PRLineage/") || !strings.HasSuffix(r.URL.Path, "/state/session") {
			http.NotFound(w, r)
			return
		}
		actorID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.0/actors/PRLineage/"), "/state/session")
		switch r.Method {
		case http.MethodGet:
			if s, ok := store[actorID]; ok {
				json.NewEncoder(w).Encode(s)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPut:
			var s Session
			_ = json.NewDecoder(r.Body).Decode(&s)
			store[actorID] = s
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	return ts, &store
}

func TestPRLineageActorRoundtripAndIsolation(t *testing.T) {
	ts, store := fakeSidecar(t)
	defer ts.Close()
	h := &Host{Sidecar: dapr.New(ts.URL)}

	// fetch on a fresh entity: zero session, no error
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/actors/PRLineage/host-o-r~99/method/fetch", nil))
	if rec.Code != 200 {
		t.Fatalf("fetch fresh: %d", rec.Code)
	}
	var s Session
	_ = json.Unmarshal(rec.Body.Bytes(), &s)
	if s.Session != "" || s.Generation != 0 {
		t.Fatalf("fresh fetch must be zero: %+v", s)
	}

	// publish → server-authoritative generation 1, then 2 (monotonic)
	pub := func(id string) int {
		rec := httptest.NewRecorder()
		body, _ := json.Marshal(Session{Session: "JSONL-" + id, LastHead: "deadbeef"})
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/actors/PRLineage/"+id+"/method/publish", bytes.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("publish %s: %d %s", id, rec.Code, rec.Body.String())
		}
		var out struct{ Generation int }
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Generation
	}
	if g := pub("host-o-r~99"); g != 1 {
		t.Fatalf("first publish generation=%d, want 1", g)
	}
	if g := pub("host-o-r~99"); g != 2 {
		t.Fatalf("second publish generation=%d, want 2", g)
	}
	// a DIFFERENT PR is a different entity: its generation starts at 1
	if g := pub("host-o-r~100"); g != 1 {
		t.Fatalf("other PR must be isolated, generation=%d want 1", g)
	}

	// fetch returns the published bytes, generation preserved
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/actors/PRLineage/host-o-r~99/method/fetch", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &s)
	if s.Session != "JSONL-host-o-r~99" || s.Generation != 2 || s.LastHead != "deadbeef" {
		t.Fatalf("fetch after publish: %+v", s)
	}
	if (*store)["host-o-r~99"].Generation != 2 || (*store)["host-o-r~100"].Generation != 1 {
		t.Fatalf("store isolation broken: %+v", *store)
	}
}

func TestPRLineageActorHostileIDsRefused(t *testing.T) {
	ts, _ := fakeSidecar(t)
	defer ts.Close()
	h := &Host{Sidecar: dapr.New(ts.URL)}
	for _, evil := range []string{"..%2F..%2Foutside", "repo~..%2Fevil", "repo~99..x"} {
		req := httptest.NewRequest(http.MethodPut, "/actors/PRLineage/"+evil+"/method/fetch", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == 200 {
			t.Fatalf("hostile id %q must not be served", evil)
		}
	}
	// unknown actor type / method → 404
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/actors/EvilType/repo~1/method/fetch", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown type must 404, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/actors/PRLineage/repo~1/method/explode", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown method must 404, got %d", rec.Code)
	}
}

func TestActorIDAndConfig(t *testing.T) {
	if id, err := ActorID("git.rezus.cloud/tibrez/rhesadox", "99"); err != nil || id != "git.rezus.cloud-tibrez-rhesadox-d782cf64~99" {
		t.Fatalf("ActorID: %q %v", id, err)
	}
	if _, err := ActorID("host/o/r", "../evil"); err == nil {
		t.Fatal("traversal pr must be refused")
	}
	h := &Host{}
	if h.Config()["entities"].([]string)[0] != ActorType {
		t.Fatal("config must register PRLineage")
	}
}
