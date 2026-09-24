package dapr

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Mutation probe note: each test below was verified red against a broken
// guard (no-op guard, wrong status-code bound, missing env check) before
// landing — see the PR description.

func TestWaitForSidecar(t *testing.T) {
	t.Run("healthy immediately", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent) // dapr's healthy healthz answer
		}))
		defer srv.Close()
		if err := WaitForSidecar(context.Background(), New(srv.URL), 2*time.Second, 10*time.Millisecond); err != nil {
			t.Fatalf("healthy sidecar should pass: %v", err)
		}
	})
	t.Run("never healthy — times out with the last error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		err := WaitForSidecar(context.Background(), New(srv.URL), 80*time.Millisecond, 10*time.Millisecond)
		if err == nil {
			t.Fatal("an unhealthy sidecar must fail the wait")
		}
		if !contains(err.Error(), "500") {
			t.Fatalf("error should carry the last probe failure, got: %v", err)
		}
	})
	t.Run("refused connection — times out", func(t *testing.T) {
		// No server: the incident's exact signature (nothing on 127.0.0.1:3500).
		c := New("http://127.0.0.1:1")
		err := WaitForSidecar(context.Background(), c, 80*time.Millisecond, 10*time.Millisecond)
		if err == nil {
			t.Fatal("a dead endpoint must fail the wait")
		}
	})
	t.Run("becomes healthy after retries", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			if hits < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()
		if err := WaitForSidecar(context.Background(), New(srv.URL), 2*time.Second, 5*time.Millisecond); err != nil {
			t.Fatalf("sidecar that converges within the bound should pass: %v", err)
		}
		if hits < 3 {
			t.Fatalf("expected >=3 probes, got %d", hits)
		}
	})
	t.Run("aborted context", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(20 * time.Millisecond); cancel() }()
		err := WaitForSidecar(ctx, New(srv.URL), 5*time.Second, 5*time.Millisecond)
		if err == nil {
			t.Fatal("an aborted wait must fail")
		}
	})
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestStartupGuard(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer healthy.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()

	t.Run("not required — no-op even with nothing injected", func(t *testing.T) {
		t.Setenv("HARMOSTES_DAPR_REQUIRED", "")
		t.Setenv("DAPR_HTTP_PORT", "")
		if err := StartupGuard(context.Background(), New("http://127.0.0.1:1"), 10*time.Millisecond, 5*time.Millisecond); err != nil {
			t.Fatalf("guard must stay silent when the chart did not require dapr: %v", err)
		}
	})
	t.Run("required + injected + healthy — passes", func(t *testing.T) {
		t.Setenv("HARMOSTES_DAPR_REQUIRED", "true")
		t.Setenv("DAPR_HTTP_PORT", "3500")
		if err := StartupGuard(context.Background(), New(healthy.URL), 2*time.Second, 5*time.Millisecond); err != nil {
			t.Fatalf("healthy injected sidecar should pass: %v", err)
		}
	})
	t.Run("required + injected + never healthy — bounded error", func(t *testing.T) {
		t.Setenv("HARMOSTES_DAPR_REQUIRED", "true")
		t.Setenv("DAPR_HTTP_PORT", "3500")
		err := StartupGuard(context.Background(), New(broken.URL), 60*time.Millisecond, 10*time.Millisecond)
		if err == nil {
			t.Fatal("an injected-but-dead sidecar must fail the guard")
		}
		if errors.Is(err, ErrSidecarMissing) {
			t.Fatal("injected-but-dead is NOT the missing-injection case")
		}
	})
	t.Run("required + NOT injected — ErrSidecarMissing without probing", func(t *testing.T) {
		t.Setenv("HARMOSTES_DAPR_REQUIRED", "true")
		t.Setenv("DAPR_HTTP_PORT", "")
		// Deliberately point at nothing: the guard must not need a sidecar to
		// report that one is missing.
		err := StartupGuard(context.Background(), New("http://127.0.0.1:1"), 10*time.Millisecond, 5*time.Millisecond)
		if !errors.Is(err, ErrSidecarMissing) {
			t.Fatalf("missing injection must surface ErrSidecarMissing, got: %v", err)
		}
	})
}
