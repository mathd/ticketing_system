package obs_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ticketing/shared/obs"
)

// The end-to-end guard: drive the REAL Setup and read the bytes it puts on the
// wire (ai-review F3, F7).
//
// Three versions of the span regression test were bypassable before this one.
// The first built its own tracer provider and installed the processor by hand —
// deleting the processor from Setup left it green. The second called a helper
// that Setup also calls — replacing that call inside Setup left it green. Both
// tested the processor; neither tested the WIRING, which was the finding.
//
// This version has nothing to route around: it points the OTLP exporter at a
// local HTTP server, calls Setup exactly as a service main() does, serves one
// guest request, and greps the exported protobuf payload for the reference. If
// any future edit stops Setup sanitising — by removing the processor, bypassing
// the helper, reordering the provider options, or replacing the construction
// outright — the reference appears in these bytes and this test fails.
//
// Payload inspected as raw bytes rather than decoded: the reference is an ASCII
// UUID and protobuf stores strings verbatim, so a substring search is exact
// enough for "did this leave the process", and it cannot go stale against a
// schema change in the OTLP proto.
func TestSetupExportsNoCapabilityOnTheWire(t *testing.T) {
	const ref = "2f1e3d4c-5b6a-4978-8899-aabbccddeeff"

	var mu sync.Mutex
	var payloads [][]byte
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		payloads = append(payloads, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)

	// Setup reads its endpoint from the environment, exactly as a service does.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")

	_, shutdown, err := obs.Setup(t.Context(), "svc")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	h := obs.Middleware("svc", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/api/access/orders/"+ref+"/tickets", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	// Shutdown flushes the batch processor, which is what actually posts.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Logf("shutdown returned %v (continuing — the assertion is on what was already sent)", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(payloads) == 0 {
		t.Fatal("the collector received nothing — this test cannot observe the export it exists to check")
	}

	var sawSpanPayload bool
	for _, p := range payloads {
		body := string(p)
		// Confirms the request's span really is in these bytes, so a clean
		// result cannot come from having exported no span at all.
		if strings.Contains(body, "url.path") {
			sawSpanPayload = true
			if !strings.Contains(body, ":capability") {
				t.Errorf("exported span carries an unsanitized url.path")
			}
		}
		if strings.Contains(body, ref) {
			t.Errorf("THE GUEST REFERENCE LEFT THE PROCESS: it appears in an OTLP payload " +
				"posted to the collector (TKT-202, ADR-012)")
		}
	}
	if !sawSpanPayload {
		t.Error("no exported payload carried a url.path attribute — the assertion above is vacuous; " +
			"fix this test rather than deleting it")
	}
}
