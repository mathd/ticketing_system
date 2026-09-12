package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"ticketing/shared/obs"
)

// The staff-login telemetry, asserted at the BOUNDARIES the values cross on
// their way out — the serialized log bytes, the exported metric attributes, the
// exported span attributes — and not on fields of the emitter.
//
// That distinction is the point (ADR-012 § TKT-202, and AGENTS.md on a harness
// that cannot catch what it hunts). A test that inspected the emitter would stay
// green if the emitter were correct and simply never called, or if a value were
// added to a sink the emitter does not own.
//
// WHAT THIS DOES NOT COVER, stated rather than implied: these tests build the
// server themselves, so they catch the emitter being removed from THIS wiring
// and not from `cmd/catalog/main.go`. Deleting `WithStaffLoginTelemetry` there
// ships a binary that emits nothing and leaves this file green. That is the
// exact edit TKT-202's F3/F7 is about, and closing it needs a test over the real
// main, which catalog has none of today — nor does access, whose scanner
// telemetry has the same gap. Not invented here for one emitter: it belongs in a
// ticket that gives every service's main the same treatment.

type staffTelemetryHarness struct {
	router http.Handler
	logs   *bytes.Buffer
	reader *sdkmetric.ManualReader
	spans  *tracetest.InMemoryExporter
	store  *fakeStore
}

func newStaffTelemetryHarness(t *testing.T) *staffTelemetryHarness {
	t.Helper()
	return newStaffTelemetryHarnessWithEmitter(t, true)
}

// withEmitter false builds the identical stack with no telemetry attached, which
// is what the span diff uses as its control.
func newStaffTelemetryHarnessWithEmitter(t *testing.T, withEmitter bool) *staffTelemetryHarness {
	t.Helper()

	logs := &bytes.Buffer{}
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")

	telemetry, err := NewStaffLoginTelemetry(obs.NewLogger("catalog", logs), meter)
	if err != nil {
		t.Fatal(err)
	}

	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(spans)))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	st := newFakeStore()
	srv := NewServer(st, &fakePublisher{}, obs.NewLogger("catalog", io.Discard), "test-internal-token", testStaffWriteToken).
		WithOrganizerAssertionKey(testOrganizerAssertionKey)
	if withEmitter {
		srv = srv.WithStaffLoginTelemetry(telemetry)
	}

	inner, err := NewRouter(srv, true)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	return &staffTelemetryHarness{
		router: obs.MiddlewareWithTracerProvider("catalog", tp, inner),
		logs:   logs, reader: reader, spans: spans, store: st,
	}
}

func (h *staffTelemetryHarness) login(t *testing.T, identifier, password, source string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(StaffCredentials{Identifier: identifier, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://catalog.local/staff/authenticate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(staffWriteHeader, testStaffWriteToken)
	req.Header.Set("X-Forwarded-For", source)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

// emitted returns every byte this ticket's telemetry put into the two sinks
// COS-2 governs — the serialized log bytes and the metric attributes — plus the
// span attributes THIS emitter set. Asserting over whole serialized output
// rather than a field allowlist is the point: an allowlist checks the fields you
// thought of.
//
// It deliberately EXCLUDES the span attributes the shared obs middleware sets.
// Those are OTel HTTP semantic conventions (`client.address`, `network.peer.*`,
// `url.path`) applied to every request in every service, and `client.address`
// therefore carries the client IP on this route exactly as it does on all the
// others. That is a repo-wide decision about `shared/go/obs`, not something this
// ticket introduces or is scoped to change — COS-2 names metric attributes and
// log fields, and this function honours that boundary rather than quietly
// widening it. `TestStaffLoginSpanCarriesOnlyThisEmittersAttributes` below pins
// what the emitter itself adds to the span, which IS this ticket's to control.
func (h *staffTelemetryHarness) emitted(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(h.logs.String())

	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			b.WriteString(m.Name)
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					fmt.Fprintf(&b, " %s=%v", kv.Key, kv.Value.AsInterface())
				}
			}
		}
	}
	for k, v := range h.spanAttrsSetByThisEmitter(t) {
		fmt.Fprintf(&b, " %s=%v", k, v)
	}
	return b.String()
}

// spanAttrsSetByThisEmitter returns only the keys this ticket's code adds to the
// span, so a test can assert their values without asserting anything about the
// middleware's semantic-convention attributes.
// It DIFFS against a control request that ran without the emitter, so the keys
// it returns are exactly those the emitter added. Nothing here names an
// attribute, in either direction, and that is the point.
//
// Two worse versions were tried first and both are worth recording, because the
// mistake is easy to repeat:
//
//   - An ALLOWLIST of the emitter's three keys. Blind by construction: a fourth
//     attribute was discarded before the assertion, so the test claiming to
//     reject one could not (ai-review pass 1).
//   - SUBTRACTING a hardcoded list of the middleware's keys. Not blind, but
//     coupled to `otelhttp`'s attribute set — a library this repo does not own,
//     whose next upgrade would break this test for a reason unrelated to the
//     code under test.
//
// The control request depends only on OUR wiring: whatever `otelhttp` sets, it
// sets for both requests and cancels out.
func (h *staffTelemetryHarness) spanAttrsSetByThisEmitter(t *testing.T) map[string]string {
	t.Helper()

	baseline := map[string]bool{}
	for _, s := range newStaffTelemetryControl(t).spans.GetSpans() {
		for _, kv := range s.Attributes {
			baseline[string(kv.Key)] = true
		}
	}

	got := map[string]string{}
	for _, s := range h.spans.GetSpans() {
		for _, kv := range s.Attributes {
			if baseline[string(kv.Key)] {
				continue
			}
			got[string(kv.Key)] = kv.Value.String()
		}
	}
	return got
}

// newStaffTelemetryControl builds the same stack WITHOUT the emitter and drives
// one request through it, so its span carries the middleware's attributes and
// nothing else.
func newStaffTelemetryControl(t *testing.T) *staffTelemetryHarness {
	t.Helper()
	h := newStaffTelemetryHarnessWithEmitter(t, false)
	h.login(t, "control@example.test", "pw", "203.0.113.1")
	return h
}

// COS-2. Every request-controlled value is a distinct canary, and EVERY one of
// them is asserted absent from EVERY sink.
//
// Distinct canaries per position, not one shared value: TKT-202's harness ran
// 576 arrangements and never saw the leak it was hunting, because it put a
// harmless placeholder in the position that leaked. A shared canary has the same
// blindness in miniature — it cannot say WHICH position leaked, and a test that
// cannot say which position leaked is one nobody trusts enough to act on.
//
// Mutation that must make this RED: add any of these values, or a hash or a
// prefix of one, to the log record, the metric attributes or the span.
func TestStaffLoginTelemetryLeaksNothingRequestControlled(t *testing.T) {
	h := newStaffTelemetryHarness(t)

	const (
		rawIdentifier = "CanaryIdentifier@Example.Test"
		password      = "canary-password-value"
		source        = "203.0.113.251"
	)
	// The NORMALIZED form is its own canary: it is what the limiter actually keys
	// on, so an emitter that logged "the key" rather than "the input" would leak
	// this one while leaving the raw value absent.
	normalizedIdentifier := strings.ToLower(rawIdentifier)

	h.login(t, rawIdentifier, password, source)

	out := h.emitted(t)
	if out == "" {
		t.Fatal("no telemetry was emitted at all; this test cannot observe a leak it never produced")
	}

	for _, canary := range []struct{ name, value string }{
		{"raw identifier", rawIdentifier},
		{"normalized identifier", normalizedIdentifier},
		{"password", password},
		{"client address", source},
	} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(canary.value)) {
			t.Errorf("the %s (%q) appears in a sink: %s", canary.name, canary.value, out)
		}
	}

	// And the positive half: the record is actually there, so the absences above
	// are absences from a REAL record rather than from silence.
	if !strings.Contains(out, staffLoginSurface) || !strings.Contains(out, staffLoginAllowed) {
		t.Errorf("the allowed decision was not recorded in every sink: %s", out)
	}
}

// COS-1 and COS-2's cardinality half. Many different identifiers and sources
// must produce exactly the fixed attribute sets, never a series per caller.
//
// Mutation that must make this RED: add any subject- or source-derived label,
// including a hash or a truncation.
func TestStaffLoginMetricCarriesOnlyFixedAttributes(t *testing.T) {
	h := newStaffTelemetryHarness(t)

	for i := range 12 {
		h.login(t, fmt.Sprintf("user-%d@example.test", i), "pw", fmt.Sprintf("198.51.100.%d", i+1))
	}

	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}

	var series int
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != staffLoginAttemptsMetric {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want a Sum[int64]", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				series++
				got := map[string]string{}
				for _, kv := range dp.Attributes.ToSlice() {
					got[string(kv.Key)] = kv.Value.String()
				}
				if len(got) != 3 {
					t.Errorf("data point has %d attributes %v, want exactly surface, subject_type and outcome: "+
						"any extra key is a series per caller", len(got), got)
				}
				if got["surface"] != staffLoginSurface || got["subject_type"] != staffLoginSubject {
					t.Errorf("fixed attributes are %v", got)
				}
			}
		}
	}
	// Twelve distinct identifiers and twelve distinct sources, all allowed, must
	// collapse to ONE series. More than one means something per-caller is a label.
	if series != 1 {
		t.Errorf("12 callers produced %d series, want 1: this surface is anonymous and its metric must be aggregate", series)
	}
}

// COS-1. An operator must be able to tell the two refusals apart, because they
// mean different things: throttled_subject is one account being ground,
// throttled_source is one client walking a list.
//
// Mutation that must make this RED: report both refusals with one value, or emit
// only on refusal so the allowed case disappears.
func TestStaffLoginTelemetryDistinguishesTheTwoBudgets(t *testing.T) {
	t.Run("subject", func(t *testing.T) {
		h := newStaffTelemetryHarness(t)
		for range staffAuthSubjectBurst + 1 {
			h.login(t, "ground@example.test", "pw", "203.0.113.5")
		}
		if out := h.emitted(t); !strings.Contains(out, staffLoginThrottledSubject) {
			t.Errorf("a subject-budget refusal was not recorded as %s: %s", staffLoginThrottledSubject, out)
		}
	})

	t.Run("source", func(t *testing.T) {
		h := newStaffTelemetryHarness(t)
		for i := range staffAuthSourceBurst + 1 {
			h.login(t, fmt.Sprintf("walk-%d@example.test", i), "pw", "203.0.113.6")
		}
		out := h.emitted(t)
		if !strings.Contains(out, staffLoginThrottledSource) {
			t.Errorf("a source-budget refusal was not recorded as %s", staffLoginThrottledSource)
		}
		// And it must NOT be reported as a subject refusal: the two answers send an
		// operator to different places.
		if strings.Contains(out, staffLoginThrottledSubject) {
			t.Errorf("a source-budget refusal was also recorded as %s, which sends an operator "+
				"looking for an account being ground when one client is walking a list", staffLoginThrottledSubject)
		}
	})
}

// What THIS emitter adds to the span, and nothing more.
//
// The span also carries the shared middleware's OTel semantic-convention
// attributes, including `client.address` — the client IP, on this route exactly
// as on every other route in every service. That is deliberately NOT asserted
// here: it is a property of `shared/go/obs` and a repo-wide decision, and COS-2
// governs metric attributes and log fields. Narrowing quietly would be the
// mistake; this test says which half is this ticket's.
//
// Mutation that must make this RED: add a fourth attribute to the emitter's span
// set, or change one of the three values.
func TestStaffLoginSpanCarriesOnlyThisEmittersAttributes(t *testing.T) {
	h := newStaffTelemetryHarness(t)
	h.login(t, "spanned@example.test", "pw", "203.0.113.77")

	got := h.spanAttrsSetByThisEmitter(t)
	want := map[string]string{
		"surface":      staffLoginSurface,
		"subject_type": staffLoginSubject,
		"outcome":      staffLoginAllowed,
	}
	if len(got) != len(want) {
		t.Fatalf("the emitter set %d span attributes %v, want exactly %v", len(got), got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("span attribute %s = %q, want %q", k, got[k], v)
		}
	}
}

// How long a subject lockout actually lasts, pinned with a controlled clock.
//
// The bucket refills CONTINUOUSLY at burst/window and admits a request as soon
// as one token exists, so a victim whose bucket was emptied is admitted again
// after `window/burst` — 90 seconds here — not after the full 15-minute window.
// That is the same number `retryAfterSeconds` already advertises.
//
// This test exists because the ADR got it wrong in both directions before review
// caught it: first claiming the short-circuit prevented cross-source denial at
// all, then that the denial lasted a whole window. A sentence can drift; a
// clock-controlled assertion cannot. (ai-review passes 1 and 2.)
//
// Mutation that must make this RED: change the refill so a token does not arrive
// by window/burst, or make Allow refuse while a token exists.
func TestASubjectLockoutRecoversAfterOneTokenRefills(t *testing.T) {
	const victim = "recovers@example.test"
	now := time.Now()
	clock := func() time.Time { return now }

	st := newFakeStore()
	srv := NewServer(st, &fakePublisher{}, obs.NewLogger("catalog", io.Discard),
		"test-internal-token", testStaffWriteToken).
		WithOrganizerAssertionKey(testOrganizerAssertionKey).
		WithClock(clock)
	handler, err := NewRouter(srv, true)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	attempt := func(source string) int {
		body, err := json.Marshal(StaffCredentials{Identifier: victim, Password: "guess"})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "http://catalog.local/staff/authenticate", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(staffWriteHeader, testStaffWriteToken)
		req.Header.Set("X-Forwarded-For", source)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// The attacker empties the victim's subject bucket from its own source.
	for i := range staffAuthSubjectBurst {
		if code := attempt("203.0.113.40"); code != http.StatusUnauthorized {
			t.Fatalf("attacker attempt %d: status %d, want 401 while the budget holds", i+1, code)
		}
	}

	// The victim, from a completely different source, is refused: the bucket is
	// keyed on the identifier alone. This is the cross-source denial the ADR
	// documents.
	if code := attempt("198.51.100.40"); code != http.StatusTooManyRequests {
		t.Fatalf("the victim got %d from a fresh source, want 429: the subject bucket is "+
			"keyed on the identifier alone, so an attacker CAN deny a named account", code)
	}

	// Just short of one token's worth of refill, still refused.
	perToken := staffAuthSubjectWindow / time.Duration(staffAuthSubjectBurst)
	now = now.Add(perToken - time.Second)
	if code := attempt("198.51.100.41"); code != http.StatusTooManyRequests {
		t.Fatalf("the victim got %d one second before a token refills, want 429", code)
	}

	// One token's worth later, admitted. The `- time.Second` above already
	// consumed part of the interval, so advance the remainder plus a margin.
	now = now.Add(2 * time.Second)
	if code := attempt("198.51.100.42"); code != http.StatusUnauthorized {
		t.Fatalf("the victim got %d after %s, want 401: the bucket refills continuously, so a "+
			"lockout lasts one token's worth of time and not the whole window", code, perToken)
	}

	// And that interval is exactly what the endpoint advertises, so the ADR, the
	// header and the behaviour cannot drift apart.
	if got, want := retryAfterSeconds, strconv.Itoa(int(perToken.Seconds())); got != want {
		t.Errorf("Retry-After advertises %s but a token refills in %s", got, want)
	}
}
