package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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
	tp := newTracerProvider(sdktrace.NewSimpleSpanProcessor(exp), nil)
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	h := MiddlewareWithTracerProvider("svc", tp,
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
	tp := newTracerProvider(sdktrace.NewSimpleSpanProcessor(exp), nil)
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	h := MiddlewareWithTracerProvider("svc", tp,
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

func TestClientSpanDoesNotCarryCapabilityOrSignedQuerySecret(t *testing.T) {
	const ref = "2f1e3d4c-5b6a-4978-8899-aabbccddeeff"
	const secretQuery = "sig=super-secret-signature&token=secret-token"

	exp := tracetest.NewInMemoryExporter()
	tp := newTracerProvider(sdktrace.NewSimpleSpanProcessor(exp), nil)
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	var upstreamReceivedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamReceivedURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: WrapTransportWithTracerProvider(http.DefaultTransport, tp),
	}

	for _, tc := range []struct {
		name     string
		path     string
		query    string
		hasCap   bool
		hasQuery bool
	}{
		{
			name:     "capability order tickets with query",
			path:     "/api/access/orders/" + ref + "/tickets",
			query:    "?" + secretQuery,
			hasCap:   true,
			hasQuery: true,
		},
		{
			name:     "ordinary path with query",
			path:     "/healthz",
			query:    "?" + secretQuery,
			hasCap:   false,
			hasQuery: true,
		},
		{
			name:     "capability with userinfo and fragment",
			path:     "/api/access/orders/" + ref + "/tickets",
			query:    "?" + secretQuery + "#secret-fragment",
			hasCap:   true,
			hasQuery: true,
		},
		{
			name:     "capability with encoded rawpath",
			path:     "/api/access/orders/%32%66%31%65%33%64%34%63-5b6a-4978-8899-aabbccddeeff/tickets",
			query:    "?" + secretQuery,
			hasCap:   true,
			hasQuery: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exp.Reset()
			upstreamReceivedURL = ""

			targetURL := srv.URL + tc.path + tc.query
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, targetURL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			_ = tp.ForceFlush(t.Context())

			// Upstream must have received the request line unchanged (fragments are never sent over the wire)
			wantUpstreamPathAndQuery := tc.path + strings.Split(tc.query, "#")[0]
			if upstreamReceivedURL != wantUpstreamPathAndQuery {
				t.Fatalf("upstream received %q, want %q; request URL must not be modified", upstreamReceivedURL, wantUpstreamPathAndQuery)
			}

			spans := exp.GetSpans()
			if len(spans) == 0 {
				t.Fatal("no client span recorded")
			}

			secretMarkers := []string{ref, "super-secret", "secret-token", "secret-fragment", "secretpass"}

			var sawURLFull bool
			for _, s := range spans {
				// Inspect status description
				for _, marker := range secretMarkers {
					if strings.Contains(s.Status.Description, marker) {
						t.Fatalf("span status description leaks secret %s: %q", marker, s.Status.Description)
					}
				}
				// Inspect events
				for _, ev := range s.Events {
					for _, a := range ev.Attributes {
						for _, marker := range secretMarkers {
							if strings.Contains(a.Value.AsString(), marker) {
								t.Fatalf("span event attribute %s leaks secret %s: %q", a.Key, marker, a.Value.AsString())
							}
						}
					}
				}
				// Inspect attributes
				for _, a := range s.Attributes {
					val := a.Value.AsString()
					for _, marker := range secretMarkers {
						if strings.Contains(val, marker) {
							t.Fatalf("span attribute %s leaks secret marker %s: %s", a.Key, marker, val)
						}
					}
					if a.Key == attribute.Key("url.full") {
						sawURLFull = true
						if tc.hasCap && !strings.Contains(val, ":capability") {
							t.Fatalf("url.full does not contain sanitized :capability: %s", val)
						}
						if !tc.hasCap && !strings.Contains(val, tc.path) {
							t.Fatalf("url.full lost ordinary path %s: %s", tc.path, val)
						}
					}
					if a.Key == attribute.Key("url.path") {
						if !tc.hasCap && val != tc.path {
							t.Fatalf("url.path lost ordinary path: %s", val)
						}
					}
				}
			}
			if !sawURLFull {
				t.Fatal("no url.full attribute found on client span")
			}
		})
	}
}

func TestClientSpanUnparseableURLFailsClosed(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := newTracerProvider(sdktrace.NewSimpleSpanProcessor(exp), nil)
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	for _, invalid := range []string{
		"http://[::1]:namedport/path",
		"mailto:secret@domain.com",
		"urn:secret:token-data",
		"scheme:opaque_payload",
	} {
		t.Run(invalid, func(t *testing.T) {
			exp.Reset()
			tr := tp.Tracer("test")
			_, span := tr.Start(t.Context(), "client-test")
			span.SetAttributes(
				attribute.String("url.full", invalid),
				attribute.String("url.path", "/ordinary"),
			)
			span.End()
			_ = tp.ForceFlush(t.Context())

			spans := exp.GetSpans()
			if len(spans) == 0 {
				t.Fatal("no span recorded")
			}
			for _, a := range spans[0].Attributes {
				if a.Key == attribute.Key("url.full") {
					if a.Value.AsString() != "" {
						t.Fatalf("url.full = %q, want empty string (fail closed on unparseable/opaque URL %q)", a.Value.AsString(), invalid)
					}
				}
			}
		})
	}
}

func TestClientSpanTransportErrorDoesNotLeakSecretInStatusOrEvents(t *testing.T) {
	const secret = "tkt336-secret-leak-marker"
	exp := tracetest.NewInMemoryExporter()
	tp := newTracerProvider(sdktrace.NewSimpleSpanProcessor(exp), nil)
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	client := &http.Client{
		Transport: WrapTransportWithTracerProvider(http.DefaultTransport, tp),
	}

	// Dial an unreachable port with secret in query
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:1/test?token="+secret, nil)
	_, _ = client.Do(req)
	_ = tp.ForceFlush(t.Context())

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("no client span recorded")
	}
	s := spans[0]
	if strings.Contains(s.Status.Description, secret) {
		t.Fatalf("span status leaks secret: %q", s.Status.Description)
	}
	for _, ev := range s.Events {
		for _, a := range ev.Attributes {
			if strings.Contains(a.Value.AsString(), secret) {
				t.Fatalf("span event attribute %s leaks secret: %q", a.Key, a.Value.AsString())
			}
		}
	}
}
