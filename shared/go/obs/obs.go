// Package obs is the platform observability foundation: structured JSON
// logging with trace correlation, and W3C trace propagation on outbound
// HTTP calls. OTLP export setup lives in Setup (see setup.go).
package obs

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

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

// clientTimeout bounds every cross-service call. Unbounded, a hung downstream could hold a
// checkout handler open past commerce's 2-minute recovery grace period — the window in
// which a live checkout and the recovery runner can both act on one order (TKT-116). It
// sits above the 15s payments->Stripe bound and below the 120s grace, and callers needing
// something stricter still pass a per-request context (the gateway's health fan-out does).
const clientTimeout = 30 * time.Second

// ClientTimeout is the bound above, exported for callers that must SIZE something against
// the calls this client makes rather than guess at them.
//
// The motivating case is a worker lease: commerce's reversal reconciler leases a batch of
// refunds and drives each through this client, so its lease has to outlast batch × calls ×
// this. Its first version borrowed another worker's 10s constant and produced a lease three
// times shorter than the work it protected (TKT-163 ai-review F1). A copied number cannot
// track a change here; this can.
const ClientTimeout = clientTimeout

// crossServiceTransport is the pooled base transport under every cross-service call.
//
// http.DefaultTransport leaves MaxIdleConnsPerHost at the package default of 2
// (net/http DefaultMaxIdleConnsPerHost), so past two concurrent requests to one
// upstream a caller opens and discards a TCP connection per request. The gateway
// already makes exactly this argument for its proxy transport, one hop earlier
// (gateway/cmd/gateway/main.go) — and the same reasoning was never applied here,
// where commerce calls inventory and payments on the CHECKOUT path, at the moment
// concurrency is highest (TKT-308).
//
// The numbers are the gateway's, deliberately rather than independently derived:
// two different ceilings on two hops of one request would be a number to reconcile
// every time either moves, and nothing in the measurement argued for a different
// one. If a load profile ever says otherwise, move both.
//
// PACKAGE-LEVEL rather than per Client() call. Two reasons, and the weaker one is
// stated first because it is the one that sounds compelling and is not load-bearing
// here: the idle pool lives in the transport, so a fresh transport per client is a
// fresh empty pool, and a caller building a client per request would reuse nothing
// however large the ceilings are. Every caller in this repo builds one at startup
// and holds it (grep obs.Client()), so that failure is currently hypothetical.
//
// The reason that DOES bite today: callers build SEVERAL long-lived clients — access
// makes one for its consumer and another for redelivery, commerce one for the API
// server and more for its runners — all talking to the same handful of upstreams. A
// transport each would give each its own pool and fragment the reuse this ticket
// exists to create. One shared transport is also what the gateway's comment argues
// for on its own hop: the pool is keyed per host, so upstreams already get
// independent pools and a second transport only splits them.
//
// Cloned rather than built fresh so the dialer, TLS config and HTTP/2 setup stay
// exactly as the standard library ships them; only the two idle ceilings move.
var crossServiceTransport = func() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 100
	t.MaxIdleConns = 500
	return otelhttp.NewTransport(t, otelhttp.WithPropagators(propagator))
}()

// Client returns an http.Client that injects the W3C traceparent header
// from the request context. All cross-service calls go through this.
//
// The client is cheap to construct and the transport under it is shared, so callers
// may keep one or make one per call — connection reuse does not depend on which,
// which was not true before TKT-308.
func Client() *http.Client {
	return &http.Client{
		Timeout:   clientTimeout,
		Transport: crossServiceTransport,
	}
}

// Middleware wraps an http.Handler with OTel server instrumentation
// (span per request + http.server.* metrics).
//
// The spans this produces carry the raw request path as `url.path`, set inside
// otelhttp. Setup installs CapabilitySpanProcessor on the tracer provider so a
// capability-bearing segment never reaches the exporter (TKT-202, ADR-012).
func Middleware(service string, next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, service)
}

// MiddlewareWithTracerProvider is Middleware bound to an explicit provider,
// so a test can observe the exported spans without touching global state.
// Production code uses Middleware and the provider Setup installs.
func MiddlewareWithTracerProvider(service string, tp trace.TracerProvider, next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, service, otelhttp.WithTracerProvider(tp))
}
