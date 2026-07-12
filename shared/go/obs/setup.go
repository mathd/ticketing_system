package obs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0" // attributes only (schemaless resource)
)

// Setup configures the OTel SDK for a service: OTLP/HTTP export of traces,
// metrics and logs (endpoint from OTEL_EXPORTER_OTLP_ENDPOINT), the W3C
// propagator installed globally, and a slog.Logger whose records go BOTH to
// stdout as JSON (asserted by the smoke suite) and to the collector via the
// otelslog bridge (visible in Loki/Grafana).
//
// The returned shutdown func flushes all exporters; call it on exit.
func Setup(ctx context.Context, service string) (*slog.Logger, func(context.Context) error, error) {
	// Schemaless so this never conflicts with the SDK default resource's
	// schema version as the otel dependency moves (latest-by-default).
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(semconv.ServiceName(service)))
	if err != nil {
		return nil, nil, err
	}

	traceExp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagator)

	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res))
	otel.SetMeterProvider(mp)

	logExp, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res))

	logger := newFanoutLogger(service, os.Stdout, lp)

	shutdown := func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
	}
	return logger, shutdown, nil
}

// newFanoutLogger builds the dual-destination logger: stdout JSON with
// trace correlation + the OTel log bridge.
func newFanoutLogger(service string, w io.Writer, lp *sdklog.LoggerProvider) *slog.Logger {
	stdout := &traceHandler{Handler: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})}
	bridge := otelslog.NewHandler(service, otelslog.WithLoggerProvider(lp))
	return slog.New(&fanoutHandler{handlers: []slog.Handler{stdout, bridge}}).With("service", service)
}

type fanoutHandler struct {
	handlers []slog.Handler
}

func (f *fanoutHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, lvl) {
			return true
		}
	}
	return false
}

func (f *fanoutHandler) Handle(ctx context.Context, rec slog.Record) error {
	var errs []error
	for _, h := range f.handlers {
		if h.Enabled(ctx, rec.Level) {
			errs = append(errs, h.Handle(ctx, rec.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (f *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &fanoutHandler{handlers: next}
}

func (f *fanoutHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		next[i] = h.WithGroup(name)
	}
	return &fanoutHandler{handlers: next}
}
