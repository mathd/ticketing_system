package obs

import (
	"context"
	"net/url"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// The span sink (TKT-202).
//
// # Why this exists at all
//
// Sanitising the two slog call sites is not enough, and the reason is invisible
// to any grep of this repo. obs.Middleware wraps every service in
// otelhttp.NewHandler, and otelhttp sets the raw request path as a span
// attribute from INSIDE the dependency:
//
//	// otelhttp/internal/semconv/server.go
//	if req.URL != nil && req.URL.Path != "" {
//	    attrs = append(attrs, semconv.URLPath(req.URL.Path))
//	}
//
// Setup then exports those spans over OTLP. So before this processor existed, a
// guest request put the order reference on a span and shipped it to a collector
// whose retention this repo does not control — a strictly worse destination than
// stdout. Executed and observed, not inferred: a probe against the real
// middleware emitted
// `url.path = /api/access/orders/2f1e3d4c-5b6a-4978-8899-aabbccddeeff/tickets`.
//
// # Why a span processor and not an otelhttp option
//
// otelhttp exposes no option that suppresses or rewrites url.path on server
// spans. WithSpanOptions only ADDS attributes; there is no attribute filter, and
// WithSpanNameFormatter renames the span rather than touching attributes. The
// supported lever is the SDK: OnStart runs after otelhttp has set its attributes
// and before export, and SetAttributes on the same key overwrites it.
//
// # One table, not two
//
// This calls the same SanitizedPath as the loggers, against the same declared
// route table. Two tables would be two sources of truth, and COS #2 — "the next
// capability URL inherits the rule" — fails the moment they drift.

// newTracerProvider builds the tracer provider Setup installs.
//
// It exists so the capability regression test can exercise the SAME construction
// production uses, rather than assembling its own provider and installing the
// processor by hand. That distinction is not cosmetic: with the test building
// its own chain, deleting the processor from Setup left the whole suite green
// while every service exported the capability (ai-review F3). The test that
// matters is the one that fails when the WIRING is removed, not when the
// processor is broken.
func newTracerProvider(exportProcessor sdktrace.SpanProcessor, res *resource.Resource) *sdktrace.TracerProvider {
	// The export processor is wrapped, not replaced: otelhttp puts the raw
	// request path on every server span, so without this a capability-bearing
	// segment reaches the collector on every guest request (TKT-202, ADR-012).
	// One install here covers all six services.
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithSpanProcessor(CapabilitySpanProcessor(exportProcessor)),
	}
	if res != nil {
		opts = append(opts, sdktrace.WithResource(res))
	}
	return sdktrace.NewTracerProvider(opts...)
}

// capabilityURLPathKey is the semconv attribute otelhttp writes the raw request
// path to. Matched by name deliberately: the alternative is importing the
// semconv package at a pinned version, which would silently stop matching when
// the dependency's schema version moves — and failing open here means leaking.
const capabilityURLPathKey = attribute.Key("url.path")

// capabilityURLFullKey is the semconv attribute otelhttp writes the full client
// request URL to.
const capabilityURLFullKey = attribute.Key("url.full")

// capabilitySpanProcessor sanitises capability-bearing paths on span attributes
// before they are exported.
type capabilitySpanProcessor struct {
	sdktrace.SpanProcessor
}

// CapabilitySpanProcessor wraps next so that any declared capability segment on
// a span's url.path is replaced before export. Ordinary paths are untouched.
//
// It wraps rather than replaces so the batching/export behaviour of the
// underlying processor is preserved exactly.
func CapabilitySpanProcessor(next sdktrace.SpanProcessor) sdktrace.SpanProcessor {
	return capabilitySpanProcessor{SpanProcessor: next}
}

// OnStart rewrites url.path if it carries a capability.
//
// Attributes present at span creation, such as server url.path, are sanitized
// here before the next processor sees them.
// Export-time attributes (such as client url.full set after creation by otelhttp)
// are sanitized in OnEnd before handing off to the exporter.
func (p capabilitySpanProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	for _, attr := range s.Attributes() {
		if attr.Key != capabilityURLPathKey {
			continue
		}
		raw := attr.Value.AsString()
		if sanitized := SanitizedPath(raw); sanitized != raw {
			s.SetAttributes(capabilityURLPathKey.String(sanitized))
		}
		break
	}
	p.SpanProcessor.OnStart(ctx, s)
}

type sanitizedReadOnlySpan struct {
	sdktrace.ReadOnlySpan
	attributes []attribute.KeyValue
}

func (s sanitizedReadOnlySpan) Attributes() []attribute.KeyValue {
	return s.attributes
}

func sanitizeURLFull(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		// Fail closed on unparseable or opaque URLs (e.g. scheme:opaque_payload).
		return ""
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	u.User = nil
	u.Path = SanitizedPath(u.Path)
	u.RawPath = ""
	return u.String()
}

// OnEnd sanitizes url.path and url.full at export time using a ReadOnlySpan wrapper.
// otelhttp sets client url.full after span creation, so OnEnd is the export-time
// defense-in-depth seam.
func (p capabilitySpanProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	attrs := s.Attributes()
	modified := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		switch a.Key {
		case capabilityURLPathKey:
			raw := a.Value.AsString()
			if sanitized := SanitizedPath(raw); sanitized != raw {
				modified = append(modified, capabilityURLPathKey.String(sanitized))
				continue
			}
		case capabilityURLFullKey:
			raw := a.Value.AsString()
			sanitized := sanitizeURLFull(raw)
			modified = append(modified, capabilityURLFullKey.String(sanitized))
			continue
		}
		modified = append(modified, a)
	}
	p.SpanProcessor.OnEnd(sanitizedReadOnlySpan{
		ReadOnlySpan: s,
		attributes:   modified,
	})
}
