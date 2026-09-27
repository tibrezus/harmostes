package dapr

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
	// 2026-09-25 live incident (charts 276+): with app-health-check enabled,
	// healthz answers 500 until the APPLICATION binds its port — on the
	// consumer that binding happens right AFTER the guard. A 500-answering
	// sidecar is PRESENT (the guard's actual question); waiting for 2xx
	// deadlocked the boot (guard ↔ daprd app-port wait). One probe, no
	// retries: presence is proven by the first answer.
	t.Run("answering 500 (app-health-check pending) — present, passes at once", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		if err := WaitForSidecar(context.Background(), New(srv.URL), 80*time.Millisecond, 10*time.Millisecond); err != nil {
			t.Fatalf("an answering sidecar is present and must pass: %v", err)
		}
		if hits != 1 {
			t.Fatalf("probes = %d, want exactly 1 (presence is proven by the first answer)", hits)
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
	t.Run("starts answering after retries (slow sidecar boot)", func(t *testing.T) {
		// The wait loop's remaining job: the sidecar's port is not listening
		// yet (daprd still booting → connection refused), then it is. The
		// first accepted connection wins — status code is irrelevant (see
		// the 500 subtest above).
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		var hits int32
		go func() {
			time.Sleep(30 * time.Millisecond) // the boot window: nothing serves yet
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(http.StatusNoContent)
			})}
			_ = srv.Serve(ln)
		}()
		if err := WaitForSidecar(context.Background(), New("http://"+ln.Addr().String()), 2*time.Second, 5*time.Millisecond); err != nil {
			t.Fatalf("sidecar that starts answering within the bound should pass: %v", err)
		}
		if atomic.LoadInt32(&hits) < 1 {
			t.Fatal("expected at least one answered probe")
		}
	})
	t.Run("aborted context", func(t *testing.T) {
		// Nothing answers here: only an unanswering sidecar can still be
		// pending when the context cancels.
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(20 * time.Millisecond); cancel() }()
		err := WaitForSidecar(ctx, New("http://127.0.0.1:1"), 5*time.Second, 5*time.Millisecond)
		if err == nil {
			t.Fatal("an aborted wait must fail")
		}
	})
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
	t.Run("required + injected + answering 500 — passes (presence)", func(t *testing.T) {
		t.Setenv("HARMOSTES_DAPR_REQUIRED", "true")
		t.Setenv("DAPR_HTTP_PORT", "3500")
		if err := StartupGuard(context.Background(), New(broken.URL), 60*time.Millisecond, 10*time.Millisecond); err != nil {
			t.Fatalf("an answering sidecar is present — the guard must pass: %v", err)
		}
	})
	t.Run("required + injected + unanswering — bounded, NOT ErrSidecarMissing", func(t *testing.T) {
		t.Setenv("HARMOSTES_DAPR_REQUIRED", "true")
		t.Setenv("DAPR_HTTP_PORT", "3500")
		err := StartupGuard(context.Background(), New("http://127.0.0.1:1"), 60*time.Millisecond, 10*time.Millisecond)
		if err == nil {
			t.Fatal("an injected-but-unanswering sidecar must fail the guard")
		}
		if errors.Is(err, ErrSidecarMissing) {
			t.Fatal("injected-but-unanswering is NOT the missing-injection case")
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
