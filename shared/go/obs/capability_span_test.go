package obs_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"ticketing/shared/obs"
)

// The OTel server span is a SECOND sink for the same capability, and the one a
// grep for `r.URL.Path` cannot find: otelhttp sets `url.path` from inside the
// dependency (semconv/server.go), and Setup exports every span over OTLP to a
// collector this repo does not control.
//
// This test lives at the obs tier because that is where the mechanism is — the
// span processor. A service-level test would observe the fake, not the pipeline.
//
// Mutation this must catch: remove CapabilitySpanProcessor from the tracer
// provider, or stop it rewriting url.path. Verified red before the processor
// existed: `url.path = /api/access/orders/2f1e.../tickets`.
func TestServerSpanDoesNotCarryTheCapability(t *testing.T) {
	const ref = "2f1e3d4c-5b6a-4978-8899-aabbccddeeff"

	// This test covers the PROCESSOR's behaviour, using the shared construction
	// helper. It deliberately does NOT claim to prove Setup installs it — two
	// earlier versions claimed exactly that and were bypassable (ai-review F3,
	// F7). The wiring is proven on the wire, in
	// capability_setup_test.go's TestSetupExportsNoCapabilityOnTheWire.
	exp := tracetest.NewInMemoryExporter()
	tp := obs.NewTracerProviderForTest(sdktrace.NewSimpleSpanProcessor(exp))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	h := obs.MiddlewareWithTracerProvider("svc", tp,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for _, path := range []string{
		"/api/access/orders/" + ref + "/tickets",
		"/orders/" + ref + "/tickets",
		"/en/tickets/" + ref,
	} {
		exp.Reset()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
		_ = tp.ForceFlush(req.Context())

		spans := exp.GetSpans()
		if len(spans) == 0 {
			t.Fatalf("%s: no span recorded — the test would prove nothing", path)
		}
		var sawURLPath bool
		for _, s := range spans {
			for _, a := range s.Attributes {
				if a.Key == attribute.Key("url.path") {
					sawURLPath = true
					if !strings.Contains(a.Value.AsString(), ":capability") {
						t.Errorf("%s: url.path not sanitized: %s", path, a.Value.AsString())
					}
				}
				if strings.Contains(a.Value.AsString(), ref) {
					t.Errorf("%s: span attribute %s leaks the reference: %s", path, a.Key, a.Value.AsString())
				}
			}
		}
		// Guards against the vacuous version of this test: if otelhttp ever
		// stopped emitting url.path, "no leak" would be true for the wrong
		// reason and the processor could be deleted unnoticed.
		if !sawURLPath {
			t.Errorf("%s: no url.path attribute at all — this test can no longer observe the leak", path)
		}
	}
}

// Ordinary routes must reach the collector unchanged: the processor is scoped to
// declared capability shapes, and a trace whose paths were all rewritten would be
// useless for debugging (COS #1, COS #3).
func TestServerSpanKeepsOrdinaryPathsIntact(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	// Built through the PRODUCTION construction path (the same helper Setup
	// calls), not by installing the processor here: a test that assembles its
	// own chain stays green when the processor is deleted from Setup, which is
	// exactly what happened (ai-review F3).
	tp := obs.NewTracerProviderForTest(sdktrace.NewSimpleSpanProcessor(exp))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	h := obs.MiddlewareWithTracerProvider("svc", tp,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	_ = tp.ForceFlush(req.Context())

	var found bool
	for _, s := range exp.GetSpans() {
		for _, a := range s.Attributes {
			if a.Key == attribute.Key("url.path") {
				found = true
				if a.Value.AsString() != "/healthz" {
					t.Errorf("ordinary path was rewritten: %s", a.Value.AsString())
				}
			}
		}
	}
	if !found {
		t.Error("no url.path attribute — cannot confirm ordinary paths survive")
	}
}
