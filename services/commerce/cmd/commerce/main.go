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
	"go.opentelemetry.io/otel"

	commerceapi "ticketing/services/commerce/internal/api"
	"ticketing/services/commerce/internal/bulkrefund"
	commerceevents "ticketing/services/commerce/internal/events"
	"ticketing/services/commerce/internal/exchangesweep"
	"ticketing/services/commerce/internal/mailer"
	"ticketing/services/commerce/internal/outbox"
	"ticketing/services/commerce/internal/recovery"
	"ticketing/services/commerce/internal/reversal"
	commercestore "ticketing/services/commerce/internal/store"
	"ticketing/shared/httpx"
	"ticketing/shared/mail"
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
	// Operator provisioning for reseller credentials (TKT-240 / ADR-056).
	if len(os.Args) > 1 && (os.Args[1] == "enrol-reseller" || os.Args[1] == "revoke-reseller" || os.Args[1] == "list-resellers") {
		var run func([]string) error
		switch os.Args[1] {
		case "enrol-reseller":
			run = enrolReseller
		case "revoke-reseller":
			run = revokeReseller
		case "list-resellers":
			run = listResellers
		}
		if err := run(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "%s %s: %v\n", serviceName, os.Args[1], err)
			os.Exit(1)
		}
		return
	}
	// Operator resolution for parked recovery orders (TKT-146). A parked order is
	// excluded from ClaimStuckOrders forever, so without these there is no path in the
	// service from "the runner gave up" to "a human resolved it".
	if len(os.Args) > 1 && (os.Args[1] == "list-parked" || os.Args[1] == "unpark-order") {
		run := listParked
		if os.Args[1] == "unpark-order" {
			run = unparkOrder
		}
		if err := run(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "%s %s: %v\n", serviceName, os.Args[1], err)
			os.Exit(1)
		}
		return
	}
	// Operator resolution for wedged exchanges (TKT-255). A wedged exchange answers 409
	// forever and leaves its source order neither exchangeable nor refundable, and
	// migration 0010 records that an exchange has no cancelled state — so without these
	// there is no path in the service from "this order is stuck" to "a human resolved it".
	if len(os.Args) > 1 && (os.Args[1] == "list-wedged-exchanges" || os.Args[1] == "unwind-exchange") {
		run := listWedgedExchanges
		if os.Args[1] == "unwind-exchange" {
			run = unwindExchange
		}
		if err := run(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "%s %s: %v\n", serviceName, os.Args[1], err)
			os.Exit(1)
		}
		return
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
// It opens three operations — the staff refund, the comped-order void (TKT-171)
// and the staff order read (TKT-201) — where INTERNAL_SERVICE_TOKEN opens every
// service's internal surface. The narrow set is the point, and
// api/staff_credential_test.go is what keeps it narrow.
const staffWriteTokenEnv = "COMMERCE_STAFF_WRITE_TOKEN"

// assertionKeyEnv names the HMAC key for customer checkout assertions (TKT-221).
// A third credential with a third blast radius: it lets a holder attribute a
// checkout to any customer, and nothing else.
const assertionKeyEnv = "COMMERCE_CUSTOMER_ASSERTION_KEY"

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
	staffWriteToken, err := runtimecfg.RequiredCredential(staffWriteTokenEnv, "", runtimecfg.CredentialMinBytes)
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
	assertionKey, err := runtimecfg.RequiredCredential(assertionKeyEnv, "", runtimecfg.CredentialMinBytes)
	if err != nil {
		return err
	}
	// The payments-only credential (ai-review S8). Commerce is payments' one
	// caller, so this is where the split is spent: commerce holds both values and
	// nothing else holds this one, which means a compromise of catalog, inventory,
	// access or the gateway no longer reaches charge, void or refund. Required
	// rather than optional-with-fallback — a fallback is how a deployment ends up
	// back on the shared token without anyone noticing.
	paymentsToken, err := runtimecfg.RequiredCredential(runtimecfg.PaymentsTokenEnv, "", runtimecfg.CredentialMinBytes)
	if err != nil {
		return err
	}
	// Three credentials with three blast radii are only three credentials if they
	// hold three different values, and nothing else in the system compares them.
	//
	// The pairs are checked EXHAUSTIVELY rather than each new one against the
	// first: a third credential added to the wiring but not to this check is the
	// one credential whose separation is never verified, and identical values look
	// configured (TKT-221 plan-review F2). Adding a fourth means adding its pairs
	// here — the loop makes that obvious instead of easy to forget.
	//
	// Comparing raw strings is sound because RequiredCredential has already
	// refused every value HTTP would normalize, notably edge whitespace: without
	// that, " secret " and "secret" would be one credential on the wire while
	// differing here (TKT-191 ai-review pass 2). The narrow claim is the true
	// one — no two DISTINCT accepted values arrive identical at a server.
	if err := credentialsAreDistinct(token, staffWriteToken, assertionKey, paymentsToken); err != nil {
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
	liveness := []httpx.NamedCheck{
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
	}
	accessURL := os.Getenv("ACCESS_URL")
	// Liveness and readiness stop being the same handler here (TKT-163, ADR-062 — they
	// were identical since the service was scaffolded).
	//
	// `/healthz` answers "this process is working": the database and the broker. It is what
	// the container healthcheck probes (`healthcheck` above, compose.yaml), and what the
	// gateway's `depends_on: service_healthy` waits on — so anything added HERE can keep the
	// stack from starting.
	//
	// `/readyz` additionally answers "this deployment is configured to keep its promises".
	// Without ACCESS_URL, commerce still refunds money and still records the obligation, but
	// NOTHING can ever discharge it — the reconciler included, since it drives voiding
	// through that URL. ADR-038 §7 made that a deliberate degradation on the grounds the
	// obligation stayed visible and retryable; TKT-163 makes it worse rather than better,
	// because a reconciler that silently cannot run converts a visible outstanding
	// obligation into an invisible one. A misconfigured deployment must not take traffic.
	//
	// It is a CONFIGURATION check, never a reachability one. ADR-021 §D6 rejected gating
	// readiness on a runtime dependency's liveness — "a broker blip would close every
	// turnstile" — and that reasoning is sound and applies to anything that can flap. A
	// missing environment variable cannot flap: it is a static fact settled at startup, and
	// it is wrong for the whole life of the process or not at all.
	readiness := readinessChecks(liveness, accessURL)
	mountHealth(r, liveness, readiness)
	catalogURL, inventoryURL, paymentsURL := os.Getenv("CATALOG_URL"), os.Getenv("INVENTORY_URL"), os.Getenv("PAYMENTS_URL")
	if catalogURL == "" || inventoryURL == "" || paymentsURL == "" {
		return errors.New("CATALOG_URL, INVENTORY_URL and PAYMENTS_URL required")
	}
	// ACCESS_URL is still optional at the BINARY level, unlike the three above (TKT-157):
	// without it commerce boots, refunds still return the money, and the obligation is
	// still recorded rather than lost. That part of ADR-038 §7 is unchanged, and it is
	// still the right failure for an obligation discharged after the money has moved.
	//
	// What changed in TKT-163 is that its absence now fails READINESS (see the split
	// above). Booting degraded and being routed traffic are different questions, and only
	// the second one was ever really answered by "optional".
	//
	// Read into a variable above rather than inlined here: three things consume it now —
	// the refund unit, the readiness probe, and this comment's original reason, the
	// cancellation refund runner (TKT-159), which refunds through this server's own refund
	// unit so both callers share one money path.
	publicURL := os.Getenv("PUBLIC_BASE_URL")
	srvHandler := commerceapi.New(db, obs.Client(), catalogURL, inventoryURL, paymentsURL, token, publisher).
		WithPaymentsToken(paymentsToken).
		WithStaffWriteCredential(staffWriteToken).
		WithCustomerAssertionKey(assertionKey).
		WithAccess(accessURL).
		WithPublicURL(publicURL)
	commerceapi.WarnIfResetMailUnconfigured(log, publicURL)
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

	// The mail drainer (TKT-226 / ADR-050). Commerce's second outbox and the same
	// protocol as the first, over a different table.
	//
	// THE FAKE IS THE DEFAULT AND IT IS PRODUCTION WIRING, not test scaffolding: this
	// repo has no provider account, so `make up`, the smoke stack and the gate all run
	// against it. ADR-032's rule — a configured provider selects the real adapter, and
	// its absence selects the fake — is what keeps the gate offline and deterministic.
	// When a provider lands it is selected here, beside this comment, and nothing else
	// about the reset path changes.
	mailSender := mail.NewFake()
	// Say so at startup, at WARN, every boot (ai-review [critical], partly upheld).
	//
	// The review pass called the fake-by-default wiring a critical defect. As a design
	// judgement that is refused — it is ADR-032's rule and TKT-226's stated non-goal —
	// but the risk underneath it is real and was not addressed: a row reaches `sent_at`
	// and LOOKS delivered while nobody received anything, so an operator can believe
	// mail works. A running process must not be silent about that.
	//
	// WARN and not INFO: this is a state to escalate out of, not a configuration to
	// settle into.
	log.WarnContext(ctx, "transactional mail sender is the offline fake; messages are captured and NEVER delivered",
		"sender", "fake", "read_them_in", "mail_outbox")
	mailDrainer := mailer.New(db, mailSender, drainInterval(), drainBatch(), log)
	stopMailDrainer := start(log, "mail drainer", mailDrainer.Run)

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
		// The money surface takes its own credential (ai-review S8).
		PaymentsToken: paymentsToken,
	}
	journal := recovery.JournalFact{
		Client:      recoveryClients.Client,
		PaymentsURL: paymentsURL,
		Token:       paymentsToken,
		DB:          recovery.StoreFactDB{DB: db},
	}
	recoverer := recovery.New(recovery.DBStore{DB: db}, recoveryClients, recoveryClients, journal,
		recovery.StoreCompleter{DB: db}, recoveryInterval(), recoveryBatch(), recoveryCallTimeout, log)
	// Parking is terminal by design (ADR-016 §Decision 1) and the attempt-exhaustion log
	// is, in its own words, "the last notice anyone gets". These gauges are what turn that
	// into something an operator can see without thinking to run `list-parked` (ADR-065).
	// Observability, not a gate — same rule as the two runners below.
	if err := recoverer.ObserveMetrics(otel.Meter("ticketing/commerce/recovery")); err != nil {
		log.WarnContext(ctx, "recovery metrics unavailable; the runner still runs", "err", err)
	}
	stopRecovery := start(log, "recovery runner", recoverer.Run)

	// Event-cancellation bulk refunds (TKT-159). It refunds through the API server's own
	// refund unit — the SAME code the staff endpoint runs — so there is one money path
	// with two callers rather than two implementations of one protocol.
	cancellations := bulkrefund.New(bulkrefund.DBStore{DB: db}, srvHandler.Refunds(),
		cancellationInterval(), cancellationBatch(),
		recovery.LeaseFor(cancellationBatch(), recoveryCallTimeout))
	stopCancellations := start(log, "cancellation refund runner", cancellations.Run)

	// Outstanding refund reversals (TKT-163, ADR-062). ADR-038 §7 shipped the reversal as
	// "visible and retryable" with nothing retrying it: an access outage left refunded
	// tickets admitting until a human replayed the idempotency key, and the caller got a
	// 200. This is the thing that comes back for them.
	//
	// Through the SAME refund unit as the other two callers — it drives only DriveReversal,
	// so there is one reversal path with three callers and this runner cannot move money.
	// The lease is sized from obs.ClientTimeout, NOT from recoveryCallTimeout: the refund
	// service drives its calls through the API server's own transport, which is
	// obs.Client() at 30s. Borrowing recovery's 10s constant here produced a lease three
	// times shorter than the work it protects, letting a second replica reclaim rows the
	// first was still driving (ai-review F1). Derived from the real value so the two cannot
	// drift apart.
	reversals := reversal.New(reversal.DBStore{DB: db}, srvHandler.Refunds(),
		reversalInterval(), reversalBatch(),
		reversal.LeaseFor(reversalBatch(), obs.ClientTimeout), log)
	// Commerce's first metrics (the MeterProvider has been live since obs.Setup; nothing
	// had registered an instrument). Observability, not a gate — a failure to register
	// gauges must not keep the service from refunding, so it is logged, not returned.
	if err := reversals.ObserveMetrics(otel.Meter("ticketing/commerce/reversal")); err != nil {
		log.WarnContext(ctx, "refund reversal metrics unavailable; the reconciler still runs", "err", err)
	}
	stopReversals := start(log, "refund reversal runner", reversals.Run)

	// The exchange sweep (TKT-259, ADR-063): the same shape for `order_exchanges`, whose
	// obligations had no commerce-side sweep at all — their only retry was access's
	// JetStream redelivery, driven by the tickets-switched callback answering 502. That
	// callback REMAINS the first line; this is the backstop for rows it gave up on.
	//
	// Through the SAME discharge unit the callback uses, so the two cannot drift, and a
	// port with one method is what keeps this runner unable to move money or mark a switch.
	// The lease is sized from obs.ClientTimeout for the reason ADR-062's ai-review F1
	// recorded on the refund side: the discharge unit borrows the API server's `call`,
	// which runs on obs.Client() at 30s, and a lease shorter than the work it protects lets
	// a second replica reclaim rows the first is still driving.
	sweeps := exchangesweep.New(exchangesweep.DBStore{DB: db}, srvHandler.Exchanges(),
		exchangeSweepInterval(), exchangeSweepBatch(),
		exchangesweep.LeaseFor(exchangeSweepBatch(), obs.ClientTimeout), log)
	if err := sweeps.ObserveMetrics(otel.Meter("ticketing/commerce/exchangesweep")); err != nil {
		log.WarnContext(ctx, "exchange sweep metrics unavailable; the sweep still runs", "err", err)
	}
	stopSweeps := start(log, "exchange sweep runner", sweeps.Run)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.InfoContext(ctx, "listening", "addr", srv.Addr)

	stopWorkers := func() {
		// Cancellations first: a claimed order mid-refund must release its lease before
		// the paths it depends on go away, so a successor reclaims it immediately rather
		// than after the lease expires.
		stopCancellations()
		// Reversals next, for the same reason and one step weaker: a claimed refund
		// mid-reversal hands its lease back on shutdown (abandonUndriven), so stopping it
		// before the drainers keeps that release ahead of anything it might need. It stops
		// AFTER cancellations because a cancellation run drives reversals of its own
		// through the shared refund unit, and stopping the reversal runner first would not
		// quiesce that.
		stopReversals()
		// The exchange sweep with them, and for the same reason: a claimed exchange hands
		// its lease back on shutdown, and doing that before the paths it depends on go away
		// is what makes an orderly restart differ from a crash only in latency.
		stopSweeps()
		// Recovery next: it completes orders through the outbox, so stopping the
		// drainer first would strand a row this pass just owed until the next boot.
		stopRecovery()
		stopDrainer()
		// The mail drainer last and independently: nothing else enqueues through it on
		// shutdown, and a row still leased is re-claimed once the lease lapses. Stopping
		// it earlier would only shorten the window in which a reset already committed
		// gets delivered before exit.
		stopMailDrainer()
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

// mountHealth puts each probe set on its own path.
//
// Extracted for the same reason as readinessChecks, one level up: a test that only exercises
// readinessChecks proves the RULE and says nothing about whether anything MOUNTS it. Deleting
// the split — serving the liveness set on both paths — leaves such a test green, because the
// rule it tests is still correct and simply no longer reached. So the wiring is a seam a test
// can call, and the assertion is made at the router, which is the boundary the answer
// actually crosses on its way out.
func mountHealth(r chi.Router, liveness, readiness []httpx.NamedCheck) {
	r.Method(http.MethodGet, "/healthz", httpx.Healthz(serviceName, liveness...))
	r.Method(http.MethodGet, "/readyz", httpx.Healthz(serviceName, readiness...))
}

// readinessChecks builds the readiness probe set: everything liveness answers, plus the
// configuration a deployment needs to keep the promises it makes to callers.
//
// A function rather than an expression inside run() so the rule is reachable by a test —
// run() needs a database, a broker and four credentials, so a check written inline there is
// only ever exercised by starting the whole service, which is how a readiness rule ends up
// asserted by nothing.
//
// It takes the liveness set rather than rebuilding it, so the two cannot drift into
// answering different questions about the same process.
func readinessChecks(liveness []httpx.NamedCheck, accessURL string) []httpx.NamedCheck {
	return append(append([]httpx.NamedCheck{}, liveness...),
		httpx.Check("access_configured", func() error {
			if accessURL == "" {
				return errors.New("ACCESS_URL is unset; refund ticket voiding can never be discharged")
			}
			return nil
		}),
	)
}

// reversalInterval bounds how long an outstanding reversal obligation waits for a retry.
// The LONGEST of the four, deliberately: every row this runner claims has already failed at
// least once against a downstream that was unavailable, and re-asking a service that is down
// every ten seconds is how a reconciler becomes a second outage. The obligation is
// under-selling in the meantime (the tickets are void, the seat is not back), which is the
// safe direction — unlike a stuck checkout, whose seat is leaking, or a cancellation run,
// whose operator is waiting for a report.
//
// A restart drains immediately regardless (Runner.Run), so this bounds the steady state, not
// recovery from a deploy.
func reversalInterval() time.Duration {
	if v := os.Getenv("REFUND_REVERSAL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return time.Minute
}

// reversalBatch bounds one claim, and with it the lease. Larger than the cancellation
// runner's: each row here makes at most two downstream calls (void, then capacity) against
// no money path, where a cancellation order makes up to four including the provider.
func reversalBatch() int {
	if v := os.Getenv("REFUND_REVERSAL_BATCH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 16
}

// exchangeSweepInterval bounds how long an outstanding exchange obligation waits for a
// retry. Matches the refund reconciler's minute, for the same reason: every row this sweep
// claims has already failed against something unavailable, and re-asking a downed service
// every ten seconds is how a reconciler becomes a second outage. An outstanding exchange
// obligation is UNDER-selling in the meantime (the source capacity is not back), which is
// the safe direction.
//
// A restart drains immediately regardless (Runner.Run), so this bounds the steady state, not
// recovery from a deploy.
//
// A plain default, never a compose `${VAR:?}`: a mandatory marker with no matching emitter
// in scripts/env-bootstrap.sh fails `make check`'s check-required-env stage (TKT-227).
func exchangeSweepInterval() time.Duration {
	if v := os.Getenv("EXCHANGE_REVERSAL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return time.Minute
}

// exchangeSweepBatch bounds one claim, and with it the lease. The same 16 as the refund
// reconciler even though each row here makes at most ONE downstream call rather than two —
// the batch bounds how many rows a pass claims, and the call count is expressed in
// MaxCallsPerExchange, where LeaseFor reads it.
func exchangeSweepBatch() int {
	if v := os.Getenv("EXCHANGE_REVERSAL_BATCH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 16
}

// credentialsAreDistinct refuses a deployment where any two of commerce's three
// credentials hold the same value.
//
// Extracted from run() so it can actually be tested: a fail-closed startup check
// that nothing exercises is a check nobody knows is broken. run() opens databases
// and listens, so it is not a unit-test seam.
func credentialsAreDistinct(internal, staffWrite, assertion, payments string) error {
	for _, pair := range []struct{ aName, a, bName, b string }{
		{staffWriteTokenEnv, staffWrite, "INTERNAL_SERVICE_TOKEN", internal},
		{assertionKeyEnv, assertion, "INTERNAL_SERVICE_TOKEN", internal},
		{assertionKeyEnv, assertion, staffWriteTokenEnv, staffWrite},
		// The fourth (ai-review S8). Its pairs are added HERE, with the others,
		// because the comment above is only true if someone actually does it:
		// a credential wired in but left out of this check is the one credential
		// whose separation is never verified.
		{runtimecfg.PaymentsTokenEnv, payments, "INTERNAL_SERVICE_TOKEN", internal},
		{runtimecfg.PaymentsTokenEnv, payments, staffWriteTokenEnv, staffWrite},
		{runtimecfg.PaymentsTokenEnv, payments, assertionKeyEnv, assertion},
	} {
		if pair.a == pair.b {
			return fmt.Errorf("%s must not equal %s: the separate credentials exist so one leaking "+
				"does not imply the others, and identical values remove that boundary while looking "+
				"configured", pair.aName, pair.bName)
		}
	}
	return nil
}
