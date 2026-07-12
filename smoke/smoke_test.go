//go:build smoke

// The US-001 integration seam: black-box assertions against the composed
// stack through the gateway, plus named infrastructure checks (JetStream,
// DB credential isolation, telemetry ingestion). Run via `make smoke`,
// which owns the compose lifecycle.
package smoke_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	gatewayURL = env("SMOKE_GATEWAY_URL", "http://localhost:8080")
	natsURL    = env("SMOKE_NATS_URL", "nats://localhost:4222")
	pgHostPort = env("SMOKE_PG", "localhost:5432")
	promURL    = env("SMOKE_PROM_URL", "http://localhost:9090")
	project    = env("SMOKE_COMPOSE_PROJECT", "ticketing-smoke")
)

// retry polls fn until it returns nil or the deadline passes.
func retry(t *testing.T, d time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	var err error
	for time.Now().Before(deadline) {
		if err = fn(); err == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("condition not met within %s: %v", d, err)
}

func get(t *testing.T, url string, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("bad request: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func TestHealthzAllUp(t *testing.T) {
	retry(t, 60*time.Second, func() error {
		code, body := get(t, gatewayURL+"/healthz/all", nil)
		if code != http.StatusOK {
			return fmt.Errorf("status %d: %s", code, body)
		}
		var r struct {
			Status   string            `json:"status"`
			Services map[string]string `json:"services"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return err
		}
		if len(r.Services) != 5 {
			return fmt.Errorf("want 5 services, got %v", r.Services)
		}
		for name, s := range r.Services {
			if s != "up" {
				return fmt.Errorf("%s is %s", name, s)
			}
		}
		return nil
	})
}

// The storefront root redirects into the default locale (Astro i18n). The
// redirect is asserted without following it so this test never warms the
// storefront's page-data cache before the US-002 flow publishes its fixture
// (us002_test.go owns the rendered-page assertions).
func TestStorefrontServedThroughGateway(t *testing.T) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(gatewayURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	loc := resp.Header.Get("Location")
	redirect := resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound
	if !redirect || !strings.HasPrefix(loc, "/en") {
		t.Fatalf("storefront via gateway: status %d, location %q", resp.StatusCode, loc)
	}
}

func TestScannerServedThroughGateway(t *testing.T) {
	code, body := get(t, gatewayURL+"/scanner/", nil)
	if code != http.StatusOK || !strings.Contains(string(body), "Gate scanner") {
		t.Fatalf("scanner via gateway: status %d, body %.120s", code, body)
	}
}

// TestTracePropagation sends a request with a known trace id and asserts the
// same trace_id shows up in the JSON logs of the gateway AND at least one
// service — proving W3C context propagates across the network boundary.
func TestTracePropagation(t *testing.T) {
	idBytes := make([]byte, 16)
	_, _ = rand.Read(idBytes)
	traceID := hex.EncodeToString(idBytes)
	traceparent := fmt.Sprintf("00-%s-00f067aa0ba902b7-01", traceID)

	code, _ := get(t, gatewayURL+"/healthz/all", map[string]string{"traceparent": traceparent})
	if code != http.StatusOK {
		t.Fatalf("healthz/all returned %d", code)
	}

	retry(t, 30*time.Second, func() error {
		out, err := exec.Command("docker", "compose", "-p", project, "logs",
			"gateway", "catalog", "inventory", "commerce", "payments", "access").CombinedOutput()
		if err != nil {
			return fmt.Errorf("compose logs: %v", err)
		}
		var gw, svc bool
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.Contains(line, traceID) {
				continue
			}
			if strings.Contains(line, "gateway") {
				gw = true
			} else {
				svc = true
			}
		}
		if !gw || !svc {
			return fmt.Errorf("trace %s: in gateway logs=%v, in service logs=%v", traceID, gw, svc)
		}
		return nil
	})
}

// TestJetStreamPersists proves the bus is JetStream and that stack init
// provisioned the PLATFORM stream (nats-init, ADR-007) — the test asserts
// the stream exists rather than creating it, then publishes and consumes
// durably through it.
func TestJetStreamPersists(t *testing.T) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := js.Stream(ctx, "PLATFORM")
	if err != nil {
		t.Fatalf("PLATFORM stream must be provisioned at stack init (nats-init): %v", err)
	}
	if _, err := js.Publish(ctx, "platform.smoke.ping", []byte("pong")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable: "smoke", FilterSubject: "platform.smoke.ping",
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	msg, err := cons.Next(jetstream.FetchMaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if string(msg.Data()) != "pong" {
		t.Fatalf("data = %q", msg.Data())
	}
	_ = msg.Ack()
}

// TestDBCredentialIsolation: a service's credentials open its own database
// and are rejected by every other service's database (ADR-007 boundary).
func TestDBCredentialIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	own, err := pgx.Connect(ctx, fmt.Sprintf("postgres://catalog:catalog@%s/catalog", pgHostPort))
	if err != nil {
		t.Fatalf("catalog creds must reach catalog db: %v", err)
	}
	_ = own.Close(ctx)

	cross, err := pgx.Connect(ctx, fmt.Sprintf("postgres://catalog:catalog@%s/inventory", pgHostPort))
	if err == nil {
		_ = cross.Close(ctx)
		t.Fatal("catalog creds connected to inventory db — boundary not enforced")
	}
}

// TestMetricsIngested asserts application metrics flow to the otel-lgtm
// Prometheus after real traffic.
func TestMetricsIngested(t *testing.T) {
	get(t, gatewayURL+"/healthz/all", nil) // generate traffic
	retry(t, 60*time.Second, func() error {
		code, body := get(t, promURL+`/api/v1/query?query=count({__name__=~"http_server_.%2B"})`, nil)
		if code != http.StatusOK {
			return fmt.Errorf("prom query status %d", code)
		}
		var r struct {
			Data struct {
				Result []struct {
					Value []any `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return err
		}
		if len(r.Data.Result) == 0 {
			return fmt.Errorf("no http_server_* series yet")
		}
		return nil
	})
}
