package observability

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// TraceparentCarrierKey is the env var name carrying the W3C traceparent the
// controller stamps on a spawned worker Job, so the worker's root run-span is a
// child of the controller's reconcile span (Phase 4 trace handoff across pods).
//
// It is the env-var NAME only. The VALUE is the W3C traceparent string
// ("00-<trace-id>-<span-id>-<flags>"); at extract time it is placed under the
// W3C carrier key "traceparent" (see ContextWithTraceparent).
const TraceparentCarrierKey = "HARMOSTES_TRACEPARENT"

// TraceparentFromContext returns the W3C traceparent string for the active span
// in ctx ("" when there is no recording span — e.g. telemetry disabled). The
// controller injects this value into the worker Job it spawns so the worker's
// root span joins the controller's trace.
func TraceparentFromContext(ctx context.Context) string {
	c := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, c)
	return c.Get("traceparent")
}

// ContextWithTraceparent returns ctx with tp extracted into it as the parent
// context. When tp is "" (no handoff) ctx is returned unchanged, so the caller's
// root span becomes its own trace root — the worker's local-dev / no-Init path.
func ContextWithTraceparent(ctx context.Context, tp string) context.Context {
	if tp == "" {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": tp})
}
