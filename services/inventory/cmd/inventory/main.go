// Inventory service. Owns holds, allocations, and reservation contention
// (ADR-002). M1 implements bounded GA holds, lifecycle transitions, availability,
// idempotency, and catalog-driven slot projection.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"ticketing/services/inventory/internal/api"
	"ticketing/services/inventory/internal/consumer"
	"ticketing/services/inventory/internal/store"
	"ticketing/shared/httpx"
	"ticketing/shared/obs"
	"ticketing/shared/runtimecfg"
)

const serviceName = "inventory"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if len(os.Args) > 1 {
		if sub, ok := subcommands()[os.Args[1]]; ok {
			if err := sub(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "%s %s: %v\n", serviceName, os.Args[1], err)
				os.Exit(1)
			}
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serviceName, err)
		os.Exit(1)
	}
}

// subcommands are the one-shot modes (the registry shape services/access uses). migrate is the
// out-of-band migration job (ADR-022); reprocess-quarantine republishes future-schema events a
// newer binary now understands (TKT-68) — deploy that binary first, run this, then restart;
// reconcile-pins reclaims catalog seat pins left behind by expired holds (TKT-112).
func subcommands() map[string]func([]string) error {
	return map[string]func([]string) error{
		"migrate":              func([]string) error { return migrate() },
		"reprocess-quarantine": reprocessQuarantine,
		"reconcile-pins":       reconcilePins,
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

	if err := store.Migrate(ctx, db); err != nil {
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
	credential, err := runtimecfg.InternalTokenFromEnv()
	if err != nil {
		return err
	}
	httpConfig, err := runtimecfg.HTTPFromEnv()
	if err != nil {
		return fmt.Errorf("http configuration: %w", err)
	}
	validateResponses, err := runtimecfg.ResponseValidationFromEnv()
	if err != nil {
		return fmt.Errorf("response validation configuration: %w", err)
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
	catalogURL := strings.TrimRight(os.Getenv("CATALOG_URL"), "/")
	if catalogURL == "" {
		return errors.New("CATALOG_URL required")
	}

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
	ttl := 10 * time.Minute
	if raw := os.Getenv("HOLD_TTL"); raw != "" {
		parsed, parseErr := time.ParseDuration(raw)
		if parseErr != nil || parsed <= 0 {
			return fmt.Errorf("invalid HOLD_TTL %q", raw)
		}
		ttl = parsed
	}
	st := store.New(db, ttl)
	catalog := consumer.NewCatalogResolver(catalogURL, credential, &http.Client{Timeout: 5 * time.Second})
	cons := consumer.New(js, st, catalog, log)
	consumerErr := make(chan error, 1)
	go func() { consumerErr <- cons.Run(ctx) }()

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
	r.Method(http.MethodGet, "/readyz", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !cons.Ready() {
			http.Error(w, "consumer not ready", http.StatusServiceUnavailable)
			return
		}
		httpx.Healthz(serviceName, httpx.Check("db", db.Ping), httpx.Check("nats", func() error {
			if !nc.IsConnected() {
				return errors.New("not connected")
			}
			return nil
		})).ServeHTTP(w, req)
	}))
	r.Mount("/", api.New(st, credential, catalog).Router(log, validateResponses))

	srv := &http.Server{
		Addr:    ":" + port(),
		Handler: obs.Middleware(serviceName, obs.RequestLogger(log, r)),
	}
	httpConfig.Apply(srv)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.InfoContext(ctx, "listening", "addr", srv.Addr)

	return awaitShutdown(ctx, errCh, consumerErr, srv.Shutdown)
}

// awaitShutdown blocks until the server fails, the consumer fails, or the signal
// context is canceled, and returns the error the process should exit with.
// Split out of run() so the both-branches-ready case is testable at all: run()
// itself needs Postgres, NATS and a bound port.
func awaitShutdown(ctx context.Context, srvErr, consumerErr <-chan error, shutdown func(context.Context) error) error {
	select {
	case err := <-srvErr:
		return err
	case err := <-consumerErr:
		// A Run tail that unwound because we asked it to is not a failure.
		// Reporting it exited non-zero on roughly half of clean shutdowns,
		// since this branch and ctx.Done() go ready together on SIGTERM and
		// select picks between them at random (TKT-121, the twin of TKT-98).
		if !isShutdownConsumerError(ctx, err) {
			return err
		}
	case <-ctx.Done():
		// The signal branch can win with a consumer error already queued, so
		// take a non-blocking look before shutting down cleanly — otherwise a
		// genuine failure racing the signal (a deleted durable, which ADR-017
		// §236-241 says must take the process down) loses the coin flip and
		// the process exits 0.
		//
		// ONE receive, not access's drain loop: inventory has a single
		// consumerErr producer (main.go's lone `go func()`) on a buffered-1
		// channel, so at most one value can ever exist and a loop's second
		// iteration is unreachable. Access loops because it has two producers
		// sharing the channel. This is a deliberate divergence from that
		// reference implementation, not an incomplete copy of it — the loop
		// comes back if and when a second producer does.
		//
		// A snapshot, deliberately: it cannot see an error that arrives after
		// the receive. Closing that window means blocking shutdown until the
		// consumer has terminated, which trades a missed exit code on an
		// operator-requested stop for a stop a wedged consumer can delay.
		select {
		case err := <-consumerErr:
			if !isShutdownConsumerError(ctx, err) {
				return err
			}
		default:
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return shutdown(sctx)
}

// isShutdownConsumerError reports whether err is the consumer's Run unwinding
// because this process's signal context was canceled. ctx is run()'s
// signal.NotifyContext, so ctx.Err() != nil means precisely "we were asked to
// stop" — without that conjunct a cancellation arriving while the process is
// otherwise running would be silently ignored.
//
// errors.Is rather than a match on the "consumer stopped" prefix: Run has five
// guarded exits before its tail, and a SIGTERM during startupConverge surfaces
// cancellation from a catalog HTTP call or a retry wait carrying no prefix at
// all. The filter stays narrow at the other end because
// durableconsumer.Wait's termination diagnostic is built with %s, not %w
// (shared/go/durableconsumer/wait.go:83) — it does not wrap context.Canceled,
// so it is never classified as shutdown-caused.
func isShutdownConsumerError(ctx context.Context, err error) bool {
	return ctx.Err() != nil && errors.Is(err, context.Canceled)
}
