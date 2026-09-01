//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/access/internal/lifecycle"
)

// persistedMode reads the organizer's posture straight from the row production writes.
//
// TKT-304 deleted `Postgres.Mode`, whose only callers were these tests — a production
// accessor kept alive by its own test suite. Reading the table directly is what the
// assertions were always about, and it is STRICTER in one way that matters: `Postgres.Mode`
// mapped `sql.ErrNoRows` to `ModeNormal`, so a test asserting `ModeNormal` would have passed
// against a row that was never written at all. This fails instead. Do not "tidy" that
// default back in.
func persistedMode(t *testing.T, ctx context.Context, db *sql.DB, organizerID uuid.UUID) Mode {
	t.Helper()
	var mode string
	err := db.QueryRowContext(ctx,
		`SELECT mode FROM lifecycle_integrity_organizer_state WHERE organizer_id=$1`,
		organizerID).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("no organizer posture row for %s: production never wrote one, and treating "+
			"that as the default posture is how an unwritten flip reads as a passing test", organizerID)
	}
	if err != nil {
		t.Fatal(err)
	}
	return Mode(mode)
}

type seeded struct {
	ticketID uuid.UUID
	id       TicketIdentity
}

func (s seeded) redeemInput() RedeemInput {
	return RedeemInput{TicketID: s.ticketID, OrderID: s.id.OrderID, OrganizerID: s.id.OrganizerID, SlotID: s.id.SlotID}
}

// issueTicket drives the real Issue path so the chain starts the way production
// starts it, rather than from hand-inserted rows.
func issueTicket(t *testing.T, ctx context.Context, st *Postgres, organizerID uuid.UUID) seeded {
	t.Helper()
	s := seeded{ticketID: uuid.New(), id: TicketIdentity{OrderID: uuid.New(), OrganizerID: organizerID, SlotID: uuid.New()}}
	err := st.Issue(ctx, IssueInput{EventID: uuid.New(), Tickets: []Ticket{{
		ID: s.ticketID, OrderID: s.id.OrderID, GuestOrderRef: uuid.New(), OrganizerID: organizerID,
		BuyerID: uuid.New(), SlotID: s.id.SlotID, TicketTypeID: uuid.New(),
		Payload: "signed-credential", IssuedAt: time.Now().UTC(),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func migratedDB(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

// corruptChain breaks a signed field. The append-only triggers block this, so
// they are disabled first — exactly what scripts/smoke.sh does to exercise the
// money journal's verifier against real corruption. The triggers are DDL and
// removable, which is the whole reason the chain exists (ADR-021 §Context).
func corruptChain(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `ALTER TABLE lifecycle_event_integrity DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE lifecycle_event_integrity SET entry_hash=decode(repeat('00',32),'hex') WHERE ticket_id=$1 AND sequence=1`, ticketID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE lifecycle_event_integrity ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIssueChainsFromGenesis(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)

	s := issueTicket(t, ctx, st, uuid.New())

	var seq int64
	var prev []byte
	if err := db.QueryRowContext(ctx, `SELECT sequence,previous_hash FROM lifecycle_event_integrity WHERE ticket_id=$1`, s.ticketID).Scan(&seq, &prev); err != nil {
		t.Fatalf("Issue did not chain the issued event: %v", err)
	}
	if seq != 1 {
		t.Fatalf("issued event at sequence %d, want 1", seq)
	}
	if len(prev) != 32 {
		t.Fatalf("genesis previous_hash is %d bytes", len(prev))
	}
	if err := New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify after issue: %v", err)
	}
}

// The regression A2 exists to stop: MarkDelivered's ON CONFLICT DO NOTHING makes
// a redelivery a silent no-op, and a blind append after it would chain an event
// that was never inserted.
func TestMarkDeliveredIsIdempotentAndChainsOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)

	s := issueTicket(t, ctx, st, uuid.New())
	messageID, err := st.DeliveryID(ctx, s.ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.MarkDelivered(ctx, s.ticketID, messageID); err != nil {
		t.Fatal(err)
	}
	// The redelivery. It must still succeed.
	if err = st.MarkDelivered(ctx, s.ticketID, messageID); err != nil {
		t.Fatalf("redelivery failed; the append path was reached after a no-op insert: %v", err)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_event_integrity WHERE ticket_id=$1`, s.ticketID); n != 2 {
		t.Fatalf("ticket has %d integrity rows after a redelivery, want 2 (issued, delivered)", n)
	}
	var head int64
	if err = db.QueryRowContext(ctx, `SELECT last_sequence FROM lifecycle_heads WHERE ticket_id=$1`, s.ticketID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	if head != 2 {
		t.Fatalf("head advanced to %d on a redelivery, want 2", head)
	}
	if err = New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify after redelivery: %v", err)
	}
}

func TestRedeemChainsContiguouslyAfterDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)

	s := issueTicket(t, ctx, st, uuid.New())
	messageID, err := st.DeliveryID(ctx, s.ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.MarkDelivered(ctx, s.ticketID, messageID); err != nil {
		t.Fatal(err)
	}
	result, err := st.Redeem(ctx, s.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.Decision != DecisionAccepted {
		t.Fatalf("redeem = %+v", result)
	}
	// A second scan of a valid chain is the ordinary duplicate, not an integrity
	// event: the trail is intact, the ticket is simply spent.
	again, err := st.Redeem(ctx, s.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if again.Accepted || again.Decision != DecisionAlreadyRedeemed {
		t.Fatalf("second scan = %+v, want already_redeemed", again)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox`); n != 0 {
		t.Fatalf("a clean duplicate raised %d integrity alarms; alarms must mean something", n)
	}
	if err = New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify after redemption: %v", err)
	}
}

// ADR-021 §Option 1 rejected the money journal because it "puts every turnstile
// for an organizer behind a single row lock". This proves that did not sneak
// back in: a scan holding one ticket's lock must not delay another ticket's scan
// for the same organizer.
func TestScansOfDifferentTicketsNeverWaitOnTheOrganizer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	organizerID := uuid.New()
	blocked := issueTicket(t, ctx, st, organizerID)
	other := issueTicket(t, ctx, st, organizerID)

	// Hold the first ticket's row lock, the same one Redeem takes.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT id FROM tickets WHERE id=$1 FOR UPDATE`, blocked.ticketID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := st.Redeem(ctx, other.redeemInput())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("redeeming a second ticket for the same organizer failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a scan blocked behind another ticket's lock: an organizer-wide serialization has been introduced (ADR-021 §Option 1, ADR-010)")
	}
}

// ADR-021 §D6, the whole clause: admit once, alarm, quarantine, deny the repeat.
func TestCorruptChainAdmitsOnceThenQuarantines(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)

	s := issueTicket(t, ctx, st, uuid.New())
	corruptChain(t, ctx, db, s.ticketID)

	first, err := st.Redeem(ctx, s.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Accepted || first.Decision != DecisionAdmittedDegraded {
		t.Fatalf("first corrupt-chain scan = %+v, want a degraded admission: denying a real customer over our own bug is the worse failure (§D6)", first)
	}
	// The chain must be untouched. Appending onto an unverified predecessor
	// would poison the ticket permanently.
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, s.ticketID); n != 0 {
		t.Fatal("a degraded admission fabricated a chained redemption on a broken chain")
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE ticket_id=$1`, s.ticketID); n != 1 {
		t.Fatalf("degraded admission left %d quarantine rows, want 1: this row is the only record that someone walked in", n)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox WHERE published_at IS NULL`); n != 1 {
		t.Fatalf("degraded admission owed %d alarms, want 1: unrouted, §D6 is a silent bypass", n)
	}

	second, err := st.Redeem(ctx, s.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if second.Accepted || second.Decision != DecisionIntegrityQuarantined {
		t.Fatalf("second corrupt-chain scan = %+v, want a quarantine denial", second)
	}
	if !second.OccurredAt.Equal(first.OccurredAt) {
		t.Fatalf("quarantine denial reports %v, want the original degraded admission time %v", second.OccurredAt, first.OccurredAt)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, s.ticketID); n != 0 {
		t.Fatal("the quarantined repeat appended a chained redemption")
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox`); n != 2 {
		t.Fatalf("the quarantined repeat did not escalate: %d alarms owed, want 2", n)
	}
}

// The gap TKT-89 closes: a degraded admission is recorded only in the
// quarantine table (appending onto an unverified predecessor would poison the
// chain), so once the chain verifies clean again — e.g. the verifier bug that
// broke it is reverted — the verified path finds no redeemed event and would
// admit a second time. ADR-025 §D2: admission decisions consult
// trace ∪ quarantine, not the trace alone.
func TestQuarantineDenialConvergesAcrossBrokenAndRecoveredChains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)

	s := issueTicket(t, ctx, st, uuid.New())

	var genuine []byte
	if err := db.QueryRowContext(ctx, `SELECT entry_hash FROM lifecycle_event_integrity WHERE ticket_id=$1 AND sequence=1`, s.ticketID).Scan(&genuine); err != nil {
		t.Fatal(err)
	}
	corruptChain(t, ctx, db, s.ticketID)

	first, err := st.Redeem(ctx, s.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Accepted || first.Decision != DecisionAdmittedDegraded {
		t.Fatalf("first corrupt-chain scan = %+v, want a degraded admission", first)
	}
	broken, err := st.Redeem(ctx, s.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if broken.Accepted || broken.Decision != DecisionIntegrityQuarantined || !broken.OccurredAt.Equal(first.OccurredAt) {
		t.Fatalf("broken-chain repeat = %+v, want a quarantine denial at %v", broken, first.OccurredAt)
	}

	// The bad build is reverted: put the genuine bytes back the same way the
	// corruption went in, and prove recovery with the real verifier before
	// trusting the scan below to exercise the verified path.
	if _, err = db.ExecContext(ctx, `ALTER TABLE lifecycle_event_integrity DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE lifecycle_event_integrity SET entry_hash=$1 WHERE ticket_id=$2 AND sequence=1`, genuine, s.ticketID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `ALTER TABLE lifecycle_event_integrity ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if err = New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("chain did not recover, the scan below would not take the verified path: %v", err)
	}

	recovered, err := st.Redeem(ctx, s.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Accepted || recovered.Decision != DecisionIntegrityQuarantined {
		t.Fatalf("recovered-chain scan = %+v: the verified path re-admitted a quarantine-admitted ticket (§D6 admit-once)", recovered)
	}
	if !recovered.OccurredAt.Equal(first.OccurredAt) {
		t.Fatalf("verified-path denial reports %v, want the original degraded admission time %v", recovered.OccurredAt, first.OccurredAt)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, s.ticketID); n != 0 {
		t.Fatal("the verified-path denial appended a redeemed event")
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox`); n != 3 {
		t.Fatalf("%d alarms owed, want 3 (degraded admission, broken-chain denial, verified-path denial)", n)
	}
}

// Precedence: a quarantine row wins over an existing redeemed event.
// DecisionAlreadyRedeemed would hide the degraded admission and skip §D6's
// escalation — the quarantine row is the only record that an extra person
// walked in.
func TestQuarantineWinsOverGrandfatheredRedeemedEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	s := issueTicket(t, ctx, st, uuid.New())
	clean, err := st.Redeem(ctx, s.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if !clean.Accepted || clean.Decision != DecisionAccepted {
		t.Fatalf("clean redemption = %+v", clean)
	}

	// A degraded admission recorded before the chain later verified clean —
	// inserted directly (append-only trigger allows INSERT) with a distinct
	// earlier timestamp so the returned time is attributable.
	quarantinedAt := clean.OccurredAt.Add(-time.Hour).UTC()
	if _, err = db.ExecContext(ctx, `INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,admitted_at) VALUES($1,$2,$3,$4)`,
		s.ticketID, s.id.OrganizerID, "grandfathered degraded admission", quarantinedAt); err != nil {
		t.Fatal(err)
	}

	r, err := st.Redeem(ctx, s.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if r.Accepted || r.Decision != DecisionIntegrityQuarantined {
		t.Fatalf("scan = %+v, want the quarantine denial to win over already_redeemed", r)
	}
	if !r.OccurredAt.Equal(quarantinedAt) {
		t.Fatalf("denial reports %v, want the quarantine admission time %v", r.OccurredAt, quarantinedAt)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, s.ticketID); n != 1 {
		t.Fatalf("redeemed count = %d after the denial, want the original 1", n)
	}
	var head int64
	if err = db.QueryRowContext(ctx, `SELECT last_sequence FROM lifecycle_heads WHERE ticket_id=$1`, s.ticketID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	if head != 2 {
		t.Fatalf("head = %d after the denial, want 2 (issued, redeemed) — the chain must not advance", head)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox`); n != 1 {
		t.Fatalf("%d alarms owed, want 1: the verified-path denial must escalate", n)
	}
}

// The quarantine row records an admission and is never rewritten, so a
// canonicalization bug's blast radius stays reconstructable after the fact.
func TestQuarantineAdmissionRecordIsAppendOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	s := issueTicket(t, ctx, st, uuid.New())
	corruptChain(t, ctx, db, s.ticketID)
	if _, err := st.Redeem(ctx, s.redeemInput()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE lifecycle_integrity_quarantine SET admitted_at=now() WHERE ticket_id=$1`, s.ticketID); err == nil {
		t.Fatal("a quarantine admission record was rewritten")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM lifecycle_integrity_quarantine WHERE ticket_id=$1`, s.ticketID); err == nil {
		t.Fatal("a quarantine admission record was deleted")
	}
}

func TestThresholdFlipsOrganizerToOperatorControlled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	cfg.Policy = Policy{FailureThreshold: 3, Window: time.Minute}
	st := New(db, cfg)

	organizerID := uuid.New()
	var tickets []seeded
	for i := 0; i < 4; i++ {
		s := issueTicket(t, ctx, st, organizerID)
		corruptChain(t, ctx, db, s.ticketID)
		tickets = append(tickets, s)
	}

	// The first three each take their one admission; the third crosses the
	// threshold and flips the organizer.
	for i := 0; i < 3; i++ {
		r, err := st.Redeem(ctx, tickets[i].redeemInput())
		if err != nil {
			t.Fatal(err)
		}
		if !r.Accepted || r.Decision != DecisionAdmittedDegraded {
			t.Fatalf("corrupt ticket %d = %+v, want a degraded admission", i, r)
		}
	}
	mode := persistedMode(t, ctx, db, organizerID)
	if mode != ModeOperatorDeny {
		t.Fatalf("organizer mode = %q after crossing the threshold, want %q: above this rate 'our bug' stops being the likely story", mode, ModeOperatorDeny)
	}

	// A fourth, previously unseen corrupt ticket is now denied — the choice to
	// keep admitting has become a human's.
	fourth, err := st.Redeem(ctx, tickets[3].redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Accepted || fourth.Decision != DecisionIntegrityOperatorControlled {
		t.Fatalf("fourth corrupt ticket = %+v, want an operator-controlled denial", fourth)
	}
	// Denied means not admitted, so there is no admission to record.
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE ticket_id=$1`, tickets[3].ticketID); n != 0 {
		t.Fatal("a denied scan wrote an admission record")
	}

	// A valid ticket for the same organizer is unaffected: degraded mode bounds
	// corrupt chains, it does not close the venue.
	clean := issueTicket(t, ctx, st, organizerID)
	r, err := st.Redeem(ctx, clean.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Accepted || r.Decision != DecisionAccepted {
		t.Fatalf("a valid ticket was refused while its organizer was operator-controlled: %+v", r)
	}
}

// PR #51 review, R2. The gate and the audit must not disagree about the same
// row. These tamper with metadata the chain's hashes do not cover — the row's
// claimed owner and its version discriminator — and assert the SCAN notices,
// not just the offline verifier. A gate that reports a clean accept while the
// audit later screams is the worst of both: the person is already inside.
func TestScanRejectsAnIntegrityRowReassignedToAnotherTicket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	victim := issueTicket(t, ctx, st, uuid.New())

	// An unchained ticket to reassign to: UNIQUE(ticket_id, sequence) means the
	// target must have no row at this sequence, so a freshly inserted ticket that
	// never went through Issue is the reachable target.
	stranger := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tickets(id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,'signed-credential',now())`,
		stranger, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE lifecycle_event_integrity DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE lifecycle_event_integrity SET ticket_id=$1 WHERE ticket_id=$2`, stranger, victim.ticketID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE lifecycle_event_integrity ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}

	// The victim's chain is cryptographically untouched — only the row's claimed
	// owner changed. Matching on event_id alone would still find it and report a
	// clean accept.
	r, err := st.Redeem(ctx, victim.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if r.Decision != DecisionAdmittedDegraded {
		t.Fatalf("scan = %+v; an integrity row reassigned to another ticket passed the gate as clean", r)
	}
}

func TestScanRejectsATamperedCanonicalVersion(t *testing.T) {
	for name, corruption := range map[string]string{
		"integrity row": `UPDATE lifecycle_event_integrity SET canonical_version=99 WHERE ticket_id=$1`,
		"head":          `UPDATE lifecycle_heads SET canonical_version=99 WHERE ticket_id=$1`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			db := migratedDB(t, ctx)
			st := New(db, testConfig(t))
			s := issueTicket(t, ctx, st, uuid.New())

			for _, table := range []string{"lifecycle_event_integrity", "lifecycle_heads"} {
				if _, err := db.ExecContext(ctx, `ALTER TABLE `+table+` DISABLE TRIGGER USER`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.ExecContext(ctx, corruption, s.ticketID); err != nil {
				t.Fatal(err)
			}
			r, err := st.Redeem(ctx, s.redeemInput())
			if err != nil {
				t.Fatal(err)
			}
			if r.Decision != DecisionAdmittedDegraded {
				t.Fatalf("scan = %+v; the gate verified a %s under v1 rules while it declared version 99", r, name)
			}
		})
	}
}

// PR #51 review, R3. The threshold is meant to bound a canonicalization bug at
// doors-open, which means SIMULTANEOUS corrupt scans — the workload the
// sequential test never exercised.
//
// Under READ COMMITTED a batch of concurrent first-time failures each sees only
// its own committed row, so none crosses the threshold and the flip is delayed
// rather than bypassed. This test pins that honestly: the batch may not flip,
// and the state converges on the next scan. Making it exact would mean
// serializing this path on an organizer row, which under total corruption
// (every scan degraded) is the organizer-wide contention ADR-021 §Option 1
// rejected, arriving exactly when it hurts most.
func TestConcurrentCorruptScansConvergeOnOperatorControl(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	cfg.Policy = Policy{FailureThreshold: 3, Window: time.Minute}
	st := New(db, cfg)

	organizerID := uuid.New()
	var tickets []seeded
	for i := 0; i < 6; i++ {
		s := issueTicket(t, ctx, st, organizerID)
		corruptChain(t, ctx, db, s.ticketID)
		tickets = append(tickets, s)
	}

	// Doors open: every corrupt ticket scanned at once.
	var wg sync.WaitGroup
	results := make([]RedeemResult, len(tickets))
	errs := make([]error, len(tickets))
	for i, s := range tickets {
		wg.Add(1)
		go func(i int, s seeded) {
			defer wg.Done()
			results[i], errs[i] = st.Redeem(ctx, s.redeemInput())
		}(i, s)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent corrupt scan %d: %v", i, err)
		}
	}
	// Every ticket took exactly its one admission — no ticket was admitted twice,
	// which is the invariant that must hold regardless of concurrency.
	for i, r := range results {
		if r.Decision != DecisionAdmittedDegraded && r.Decision != DecisionIntegrityOperatorControlled {
			t.Fatalf("scan %d = %+v", i, r)
		}
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE organizer_id=$1`, organizerID); n > len(tickets) {
		t.Fatalf("%d quarantine rows for %d tickets: a ticket was admitted more than once", n, len(tickets))
	}

	// Convergence: with the batch committed, the next corrupt scan sees the rate
	// and escalates. This is the delay being bounded, not the threshold failing.
	extra := issueTicket(t, ctx, st, organizerID)
	corruptChain(t, ctx, db, extra.ticketID)
	if _, err := st.Redeem(ctx, extra.redeemInput()); err != nil {
		t.Fatal(err)
	}
	mode := persistedMode(t, ctx, db, organizerID)
	if mode != ModeOperatorDeny {
		t.Fatalf("organizer mode = %q after %d corrupt tickets; the threshold never converged", mode, len(tickets)+1)
	}
}

// The second race in R3: mode is read before the flip is written, so an
// unconditional upsert would let an in-flight degraded scan silently revert a
// human who chose operator_admit. §D6 gives the human that choice knowingly —
// the system must not take it back.
func TestSystemFlipNeverOverwritesAnOperatorDecision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	cfg.Policy = Policy{FailureThreshold: 2, Window: time.Minute}
	st := New(db, cfg)

	organizerID := uuid.New()
	if err := st.SetMode(ctx, organizerID, ModeOperatorAdmit, "ops@example.test"); err != nil {
		t.Fatal(err)
	}
	// Enough corrupt tickets to cross the threshold several times over.
	for i := 0; i < 4; i++ {
		s := issueTicket(t, ctx, st, organizerID)
		corruptChain(t, ctx, db, s.ticketID)
		if _, err := st.Redeem(ctx, s.redeemInput()); err != nil {
			t.Fatal(err)
		}
	}
	mode := persistedMode(t, ctx, db, organizerID)
	if mode != ModeOperatorAdmit {
		t.Fatalf("the system reverted a human's %q decision to %q; §D6 makes that choice the operator's", ModeOperatorAdmit, mode)
	}
	var by string
	if err := db.QueryRowContext(ctx, `SELECT mode_set_by FROM lifecycle_integrity_organizer_state WHERE organizer_id=$1`, organizerID).Scan(&by); err != nil {
		t.Fatal(err)
	}
	if by != "ops@example.test" {
		t.Fatalf("mode_set_by = %q: the operator's attribution was overwritten", by)
	}
}

func TestOperatorAdmitNeverBypassesTicketQuarantine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	organizerID := uuid.New()
	s := issueTicket(t, ctx, st, organizerID)
	corruptChain(t, ctx, db, s.ticketID)
	if _, err := st.Redeem(ctx, s.redeemInput()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMode(ctx, organizerID, ModeOperatorAdmit, "ops@example.test"); err != nil {
		t.Fatal(err)
	}
	// A ticket that already took its one admission stays denied whatever the
	// organizer's posture: one degraded entry per ticket is the cap.
	r, err := st.Redeem(ctx, s.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if r.Accepted || r.Decision != DecisionIntegrityQuarantined {
		t.Fatalf("operator-admit re-admitted a quarantined ticket: %+v", r)
	}
}

func TestSetModeRecordsWhoDecided(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	organizerID := uuid.New()

	if err := st.SetMode(ctx, organizerID, ModeOperatorAdmit, ""); err == nil {
		t.Fatal("an operator decision was accepted with no operator recorded")
	}
	if err := st.SetMode(ctx, organizerID, "nonsense", "ops@example.test"); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if err := st.SetMode(ctx, organizerID, ModeOperatorAdmit, "ops@example.test"); err != nil {
		t.Fatal(err)
	}
	var by string
	if err := db.QueryRowContext(ctx, `SELECT mode_set_by FROM lifecycle_integrity_organizer_state WHERE organizer_id=$1`, organizerID).Scan(&by); err != nil {
		t.Fatal(err)
	}
	if by != "ops@example.test" {
		t.Fatalf("mode_set_by = %q", by)
	}
}

func TestCheckpointCollapsesRepeatedHeadChangesToTheLatest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)

	organizerID := uuid.New()
	s := issueTicket(t, ctx, st, organizerID)
	messageID, err := st.DeliveryID(ctx, s.ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.MarkDelivered(ctx, s.ticketID, messageID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Redeem(ctx, s.redeemInput()); err != nil {
		t.Fatal(err)
	}
	// Three head changes for one ticket inside one interval.
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_head_changes WHERE ticket_id=$1 AND checkpoint_id IS NULL`, s.ticketID); n != 3 {
		t.Fatalf("%d pending head changes, want 3", n)
	}

	result, err := st.CheckpointOrganizer(ctx, organizerID, LastRoot{})
	if err != nil {
		t.Fatal(err)
	}
	// One leaf: the ticket's latest head. The dedup is what keeps the Merkle
	// root unambiguous (CVE-2012-2459 via odd-level duplication).
	if result.LeafCount != 1 || result.Sequence != 1 {
		t.Fatalf("checkpoint = %+v, want 1 leaf at sequence 1", result)
	}
	// Every claimed change is assigned, not just the deduped leaf, or the older
	// ones would re-queue forever.
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_head_changes WHERE ticket_id=$1 AND checkpoint_id IS NULL`, s.ticketID); n != 0 {
		t.Fatalf("%d head changes stayed pending after being checkpointed", n)
	}
	if err = New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify after checkpoint: %v", err)
	}
}

func TestCheckpointChainsAndLeavesNothingPending(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)

	organizerID := uuid.New()
	issueTicket(t, ctx, st, organizerID)
	first, err := st.CheckpointOrganizer(ctx, organizerID, LastRoot{})
	if err != nil {
		t.Fatal(err)
	}
	// A change arriving after the first checkpoint belongs to the next one.
	issueTicket(t, ctx, st, organizerID)
	second, err := st.CheckpointOrganizer(ctx, organizerID, LastRoot{Sequence: first.Sequence, Root: first.Root})
	if err != nil {
		t.Fatal(err)
	}
	if second.Sequence != 2 {
		t.Fatalf("second checkpoint at sequence %d, want 2", second.Sequence)
	}
	var previousRoot []byte
	if err = db.QueryRowContext(ctx, `SELECT previous_root FROM lifecycle_checkpoints WHERE organizer_id=$1 AND sequence=2`, organizerID).Scan(&previousRoot); err != nil {
		t.Fatal(err)
	}
	if string(previousRoot) != string(first.Root) {
		t.Fatal("the second checkpoint does not chain to the first")
	}
	// Nothing pending, and an empty pass is a no-op rather than an empty
	// checkpoint: a checkpoint over nothing commits to nothing.
	empty, err := st.CheckpointOrganizer(ctx, organizerID, LastRoot{Sequence: second.Sequence, Root: second.Root})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Sequence != 0 {
		t.Fatalf("an idle pass wrote checkpoint %+v", empty)
	}
	if err = New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify after two checkpoints: %v", err)
	}
}

// The worker refuses to extend a chain that regressed where it can see it,
// rather than re-blessing the result under a fresh signature. This is NOT a
// rollback tripwire — see store.ErrCheckpointRegression.
func TestCheckpointRefusesToLaunderAnObservedRegression(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	organizerID := uuid.New()
	issueTicket(t, ctx, st, organizerID)
	first, err := st.CheckpointOrganizer(ctx, organizerID, LastRoot{})
	if err != nil {
		t.Fatal(err)
	}

	// Truncate the checkpoint suffix, the cheap attack §D2 describes.
	if _, err = db.ExecContext(ctx, `ALTER TABLE lifecycle_checkpoints DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE lifecycle_head_changes SET checkpoint_id=NULL WHERE organizer_id=$1`, organizerID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM lifecycle_checkpoints WHERE organizer_id=$1`, organizerID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `ALTER TABLE lifecycle_checkpoints ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}

	_, err = st.CheckpointOrganizer(ctx, organizerID, LastRoot{Sequence: first.Sequence, Root: first.Root})
	if err == nil {
		t.Fatal("the worker extended a chain that regressed against what it had just observed, laundering the rollback under a fresh signature")
	}
}

func TestSealEpochRetainsHeadSignaturesAndIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)

	issueTicket(t, ctx, st, uuid.New())
	issueTicket(t, ctx, st, uuid.New())

	sealed, err := st.SealEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sealed != 2 {
		t.Fatalf("sealed %d heads, want 2", sealed)
	}
	// Re-sealing under the same key keeps the first seal: the epoch signature is
	// the head as it stood at rotation.
	again, err := st.SealEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("re-sealing wrote %d rows, want 0", again)
	}
	// The retained signatures must verify — that is all the verifier may claim
	// about them, never that one should exist (ADR-021 §D5).
	if err = New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify after sealing an epoch: %v", err)
	}
}

func TestAlarmOutboxClaimPublishCycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	s := issueTicket(t, ctx, st, uuid.New())
	corruptChain(t, ctx, db, s.ticketID)
	if _, err := st.Redeem(ctx, s.redeemInput()); err != nil {
		t.Fatal(err)
	}

	claimed, err := st.ClaimAlarms(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d alarms, want 1", len(claimed))
	}
	if claimed[0].Subject != SubjectIntegrityAlarm {
		t.Fatalf("alarm subject = %q", claimed[0].Subject)
	}
	// The payload carries bounded identifiers, enums and the occurrence time: no QR
	// payload, no buyer, no guest reference (ADR-025 §D9 amended TKT-119; ADR-003 §D3).
	body := string(claimed[0].Envelope)
	for _, forbidden := range []string{"qr_payload", "guest_order_ref", "buyer_id", "signed-credential"} {
		if contains(body, forbidden) {
			t.Fatalf("alarm envelope leaks %q: %s", forbidden, body)
		}
	}
	// A claimed alarm is not re-claimable while leased.
	second, err := st.ClaimAlarms(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("a leased alarm was claimed twice")
	}
	retired, err := st.MarkAlarmPublished(ctx, claimed[0].EventID, claimed[0].ClaimID)
	if err != nil {
		t.Fatal(err)
	}
	if !retired {
		t.Fatal("could not retire an alarm this claimant held")
	}
	// A stale claimant must not retire someone else's row.
	stolen, err := st.MarkAlarmPublished(ctx, claimed[0].EventID, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if stolen {
		t.Fatal("an alarm was retired by a claimant that did not hold it")
	}
	if _, ok, err := st.OldestUnpublishedAlarm(ctx); err != nil || ok {
		t.Fatalf("backlog still reports an owed alarm after publication (ok=%v err=%v)", ok, err)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// Each case is one row of ADR-021 §Threat model's "detected cryptographically"
// list. They are separate so a verifier that stops catching one of them fails
// loudly rather than hiding behind the others.
func TestVerifyLifecycleRejectsEachCorruption(t *testing.T) {
	cases := map[string]string{
		"modified entry hash":   `UPDATE lifecycle_event_integrity SET entry_hash=decode(repeat('00',32),'hex') WHERE sequence=1`,
		"broken chain link":     `UPDATE lifecycle_event_integrity SET previous_hash=decode(repeat('11',32),'hex') WHERE sequence=1`,
		"deleted integrity row": `DELETE FROM lifecycle_event_integrity WHERE sequence=1`,
		// The foreign key stops this while it exists — but the adversary who can
		// DROP TRIGGER can DROP CONSTRAINT, and ADR-021 §Context rests on exactly
		// that: DDL is removable, so the chain, not the schema, has to catch it.
		"forged integrity row": `ALTER TABLE lifecycle_event_integrity DROP CONSTRAINT lifecycle_event_integrity_event_id_fkey;
			INSERT INTO lifecycle_event_integrity(event_id,ticket_id,sequence,canonical_version,previous_hash,entry_hash)
			SELECT gen_random_uuid(),ticket_id,99,1,decode(repeat('00',32),'hex'),decode(repeat('00',32),'hex') FROM lifecycle_heads LIMIT 1`,
		"substituted head hash":   `UPDATE lifecycle_heads SET last_hash=decode(repeat('22',32),'hex')`,
		"forged head signature":   `UPDATE lifecycle_heads SET signature=decode(repeat('33',64),'hex')`,
		"unknown head key":        `UPDATE lifecycle_heads SET key_id='access-lifecycle/attacker'`,
		"corrupted epoch row":     `UPDATE lifecycle_head_epoch_signatures SET head_hash=decode(repeat('44',32),'hex')`,
		"substituted checkpoint":  `UPDATE lifecycle_checkpoints SET root=decode(repeat('55',32),'hex')`,
		"forged checkpoint sig":   `UPDATE lifecycle_checkpoints SET signature=decode(repeat('66',64),'hex')`,
		"broken checkpoint link":  `UPDATE lifecycle_checkpoints SET previous_root=decode(repeat('77',32),'hex') WHERE sequence=1`,
		"checkpoint leaf removed": `UPDATE lifecycle_head_changes SET checkpoint_id=NULL`,
	}
	for name, corruption := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			db := migratedDB(t, ctx)
			cfg := testConfig(t)
			st := New(db, cfg)

			organizerID := uuid.New()
			s := issueTicket(t, ctx, st, organizerID)
			messageID, err := st.DeliveryID(ctx, s.ticketID)
			if err != nil {
				t.Fatal(err)
			}
			if err = st.MarkDelivered(ctx, s.ticketID, messageID); err != nil {
				t.Fatal(err)
			}
			if _, err = st.CheckpointOrganizer(ctx, organizerID, LastRoot{}); err != nil {
				t.Fatal(err)
			}
			if _, err = st.SealEpoch(ctx); err != nil {
				t.Fatal(err)
			}
			verifier := New(db, verifyOnlyConfig(t, cfg))
			if err = verifier.VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
				t.Fatalf("verify rejected an honest trail: %v", err)
			}

			for _, table := range []string{"lifecycle_event_integrity", "lifecycle_heads", "lifecycle_head_epoch_signatures", "lifecycle_checkpoints", "lifecycle_head_changes"} {
				if _, err = db.ExecContext(ctx, `ALTER TABLE `+table+` DISABLE TRIGGER USER`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = db.ExecContext(ctx, corruption); err != nil {
				t.Fatalf("apply corruption: %v", err)
			}
			if err = verifier.VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err == nil {
				t.Fatalf("verify-lifecycle accepted %s", name)
			}
		})
	}
}

// PR #51 review, R1. The checkpoint signs whatever lifecycle_head_changes says,
// and the verifier recomputes the root from those same rows — so if the rows are
// attacker-supplied, signer and verifier simply agree on the attacker's story.
// Nothing blocks an INSERT here (the trigger covers UPDATE and DELETE only; an
// append-only queue must accept inserts), so this needs no DDL privilege at all.
//
// It matters because the checkpoint's ONLY purpose is to be anchored by TKT-11.
// A root over an invented head is worse than no anchor.
func TestCheckpointRefusesAForgedHeadSnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)

	organizerID := uuid.New()
	s := issueTicket(t, ctx, st, organizerID)

	// A fabricated newer snapshot for a real ticket: plausible sequence, invented
	// head, no valid signature. Dedup takes the highest change_id, so this would
	// become the leaf.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO lifecycle_head_changes(ticket_id,organizer_id,sequence,head_hash,canonical_version,key_id,signature)
		VALUES($1,$2,$3,decode(repeat('ab',32),'hex'),1,$4,decode(repeat('cd',64),'hex'))`,
		s.ticketID, organizerID, 99, testKID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CheckpointOrganizer(ctx, organizerID, LastRoot{}); err == nil {
		t.Fatal("the worker signed a checkpoint over a head that never existed; TKT-11 would anchor a fabrication")
	}
	// And nothing was committed on the way to refusing.
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_checkpoints`); n != 0 {
		t.Fatalf("%d checkpoints written despite a forged leaf", n)
	}
}

// The head signature binds ticket, sequence, version and key id — NOT the
// organizer. So nothing but this check stops a change being filed under another
// organizer's checkpoint and anchored there.
func TestCheckpointRefusesAReassignedOrganizer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)

	victim := uuid.New()
	attacker := uuid.New()
	s := issueTicket(t, ctx, st, victim)

	// Move the genuine, correctly-signed change to another organizer's queue.
	if _, err := db.ExecContext(ctx, `ALTER TABLE lifecycle_head_changes DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE lifecycle_head_changes SET organizer_id=$1 WHERE ticket_id=$2`, attacker, s.ticketID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE lifecycle_head_changes ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CheckpointOrganizer(ctx, attacker, LastRoot{}); err == nil {
		t.Fatal("a ticket's head was checkpointed under an organizer that does not own it")
	}
}

// The verifier must catch a forged leaf too, not just the signer: it is the
// offline audit, and it reads the same attacker-writable table.
func TestVerifyRejectsAForgedLeafInACommittedCheckpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)

	organizerID := uuid.New()
	s := issueTicket(t, ctx, st, organizerID)
	if _, err := st.CheckpointOrganizer(ctx, organizerID, LastRoot{}); err != nil {
		t.Fatal(err)
	}
	verifier := New(db, verifyOnlyConfig(t, cfg))
	if err := verifier.VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatal(err)
	}

	// Swap the committed leaf's head hash for an invented one. The root will no
	// longer match, but a verifier that recomputed WITHOUT checking signatures
	// would only catch it via the root — and an attacker who also re-points the
	// checkpoint would not be caught at all. The signature check is what makes
	// the leaf itself meaningful.
	if _, err := db.ExecContext(ctx, `ALTER TABLE lifecycle_head_changes DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE lifecycle_head_changes SET head_hash=decode(repeat('ab',32),'hex') WHERE ticket_id=$1`, s.ticketID); err != nil {
		t.Fatal(err)
	}
	if err := verifier.VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err == nil {
		t.Fatal("verify accepted a checkpoint leaf that is not a signed head")
	}
}

// PR #51 review, R2. The canonical version travels inside the signed domain
// prefix, so the hash covers it — but the stored COLUMN is a separate copy that
// nothing verified, and it is the discriminator a future migration dispatches
// on. An unverified discriminator can lie (ADR-017 §5b′, same shape).
func TestVerifyRejectsATamperedCanonicalVersion(t *testing.T) {
	cases := map[string]string{
		"integrity row":   `UPDATE lifecycle_event_integrity SET canonical_version=99`,
		"head":            `UPDATE lifecycle_heads SET canonical_version=99`,
		"epoch signature": `UPDATE lifecycle_head_epoch_signatures SET canonical_version=99`,
		"checkpoint":      `UPDATE lifecycle_checkpoints SET canonical_version=99`,
		"head change":     `UPDATE lifecycle_head_changes SET canonical_version=99`,
	}
	for name, corruption := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			db := migratedDB(t, ctx)
			cfg := testConfig(t)
			st := New(db, cfg)

			organizerID := uuid.New()
			issueTicket(t, ctx, st, organizerID)
			if _, err := st.CheckpointOrganizer(ctx, organizerID, LastRoot{}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.SealEpoch(ctx); err != nil {
				t.Fatal(err)
			}
			verifier := New(db, verifyOnlyConfig(t, cfg))
			if err := verifier.VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"lifecycle_event_integrity", "lifecycle_heads", "lifecycle_head_epoch_signatures", "lifecycle_checkpoints", "lifecycle_head_changes"} {
				if _, err := db.ExecContext(ctx, `ALTER TABLE `+table+` DISABLE TRIGGER USER`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.ExecContext(ctx, corruption); err != nil {
				t.Fatal(err)
			}
			if err := verifier.VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err == nil {
				t.Fatalf("verify accepted a tampered canonical version on the %s: the field future migrations dispatch on can lie", name)
			}
		})
	}
}

// PR #51 review, R3. A dead-lettered alarm leaves the backlog gauge, so without
// its own signal the queue reads empty at the exact moment a degraded admission
// became permanently unreportable.
func TestDeadLetteredAlarmsStayVisible(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	s := issueTicket(t, ctx, st, uuid.New())
	corruptChain(t, ctx, db, s.ticketID)
	if _, err := st.Redeem(ctx, s.redeemInput()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE lifecycle_integrity_alarm_outbox SET dead_lettered_at=now()`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.OldestUnpublishedAlarm(ctx); err != nil || ok {
		t.Fatalf("dead letters still count as live backlog (ok=%v err=%v)", ok, err)
	}
	dead, err := st.DeadLetteredAlarms(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dead != 1 {
		t.Fatalf("dead-lettered alarms = %d, want 1: an unreportable degraded admission must not vanish from every signal", dead)
	}
}

// The limitation, pinned so nobody later mistakes a green verify for proof the
// trail is intact. A ticket head rolled back together with the checkpoint suffix
// that committed it leaves a chain that is internally consistent and verifies
// clean. ADR-021 §Threat model records this attack as undetected, and no
// in-database check can change that — it is TKT-11's to close.
func TestVerifyLifecycleAcceptsACoordinatedRollback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)

	organizerID := uuid.New()
	s := issueTicket(t, ctx, st, organizerID)

	// Capture the genuine sequence-1 head BEFORE the redemption advances it.
	// These are the exact bytes an attacker who merely reads the database keeps —
	// no signing happens anywhere in this test, because the adversary ADR-021
	// scopes to holds no private key. Re-deriving them through cfg.Signer would
	// hand the attacker the key and prove nothing about the stated threat model
	// (PR #51 review, R4).
	var retainedHash, retainedSignature []byte
	var retainedKeyID string
	if err := db.QueryRowContext(ctx, `SELECT last_hash,signature,key_id FROM lifecycle_heads WHERE ticket_id=$1`, s.ticketID).
		Scan(&retainedHash, &retainedSignature, &retainedKeyID); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Redeem(ctx, s.redeemInput()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CheckpointOrganizer(ctx, organizerID, LastRoot{}); err != nil {
		t.Fatal(err)
	}
	verifier := New(db, verifyOnlyConfig(t, cfg))
	if err := verifier.VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatal(err)
	}

	// Un-redeem the ticket and truncate the checkpoint that committed it: the
	// two row operations ADR-021 §Why Option 2 alone fails describes.
	for _, table := range []string{"lifecycle_events", "lifecycle_event_integrity", "lifecycle_heads", "lifecycle_checkpoints", "lifecycle_head_changes"} {
		if _, err := db.ExecContext(ctx, `ALTER TABLE `+table+` DISABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM lifecycle_event_integrity WHERE ticket_id=$1 AND sequence=2`, s.ticketID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, s.ticketID); err != nil {
		t.Fatal(err)
	}
	// Roll the head back to sequence 1 by restoring the bytes captured before the
	// redemption. The attacker cannot re-sign — that is exactly what the chain
	// closes — so they replay a signature that genuinely existed. Nothing here
	// touches cfg.Signer.
	if _, err := db.ExecContext(ctx, `UPDATE lifecycle_heads SET last_sequence=1,last_hash=$2,signature=$3,key_id=$4 WHERE ticket_id=$1`,
		s.ticketID, retainedHash, retainedSignature, retainedKeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM lifecycle_head_changes WHERE ticket_id=$1 AND sequence=2`, s.ticketID); err != nil {
		t.Fatal(err)
	}
	// Unassign before truncating the checkpoint suffix. The surviving change is
	// left pending, which is the point of §D2's argument: an uncommitted recent
	// head is indistinguishable from ordinary activity inside the current
	// interval, so the truncated chain reads as a quiet organizer.
	if _, err := db.ExecContext(ctx, `UPDATE lifecycle_head_changes SET checkpoint_id=NULL WHERE organizer_id=$1`, organizerID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM lifecycle_checkpoints WHERE organizer_id=$1`, organizerID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM lifecycle_head_epoch_signatures WHERE ticket_id=$1`, s.ticketID); err != nil {
		t.Fatal(err)
	}

	if err := verifier.VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("this test pins a KNOWN GAP and it just changed behaviour: a coordinated rollback now fails verification (%v).\n"+
			"If that is real, ADR-021 §Threat model and §The trust boundary need updating — do not simply delete this test.", err)
	}
	// The ticket is redeemable again. That is the fraud TKT-57 was opened for,
	// and it stays open until an attestation exists outside this database.
	r, err := st.Redeem(ctx, s.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Accepted || r.Decision != DecisionAccepted {
		t.Fatalf("re-redemption after a coordinated rollback = %+v", r)
	}
}

// --- TKT-84: repeatable admission events (ADR-025 §D1/§D3/§D4) ---

func admissionInput(s seeded, eventType AdmissionEventType) RecordAdmissionInput {
	return RecordAdmissionInput{
		TicketID: s.ticketID, OrderID: s.id.OrderID, OrganizerID: s.id.OrganizerID, SlotID: s.id.SlotID,
		OccurrenceID: uuid.New(), Type: eventType, OccurredAt: time.Now().UTC(),
	}
}

func TestRecordAdmissionAcceptsEveryRepeatableType(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)
	s := issueTicket(t, ctx, st, uuid.New())

	for i, eventType := range []AdmissionEventType{AdmissionEntry, AdmissionExit, AdmissionDuplicateAdmit} {
		in := admissionInput(s, eventType)
		res, err := st.RecordAdmission(ctx, in)
		if err != nil {
			t.Fatalf("%s: %v", eventType, err)
		}
		if res.Replayed {
			t.Fatalf("%s: fresh occurrence reported as replay", eventType)
		}
		if res.Event.ID != in.OccurrenceID || res.Event.Type != string(eventType) {
			t.Fatalf("%s: stored event = %+v", eventType, res.Event)
		}
		// issued is sequence 1; each admission chains contiguously after it.
		if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_event_integrity WHERE ticket_id=$1 AND sequence=$2`, s.ticketID, i+2); n != 1 {
			t.Fatalf("%s: no integrity row at sequence %d", eventType, i+2)
		}
	}
	// The same type again: repeatable means a second occurrence appends.
	if _, err := st.RecordAdmission(ctx, admissionInput(s, AdmissionEntry)); err != nil {
		t.Fatalf("second entry occurrence rejected: %v", err)
	}
	if err := New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify after repeatable appends: %v", err)
	}
}

func TestRecordAdmissionRejectsNonRepeatableTypes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())

	before := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events`)
	for _, eventType := range []AdmissionEventType{"issued", "delivered", "redeemed", "", "refunded"} {
		if _, err := st.RecordAdmission(ctx, admissionInput(s, eventType)); err == nil {
			t.Fatalf("%q accepted through the repeatable path", eventType)
		}
	}
	if after := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events`); after != before {
		t.Fatalf("rejected types changed lifecycle_events: %d -> %d", before, after)
	}
}

func TestRecordAdmissionRequiresUUIDv4Occurrence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())

	deterministic := uuid.NewSHA1(uuid.NameSpaceOID, []byte("gate:entry")) // v5-style, not v4
	for name, id := range map[string]uuid.UUID{"nil": uuid.Nil, "sha1": deterministic} {
		in := admissionInput(s, AdmissionEntry)
		in.OccurrenceID = id
		if _, err := st.RecordAdmission(ctx, in); err == nil {
			t.Fatalf("%s occurrence id accepted; ADR-025 §D3 requires UUIDv4", name)
		}
	}
	in := admissionInput(s, AdmissionEntry)
	in.OccurredAt = time.Time{}
	if _, err := st.RecordAdmission(ctx, in); err == nil {
		t.Fatal("zero OccurredAt accepted; the gate's claimed time is required")
	}
}

func TestRecordAdmissionRejectsMismatchedTicketIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())

	in := admissionInput(s, AdmissionEntry)
	in.OrganizerID = uuid.New()
	if _, err := st.RecordAdmission(ctx, in); !errors.Is(err, ErrTicketCredential) {
		t.Fatalf("mismatched identity: err = %v, want ErrTicketCredential", err)
	}
	in = admissionInput(s, AdmissionEntry)
	in.TicketID = uuid.New()
	if _, err := st.RecordAdmission(ctx, in); !errors.Is(err, ErrTicketCredential) {
		t.Fatalf("unknown ticket: err = %v, want ErrTicketCredential", err)
	}
}

// A transport retry reuses its occurrence id and must get the original result
// back without touching the chain (ADR-025 §D3/§D4). Running the replay through
// a store that CANNOT sign proves structurally that the append path was never
// reached: reaching it would fail with ErrLifecycleUnsigned.
func TestRecordAdmissionReplayReturnsOriginalWithoutAppending(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)
	s := issueTicket(t, ctx, st, uuid.New())

	in := admissionInput(s, AdmissionEntry)
	first, err := st.RecordAdmission(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	events := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events`)
	integrity := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_event_integrity`)
	changes := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_head_changes`)

	retry := in
	retry.OccurredAt = in.OccurredAt.Add(45 * time.Second) // retries carry a later clock
	unsigned := New(db, Config{Keyring: cfg.Keyring})
	replay, err := unsigned.RecordAdmission(ctx, retry)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("retry of a recorded occurrence not marked Replayed")
	}
	if !replay.Event.OccurredAt.Equal(first.Event.OccurredAt) {
		t.Fatalf("replay rewrote the recorded time: %v -> %v", first.Event.OccurredAt, replay.Event.OccurredAt)
	}
	if countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events`) != events ||
		countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_event_integrity`) != integrity ||
		countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_head_changes`) != changes {
		t.Fatal("replay appended rows")
	}
}

// The same UUID on a different ticket or type is not a retry — it is either a
// generator bug or a copied id, and silently treating it as a replay would hand
// back another admission's result.
func TestRecordAdmissionRejectsOccurrenceCollision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	organizerID := uuid.New()
	a, b := issueTicket(t, ctx, st, organizerID), issueTicket(t, ctx, st, organizerID)

	in := admissionInput(a, AdmissionEntry)
	if _, err := st.RecordAdmission(ctx, in); err != nil {
		t.Fatal(err)
	}
	crossTicket := admissionInput(b, AdmissionEntry)
	crossTicket.OccurrenceID = in.OccurrenceID
	if _, err := st.RecordAdmission(ctx, crossTicket); !errors.Is(err, ErrOccurrenceCollision) {
		t.Fatalf("cross-ticket occurrence reuse: err = %v, want ErrOccurrenceCollision", err)
	}
	crossType := admissionInput(a, AdmissionExit)
	crossType.OccurrenceID = in.OccurrenceID
	if _, err := st.RecordAdmission(ctx, crossType); !errors.Is(err, ErrOccurrenceCollision) {
		t.Fatalf("cross-type occurrence reuse: err = %v, want ErrOccurrenceCollision", err)
	}
}

// Two racing calls with one occurrence id: exactly one append, the loser sees
// the winner's event as a replay, and nobody leaks a constraint error.
func TestConcurrentRecordAdmissionSerializesPerTicket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)
	s := issueTicket(t, ctx, st, uuid.New())

	in := admissionInput(s, AdmissionEntry)
	results := make([]RecordAdmissionResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = st.RecordAdmission(ctx, in)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}
	if results[0].Replayed == results[1].Replayed {
		t.Fatalf("want one fresh append and one replay, got %v/%v", results[0].Replayed, results[1].Replayed)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE id=$1`, in.OccurrenceID); n != 1 {
		t.Fatalf("occurrence stored %d times", n)
	}
	if err := New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify after race: %v", err)
	}
}

// §D5: the integrity sequence is the authoritative order; occurred_at is the
// gate's claimed time. Cross-device offline events reconcile out of timestamp
// order, and History must not reorder them to impose device-clock order.
func TestHistoryUsesIntegritySequenceNotClaimedTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)
	s := issueTicket(t, ctx, st, uuid.New())

	// Two admissions appended in one order, timestamped in the other.
	late := admissionInput(s, AdmissionEntry)
	late.OccurredAt = time.Now().UTC().Add(time.Hour)
	early := admissionInput(s, AdmissionEntry)
	early.OccurredAt = time.Now().UTC().Add(-time.Hour)
	if _, err := st.RecordAdmission(ctx, late); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordAdmission(ctx, early); err != nil {
		t.Fatal(err)
	}

	history, err := st.History(ctx, s.ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("history has %d events, want 3", len(history))
	}
	for i, e := range history {
		if e.Sequence == nil {
			t.Fatalf("chained event %s has no sequence in History", e.ID)
		}
		if *e.Sequence != int64(i+1) {
			t.Fatalf("history position %d has sequence %d: not ordered by integrity sequence", i, *e.Sequence)
		}
	}
	if history[1].ID != late.OccurrenceID || history[2].ID != early.OccurrenceID {
		t.Fatalf("history reordered by claimed time: %v then %v, want append order", history[1].ID, history[2].ID)
	}
	if !history[2].OccurredAt.Equal(lifecycle.Normalize(early.OccurredAt)) {
		t.Fatalf("claimed time rewritten: %v", history[2].OccurredAt)
	}

	// TKT-234 / ADR-021 §Amendment. Everything above is about DISPLAY — what History
	// returns. This half is about the INTEGRITY CLAIM, and it is the half the amendment
	// records: a non-monotonic occurred_at sequence is not a corruption. Each event's own
	// timestamp is signed honestly; only their ORDER is inverted, which is exactly what a
	// backward clock step or an out-of-order offline reconcile produces. The chain must
	// verify clean, because what it fixes is `sequence`, not time.
	//
	// WITHOUT THIS, the test proves History sorts correctly and says NOTHING about what the
	// chain guarantees — and the amendment's claim would be recorded with nothing asserting
	// it, which is the failure ADR-021 §D and TKT-262's amendment both warn about.
	//
	// The contrast that gives it meaning: MODIFYING a stored occurred_at is a different
	// thing entirely and MUST fail, because occurred_at is inside the signed canonical form
	// (lifecycle.go CanonicalEvent) and the verifier recomputes from the stored value
	// (lifecycle_verify.go). §Threat model already says so — "modification of any signed
	// field" is detected. So the ticket's own COS-3, "perturb occurred_at and confirm
	// verification still passes", is impossible as literally written; the honest-writer
	// inversion below is what it can mean.
	if err := New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("inverted timestamps must not break the chain -- they are honestly signed, "+
			"and the chain fixes sequence rather than time: %v", err)
	}

	// And read the integrity rows DIRECTLY rather than through History, which is the only
	// way to separate "the chain recorded append order" from "the read sorted by it". A
	// History-only assertion is satisfied by a correct ORDER BY over a wrong chain.
	rows, err := db.QueryContext(ctx, `
		SELECT i.sequence
		FROM lifecycle_event_integrity i
		WHERE i.event_id=$1`, late.OccurrenceID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var lateSequence int64
	if !rows.Next() {
		t.Fatal("the later-timestamped event that was appended FIRST has no integrity row")
	}
	if err := rows.Scan(&lateSequence); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	var earlySequence int64
	if err := db.QueryRowContext(ctx,
		`SELECT sequence FROM lifecycle_event_integrity WHERE event_id=$1`, early.OccurrenceID).
		Scan(&earlySequence); err != nil {
		t.Fatal(err)
	}
	// Appended late-first, so its sequence must be the LOWER one -- the opposite of what
	// their timestamps imply. Asserting the relation rather than the literals 2 and 3 keeps
	// this about the invariant ("a draw-down moves a redemption, never creates one") rather
	// than about the issuance event that happens to occupy sequence 1.
	if lateSequence >= earlySequence {
		t.Fatalf("the chain followed claimed time instead of append order: the event appended "+
			"FIRST (timestamped +1h) got sequence %d, the one appended SECOND (timestamped -1h) "+
			"got %d", lateSequence, earlySequence)
	}
}

// Concurrent cross-ticket reuse of one occurrence id: each ticket's lock only
// serializes its own ticket, so both replay checks can pass and the collision
// is only caught by the event primary key inside the append. The caller must
// still see ErrOccurrenceCollision — never a raw unique-violation error
// (rung-1 ai-review finding, PR #62).
func TestConcurrentCrossTicketOccurrenceReuseIsACollision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)
	organizerID := uuid.New()
	a, b := issueTicket(t, ctx, st, organizerID), issueTicket(t, ctx, st, organizerID)

	shared := uuid.New()
	inputs := []RecordAdmissionInput{}
	for _, s := range []seeded{a, b} {
		in := admissionInput(s, AdmissionEntry)
		in.OccurrenceID = shared
		inputs = append(inputs, in)
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range inputs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = st.RecordAdmission(ctx, inputs[i])
		}(i)
	}
	wg.Wait()
	var ok, collided int
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrOccurrenceCollision):
			collided++
		default:
			t.Fatalf("racer %d leaked a non-collision error: %v", i, err)
		}
	}
	if ok != 1 || collided != 1 {
		t.Fatalf("want exactly one append and one collision, got ok=%d collided=%d", ok, collided)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE id=$1`, shared); n != 1 {
		t.Fatalf("occurrence stored %d times", n)
	}
	if err := New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify after cross-ticket race: %v", err)
	}
}

// The deterministic version of the window above: a competing writer holds the
// shared occurrence id uncommitted while RecordAdmission runs. The replay check
// (READ COMMITTED) sees nothing, the append blocks on the in-flight primary
// key, and the commit turns it into a unique violation — which the caller must
// receive as ErrOccurrenceCollision, not as a raw SQLSTATE error. The direct
// insert stands in for the competing transaction's in-flight append; it is
// never verified as a trail.
func TestOccurrenceCollisionInsideTheAppendWindowIsACollision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	organizerID := uuid.New()
	a, b := issueTicket(t, ctx, st, organizerID), issueTicket(t, ctx, st, organizerID)

	shared := uuid.New()
	competing, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = competing.Rollback() }()
	if _, err = competing.ExecContext(ctx, `INSERT INTO lifecycle_events(id,ticket_id,event_type) VALUES($1,$2,'entry')`, shared, b.ticketID); err != nil {
		t.Fatal(err)
	}

	in := admissionInput(a, AdmissionEntry)
	in.OccurrenceID = shared
	done := make(chan error, 1)
	go func() {
		_, err := st.RecordAdmission(ctx, in)
		done <- err
	}()
	// Not a sleep: assert the backend is actually lock-blocked on the
	// conflicting insert before committing, or this test could pass through
	// the visible-row path without ever exercising the append-window mapping.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var blocked int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type='Lock' AND query ILIKE '%INSERT INTO lifecycle_events%'`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("RecordAdmission never blocked on the conflicting insert; the append-window path was not exercised")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err = competing.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrOccurrenceCollision) {
		t.Fatalf("append-window collision surfaced as %v, want ErrOccurrenceCollision", err)
	}
}

// A unique violation from INSIDE the chain — a stale or missing head deriving
// an already-used integrity sequence — is corruption, not occurrence reuse.
// Mapping it to ErrOccurrenceCollision would report a database-adversary state
// as a scanner mistake (second-pass ai-review finding, PR #62).
func TestStaleHeadUniqueViolationIsNotACollision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())

	// The rollback shape: integrity rows survive, the head vanishes. loadHead
	// falls back to genesis and re-derives sequence 1, which the integrity
	// UNIQUE(ticket_id,sequence) refuses.
	if _, err := db.ExecContext(ctx, `DELETE FROM lifecycle_heads WHERE ticket_id=$1`, s.ticketID); err != nil {
		t.Fatal(err)
	}
	_, err := st.RecordAdmission(ctx, admissionInput(s, AdmissionEntry))
	if err == nil {
		t.Fatal("append onto a corrupted head succeeded")
	}
	if errors.Is(err, ErrOccurrenceCollision) {
		t.Fatalf("chain corruption misreported as occurrence collision: %v", err)
	}
}
