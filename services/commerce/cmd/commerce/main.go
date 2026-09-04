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
	"ticketing/shared/cmdline"
	"ticketing/shared/httpx"
	"ticketing/shared/mail"
	"ticketing/shared/obs"
	"ticketing/shared/runtimecfg"
)

const (
	serviceName         = "commerce"
	recoveryCallTimeout = 10 * time.Second
)

func main() {
	result := execute(os.Args[1:], productionCommandCallbacks(), run)
	if result.Err != nil {
		if result.Name == "" {
			fmt.Fprintf(os.Stderr, "%s: %v\n", serviceName, result.Err)
		} else {
			fmt.Fprintf(os.Stderr, "%s %s: %v\n", serviceName, result.Name, result.Err)
		}
	}
	if result.ExitCode != 0 {
		os.Exit(result.ExitCode)
	}
}

// subcommands keeps operator commands in the path main executes. The parked-order and
// wedged-exchange commands are the supported way to resume work that automated recovery
// has stopped claiming (TKT-146 and TKT-255).
type commandCallbacks struct {
	migrate                                                      func() error
	healthcheck                                                  func() int
	enrolReseller, revokeReseller, listResellers                 func([]string) error
	listParked, unparkOrder, listWedgedExchanges, unwindExchange func([]string) error
}

func productionCommandCallbacks() commandCallbacks {
	return commandCallbacks{
		migrate: migrate, healthcheck: healthcheck,
		enrolReseller: enrolReseller, revokeReseller: revokeReseller, listResellers: listResellers,
		listParked: listParked, unparkOrder: unparkOrder,
		listWedgedExchanges: listWedgedExchanges, unwindExchange: unwindExchange,
	}
}

func execute(args []string, callbacks commandCallbacks, serve func() error) cmdline.Result {
	return cmdline.Dispatch(args, commandRegistry(callbacks), serve)
}

func commandRegistry(callbacks commandCallbacks) cmdline.Registry {
	return cmdline.Registry{
		"migrate":               cmdline.WithoutArgs(callbacks.migrate),
		"healthcheck":           cmdline.ExitStatus(callbacks.healthcheck),
		"enrol-reseller":        cmdline.WithArgs(callbacks.enrolReseller),
		"revoke-reseller":       cmdline.WithArgs(callbacks.revokeReseller),
		"list-resellers":        cmdline.WithArgs(callbacks.listResellers),
		"list-parked":           cmdline.WithArgs(callbacks.listParked),
		"unpark-order":          cmdline.WithArgs(callbacks.unparkOrder),
		"list-wedged-exchanges": cmdline.WithArgs(callbacks.listWedgedExchanges),
		"unwind-exchange":       cmdline.WithArgs(callbacks.unwindExchange),
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
// It lets a holder attribute a checkout to any customer, and nothing else.
const assertionKeyEnv = "COMMERCE_CUSTOMER_ASSERTION_KEY"

type workerSettings struct {
	interval time.Duration
	batch    int
}

type workerConfig struct {
	outbox       workerSettings
	recovery     workerSettings
	cancellation workerSettings
	reversal     workerSettings
	exchange     workerSettings
}

type workerLeaseConfig struct {
	recovery     time.Duration
	cancellation time.Duration
	reversal     time.Duration
	exchange     time.Duration
}

type workerDependencies struct {
	recoveryStore     recovery.Store
	recoveryPayments  recovery.Payments
	recoveryInventory recovery.Inventory
	recoveryJournal   recovery.Journal
	recoveryCompleter recovery.Completer
	cancellationStore bulkrefund.Store
	refunds           bulkrefund.Refunder
	reversalStore     reversal.Store
	exchangeStore     exchangesweep.Store
	exchanges         exchangesweep.Discharger
}

type workerRunners struct {
	recovery     *recovery.Runner
	cancellation *bulkrefund.Runner
	reversal     *reversal.Runner
	exchange     *exchangesweep.Runner
}

func workerLeasesFor(config workerConfig, recoveryClient, apiClient *http.Client) (workerLeaseConfig, error) {
	if recoveryClient == nil || recoveryClient.Timeout <= 0 {
		return workerLeaseConfig{}, errors.New("recovery client must have a positive timeout")
	}
	if apiClient == nil || apiClient.Timeout <= 0 {
		return workerLeaseConfig{}, errors.New("API client must have a positive timeout")
	}

	recoveryLease, err := recovery.LeaseFor(config.recovery.batch, recoveryClient.Timeout)
	if err != nil {
		return workerLeaseConfig{}, fmt.Errorf("recovery lease: %w", err)
	}
	cancellationLease, err := bulkrefund.LeaseFor(config.cancellation.batch, apiClient.Timeout)
	if err != nil {
		return workerLeaseConfig{}, fmt.Errorf("cancellation lease: %w", err)
	}
	reversalLease, err := reversal.LeaseFor(config.reversal.batch, apiClient.Timeout)
	if err != nil {
		return workerLeaseConfig{}, fmt.Errorf("reversal lease: %w", err)
	}
	exchangeLease, err := exchangesweep.LeaseFor(config.exchange.batch, apiClient.Timeout)
	if err != nil {
		return workerLeaseConfig{}, fmt.Errorf("exchange lease: %w", err)
	}
	return workerLeaseConfig{
		recovery: recoveryLease, cancellation: cancellationLease,
		reversal: reversalLease, exchange: exchangeLease,
	}, nil
}

// constructWorkerRunners is the production construction seam for the four leased workers.
// Keeping each batch and lease assignment here lets a test observe the values at Store.Claim,
// after they have crossed the runner constructor rather than only at the sizing helper.
func constructWorkerRunners(config workerConfig, recoveryClient, apiClient *http.Client,
	dependencies workerDependencies, log *slog.Logger) (workerRunners, error) {
	leases, err := workerLeasesFor(config, recoveryClient, apiClient)
	if err != nil {
		return workerRunners{}, err
	}
	recoverer, err := recovery.New(dependencies.recoveryStore, dependencies.recoveryPayments,
		dependencies.recoveryInventory, dependencies.recoveryJournal, dependencies.recoveryCompleter,
		config.recovery.interval, config.recovery.batch, leases.recovery, log)
	if err != nil {
		return workerRunners{}, fmt.Errorf("recovery runner: %w", err)
	}
	return workerRunners{
		recovery: recoverer,
		cancellation: bulkrefund.New(dependencies.cancellationStore, dependencies.refunds,
			config.cancellation.interval, config.cancellation.batch, leases.cancellation),
		reversal: reversal.New(dependencies.reversalStore, dependencies.refunds,
			config.reversal.interval, config.reversal.batch, leases.reversal, log),
		exchange: exchangesweep.New(dependencies.exchangeStore, dependencies.exchanges,
			config.exchange.interval, config.exchange.batch, leases.exchange, log),
	}, nil
}

var defaultWorkerConfig = workerConfig{
	outbox:       workerSettings{interval: 5 * time.Second, batch: 32},
	recovery:     workerSettings{interval: 30 * time.Second, batch: 16},
	cancellation: workerSettings{interval: 10 * time.Second, batch: 8},
	reversal:     workerSettings{interval: time.Minute, batch: 16},
	exchange:     workerSettings{interval: time.Minute, batch: 16},
}

func workerConfigFromEnv() (workerConfig, error) {
	config := defaultWorkerConfig
	settings := []struct {
		intervalEnv string
		batchEnv    string
		target      *workerSettings
	}{
		{"OUTBOX_DRAIN_INTERVAL", "OUTBOX_DRAIN_BATCH", &config.outbox},
		{"RECOVERY_INTERVAL", "RECOVERY_BATCH", &config.recovery},
		{"CANCELLATION_REFUND_INTERVAL", "CANCELLATION_REFUND_BATCH", &config.cancellation},
		{"REFUND_REVERSAL_INTERVAL", "REFUND_REVERSAL_BATCH", &config.reversal},
		{"EXCHANGE_REVERSAL_INTERVAL", "EXCHANGE_REVERSAL_BATCH", &config.exchange},
	}

	for _, setting := range settings {
		interval, err := positiveDurationFromEnv(setting.intervalEnv, setting.target.interval)
		if err != nil {
			return workerConfig{}, err
		}
		batch, err := positiveIntegerFromEnv(setting.batchEnv, setting.target.batch)
		if err != nil {
			return workerConfig{}, err
		}
		setting.target.interval = interval
		setting.target.batch = batch
	}
	return config, nil
}

func positiveDurationFromEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}

func positiveIntegerFromEnv(name string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

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
	assertionKey, err := runtimecfg.RequiredCredential(assertionKeyEnv, "", runtimecfg.CredentialMinBytes)
	if err != nil {
		return err
	}
	// The payments-only credential. Commerce is payments' one
	// caller, so this is where the split is spent: commerce holds both values and
	// nothing else holds this one, which means a compromise of catalog, inventory,
	// access or the gateway no longer reaches charge, void or refund. Required
	// rather than optional-with-fallback — a fallback is how a deployment ends up
	// back on the shared token without anyone noticing.
	paymentsToken, err := runtimecfg.RequiredCredential(runtimecfg.PaymentsTokenEnv, "", runtimecfg.CredentialMinBytes)
	if err != nil {
		return err
	}
	// These four credentials grant different privileges, so compare all six pairs.
	// RequiredCredential rejects edge whitespace before this raw-string comparison;
	// two accepted values that differ here also differ on the wire.
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
	workers, err := workerConfigFromEnv()
	if err != nil {
		return fmt.Errorf("worker configuration: %w", err)
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
	apiClient := obs.Client()
	recoveryAPIClient := obs.ClientWithTimeout(recoveryCallTimeout)
	srvHandler := commerceapi.New(commerceapi.ServerConfig{
		DB:                   db,
		Client:               apiClient,
		CatalogURL:           catalogURL,
		InventoryURL:         inventoryURL,
		PaymentsURL:          paymentsURL,
		AccessURL:            accessURL,
		PublicURL:            publicURL,
		InternalToken:        token,
		PaymentsToken:        paymentsToken,
		StaffWriteToken:      staffWriteToken,
		CustomerAssertionKey: assertionKey,
		Publisher:            publisher,
	})
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
	drainer := outbox.New(db, publisher, workers.outbox.interval, workers.outbox.batch,
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
	// Warn on every boot: a row reaches `sent_at` even though the offline fake delivers
	// nothing, so this configuration must remain visible to operators.
	log.WarnContext(ctx, "transactional mail sender is the offline fake; messages are captured and NEVER delivered",
		"sender", "fake", "read_them_in", "mail_outbox")
	mailDrainer := mailer.New(db, mailSender, workers.outbox.interval, workers.outbox.batch, log)
	stopMailDrainer := start(log, "mail drainer", mailDrainer.Run)

	// The second background worker (ADR-016 §Decision 1): recovery is driven, not
	// awaited. Without it an order that lost its request stays parked forever and its
	// seat leaks — a byte-identical checkout replay is the only other thing that would
	// advance it, and nothing in the system generates one.
	recoveryClients := recovery.HTTPClients{
		// obs.ClientWithTimeout, not a bare &http.Client, so this shares the tuned
		// cross-service pool with the API server's client (TKT-308). Both call the same
		// inventory and payments hosts; a nil Transport here would give the recovery
		// runner its own untuned pool — which is what it had before that ticket, back
		// when obs.Client() also resolved to DefaultTransport and the two shared by
		// accident. Only the timeout differs, and it is deliberately tighter.
		Client:       recoveryAPIClient,
		InventoryURL: inventoryURL,
		PaymentsURL:  paymentsURL,
		Token:        token,
		// The money surface takes its own credential.
		PaymentsToken: paymentsToken,
	}
	journal := recovery.JournalFact{
		Client:      recoveryClients.Client,
		PaymentsURL: paymentsURL,
		Token:       paymentsToken,
		DB:          recovery.StoreFactDB{DB: db},
	}
	runners, err := constructWorkerRunners(workers, recoveryAPIClient, apiClient, workerDependencies{
		recoveryStore:     recovery.DBStore{DB: db},
		recoveryPayments:  recoveryClients,
		recoveryInventory: recoveryClients,
		recoveryJournal:   journal,
		recoveryCompleter: recovery.StoreCompleter{DB: db},
		cancellationStore: bulkrefund.DBStore{DB: db},
		refunds:           srvHandler.Refunds(),
		reversalStore:     reversal.DBStore{DB: db},
		exchangeStore:     exchangesweep.DBStore{DB: db},
		exchanges:         srvHandler.Exchanges(),
	}, log)
	if err != nil {
		return fmt.Errorf("worker construction: %w", err)
	}
	recoverer := runners.recovery
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
	cancellations := runners.cancellation
	stopCancellations := start(log, "cancellation refund runner", cancellations.Run)

	// Outstanding refund reversals (TKT-163, ADR-062). ADR-038 §7 shipped the reversal as
	// "visible and retryable" with nothing retrying it: an access outage left refunded
	// tickets admitting until a human replayed the idempotency key, and the caller got a
	// 200. This is the thing that comes back for them.
	//
	// It uses the same refund unit as the API and can only drive outstanding obligations.
	reversals := runners.reversal
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
	// It uses the same discharge unit and HTTP client as the callback. Its port cannot
	// move money or mark a ticket switch.
	sweeps := runners.exchange
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

// credentialsAreDistinct refuses any collision among commerce's four credentials.
func credentialsAreDistinct(internal, staffWrite, assertion, payments string) error {
	for _, pair := range []struct{ aName, a, bName, b string }{
		{staffWriteTokenEnv, staffWrite, "INTERNAL_SERVICE_TOKEN", internal},
		{assertionKeyEnv, assertion, "INTERNAL_SERVICE_TOKEN", internal},
		{assertionKeyEnv, assertion, staffWriteTokenEnv, staffWrite},
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
