package dapr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// The sidecar guard (#613): a dapr-injected pod whose sidecar never arrived
// must fail loudly at startup instead of serving hours of degraded
// correctness (live incident 2026-09-24 — chart rollout raced dapr-system
// churn; the injector's failurePolicy:Ignore admitted worker + controller
// pods with no daprd at all, and the platform silently lost webhooks, state,
// and timeline for 6.5h).

// ErrSidecarMissing means the pod PROMISES a sidecar (the chart stamps
// HARMOSTES_DAPR_REQUIRED=true on every dapr-injected workload) but injection
// did not run — DAPR_HTTP_PORT, which only daprd injection sets, is absent.
// Restarting the application container cannot fix this: the sidecar is part of
// the POD SPEC and admission does not re-run for container restarts. The pod
// itself must be recreated (the deployment's Replacement re-hits the injector
// webhook; attempt Jobs are re-dispatched fresh by the churn machinery).
var ErrSidecarMissing = errors.New("dapr: pod requires a sidecar (HARMOSTES_DAPR_REQUIRED) but injection did not run (no DAPR_HTTP_PORT) — recreate the pod; a container restart cannot re-run admission")

// Required reports whether this process runs in a workload the chart declares
// dapr-mandatory. Unset (local dev, fixtures, sidecar-less runs) disables the
// guard entirely — the guard is chart-signalled, never guessed.
func Required() bool { return os.Getenv("HARMOSTES_DAPR_REQUIRED") == "true" }

// Injected reports whether the sidecar injector ran for this pod. The
// injector stamps DAPR_HTTP_PORT into the container env iff it mutated the
// pod, so its presence is the authoritative "a sidecar container exists"
// signal — its absence with Required() is exactly the failurePolicy:Ignore
// skip the guard exists for.
func Injected() bool { return os.Getenv("DAPR_HTTP_PORT") != "" }

// WaitForSidecar polls the sidecar's health endpoint until it answers 2xx or
// the timeout elapses. healthz is the endpoint dapr itself recommends for
// readiness gating; 204 No Content is its healthy answer (any 2xx accepted —
// proxies in the path must not turn this into a false negative).
func WaitForSidecar(ctx context.Context, c *HTTPClient, timeout, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		lastErr = probeHealthz(ctx, c)
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dapr: sidecar at %s not healthy after %s: %w", c.BaseURL, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("dapr: sidecar wait aborted: %w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(interval):
		}
	}
}

func probeHealthz(ctx context.Context, c *HTTPClient) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1.0/healthz", nil)
	if err != nil {
		return err
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	// A probe must never hang behind the default no-timeout transport: bound
	// each attempt so a half-open sidecar socket cannot eat the whole budget.
	attemptCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := client.Do(req.WithContext(attemptCtx))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}

// StartupGuard is the startup seam for the mains: no-op unless the chart
// declared dapr mandatory; then either wait out a slow-but-injected sidecar
// or report the unfixable-here missing-injection case as ErrSidecarMissing.
func StartupGuard(ctx context.Context, c *HTTPClient, wait, interval time.Duration) error {
	if !Required() {
		return nil
	}
	if !Injected() {
		return ErrSidecarMissing
	}
	return WaitForSidecar(ctx, c, wait, interval)
}
