package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	"ticketing/services/access/internal/lifecycle"
	"ticketing/services/access/internal/lifecyclejob"
	accessstore "ticketing/services/access/internal/store"
)

// Lifecycle trail configuration (ADR-021). The key material is deliberately
// separate from ACCESS_QR_*: a leaked credential-signing key must not also
// authorize rewriting history (§D4).
const (
	envLifecycleSeed        = "ACCESS_LIFECYCLE_PRIVATE_KEY"
	envLifecycleKID         = "ACCESS_LIFECYCLE_KID"
	envLifecyclePublicKeys  = "ACCESS_LIFECYCLE_PUBLIC_KEYS"
	envCheckpointInterval   = "ACCESS_LIFECYCLE_CHECKPOINT_INTERVAL"
	envFailureThreshold     = "ACCESS_LIFECYCLE_FAILURE_THRESHOLD"
	envFailureWindow        = "ACCESS_LIFECYCLE_FAILURE_WINDOW"
	envAlarmStream          = "ACCESS_LIFECYCLE_ALARM_STREAM"
	envAlarmDurable         = "ACCESS_LIFECYCLE_ALARM_DURABLE"
	envConflictDurable      = "ACCESS_ADMISSION_CONFLICT_DURABLE"
	envPolicyDurable        = "ACCESS_POLICY_CONFLICT_DURABLE"
	defaultAlarmStreamValue = "PLATFORM"
)

func openDB() (*sql.DB, error) {
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	return db, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return d, nil
}

func envInt(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return n, nil
}

// lifecycleKeyring loads public lifecycle keys. This is all `verify-lifecycle`
// gets: ADR-021 §D4 chose Ed25519 over the money journal's HMAC precisely so a
// verifier need not hold the power to write, and a keyring without a signer is
// what makes that structural rather than a promise.
func lifecycleKeyring() (*lifecycle.Keyring, error) {
	return lifecycle.NewKeyring(os.Getenv(envLifecyclePublicKeys))
}

// lifecycleSigner loads the active signing key and checks the keyring can verify
// what it signs — a typo that left issuance signing under a key nothing can
// verify would otherwise surface only at the next audit.
func lifecycleSigner(keyring *lifecycle.Keyring) (*lifecycle.Signer, error) {
	signer, err := lifecycle.NewSigner(os.Getenv(envLifecycleSeed), os.Getenv(envLifecycleKID))
	if err != nil {
		return nil, err
	}
	if !keyring.Has(signer.KeyID()) {
		return nil, fmt.Errorf("%s must include %s (%q)", envLifecyclePublicKeys, envLifecycleKID, signer.KeyID())
	}
	return signer, nil
}

func lifecyclePolicy() (accessstore.Policy, error) {
	def := accessstore.DefaultPolicy()
	threshold, err := envInt(envFailureThreshold, def.FailureThreshold)
	if err != nil {
		return accessstore.Policy{}, err
	}
	window, err := envDuration(envFailureWindow, def.Window)
	if err != nil {
		return accessstore.Policy{}, err
	}
	return accessstore.Policy{FailureThreshold: threshold, Window: window}, nil
}

func checkpointInterval() (time.Duration, error) {
	return envDuration(envCheckpointInterval, lifecyclejob.DefaultInterval)
}

// maxPendingAge is the staleness bound `verify-lifecycle` applies to
// uncheckpointed head changes: two intervals, so one missed pass is not a
// finding but a stopped worker is. It bounds the audit tool only — nothing here
// touches readiness, because the checkpoint is TKT-11 scaffolding (§D2) and a
// stalled scaffold must not close a turnstile.
//
// The floor matters. This check asks "is the worker dead", not "is the worker
// quick", and 2x a sub-second interval would answer the second question — a
// smoke run or a loaded box would fail an audit over a scheduling hiccup.
func maxPendingAge(interval time.Duration) time.Duration {
	if bound := 2 * interval; bound > 30*time.Second {
		return bound
	}
	return 30 * time.Second
}

// writableStore builds a store that can append to the trail.
func writableStore(db *sql.DB) (*accessstore.Postgres, error) {
	keyring, err := lifecycleKeyring()
	if err != nil {
		return nil, err
	}
	signer, err := lifecycleSigner(keyring)
	if err != nil {
		return nil, err
	}
	policy, err := lifecyclePolicy()
	if err != nil {
		return nil, err
	}
	return accessstore.New(db, accessstore.Config{Signer: signer, Keyring: keyring, Policy: policy}), nil
}

// backfillLifecycle adopts existing history into the chain, then drains the
// resulting head changes into baseline checkpoints.
//
// It runs as its own one-shot job, NOT inside `migrate` (ADR-021 §D9 as amended
// for ADR-022). ADR-008's 30-second fail-fast deadline still bounds the migrate
// job, and the service depends on that job with service_completed_successfully —
// so a backfill that overran it would not merely be slow, it would stop Access
// from starting at all. This work Ed25519-signs a head per ticket, so its cost
// scales with history. It gets no overall deadline; it gets resumability and
// cancellation instead.
func backfillLifecycle() error {
	ctx, stop := signalContext()
	defer stop()

	db, err := openDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	st, err := writableStore(db)
	if err != nil {
		return err
	}
	chained, err := st.BackfillLifecycle(ctx, 128)
	if err != nil {
		return fmt.Errorf("backfill: %w", err)
	}
	fmt.Printf("access lifecycle-backfill: chained %d ticket(s)\n", chained)

	// Drain the backlog this backfill just queued into baseline checkpoints, so
	// the service does not start with an unbounded pending set that the first
	// checkpoint pass would have to swallow whole.
	cp := lifecyclejob.NewCheckpointer(st, time.Second, nil, nil)
	for {
		organizers, err := st.PendingCheckpointOrganizers(ctx)
		if err != nil {
			return err
		}
		if len(organizers) == 0 {
			return nil
		}
		if cp.Once(ctx) == 0 {
			return errors.New("backfill checkpoint drain made no progress")
		}
	}
}

// verifyLifecycle is the offline verifier (ADR-021 §D7). Public keys only: it
// never loads ACCESS_LIFECYCLE_PRIVATE_KEY, and the store it builds has no
// signer, so it is structurally incapable of writing to the trail.
//
// A clean result means modification, insertion and reordering have not touched
// the covered history — against an adversary who cannot re-sign it. A
// consistently rolled-back trail verifies clean and always will (§The trust
// boundary). Do not quote a green run without naming the adversary.
func verifyLifecycle() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db, err := openDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	keyring, err := lifecycleKeyring()
	if err != nil {
		return err
	}
	interval, err := checkpointInterval()
	if err != nil {
		return err
	}
	st := accessstore.New(db, accessstore.Config{Keyring: keyring})
	if err := st.VerifyLifecycle(ctx, accessstore.VerifyOptions{
		MaxPendingAge:   maxPendingAge(interval),
		RequireCoverage: true,
	}); err != nil {
		return err
	}
	fmt.Println("access verify-lifecycle: chains, heads, epoch signatures and checkpoints verified")
	fmt.Println("  scope: modification and insertion are evident against an adversary who cannot re-sign.")
	fmt.Println("  NOT covered: targeted rollback, or a compromised current key (ADR-021 §The trust boundary; TKT-11).")
	return nil
}

// sealLifecycleEpoch retains each ticket's head signature under the outgoing key
// before rotation (ADR-021 §D5). Run at quiescence, immediately before rotating
// and destroying the outgoing seed.
func sealLifecycleEpoch() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db, err := openDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// A copy of existing head signatures, so no private key is needed.
	keyring, err := lifecycleKeyring()
	if err != nil {
		return err
	}
	st := accessstore.New(db, accessstore.Config{Keyring: keyring})
	sealed, err := st.SealEpoch(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("access seal-lifecycle-epoch: retained %d head signature(s)\n", sealed)
	fmt.Println("  this raises the cost of a current-key compromise; it does not contain one.")
	fmt.Println("  these rows are deletable by anyone who can write to this database (ADR-021 §D5).")
	return nil
}

// setLifecycleMode records an operator's explicit degraded-mode choice
// (ADR-021 §D6): usage `access set-lifecycle-mode <organizer-uuid> <normal|operator_deny|operator_admit> <who>`.
func setLifecycleMode(args []string) error {
	if len(args) != 3 {
		return errors.New("usage: access set-lifecycle-mode <organizer-id> <normal|operator_deny|operator_admit> <operator>")
	}
	organizerID, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("organizer id: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := openDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	keyring, err := lifecycleKeyring()
	if err != nil {
		return err
	}
	st := accessstore.New(db, accessstore.Config{Keyring: keyring})
	if err := st.SetMode(ctx, organizerID, accessstore.Mode(args[1]), args[2]); err != nil {
		return err
	}
	fmt.Printf("access set-lifecycle-mode: organizer %s is now %s (set by %s)\n", organizerID, args[1], args[2])
	return nil
}
