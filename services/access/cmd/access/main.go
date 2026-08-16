// Access service. Owns ticket issuance, delivery projection, signed credentials,
// scanning/redemption, and admission history (ADR-002). M1 implements order-event
// consumption, guest ticket links, QR issuance, and atomic single-use redemption.
package main

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"

	apispec "ticketing/services/access/api"
	accessapi "ticketing/services/access/internal/api"
	"ticketing/services/access/internal/consumer"
	"ticketing/services/access/internal/lifecyclejob"
	accessstore "ticketing/services/access/internal/store"
	"ticketing/services/access/internal/ticket"
	"ticketing/shared/httpx"
	"ticketing/shared/obs"
	"ticketing/shared/runtimecfg"
)

const serviceName = "access"

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

// subcommands are the one-shot modes. migrate and lifecycle-backfill are
// separate jobs on purpose (ADR-021 §D9 as amended for ADR-022): migrate is DDL
// under ADR-008's surviving 30-second deadline, while the backfill signs a head
// per ticket and cannot be held to it.
func subcommands() map[string]func([]string) error {
	return map[string]func([]string) error{
		"migrate":              func([]string) error { return migrate() },
		"lifecycle-backfill":   func([]string) error { return backfillLifecycle() },
		"verify-lifecycle":     func([]string) error { return verifyLifecycle() },
		"seal-lifecycle-epoch": func([]string) error { return sealLifecycleEpoch() },
		"set-lifecycle-mode":   setLifecycleMode,
		"keygen":               func([]string) error { return keygen(os.Stdout) },
		// Scanner device enrolment (ai-review S1). See scanner_devices.go for why
		// this is a CLI rather than an HTTP endpoint.
		"enrol-scanner":  enrolScanner,
		"revoke-scanner": revokeScanner,
		"list-scanners":  listScanners,
	}
}

// keygen prints one fresh Ed25519 key pair as "<seed> <public key>", both in the
// raw-standard base64 the key loaders and keyrings already read.
//
// It exists because the three signing keys shipped as ACTIVE Compose defaults
// until ai-review S5, and removing a default is only half a fix: something has to
// mint the replacement, in the same env-bootstrap loop that already mints the
// bearer tokens (Makefile) and in the isolated smoke/browser stacks
// (scripts/stack-env.sh). Neither is a place that can derive a public key from a
// seed on its own, and `openssl genpkey -algorithm ED25519` is not portable
// enough to be the platform's key generator — Go is already required to build
// anything here.
//
// It serves both ACCESS_QR_* and ACCESS_LIFECYCLE_*: same algorithm, same
// encoding, and the namespaces that must stay distinct live in the KID, which the
// caller supplies. It deliberately prints only the pair and never touches the
// environment or a file — what to name the key and where to put it is the
// caller's decision, not this subcommand's.
func keygen(out io.Writer) error {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s %s\n",
		base64.RawStdEncoding.EncodeToString(priv.Seed()),
		base64.RawStdEncoding.EncodeToString(pub))
	return err
}

// signalContext cancels on interrupt, so a long backfill stops cleanly and
// resumes where it left off rather than being killed mid-ticket.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	token, err := runtimecfg.InternalTokenFromEnv()
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
	commerceURL := os.Getenv("COMMERCE_URL")
	if commerceURL == "" {
		return errors.New("COMMERCE_URL required")
	}
	// Required, with the checked-in dev seed refused forever (ai-review S5). It
	// shipped as an ACTIVE compose default — an all-zero Ed25519 seed, in the
	// repository — so any stack whose env was unset issued and verified QR codes
	// under key material an attacker already has.
	qrSeed, err := runtimecfg.RequiredCredential("ACCESS_QR_PRIVATE_KEY", runtimecfg.RetiredAccessQRSeed, runtimecfg.CredentialMinBytes)
	if err != nil {
		return err
	}
	signer, err := ticket.New(qrSeed, os.Getenv("ACCESS_QR_KID"))
	if err != nil {
		return err
	}
	// The HMAC key for short-lived QR image links (ai-review S2). A DISTINCT
	// value from the QR signing seed above, and not an optional one: this key
	// proves "this URL was minted by us recently", the seed proves "this
	// credential admits at a gate", and a service that let one key make both
	// claims would spend a leak of the cheap one at the price of the expensive
	// one. Required so a deployment cannot silently fall back to unsigned links,
	// which is the state this finding is about.
	qrLinkKey, err := runtimecfg.RequiredCredential("ACCESS_TICKET_LINK_KEY", "", runtimecfg.CredentialMinBytes)
	if err != nil {
		return err
	}
	verifier, err := ticket.NewVerifier(os.Getenv("ACCESS_QR_PUBLIC_KEYS"), os.Getenv("ACCESS_QR_KID"))
	if err != nil {
		return err
	}
	st, err := writableStore(db)
	if err != nil {
		return err
	}

	// Refuse to boot without somewhere for integrity alarms to land. ADR-021 §D6
	// admits on a chain that does not verify, which is only defensible while the
	// alarm is load-bearing.
	//
	// Read RequireAlarmRoute's contract before quoting this: it proves an alarm
	// is RETAINED, not that anyone reads it — this repo ships no consumer for the
	// durable, so a default deployment passes and is still unmonitored. §D6's "an
	// unmonitored deployment must not run fail-open" is a deployment obligation
	// no boot check can discharge. The pending-depth gauge below is the runtime
	// signal that nobody is collecting.
	//
	// Boot-time rather than /readyz is deliberate — /readyz is what the container
	// healthcheck probes, so a continuous check would let a broker hiccup close
	// every turnstile, which is the customer-denying failure §D6 exists to avoid.
	alarmStream := os.Getenv(envAlarmStream)
	if alarmStream == "" {
		alarmStream = defaultAlarmStreamValue
	}
	if err := lifecyclejob.RequireAlarmRoute(ctx, js, lifecyclejob.AlarmRoute{
		Stream: alarmStream, Durable: os.Getenv(envAlarmDurable), Subject: accessstore.SubjectIntegrityAlarm,
		DurableEnv: envAlarmDurable, Class: "integrity",
	}); err != nil {
		return fmt.Errorf("integrity alarm route: %w", err)
	}
	// The admission-conflict class (ADR-025 §D6) gets the same fail-closed boot
	// guard: reconciliation owes a durable alarm per conflict, which is only
	// meaningful while the subject has somewhere durable to land.
	if err := lifecyclejob.RequireAlarmRoute(ctx, js, lifecyclejob.AlarmRoute{
		Stream: alarmStream, Durable: os.Getenv(envConflictDurable), Subject: accessstore.SubjectAdmissionConflictAlarm,
		DurableEnv: envConflictDurable, Class: "admission conflict",
	}); err != nil {
		return fmt.Errorf("admission conflict alarm route: %w", err)
	}
	// The derived policy-conflict class (ADR-025 §D2, TKT-87) is revisable —
	// raise/withdraw pairs — but no less owed: same fail-closed boot guard.
	if err := lifecyclejob.RequireAlarmRoute(ctx, js, lifecyclejob.AlarmRoute{
		Stream: alarmStream, Durable: os.Getenv(envPolicyDurable), Subject: accessstore.SubjectAdmissionPolicyConflictAlarm,
		DurableEnv: envPolicyDurable, Class: "policy conflict",
	}); err != nil {
		return fmt.Errorf("policy conflict alarm route: %w", err)
	}
	interval, err := checkpointInterval()
	if err != nil {
		return err
	}
	checkpointer := lifecyclejob.NewCheckpointer(st, interval, log, nil)
	drainer := lifecyclejob.NewAlarmDrainer(st, lifecyclejob.NewJetStreamPublisher(js), 5*time.Second, 32, log)
	if err := checkpointer.ObserveMetrics(otel.Meter("ticketing/access/lifecycle")); err != nil {
		return fmt.Errorf("checkpoint metrics: %w", err)
	}
	if err := drainer.ObserveMetrics(otel.Meter("ticketing/access/lifecycle"), nil); err != nil {
		return fmt.Errorf("alarm metrics: %w", err)
	}
	if err := lifecyclejob.ObserveAlarmRoute(otel.Meter("ticketing/access/lifecycle"), js, alarmStream, os.Getenv(envAlarmDurable)); err != nil {
		return fmt.Errorf("alarm route metrics: %w", err)
	}
	// The conflict class gets the same pending-depth evidence as the integrity
	// class: RequireAlarmRoute proves the durable exists, only this proves
	// anyone is draining it (ai-review R2).
	if err := lifecyclejob.ObserveAlarmRoute(otel.Meter("ticketing/access/lifecycle"), js, alarmStream, os.Getenv(envConflictDurable)); err != nil {
		return fmt.Errorf("conflict alarm route metrics: %w", err)
	}
	if err := lifecyclejob.ObserveAlarmRoute(otel.Meter("ticketing/access/lifecycle"), js, alarmStream, os.Getenv(envPolicyDurable)); err != nil {
		return fmt.Errorf("policy conflict alarm route metrics: %w", err)
	}
	workers, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	go checkpointer.Run(workers)
	go drainer.Run(workers)
	consumerOptions, err := consumer.ParseOptions(os.Getenv("ACCESS_EVENT_RETRY_BACKOFF"))
	if err != nil {
		return err
	}
	cons := consumer.New(js, st, signer, obs.Client(), commerceURL, token, os.Getenv("PUBLIC_BASE_URL"), consumer.NewLogMailer(log), log, consumerOptions)
	// One slot per producer below. At capacity 1 the second consumer to fail
	// blocks on the send, and its error — which may be a genuine failure, not a
	// shutdown cancellation — is never observable by awaitShutdown (ai-review R1).
	consumerErr := make(chan error, 2)
	// Log at the PRODUCER, not only where the error is consumed (TKT-122).
	// awaitShutdown's drain is a deliberate snapshot: an error arriving after it
	// goes to a channel nobody reads, and main's stderr print never happens, so
	// today that failure vanishes with no exit code AND no line anywhere. The
	// exit code is genuinely not worth blocking shutdown for — compose runs these
	// services `restart: unless-stopped`, which suppresses restart on an
	// operator-requested stop whatever the status — but losing the evidence is a
	// separate cost, and one line buys it back with no latency and no lifecycle
	// coupling. Same predicate awaitShutdown uses, so a clean unwind stays quiet.
	reportConsumer := func(err error) error {
		if err != nil && !isShutdownConsumerError(ctx, err) {
			log.ErrorContext(ctx, "consumer stopped with an error", "err", err)
		}
		return err
	}
	go func() { consumerErr <- reportConsumer(cons.Run(ctx)) }()
	// The slot-policy projector (TKT-87): its DeliverAll durable replays the
	// publication history, so pass policies backfill on first boot for free.
	policyCons := consumer.NewPolicyConsumer(js, st, log)
	go func() { consumerErr <- reportConsumer(policyCons.Run(ctx)) }()

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
		if !policyCons.Ready() {
			// A parked future publication schema latches this false: better
			// loudly unready than silently enforcing single on a pass slot.
			http.Error(w, "policy projector not ready", http.StatusServiceUnavailable)
			return
		}
		health.ServeHTTP(w, req)
	}))
	r.Method(http.MethodGet, "/openapi.yaml", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(apispec.Spec)
	}))
	r.Mount("/", accessapi.New(st, verifier, token).WithQRLinkKey(qrLinkKey).Router(log, validateResponses))

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

// awaitShutdown blocks until the server fails, a consumer fails, or the signal
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
		// select picks between them at random (TKT-98).
		if !isShutdownConsumerError(ctx, err) {
			return err
		}
	case <-ctx.Done():
	}
	// Both paths above arrive here on a shutdown-caused exit, and either can
	// leave a second consumer's error still queued — consumerErr is shared and
	// a select takes one value. Drain everything that has arrived so a genuine
	// failure racing the signal still takes the process down, whichever branch
	// won the flip (ai-review R1).
	//
	// A snapshot, deliberately: it cannot see an error that arrives after the
	// drain. Closing that last window means blocking shutdown until both
	// consumers have terminated, which trades a missed exit code on an
	// operator-requested stop for a stop a wedged consumer can delay.
	//
	// TKT-122 weighed that trade and kept the snapshot — see ADR-034 §"The
	// shutdown drain stays a snapshot": `restart: unless-stopped` suppresses
	// restart on an operator stop whatever the exit code, so the status feeds
	// nothing. The error a late failure produces is no longer lost, though: the
	// producing goroutine above logs it before sending.
drained:
	for {
		select {
		case err := <-consumerErr:
			if !isShutdownConsumerError(ctx, err) {
				return err
			}
		default:
			break drained
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return shutdown(sctx)
}

// isShutdownConsumerError reports whether err is a consumer's Run unwinding
// because this process's signal context was canceled. errors.Is rather than a
// match on the "consumer stopped" prefix: the policy projector has a third
// cancellation exit — cons.Info during the initial backlog drain — that returns
// a nats-wrapped context.Canceled carrying no prefix at all.
func isShutdownConsumerError(ctx context.Context, err error) bool {
	return ctx.Err() != nil && errors.Is(err, context.Canceled)
}
