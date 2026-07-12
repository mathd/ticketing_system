// Catalog service. Owns: organizers/tenants, venues, seat maps, events,
// performances, series/seasons, festival structure, rule definitions
// (ADR-002). US-002 (TKT-26): venues/events/performances/ticket types,
// publish + domain event, aggregated public reads (contract in api/openapi.yaml).
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
	_ "time/tzdata" // IANA zones inside distroless images (timezone validation)

	"github.com/go-chi/chi/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"

	"ticketing/services/catalog/internal/api"
	"ticketing/services/catalog/internal/events"
	"ticketing/services/catalog/internal/store"
	"ticketing/shared/httpx"
	"ticketing/shared/obs"
)

const serviceName = "catalog"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serviceName, err)
		os.Exit(1)
	}
}

// healthcheck is the container health probe: distroless images have no
// shell/curl, so the binary probes itself (compose exec's this subcommand).
func healthcheck() int {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://localhost:" + port() + "/healthz")
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

	// Migrate before listening; fail fast on a bad migration (ADR-008).
	mctx, mcancel := context.WithTimeout(ctx, 30*time.Second)
	defer mcancel()
	if err := store.Migrate(mctx, db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	log.InfoContext(ctx, "migrations applied")

	nc, err := nats.Connect(os.Getenv("NATS_URL"),
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1))
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()

	publisher, err := events.NewJetStream(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}

	apiHandler, err := api.NewRouter(api.NewServer(store.NewPostgres(db), publisher, log))
	if err != nil {
		return fmt.Errorf("api router: %w", err)
	}

	r := chi.NewRouter()
	r.Method(http.MethodGet, "/healthz", httpx.Healthz(serviceName,
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
	))
	r.Mount("/", apiHandler)

	srv := &http.Server{
		Addr:              ":" + port(),
		Handler:           obs.Middleware(serviceName, obs.RequestLogger(log, r)),
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
