// Package obs is the platform observability foundation: structured JSON
// logging with trace correlation, and W3C trace propagation on outbound
// HTTP calls. OTLP export setup lives in Setup (see setup.go).
package obs

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// propagator is explicit so tracing works without relying on the global
// propagator having been configured (Setup also installs it globally).
var propagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{}, propagation.Baggage{},
)

// NewLogger returns a JSON slog.Logger that stamps every record with the
// service name and, when the context carries an active span, trace_id and
// span_id — the correlation contract the smoke suite asserts on.
func NewLogger(service string, w io.Writer) *slog.Logger {
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(&traceHandler{Handler: base}).With("service", service)
}

type traceHandler struct {
	slog.Handler
}

func (h *traceHandler) Handle(ctx context.Context, rec slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		rec.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, rec)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithGroup(name)}
}

// Client returns an http.Client that injects the W3C traceparent header
// from the request context. All cross-service calls go through this.
func Client() *http.Client {
	return &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport,
		otelhttp.WithPropagators(propagator))}
}

// Middleware wraps an http.Handler with OTel server instrumentation
// (span per request + http.server.* metrics).
func Middleware(service string, next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, service)
}
