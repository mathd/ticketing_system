// Commerce service. Owns reservations, orders, buyers, pricing snapshots, and
// post-purchase lifecycle (ADR-002). M1 implements reservation orchestration,
// serialized checkout completion, payment outcomes, and ticket delivery events.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"

	commerceapi "ticketing/services/commerce/internal/api"
	commerceevents "ticketing/services/commerce/internal/events"
	"ticketing/services/commerce/internal/outbox"
	"ticketing/services/commerce/internal/recovery"
	commercestore "ticketing/services/commerce/internal/store"
	"ticketing/shared/httpx"
	"ticketing/shared/obs"
	"ticketing/shared/runtimecfg"
)

const serviceName = "commerce"

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
//
// The completion-outbox backfill deliberately does NOT run here: it is data
// repair, not schema, it is idempotent, and run() reaches it against a schema
// this job has already migrated.
func migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := commercestore.Migrate(ctx, db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
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
	token, err := runtimecfg.InternalTokenFromEnv()
	if err != nil {
		return err
	}
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
	// Migrations ran out-of-band before this process started (ADR-022); the
	// backfill below is data repair, not schema, so it stays on the server path
	// and keeps its own bound.
	mctx, mcancel := context.WithTimeout(ctx, 30*time.Second)
	defer mcancel()
	// Orders completed before the outbox existed owe an event no code path will ever
	// insert, because CompleteOrder short-circuits on an already-completed order. Left
	// alone they keep the paid-but-no-ticket window open forever — the exact bug the
	// outbox closes. Idempotent, and a no-op on every boot after the first.
	if owed, err := commercestore.BackfillCompletionOutbox(mctx, db); err != nil {
		return fmt.Errorf("backfill completion outbox: %w", err)
	} else if owed > 0 {
		log.InfoContext(ctx, "backfilled owed completion events", "count", owed)
	}

	nc, err := nats.Connect(os.Getenv("NATS_URL"),
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1))
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()
	publisher, err := commerceevents.NewJetStream(nc)
	if err != nil {
		return err
	}

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
	r.Method(http.MethodGet, "/readyz", health)
	catalogURL, inventoryURL, paymentsURL := os.Getenv("CATALOG_URL"), os.Getenv("INVENTORY_URL"), os.Getenv("PAYMENTS_URL")
	if catalogURL == "" || inventoryURL == "" || paymentsURL == "" {
		return errors.New("CATALOG_URL, INVENTORY_URL and PAYMENTS_URL required")
	}
	r.Mount("/", commerceapi.New(db, obs.Client(), catalogURL, inventoryURL, paymentsURL, token, publisher).Router())

	srv := &http.Server{
		Addr:    ":" + port(),
		Handler: obs.Middleware(serviceName, obs.RequestLogger(log, r)),
	}
	httpConfig.Apply(srv)

	// Commerce's first background worker (ADR-016 §Decision 6). It drains once on
	// start, which is what recovers events owed by a process that died before
	// publishing — the paid-but-no-ticket window.
	drainer := outbox.New(db, publisher, drainInterval(), drainBatch(), log)
	stopDrainer := start(log, "outbox drainer", drainer.Run)

	// The second background worker (ADR-016 §Decision 1): recovery is driven, not
	// awaited. Without it an order that lost its request stays parked forever and its
	// seat leaks — a byte-identical checkout replay is the only other thing that would
	// advance it, and nothing in the system generates one.
	// One constant for both the client and the lease: the lease must outlast the calls
	// it covers, so deriving them from different numbers is how the lease ends up
	// shorter than its own batch.
	const recoveryCallTimeout = 10 * time.Second
	recoveryClients := recovery.HTTPClients{
		Client:       &http.Client{Timeout: recoveryCallTimeout},
		InventoryURL: inventoryURL,
		PaymentsURL:  paymentsURL,
		Token:        token,
	}
	journal := recovery.JournalFact{
		Client:      recoveryClients.Client,
		PaymentsURL: paymentsURL,
		Token:       token,
		DB:          recovery.StoreFactDB{DB: db},
	}
	recoverer := recovery.New(recovery.DBStore{DB: db}, recoveryClients, recoveryClients, journal,
		recovery.StoreCompleter{DB: db}, recoveryInterval(), recoveryBatch(), recoveryCallTimeout, log)
	stopRecovery := start(log, "recovery runner", recoverer.Run)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.InfoContext(ctx, "listening", "addr", srv.Addr)

	stopWorkers := func() {
		// Recovery first: it completes orders through the outbox, so stopping the
		// drainer first would strand a row this pass just owed until the next boot.
		stopRecovery()
		stopDrainer()
	}

	select {
	case err := <-errCh:
		stopWorkers()
		return err
	case <-ctx.Done():
		// Stop serving first, then the workers: draining an owed event is useful right
		// up to exit, and any row still leased is re-claimed once the lease lapses.
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := srv.Shutdown(sctx)
		stopWorkers()
		return err
	}
}

// start runs a worker on its own context and returns its stop func. The context is
// deliberately NOT the signal context: workers are stopped after srv.Shutdown returns,
// so cancelling them on the signal would cut a pass short while requests are still
// draining. The returned func blocks until the worker exits or the grace period lapses.
func start(log *slog.Logger, name string, run func(context.Context)) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); run(ctx) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			log.Warn(name + " did not stop within the grace period")
		}
	}
}

// drainInterval bounds how long an owed event can sit unpublished when the inline
// publish failed. Short enough that a buyer is not waiting on it; long enough that an
// idle service is not polling PostgreSQL hard.
func drainInterval() time.Duration {
	if v := os.Getenv("OUTBOX_DRAIN_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 5 * time.Second
}

func drainBatch() int {
	if v := os.Getenv("OUTBOX_DRAIN_BATCH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 32
}

// recoveryInterval bounds how long a stuck order waits for a re-drive. Longer than the
// outbox interval: an owed event is one publish away, while a re-drive makes network
// calls per order and every claim already survived the grace period that keeps recovery
// off live checkouts.
func recoveryInterval() time.Duration {
	if v := os.Getenv("RECOVERY_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 30 * time.Second
}

func recoveryBatch() int {
	if v := os.Getenv("RECOVERY_BATCH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 16
}
