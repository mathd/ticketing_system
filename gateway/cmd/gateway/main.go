// API gateway (US-001): the single public entry point (ADR-002).
// Scaffold-only behavior: an explicit route table proxies /api/<service>/*
// to registered services and / to the storefront — real public contracts
// arrive with US-002's OpenAPI work and must be registered here explicitly.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"ticketing/shared/httpx"
	"ticketing/shared/obs"
)

const serviceName = "gateway"

// routes is the explicit registration table: public prefix -> upstream env var.
var routes = map[string]string{
	"/api/catalog/":   "CATALOG_URL",
	"/api/inventory/": "INVENTORY_URL",
	"/api/commerce/":  "COMMERCE_URL",
	"/api/payments/":  "PAYMENTS_URL",
	"/api/access/":    "ACCESS_URL",
	"/scanner/":       "SCANNER_URL",
	"/":               "STOREFRONT_URL", // catch-all LAST by construction (longest prefix wins)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get("http://localhost:" + port() + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serviceName, err)
		os.Exit(1)
	}
}

func port() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "8080"
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log, shutdown, err := obs.Setup(ctx, serviceName)
	if err != nil {
		return fmt.Errorf("otel setup: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(sctx)
	}()

	mux := http.NewServeMux()

	// /healthz = liveness (self only, no downstream circularity with compose
	// healthchecks). /readyz and /healthz/all = readiness: the downstream
	// fan-out, 200 iff every service is up.
	all := healthzAll(log)
	mux.Handle("GET /healthz", httpx.Healthz(serviceName))
	mux.Handle("GET /healthz/all", all)
	mux.Handle("GET /readyz", all)

	// Longest-prefix registration; ServeMux ordering handles it.
	for prefix, envVar := range routes {
		target := os.Getenv(envVar)
		if target == "" {
			return fmt.Errorf("route %s: %s not set", prefix, envVar)
		}
		u, err := url.Parse(target)
		if err != nil {
			return fmt.Errorf("route %s: bad url %q: %w", prefix, target, err)
		}
		// Only /api/* prefixes are stripped; web shells (storefront at /,
		// scanner at /scanner/) are built to serve under their public path.
		stripAPIPrefix := strings.HasPrefix(prefix, "/api/")
		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(u)
				if stripAPIPrefix {
					pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, strings.TrimSuffix(prefix, "/"))
					if pr.Out.URL.Path == "" {
						pr.Out.URL.Path = "/"
					}
				} else {
					pr.Out.URL.Path = pr.In.URL.Path
				}
				pr.SetXForwarded()
			},
		}
		mux.Handle(prefix, proxy)
	}

	srv := &http.Server{
		Addr:              ":" + port(),
		Handler:           obs.Middleware(serviceName, obs.RequestLogger(log, mux)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.InfoContext(ctx, "listening", "addr", srv.Addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}

// backends returns the five service healthz URLs from the route table.
func backends() map[string]string {
	m := make(map[string]string)
	for prefix, envVar := range routes {
		if !strings.HasPrefix(prefix, "/api/") {
			continue
		}
		name := strings.Trim(strings.TrimPrefix(prefix, "/api/"), "/")
		m[name] = strings.TrimSuffix(os.Getenv(envVar), "/") + "/healthz"
	}
	return m
}

// healthzAll fans out to every service's /healthz concurrently with a 2s
// per-downstream timeout: 200 iff all up, else 503. Response is
// {"status":..., "services":{name: "up"|"down"|"timeout"}}. This call is
// also the trace-propagation demo — one trace covers gateway + services.
func healthzAll(log interface {
	InfoContext(context.Context, string, ...any)
}) http.Handler {
	client := obs.Client()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		results := make(map[string]string)
		var mu sync.Mutex
		var wg sync.WaitGroup
		for name, target := range backends() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				defer cancel()
				status := probe(ctx, client, target)
				mu.Lock()
				results[name] = status
				mu.Unlock()
			}()
		}
		wg.Wait()

		overall, code := "ok", http.StatusOK
		for _, s := range results {
			if s != "up" {
				overall, code = "degraded", http.StatusServiceUnavailable
				break
			}
		}
		log.InfoContext(r.Context(), "healthz fan-out", "status", overall)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": overall, "services": results})
	})
}

func probe(ctx context.Context, client *http.Client, target string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "down"
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "timeout"
		}
		return "down"
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return "up"
	}
	return "down"
}
