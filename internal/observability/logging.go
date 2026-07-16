package observability

import (
	"context"
	"io"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
)

// traceHandler wraps an slog.Handler so each record carries the active span's
// trace_id and span_id (read from the record's context). JSON logs then join the
// trace timeline in the backend (e.g. SigNoz's log explorer links to the trace).
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs / WithGroup preserve the traceHandler wrapper across child loggers.
// Without these, slog.New(h).With(...) returns a logger backed by the embedded
// handler directly (the trace injection is silently lost).
func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}

// NewLogger returns a structured JSON logger tagged with component + namespace,
// trace-aware. It writes JSON (parseable by the collector's filelog receiver) to
// w (defaults to os.Stdout). The returned *slog.Logger is safe for concurrent use.
func NewLogger(component string, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	h := traceHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})}
	return slog.New(h).With(
		slog.String("component", component),
		slog.String("service.namespace", "harmostes"),
	)
}
