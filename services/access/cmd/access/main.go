// Access service. Owns ticket issuance, delivery projection, signed credentials,
// scanning/redemption, and admission history (ADR-002). M1 implements order-event
// consumption, guest ticket links, QR issuance, and atomic single-use redemption.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	apispec "ticketing/services/access/api"
	accessapi "ticketing/services/access/internal/api"
	"ticketing/services/access/internal/consumer"
	accessstore "ticketing/services/access/internal/store"
	"ticketing/services/access/internal/ticket"
	"ticketing/shared/httpx"
	"ticketing/shared/obs"
	"ticketing/shared/runtimecfg"
)

const serviceName = "access"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := migrate(); err != nil {
			fmt.Fprintf(os.Stderr, "%s migrate: %v\n", serviceName, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serviceName, err)
		os.Exit(1)
	}
}

// migrate applies this service's embedded migrations and exits (ADR-022).
// It runs as a one-shot job that must complete before the service starts;
// the server path never migrates. Fail-fast and the 30s deadline are kept
// from ADR-008 — only the placement changed.
func migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := accessstore.Migrate(ctx, db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// healthcheck is the container health probe: distroless images have no
// shell/curl, so the binary probes itself (compose exec's this subcommand).
func healthcheck() int {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://localhost:" + port() + "/readyz")
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	_ = resp.Body.Close()
	return 0
}

func port() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "8080"
}

func run() error {
	httpConfig, err := runtimecfg.HTTPFromEnv()
	if err != nil {
		return fmt.Errorf("http configuration: %w", err)
	}
	dbConfig, err := runtimecfg.DatabaseFromEnv()
	if err != nil {
		return fmt.Errorf("database configuration: %w", err)
	}
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

	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()
	dbConfig.Apply(db)
	// Migrations ran out-of-band before this process started (ADR-022).

	nc, err := nats.Connect(os.Getenv("NATS_URL"),
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1))
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}
	commerceURL, token := os.Getenv("COMMERCE_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN")
	if commerceURL == "" || token == "" {
		return errors.New("COMMERCE_URL and INTERNAL_SERVICE_TOKEN required")
	}
	signer, err := ticket.New(os.Getenv("ACCESS_QR_PRIVATE_KEY"), os.Getenv("ACCESS_QR_KID"))
	if err != nil {
		return err
	}
	verifier, err := ticket.NewVerifier(os.Getenv("ACCESS_QR_PUBLIC_KEYS"), os.Getenv("ACCESS_QR_KID"))
	if err != nil {
		return err
	}
	st := accessstore.New(db)
	consumerOptions, err := consumer.ParseOptions(os.Getenv("ACCESS_EVENT_RETRY_BACKOFF"))
	if err != nil {
		return err
	}
	cons := consumer.New(js, st, signer, obs.Client(), commerceURL, token, os.Getenv("PUBLIC_BASE_URL"), consumer.NewLogMailer(log), log, consumerOptions)
	consumerErr := make(chan error, 1)
	go func() { consumerErr <- cons.Run(ctx) }()

	r := chi.NewRouter()
	health := httpx.Healthz(serviceName,
		httpx.Check("db", func() error {
			pctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return db.PingContext(pctx)
		}),
		httpx.Check("nats", func() error {
			if !nc.IsConnected() {
				return errors.New("not connected")
			}
			return nil
		}),
	)
	r.Method(http.MethodGet, "/healthz", health)
	r.Method(http.MethodGet, "/readyz", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !cons.Ready() {
			http.Error(w, "consumer not ready", http.StatusServiceUnavailable)
			return
		}
		health.ServeHTTP(w, req)
	}))
	r.Method(http.MethodGet, "/openapi.yaml", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(apispec.Spec)
	}))
	r.Mount("/", accessapi.New(st, verifier).Router())

	srv := &http.Server{
		Addr:    ":" + port(),
		Handler: obs.Middleware(serviceName, obs.RequestLogger(log, r)),
	}
	httpConfig.Apply(srv)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.InfoContext(ctx, "listening", "addr", srv.Addr)

	select {
	case err := <-errCh:
		return err
	case err := <-consumerErr:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}
