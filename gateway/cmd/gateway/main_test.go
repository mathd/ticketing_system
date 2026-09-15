package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	otrace "go.opentelemetry.io/otel/trace"
	"ticketing/shared/obs"
)

type discardInfoLogger struct{}

func (discardInfoLogger) InfoContext(context.Context, string, ...any) {}

// Gateway liveness and readiness answer different questions. A running service
// may still refuse traffic while it cannot keep an operational promise. The
// gateway must preserve that distinction at the routes an operator probes.
func TestGatewayReadinessProbesDownstreamReadiness(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]int)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path]++
		mu.Unlock()

		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/readyz":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	serviceCount := 0
	for prefix, envVar := range routes {
		if !strings.HasPrefix(prefix, "/api/") {
			continue
		}
		t.Setenv(envVar, upstream.URL)
		serviceCount++
	}

	mux := http.NewServeMux()
	registerHealthRoutes(mux, discardInfoLogger{})

	for _, tc := range []struct {
		path string
		want int
	}{
		{path: "/healthz", want: http.StatusOK},
		{path: "/healthz/all", want: http.StatusOK},
		{path: "/readyz", want: http.StatusServiceUnavailable},
	} {
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if res.Code != tc.want {
			t.Errorf("GET %s status = %d, want %d", tc.path, res.Code, tc.want)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if got := seen["/healthz"]; got != serviceCount {
		t.Errorf("downstream /healthz probes = %d, want %d", got, serviceCount)
	}
	if got := seen["/readyz"]; got != serviceCount {
		t.Errorf("downstream /readyz probes = %d, want %d", got, serviceCount)
	}
}

// An encoded separator must not be able to walk past the /internal/ boundary.
//
// ServeMux matches escaped paths segment by segment, so "internal%2Fholds" is ONE
// segment and does not match the literal "internal" child — the request falls through
// to the broader /api/<svc>/ proxy registration. The proxy's Rewrite then works from
// r.URL.Path, which is already decoded, so the upstream service receives a perfectly
// ordinary /internal/holds/{id}/confirm and the edge refusal never happened.
//
// Found by the TKT-124 adversarial review. It is not specific to inventory or to the
// hold transitions: every /api/<svc>/internal/* route was reachable this way, which is
// why the guard sits in front of the whole mux rather than on one prefix.
func TestEncodedSeparatorCannotReachInternalRoutes(t *testing.T) {
	// A REAL upstream behind the REAL proxy: what the service receives is the only
	// thing that settles whether an edge refusal held. A stub that ignores the request
	// path would report "200, proxied" without ever showing what got proxied — which
	// is how a rewrite that decodes a separator would slip past this test.
	var gotPath, gotRawPath, gotRequestURI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotRawPath, gotRequestURI = r.URL.Path, r.URL.RawPath, r.RequestURI
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/inventory/internal/", http.HandlerFunc(edgeDenied))
	mux.Handle("/api/inventory/", apiProxy(upstreamURL, "/api/inventory/", true))
	guarded := denyEncodedSeparators(mux)

	const id = "00000000-0000-0000-0000-000000000001"
	for name, tc := range map[string]struct {
		path string
		want int
		why  string
	}{
		"encoded separator, lowercase": {"/api/inventory/internal%2fholds/" + id + "/confirm", http.StatusNotFound,
			"the bypass this guard exists for"},
		"encoded separator, uppercase": {"/api/inventory/internal%2Fholds/" + id + "/confirm", http.StatusNotFound,
			"case in the escape must not matter"},
		"encoded separator, another internal route": {"/api/inventory/internal%2Fslots/" + id + "/capacity-adjustments", http.StatusNotFound,
			"the bypass was never specific to the hold transitions"},
		"plain internal route": {"/api/inventory/internal/holds/" + id + "/confirm", http.StatusNotFound,
			"the ordinary edge refusal still works"},
		"public route, no escaping": {"/api/inventory/holds", http.StatusOK,
			"the guard must not break ordinary traffic"},
		// %252F decodes to the literal text "%2F" — a character sequence inside one
		// segment, not a separator. It cannot walk the boundary, so the gateway is right
		// to pass it on. The assertion below on what the upstream RECEIVED is what makes
		// this case worth having: it fails if a future rewrite ever turns it into a "/".
		"double-encoded is text, not a separator": {"/api/inventory/internal%252Fholds/" + id + "/confirm", http.StatusOK,
			"double-encoding yields no separator, so no boundary is crossed at the edge"},
	} {
		t.Run(name, func(t *testing.T) {
			gotPath, gotRawPath, gotRequestURI = "", "", ""
			res := httptest.NewRecorder()
			guarded.ServeHTTP(res, httptest.NewRequest(http.MethodPost, tc.path, nil))
			if res.Code != tc.want {
				t.Fatalf("%s: status=%d want=%d — %s", tc.path, res.Code, tc.want, tc.why)
			}
			// Whatever reached the upstream must not contain an internal route with a
			// real separator. This is the assertion that survives a rewrite change: a
			// status code says the edge answered, not what the service was asked to do.
			if strings.HasPrefix(gotPath, "/internal/") {
				t.Fatalf("%s: upstream received internal path %q (rawpath=%q requesturi=%q) — the boundary leaked",
					tc.path, gotPath, gotRawPath, gotRequestURI)
			}
		})
	}
}

// The gateway's refusal must be distinguishable from every other layer's 404, or a test
// asserting "the edge refused" is asserting nothing (ai-review pass 2, F1).
func TestEdgeDenialIsDistinguishableFromAGenericNotFound(t *testing.T) {
	res := httptest.NewRecorder()
	edgeDenied(res, httptest.NewRequest(http.MethodGet, "/api/inventory/internal/whatever", nil))

	if res.Code != http.StatusNotFound {
		t.Fatalf("status=%d want=404", res.Code)
	}
	if got := strings.TrimSpace(res.Body.String()); got != edgeDeniedBody {
		t.Fatalf("body=%q want=%q", got, edgeDeniedBody)
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type=%q want=application/json", got)
	}
	// The specific thing that must never be true again: matching what net/http, chi and
	// the services all emit.
	generic := httptest.NewRecorder()
	http.NotFound(generic, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.TrimSpace(generic.Body.String()) == strings.TrimSpace(res.Body.String()) {
		t.Fatal("the edge refusal is byte-identical to http.NotFound — provenance assertions built on it prove nothing")
	}
}

// The back-office (US-018) is a web shell like the scanner: registered at a
// non-/api/ prefix, NOT path-stripped (it serves under /admin/). This pins the
// route table so a careless edit can't drop the admin route or promote the
// storefront catch-all above it.
func TestBackofficeRoute(t *testing.T) {
	if routes["/admin/"] != "BACKOFFICE_URL" {
		t.Fatalf("gateway must proxy /admin/ to BACKOFFICE_URL, got %q", routes["/admin/"])
	}
	// The catch-all stays the storefront; longest-prefix (ServeMux) resolves
	// /admin/ ahead of / regardless of map order. The strip logic keys on the
	// /api/ prefix, so a non-/api/ web shell like /admin/ is passed through
	// intact — that behavior is exercised end-to-end by the smoke suite.
	if routes["/"] != "STOREFRONT_URL" {
		t.Fatalf("/ must remain the storefront catch-all, got %q", routes["/"])
	}
}

// A caller cannot forge the client IP that commerce rate-limits on (TKT-224).
//
// commerce keys its per-source budget on X-Forwarded-For (services/commerce/internal/api/ratelimit.go),
// which is only safe because of what this proxy does with the header: the Rewrite
// hook receives an outbound request with the inbound X-Forwarded-* already
// STRIPPED, and SetXForwarded then writes the connecting peer. A forged chain is
// discarded, not appended to — so there is no attacker-controlled prefix for the
// limiter to mistake for the client.
//
// This is asserted against the REAL proxy rather than reasoned about from the
// httputil docs, because the difference between "replaces" and "appends" is the
// difference between a limiter and a bypass, and Director (the older hook) does
// append. If a future change swaps Rewrite for Director, this test is what says
// so — and commerce's "take the last element" would then become load-bearing
// rather than belt-and-braces.
func TestAForgedXForwardedForDoesNotReachTheUpstream(t *testing.T) {
	var got []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header["X-Forwarded-For"]
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/commerce/", apiProxy(upstreamURL, "/api/commerce/", true))

	const peer = "203.0.113.9"
	for name, forged := range map[string]string{
		"no header at all":     "",
		"one forged hop":       "1.2.3.4",
		"a whole forged chain": "1.2.3.4, 5.6.7.8",
		"a forged private hop": "10.0.0.1",
	} {
		t.Run(name, func(t *testing.T) {
			got = nil
			req := httptest.NewRequest(http.MethodPost, "/api/commerce/customers", nil)
			req.RemoteAddr = peer + ":44444"
			if forged != "" {
				req.Header.Set("X-Forwarded-For", forged)
			}
			mux.ServeHTTP(httptest.NewRecorder(), req)

			if len(got) != 1 || got[0] != peer {
				t.Fatalf("upstream saw X-Forwarded-For %#v, want exactly [%q] — a forged hop survived, "+
					"so commerce's per-source rate limit is forgeable", got, peer)
			}
		})
	}
}

// The headers must reach BOTH a proxied answer and the gateway's own refusal —
// a middleware installed inside the mux would cover only one of them, and the
// refusal path is exactly where a framed page would be served from a 404.
//
// The upstream here sets one of the three itself, which pins the override
// direction: what the application says wins over the blanket default, because a
// page that needs SAMEORIGIN framing must be able to say so without editing the
// gateway.
func TestSecurityHeadersOnEveryAnswer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/inventory/internal/", http.HandlerFunc(edgeDenied))
	mux.Handle("/api/inventory/", apiProxy(upstreamURL, "/api/inventory/", true))
	handler := securityHeaders(denyEncodedSeparators(mux))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	for _, tc := range []struct{ name, path, wantFrame string }{
		{"proxied answer", "/api/inventory/holds", "SAMEORIGIN"},
		{"edge refusal", "/api/inventory/internal/holds", "DENY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := srv.Client().Get(srv.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := resp.Header.Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
				t.Errorf("Content-Security-Policy = %q, want frame-ancestors 'none'", got)
			}
			if got := resp.Header.Values("X-Frame-Options"); len(got) != 1 || got[0] != tc.wantFrame {
				t.Errorf("X-Frame-Options = %v, want [%s]", got, tc.wantFrame)
			}
		})
	}
}

func TestGatewayCommandRegistry(t *testing.T) {
	healthcheckRuns := 0
	callbacks := commandCallbacks{
		healthcheck: func() int {
			healthcheckRuns++
			return 7
		},
	}

	t.Run("healthcheck runs and returns exit status", func(t *testing.T) {
		healthcheckRuns = 0
		got := execute([]string{"healthcheck"}, callbacks, func() error {
			t.Fatal("server ran after healthcheck was selected")
			return nil
		})
		if got.Name != "healthcheck" || got.ExitCode != 7 || got.Err != nil {
			t.Fatalf("got = %+v, want healthcheck with exit 7", got)
		}
		if healthcheckRuns != 1 {
			t.Fatalf("healthcheck ran %d times, want 1", healthcheckRuns)
		}
	})

	t.Run("healthcheck rejects trailing arguments", func(t *testing.T) {
		healthcheckRuns = 0
		serverRuns := 0
		got := execute([]string{"healthcheck", "extra"}, callbacks, func() error {
			serverRuns++
			return nil
		})
		if got.Name != "healthcheck" || got.ExitCode == 0 || got.Err == nil {
			t.Fatalf("got = %+v, want error for trailing arguments", got)
		}
		if healthcheckRuns != 0 || serverRuns != 0 {
			t.Fatalf("healthcheck=%d server=%d, want 0", healthcheckRuns, serverRuns)
		}
	})

	t.Run("unknown command is rejected without starting server", func(t *testing.T) {
		serverRuns := 0
		got := execute([]string{"not-a-command"}, callbacks, func() error {
			serverRuns++
			return nil
		})
		if got.Name != "not-a-command" || got.ExitCode == 0 || got.Err == nil {
			t.Fatalf("got = %+v, want error for unknown command", got)
		}
		if serverRuns != 0 {
			t.Fatalf("server ran %d times, want 0", serverRuns)
		}
	})

	t.Run("empty arguments starts server", func(t *testing.T) {
		serverRuns := 0
		got := execute(nil, callbacks, func() error {
			serverRuns++
			return nil
		})
		if got.Name != "" || got.ExitCode != 0 || got.Err != nil {
			t.Fatalf("got = %+v, want server run", got)
		}
		if serverRuns != 1 {
			t.Fatalf("server ran %d times, want 1", serverRuns)
		}
	})
}

func TestGatewayProxyTimeoutPreHeaderReturns504(t *testing.T) {
	var upstreamRequests atomic.Int32
	headerRelease := make(chan struct{})
	upstreamCanceled := make(chan struct{}, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		select {
		case <-headerRelease:
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
			select {
			case upstreamCanceled <- struct{}{}:
			default:
			}
			return
		}
	}))
	defer func() {
		close(headerRelease)
		upstream.Close()
	}()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := apiProxy(u, "/api/test/", true, withProxyTimeout(1*time.Second))
	srv := httptest.NewServer(securityHeaders(proxy))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/test/slow", strings.NewReader("client-body"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d (504 Gateway Timeout)", resp.StatusCode, http.StatusGatewayTimeout)
	}

	if got := upstreamRequests.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1 (no automatic retries on write)", got)
	}

	select {
	case <-upstreamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream context was not canceled after proxy deadline exceeded")
	}
}

func TestGatewayProxyTimeoutPostHeaderAbortsStream(t *testing.T) {
	bodyRelease := make(chan struct{})
	upstreamCanceled := make(chan struct{}, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("initial-chunk"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-bodyRelease:
			_, _ = w.Write([]byte("-final-chunk"))
		case <-r.Context().Done():
			select {
			case upstreamCanceled <- struct{}{}:
			default:
			}
			return
		}
	}))
	defer func() {
		close(bodyRelease)
		upstream.Close()
	}()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := apiProxy(u, "/api/test/", true, withProxyTimeout(1*time.Second))
	srv := httptest.NewServer(securityHeaders(proxy))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/test/stall", strings.NewReader("client-body"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 OK (headers already sent, must not convert to 504)", resp.StatusCode)
	}

	body, readErr := io.ReadAll(resp.Body)
	if readErr == nil {
		t.Fatalf("expected stream truncation/abort error, got nil with body: %q", string(body))
	}
	if errors.Is(readErr, context.DeadlineExceeded) {
		t.Fatalf("read timed out on client context deadline instead of proxy aborting stream: %v", readErr)
	}
	var netErr net.Error
	if errors.As(readErr, &netErr) && netErr.Timeout() {
		t.Fatalf("read timed out on client deadline instead of proxy aborting stream: %v", readErr)
	}
	if !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("expected premature stream termination (io.ErrUnexpectedEOF), got: %v", readErr)
	}
	if string(body) != "initial-chunk" {
		t.Fatalf("body = %q, want exact %q", string(body), "initial-chunk")
	}

	select {
	case <-upstreamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream context was not canceled after post-header stall")
	}
}

func TestGatewayProxyWebSocketUpgrade(t *testing.T) {
	upstreamDone := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "expected websocket upgrade", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack not supported", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("upstream hijack failed: %v", err)
			return
		}
		defer func() {
			_ = conn.Close()
			close(upstreamDone)
		}()

		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n\r\n")
		_ = buf.Flush()

		for {
			line, err := buf.ReadString('\n')
			if err != nil {
				return
			}
			_, _ = buf.WriteString("echo:" + line)
			_ = buf.Flush()
		}
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := apiProxy(u, "/api/test/", true, withProxyTimeout(1*time.Second))
	// Pass through securityHeaders to verify securedWriter.Unwrap() preserves http.Hijacker
	srv := httptest.NewServer(securityHeaders(proxy))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Bound overall connection to prevent hang if a broken mutation never finishes
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	req := "GET /api/test/ws HTTP/1.1\r\n" +
		"Host: " + srv.Listener.Addr().String() + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write handshake failed: %v", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("http.ReadResponse failed: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101 Switching Protocols", resp.StatusCode)
	}

	// Real bidirectional exchange
	for _, msg := range []string{"ping1\n", "ping2\n"} {
		if _, err := conn.Write([]byte(msg)); err != nil {
			t.Fatalf("write msg failed: %v", err)
		}
		echo, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read echo failed: %v", err)
		}
		if echo != "echo:"+msg {
			t.Fatalf("echo = %q, want %q", echo, "echo:"+msg)
		}
	}

	// Wait for proxy timeout (1s) to close the connection
	_, err = reader.ReadString('\n')
	if err == nil {
		t.Fatal("expected connection closure after proxy timeout, but read succeeded")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("read timed out on client deadline instead of proxy closing connection: %v", err)
	}

	select {
	case <-upstreamDone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream connection was not closed upon proxy cancellation")
	}
}

func TestGatewayProxyTracePropagation(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
	)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	upstreamHandler := obs.MiddlewareWithTracerProvider("upstream-service", tp, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	upstream := httptest.NewServer(upstreamHandler)
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := apiProxy(u, "/api/upstream/", true)

	tracer := tp.Tracer("test-tracer")
	ctx, parentSpan := tracer.Start(context.Background(), "parent-client-call")

	req := httptest.NewRequest(http.MethodGet, "/api/upstream/test", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	parentSpan.End()

	if rec.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200", rec.Code)
	}

	spans := exporter.GetSpans()
	var parentSnap, clientSnap, serverSnap tracetest.SpanStub
	for _, s := range spans {
		switch {
		case s.Name == "parent-client-call":
			parentSnap = s
		case s.SpanKind == otrace.SpanKindClient:
			clientSnap = s
		case s.SpanKind == otrace.SpanKindServer:
			serverSnap = s
		}
	}

	if parentSnap.Name == "" {
		t.Fatalf("parent span not found in exported spans: %+v", spans)
	}
	if clientSnap.Name == "" {
		t.Fatalf("proxy client span not found in exported spans: %+v", spans)
	}
	if serverSnap.Name == "" {
		t.Fatalf("upstream server span not found in exported spans: %+v", spans)
	}

	traceID := parentSnap.SpanContext.TraceID()
	if !traceID.IsValid() {
		t.Fatal("parent trace ID is invalid")
	}
	if clientSnap.SpanContext.TraceID() != traceID {
		t.Errorf("client trace ID = %s, want %s", clientSnap.SpanContext.TraceID(), traceID)
	}
	if serverSnap.SpanContext.TraceID() != traceID {
		t.Errorf("server trace ID = %s, want %s", serverSnap.SpanContext.TraceID(), traceID)
	}

	if clientSnap.Parent.SpanID() != parentSnap.SpanContext.SpanID() {
		t.Errorf("client span parent = %s, want parent span ID %s", clientSnap.Parent.SpanID(), parentSnap.SpanContext.SpanID())
	}
	if serverSnap.Parent.SpanID() != clientSnap.SpanContext.SpanID() {
		t.Errorf("server span parent = %s, want client span ID %s", serverSnap.Parent.SpanID(), clientSnap.SpanContext.SpanID())
	}
}
