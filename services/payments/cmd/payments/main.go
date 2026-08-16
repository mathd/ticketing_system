// Payments service. Owns the PSP boundary and immutable payment facts (ADR-002).
// M1 implements deterministic fake-provider charges, idempotent operations, and
// a signed, hash-chained journal with verification tooling.
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"

	"ticketing/services/payments/internal/api"
	"ticketing/services/payments/internal/psp"
	paymentstore "ticketing/services/payments/internal/store"
	"ticketing/shared/httpx"
	"ticketing/shared/obs"
	"ticketing/shared/runtimecfg"
)

const serviceName = "payments"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := migrate(); err != nil {
			fmt.Fprintf(os.Stderr, "%s migrate: %v\n", serviceName, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "verify-concurrent-append" {
		if err := verifyConcurrentAppend(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "verify-journal" {
		if err := verifyJournal(); err != nil {
			fmt.Fprintln(os.Stderr, err)
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

	if err := paymentstore.Migrate(ctx, db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func verifyConcurrentAppend() error {
	keys, err := signingConfig()
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	j := paymentstore.New(db, keys)
	f := paymentstore.Fact{
		ID:          uuid.MustParse("00000000-0000-0000-0000-000000000901"),
		OrganizerID: uuid.MustParse("00000000-0000-0000-0000-000000000902"),
		Type:        "order.created", OccurredAt: time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
		BuyerID: uuid.MustParse("00000000-0000-0000-0000-000000000903"),
		Amount:  100, Currency: "EUR", Payload: map[string]string{"order_id": "00000000-0000-0000-0000-000000000904"},
	}
	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	newCount, replayCount := 0, 0
	var firstErr error
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, replay, appendErr := j.Append(context.Background(), f)
			mu.Lock()
			defer mu.Unlock()
			if appendErr != nil && firstErr == nil {
				firstErr = appendErr
			}
			if replay {
				replayCount++
			} else if appendErr == nil {
				newCount++
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if newCount != 1 || replayCount != workers-1 {
		return fmt.Errorf("concurrent append: new=%d replay=%d", newCount, replayCount)
	}
	return nil
}

// pspForKey selects the payment provider from STRIPE_SECRET_KEY, fail-fast like
// signingConfig (ADR-032): unset or the explicit "fake" sentinel selects the fake (the
// offline default — the gate never talks to Stripe), a test-mode secret key selects the
// Stripe adapter, and a LIVE key or any unrecognized value refuses startup — a typo must
// never silently fall back to the fake, and this testbed must never hold a live key.
func pspForKey(key string) (psp.PSP, time.Duration, error) {
	switch {
	case key == "" || key == "fake":
		// The fake retains "idempotency keys" forever (its Status is a pure function of
		// the replayed token), so the status-replay contract is unbounded: retention 0.
		return psp.NewFake(), 0, nil
	case strings.HasPrefix(key, "sk_test_"):
		// Stripe retains idempotency keys ~24h; past that a same-key replay mints a NEW
		// PaymentIntent, so status replay is bounded (ADR-032 §Status/replay amendment).
		return psp.NewStripe(key, "https://api.stripe.com", nil), 24 * time.Hour, nil
	case strings.HasPrefix(key, "sk_live_"):
		return nil, 0, errors.New("STRIPE_SECRET_KEY is a LIVE key; this service only accepts test-mode keys")
	default:
		return nil, 0, errors.New("STRIPE_SECRET_KEY unrecognized: expected empty, \"fake\", or an sk_test_ key")
	}
}

// statusReplayRetention resolves the effective retention: the provider's own bound,
// overridable by PAYMENTS_STATUS_REPLAY_RETENTION (a Go duration). The override exists
// for the offline stack: the fake never expires keys, so proving the deadline path
// against real predicates needs an injected bound — never a changed default.
//
// Against a BOUNDED provider (Stripe) the override may only SHORTEN the window
// (ai-review B3): lengthening or disabling it would let the status path replay an
// idempotency key the provider has already forgotten — minting a second PaymentIntent,
// the one thing the deadline exists to prevent. An unparseable, negative, or
// bound-extending value refuses startup like every other config error here.
func statusReplayRetention(fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv("PAYMENTS_STATUS_REPLAY_RETENTION")
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("PAYMENTS_STATUS_REPLAY_RETENTION unparseable: %q", raw)
	}
	if fallback > 0 && (d == 0 || d > fallback) {
		return 0, fmt.Errorf("PAYMENTS_STATUS_REPLAY_RETENTION %q would extend the provider's %s replay bound; it may only shorten it", raw, fallback)
	}
	return d, nil
}

// signingConfig builds the journal keyring: the active key stays in the variables it
// has always used, and JOURNAL_HISTORICAL_KEYS optionally carries retired keys as
// "kid=<base64.RawStdEncoding secret>,..." so their era stays verifiable after a
// rotation (ADR-016 §Decision 8; ADR-032 §Keyring configuration, rotation and retirement).
// the previous single-key behaviour exactly, so no deployed configuration changes.
//
// Fail-fast, like every other config reader here: a malformed ring refuses startup
// rather than surfacing as a verification failure long after the fact.
// journalKeyMinBytes is the floor JOURNAL_SIGNING_KEY has always been held to
// (ADR-032: "a secret under 16 bytes" is a stated rejected configuration). It is
// named here so the startup check and the keyring agree on one number, and so the
// value that diverges from runtimecfg.CredentialMinBytes is a declared policy
// rather than a literal argument someone might "correct".
//
// paymentstore.NewKeyring applies the same floor to the ACTIVE key and to every
// historical key. Checking it here as well is not redundant: it fails before the
// keyring is built, with a message naming the environment variable an operator
// actually set.
const journalKeyMinBytes = 16

func signingConfig() (*paymentstore.Keyring, error) {
	id := os.Getenv("JOURNAL_KEY_ID")
	if id == "" {
		return nil, errors.New("JOURNAL_KEY_ID and JOURNAL_SIGNING_KEY (>=16 bytes) required")
	}
	// The checked-in "local-development-journal-key" was an ACTIVE compose
	// default until ai-review S5, so a stack with no env signed its money journal
	// under a key that is in the repository — and the comment below already knew
	// it was "exactly the low-entropy kind" an oracle would enjoy. Refused
	// forever now, the same way the bearer tokens are (TKT-83).
	// journalKeyMinBytes, NOT runtimecfg.CredentialMinBytes: this key keeps the
	// 16-byte contract ADR-032 already states, while TKT-252 moved every ordinary
	// credential to 32. The divergence is deliberate and ADR-059 records why —
	// raising the journal floor is a separate decision with its own blast radius
	// (the smoke journal literals, JOURNAL_HISTORICAL_KEYS, and a documented claim
	// in docs/development.md that rests on it), and it was ruled out of TKT-252's
	// scope rather than left unexamined.
	//
	// Changing this argument to the shared constant would look like tidying and
	// would silently refuse conformant deployed keys.
	// TestJournalSigningKeyKeepsItsOwnFloorBelowTheOrdinaryOne pins it.
	secret, err := runtimecfg.RequiredCredential("JOURNAL_SIGNING_KEY", runtimecfg.RetiredJournalSigningKey, journalKeyMinBytes)
	if err != nil {
		return nil, err
	}
	return paymentstore.NewKeyring(id, []byte(secret), os.Getenv("JOURNAL_HISTORICAL_KEYS"))
}

// logJournalSigningKey states which key id the journal is being signed under, so the
// active kid is visible without reading the environment of a running container.
//
// It logs the key ID ONLY. An earlier revision also logged a truncated HMAC of the key as
// a "fingerprint"; the ai-review caught that this publishes a deterministic tag over a
// fixed public message for a SYMMETRIC secret, which is an offline oracle for guessing the
// key — and this repo's own default key (local-development-journal-key) is exactly the
// low-entropy kind that makes such an oracle useful. The mis-paste it was meant to detect
// is now rejected at startup by NewKeyring instead, which leaks nothing. The kid is
// already stored in plaintext on every journal row, so logging it discloses nothing new.
//
// Deliberately NOT called from verifyConcurrentAppend: that subcommand is a smoke
// fault-injection helper, not an operator surface (plan-final D4).
func logJournalSigningKey(ctx context.Context, log *slog.Logger, keys *paymentstore.Keyring) {
	log.InfoContext(ctx, "journal signing key loaded", "journal_key_id", keys.ActiveKeyID())
}

func verifyJournal() error {
	keys, err := signingConfig()
	if err != nil {
		return err
	}
	// An operator running verify-journal to diagnose a failure is exactly the person who
	// needs to know which key is loaded. stderr keeps stdout free for scripting, and
	// smoke.sh's retired-key assertion is a substring match, so an extra line is safe.
	logJournalSigningKey(context.Background(), obs.NewLogger(serviceName, os.Stderr), keys)
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return paymentstore.New(db, keys).Verify(context.Background())
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
	// Payments has its OWN credential (ai-review S8). It used to authenticate on
	// INTERNAL_SERVICE_TOKEN, the one value every service holds — so a compromise
	// of any of them opened charge, void, refund and partial refund. Commerce is
	// the only caller and holds both; nothing else needs to reach this surface.
	internalToken, err := runtimecfg.PaymentsTokenFromEnv()
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
	keys, err := signingConfig()
	if err != nil {
		return err
	}
	logJournalSigningKey(ctx, log, keys)
	provider, providerRetention, err := pspForKey(os.Getenv("STRIPE_SECRET_KEY"))
	if err != nil {
		return err
	}
	retention, err := statusReplayRetention(providerRetention)
	if err != nil {
		return err
	}

	nc, err := nats.Connect(os.Getenv("NATS_URL"),
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1))
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()

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
	r.Mount("/", api.NewWithPSPRetention(paymentstore.New(db, keys), internalToken, provider, retention).Router(log, validateResponses))

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
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}
