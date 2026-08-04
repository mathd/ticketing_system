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
	"ticketing/services/commerce/internal/bulkrefund"
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

// staffWriteTokenEnv names the back office's commerce credential (TKT-194).
// It opens exactly one operation — the staff refund — where
// INTERNAL_SERVICE_TOKEN opens every service's internal surface.
const staffWriteTokenEnv = "COMMERCE_STAFF_WRITE_TOKEN"

func run() error {
	token, err := runtimecfg.InternalTokenFromEnv()
	if err != nil {
		return err
	}
	// Read before any dependency so a misconfigured deployment fails fast. It
	// matters more here than it did for catalog: a commerce started without this
	// answers every refund with 404, which is indistinguishable from "no such
	// order" — so the misconfiguration would arrive as a support ticket about a
	// missing order, not as a deployment failure.
	staffWriteToken, err := runtimecfg.RequiredCredential(staffWriteTokenEnv, "")
	if err != nil {
		return err
	}
	// Two credentials with different blast radii are only two credentials if
	// they hold different values. Nothing else compares them, so a deployment
	// setting both to one string would run normally while the back office — an
	// internet-facing SSR process — quietly held the key to every service's
	// internal surface. Neither value is echoed.
	//
	// Comparing raw strings is sound because RequiredCredential has already
	// refused every value HTTP would normalize, notably edge whitespace: without
	// that, " secret " and "secret" would be one credential on the wire while
	// differing here (TKT-191 ai-review pass 2). The narrow claim is the true
	// one — no two DISTINCT accepted values arrive identical at a server.
	if staffWriteToken == token {
		return fmt.Errorf("%s must not equal INTERNAL_SERVICE_TOKEN: the separate credential exists "+
			"so the back office cannot reach other services' internal surfaces, and identical values "+
			"remove that boundary while looking configured", staffWriteTokenEnv)
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
	// ACCESS_URL is optional, unlike the three above (TKT-157). Without it a refund
	// still returns the money and leaves ticket voiding outstanding and retryable —
	// degrading rather than refusing to start is the right failure for an obligation
	// that is discharged after the money has already moved.
	//
	// Bound to a variable rather than inlined: the cancellation refund runner (TKT-159)
	// refunds through this server's own refund unit, so both callers share one money path.
	srvHandler := commerceapi.New(db, obs.Client(), catalogURL, inventoryURL, paymentsURL, token, publisher).
		WithStaffWriteCredential(staffWriteToken).
		WithAccess(os.Getenv("ACCESS_URL"))
	r.Mount("/", srvHandler.Router(log, validateResponses))

	srv := &http.Server{
		Addr:    ":" + port(),
		Handler: obs.Middleware(serviceName, obs.RequestLogger(log, r)),
	}
	httpConfig.Apply(srv)

	// Commerce's first background worker (ADR-016 §Decision 6). It drains once on
	// start, which is what recovers events owed by a process that died before
	// publishing — the paid-but-no-ticket window.
	// The backfill rides the drainer, not the startup path (TKT-71): orders
	// completed before the outbox existed owe an event no code path will ever
	// insert, because CompleteOrder short-circuits on an already-completed order.
	// Idempotent, and a no-op on every boot after the first — but on a populated
	// table the scan is real work, and behind the drainer neither its duration
	// nor its failure can keep commerce from listening.
	// The 30s bound survives the move: the drainer runs the backfill before its
	// first drain pass, so an unbounded pass (lock wait, huge scan) would stall
	// owed-event recovery — the window this worker exists to close — not just
	// delay repair. Timing out cancels this pass only; the next boot retries.
	drainer := outbox.New(db, publisher, drainInterval(), drainBatch(),
		func(bctx context.Context) (int, error) {
			bctx, cancel := context.WithTimeout(bctx, 30*time.Second)
			defer cancel()
			return commercestore.BackfillCompletionOutbox(bctx, db)
		}, log)
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

	// Event-cancellation bulk refunds (TKT-159). It refunds through the API server's own
	// refund unit — the SAME code the staff endpoint runs — so there is one money path
	// with two callers rather than two implementations of one protocol.
	cancellations := bulkrefund.New(bulkrefund.DBStore{DB: db}, srvHandler.Refunds(),
		cancellationInterval(), cancellationBatch(),
		recovery.LeaseFor(cancellationBatch(), recoveryCallTimeout))
	stopCancellations := start(log, "cancellation refund runner", cancellations.Run)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.InfoContext(ctx, "listening", "addr", srv.Addr)

	stopWorkers := func() {
		// Cancellations first: a claimed order mid-refund must release its lease before
		// the paths it depends on go away, so a successor reclaims it immediately rather
		// than after the lease expires.
		stopCancellations()
		// Recovery next: it completes orders through the outbox, so stopping the
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

// cancellationInterval bounds how long a cancellation run's next batch waits. Shorter than
// recovery's: a cancelled event's buyers are owed money now, and unlike a stuck checkout
// there is no grace period to respect — the run was created by an operator who is waiting
// for the report.
func cancellationInterval() time.Duration {
	if v := os.Getenv("CANCELLATION_REFUND_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 10 * time.Second
}

// cancellationBatch bounds one claim, and with it the lease. Smaller than recovery's: each
// order here makes up to four downstream calls (payments, journal, access, inventory), and
// the lease has to outlast the whole batch.
func cancellationBatch() int {
	if v := os.Getenv("CANCELLATION_REFUND_BATCH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 8
}
