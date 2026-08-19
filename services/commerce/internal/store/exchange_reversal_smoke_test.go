//go:build smoke

package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Exchange reversal reconciliation, SQL half (TKT-259, ADR-063).
//
// Everything here is a claim about a PREDICATE, which is why it lives against real
// PostgreSQL rather than against the runner's fakes: the five claim conjuncts, the
// organizer-scoped hold join, the lease, the claim fence, the backoff, parking and the
// progress computation are all enforced by the shipped SQL, and a fake enforcing the same
// rules in Go would prove only that the fake and the runner agree. The runner's DECISIONS
// live in internal/exchangesweep/runner_test.go instead.

type exchangeSeed struct {
	ID, OrderID, OrganizerID uuid.UUID
	Quantity                 int32
	SettledAt                *time.Time
	SwitchedAt, ReturnedAt   *time.Time
	ParkedAt, NextAttemptAt  *time.Time
	LeaseUntil               *time.Time
	ClaimID                  *uuid.UUID
	Attempts                 int
}

// settledExchange seeds a settled exchange with both obligations outstanding — the state an
// exchange is left in when the tickets-switched callback never lands.
//
// It writes `order_exchanges` directly rather than going through the bind/settle path: this
// file's subject is which rows the claim query SELECTS, so the fixture must be able to
// express states the happy path cannot reach (an unsettled row, one already parked, a lease
// in the future). Money is never moved by these tests.
func settledExchange(t *testing.T, db *sql.DB, ctx context.Context, key string, mutate func(*exchangeSeed)) exchangeSeed {
	t.Helper()
	c, _ := seedCompleted(t, db, ctx, key, 3, 1250)
	now := time.Now()
	s := exchangeSeed{
		ID: uuid.New(), OrderID: c.OrderID, OrganizerID: c.OrganizerID, Quantity: 2,
		SettledAt: &now,
	}
	if mutate != nil {
		mutate(&s)
	}
	// The row must satisfy the table's own shape, not just this file's interests: 0010's
	// CHECKs tie settlement to a replacement order, a target total and a delta, and
	// `tickets_exchanged_at` may not precede `settled_at`. Getting that wrong is how a
	// fixture fails for a reason the test is not about.
	var replacement any
	var targetTotal, delta any
	if s.SettledAt != nil {
		replacement = c.OrderID
		targetTotal, delta = int64(2500), int64(0)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_exchanges(organizer_id,id,source_order_id,replacement_order_id,target_ticket_type_id,
		                            idempotency_key,request_fingerprint,quantity,source_total,source_gross_total,
		                            target_total,delta_amount,currency,actor,reason,settled_at,
		                            tickets_exchanged_at,capacity_returned_at,
		                            reversal_parked_at,reversal_next_attempt_at,reversal_attempts,
		                            reversal_claim_id,reversal_lease_until)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,2500,2500,$9,$10,'EUR','ops@example.test','exchange sweep test',
		       $11,$12,$13,$14,coalesce($15,now()),$16,$17,$18)`,
		s.OrganizerID, s.ID, s.OrderID, replacement, uuid.New(),
		"xkey-"+key, "xfingerprint-"+key, s.Quantity, targetTotal, delta,
		s.SettledAt, s.SwitchedAt, s.ReturnedAt,
		s.ParkedAt, s.NextAttemptAt, s.Attempts, s.ClaimID, s.LeaseUntil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM order_exchanges WHERE id=$1`, s.ID) })
	return s
}

func claimExchange(t *testing.T, db *sql.DB, ctx context.Context, want uuid.UUID) (ClaimedExchangeReversal, bool) {
	t.Helper()
	claimed, err := ClaimOutstandingExchangeReversals(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, c := range claimed {
		if c.Exchange.ID == want {
			return c, true
		}
	}
	return ClaimedExchangeReversal{}, false
}

func readExchange(t *testing.T, db *sql.DB, ctx context.Context, org, id uuid.UUID) exchangeSeed {
	t.Helper()
	var s exchangeSeed
	s.ID, s.OrganizerID = id, org
	var switched, returned, parked, next, lease sql.NullTime
	var claim uuid.NullUUID
	if err := db.QueryRowContext(ctx, `
		SELECT tickets_exchanged_at,capacity_returned_at,reversal_parked_at,
		       reversal_next_attempt_at,reversal_lease_until,reversal_claim_id,reversal_attempts
		FROM order_exchanges WHERE organizer_id=$1 AND id=$2`, org, id).
		Scan(&switched, &returned, &parked, &next, &lease, &claim, &s.Attempts); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		n  sql.NullTime
		to **time.Time
	}{{switched, &s.SwitchedAt}, {returned, &s.ReturnedAt}, {parked, &s.ParkedAt}, {next, &s.NextAttemptAt}, {lease, &s.LeaseUntil}} {
		if p.n.Valid {
			v := p.n.Time
			*p.to = &v
		}
	}
	if claim.Valid {
		v := claim.UUID
		s.ClaimID = &v
	}
	return s
}

// ONE CASE PER CONJUNCT. The claim predicate has five independent conjuncts and an earlier
// refusal short-circuits the rest, so a single "the wrong row was not claimed" case proves
// one predicate and is silent about the other four (AGENTS.md). Each case below satisfies
// every earlier conjunct and violates exactly one.

// Conjunct 1: settled_at IS NOT NULL. An unsettled exchange owes nothing yet — the money has
// not moved, so there is no obligation to discharge.
func TestConjunct1AnUnsettledExchangeIsNotClaimed(t *testing.T) {
	db, ctx := outboxDB(t)
	s := settledExchange(t, db, ctx, "unsettled", func(s *exchangeSeed) { s.SettledAt = nil })
	if _, ok := claimExchange(t, db, ctx, s.ID); ok {
		t.Fatal("an unsettled exchange was claimed: no money has moved, so nothing is owed — " +
			"driving it would return capacity for a sale that never completed")
	}
}

// Conjunct 2: at least one obligation outstanding. A fully discharged exchange must never be
// claimed again — re-driving it would ask inventory to return capacity a second time.
func TestConjunct2AFullyDischargedExchangeIsNotClaimed(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()
	s := settledExchange(t, db, ctx, "discharged", func(s *exchangeSeed) {
		s.SwitchedAt, s.ReturnedAt = &now, &now
	})
	if _, ok := claimExchange(t, db, ctx, s.ID); ok {
		t.Fatal("a fully discharged exchange was claimed; nothing is owed")
	}
}

// Conjunct 3: reversal_parked_at IS NULL. A parked row has spent its budget and awaits a
// human — claiming it again is the starvation the bound exists to prevent.
func TestConjunct3AParkedExchangeIsNotClaimed(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()
	s := settledExchange(t, db, ctx, "parked", func(s *exchangeSeed) {
		s.SwitchedAt = &now
		s.ParkedAt = &now
	})
	if _, ok := claimExchange(t, db, ctx, s.ID); ok {
		t.Fatal("a parked exchange was claimed: a row that gave up must stay given up until a " +
			"human intervenes, or it starves everything behind it")
	}
}

// Conjunct 4: reversal_next_attempt_at <= now(). A row still backing off is not due.
func TestConjunct4AnExchangeStillBackingOffIsNotClaimed(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()
	future := now.Add(time.Hour)
	s := settledExchange(t, db, ctx, "backoff", func(s *exchangeSeed) {
		s.SwitchedAt = &now
		s.NextAttemptAt = &future
	})
	if _, ok := claimExchange(t, db, ctx, s.ID); ok {
		t.Fatal("an exchange still backing off was claimed; the backoff would never take effect")
	}
}

// Conjunct 5: the lease is free or expired. A live lease means another replica is driving
// this row right now, and the claim token fences only the final write — never the inventory
// call already in flight.
func TestConjunct5ALiveLeaseIsNotClaimedAndAnExpiredOneIs(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	claim := uuid.New()

	live := settledExchange(t, db, ctx, "leased-live", func(s *exchangeSeed) {
		s.SwitchedAt = &now
		s.ClaimID, s.LeaseUntil = &claim, &future
	})
	if _, ok := claimExchange(t, db, ctx, live.ID); ok {
		t.Fatal("a live lease was claimed: two replicas would drive the same obligation, and " +
			"the token fences only the database write, not the inventory call in flight")
	}

	// The mirror, and what makes the case above prove something: an EXPIRED lease is
	// reclaimable. Without this, a claim query that simply never matched leased rows would
	// pass the assertion above and strand every row a crashed replica had claimed.
	expired := settledExchange(t, db, ctx, "leased-expired", func(s *exchangeSeed) {
		s.SwitchedAt = &now
		s.ClaimID, s.LeaseUntil = &claim, &past
	})
	if _, ok := claimExchange(t, db, ctx, expired.ID); !ok {
		t.Fatal("an expired lease was not reclaimed: the rows a crashed replica held would be " +
			"stranded forever")
	}
}

// The claim charges NO attempt. A crash after claiming must not spend budget on work that
// never ran — repeated crash-after-claim cycles would otherwise push a row most of the way
// to parked without a single real failure.
func TestClaimingDoesNotChargeAnAttempt(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()
	s := settledExchange(t, db, ctx, "no-charge", func(s *exchangeSeed) { s.SwitchedAt = &now })

	if _, ok := claimExchange(t, db, ctx, s.ID); !ok {
		t.Fatal("the row was not claimed; this fixture can no longer reach the case")
	}
	if got := readExchange(t, db, ctx, s.OrganizerID, s.ID); got.Attempts != 0 {
		t.Fatalf("attempts = %d after a claim, want 0: the attempt is charged on RELEASE, when a "+
			"drive actually ran and failed", got.Attempts)
	}
}

// Progress resets the budget — and the two obligations are told apart. Exactly ONE moves
// here, in each direction, because a fixture that moves both cannot detect the release
// query crossing its column/observation pairing ($7 with capacity, $8 with the switch):
// crossed, it type-checks, runs, and reports progress exactly backwards.
func TestProgressOnTheSwitchAloneResetsTheBudget(t *testing.T) {
	db, ctx := outboxDB(t)
	s := settledExchange(t, db, ctx, "progress-switch", func(s *exchangeSeed) { s.Attempts = 4 })

	c, ok := claimExchange(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimed")
	}
	if c.Exchange.TicketsExchanged {
		t.Fatal("fixture: the switch must be outstanding AT CLAIM TIME for this to be progress")
	}
	// The callback lands while this claimant is in flight: the switch, and only the switch.
	if _, err := db.ExecContext(ctx, `UPDATE order_exchanges SET tickets_exchanged_at=now() WHERE organizer_id=$1 AND id=$2`,
		s.OrganizerID, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseExchangeReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID,
		c.Exchange.TicketsExchanged, c.Exchange.CapacityReturned, "capacity return outstanding"); err != nil {
		t.Fatal(err)
	}
	if got := readExchange(t, db, ctx, s.OrganizerID, s.ID); got.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0: the SWITCH moved since claim time, which is progress — "+
			"a budget that does not reset would retire an exchange that is recovering", got.Attempts)
	}
}

func TestProgressOnTheCapacityAloneResetsTheBudget(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()
	s := settledExchange(t, db, ctx, "progress-capacity", func(s *exchangeSeed) {
		s.SwitchedAt = &now // switched at claim time; only capacity is outstanding
		s.Attempts = 4
	})

	c, ok := claimExchange(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimed")
	}
	if !c.Exchange.TicketsExchanged || c.Exchange.CapacityReturned {
		t.Fatal("fixture: the switch must be present and capacity outstanding at claim time")
	}
	if _, err := db.ExecContext(ctx, `UPDATE order_exchanges SET capacity_returned_at=now() WHERE organizer_id=$1 AND id=$2`,
		s.OrganizerID, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseExchangeReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID,
		c.Exchange.TicketsExchanged, c.Exchange.CapacityReturned, "capacity return outstanding"); err != nil {
		t.Fatal(err)
	}
	got := readExchange(t, db, ctx, s.OrganizerID, s.ID)
	if got.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0: CAPACITY moved since claim time", got.Attempts)
	}
	// It is no longer outstanding, so it gets finish semantics: no error is written onto a
	// row whose work succeeded, or the claim query never selects it again to clear it and
	// 0022's rollback guard reads it as failed reconciliation.
	if got.ParkedAt != nil {
		t.Fatal("a fully discharged exchange was parked")
	}
}

// No progress spends the budget and backs off; at the bound the row parks.
func TestAnExchangeThatNeverProgressesParks(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()
	s := settledExchange(t, db, ctx, "parks", func(s *exchangeSeed) {
		s.SwitchedAt = &now
		s.Attempts = MaxExchangeReversalAttempts - 1
	})

	c, ok := claimExchange(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimed")
	}
	if err := ReleaseExchangeReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID,
		c.Exchange.TicketsExchanged, c.Exchange.CapacityReturned, "capacity return outstanding"); err != nil {
		t.Fatal(err)
	}
	got := readExchange(t, db, ctx, s.OrganizerID, s.ID)
	if got.ParkedAt == nil {
		t.Fatalf("attempts = %d and the row did not park: an obligation that never progresses "+
			"must stop rather than retry forever", got.Attempts)
	}
	if _, ok := claimExchange(t, db, ctx, s.ID); ok {
		t.Fatal("a parked row was claimed again")
	}
}

// THE TENANT TEST. `order_exchanges`' primary key is (organizer_id, id), so `id` alone is not
// unique by schema. Every statement in this lifecycle must match the FULL composite, or one
// eligible row's release/finish/abandon reaches into another tenant's row.
//
// Delete `organizer_id=$1` from any of those statements and this is what goes red. The
// runner's fakes cannot catch it: they scope in Go what the SQL must scope.
func TestOneTenantsReleaseDoesNotTouchAnotherTenantsRowWithTheSameID(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()
	shared := uuid.New()

	mine := settledExchange(t, db, ctx, "tenant-mine", func(s *exchangeSeed) {
		s.ID = shared
		s.SwitchedAt = &now
	})
	theirs := settledExchange(t, db, ctx, "tenant-theirs", func(s *exchangeSeed) {
		s.ID = shared // same exchange id, different organizer
		s.SwitchedAt = &now
		s.Attempts = 7
	})
	if mine.OrganizerID == theirs.OrganizerID {
		t.Fatal("fixture is broken: the two rows must belong to different tenants")
	}

	c, ok := claimExchange(t, db, ctx, shared)
	if !ok {
		t.Fatal("neither row was claimed")
	}
	if err := ReleaseExchangeReversalClaim(ctx, db, c.Exchange.OrganizerID, shared, c.ClaimID,
		c.Exchange.TicketsExchanged, c.Exchange.CapacityReturned, "capacity return outstanding"); err != nil {
		t.Fatal(err)
	}

	// The OTHER tenant's row must be untouched: same attempts, no lease, no error.
	other := mine
	if c.Exchange.OrganizerID == mine.OrganizerID {
		other = theirs
	}
	before := 0
	if other.OrganizerID == theirs.OrganizerID {
		before = 7
	}
	got := readExchange(t, db, ctx, other.OrganizerID, shared)
	if got.Attempts != before {
		t.Fatalf("the other tenant's attempts moved from %d to %d: a statement matched on `id` "+
			"alone and reached across the tenant boundary", before, got.Attempts)
	}
	if got.ClaimID != nil || got.LeaseUntil != nil {
		t.Fatal("the other tenant's row was leased by this tenant's claim")
	}
}

// The claim's hold comes from the claimant's OWN reservation. An unscoped join is how a row
// acquires another tenant's hold — and the hold is what capacity is returned to, so a wrong
// one returns capacity to a stranger's inventory pool.
func TestTheClaimedHoldBelongsToTheClaimingTenant(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()
	s := settledExchange(t, db, ctx, "hold-scope", func(s *exchangeSeed) { s.SwitchedAt = &now })

	c, ok := claimExchange(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimed")
	}
	var want uuid.UUID
	if err := db.QueryRowContext(ctx, `
		SELECT r.hold_id FROM orders o JOIN reservations r ON r.id=o.reservation_id
		WHERE o.id=$1 AND r.organizer_id=$2`, s.OrderID, s.OrganizerID).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if c.Exchange.SourceHoldID != want {
		t.Fatalf("hold = %v, want %v: the source hold must come from the claimant's own "+
			"organizer-scoped reservation", c.Exchange.SourceHoldID, want)
	}
}

// Abandon hands a claim back without spending anything: the row was never driven, so there
// is no attempt to charge and nothing to back off from. It must stay DUE, or a shutdown
// mid-batch would delay every undriven row by a full backoff.
func TestAbandonPreservesTheBudgetAndLeavesTheRowDue(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()
	s := settledExchange(t, db, ctx, "abandon", func(s *exchangeSeed) {
		s.SwitchedAt = &now
		s.Attempts = 3
	})

	c, ok := claimExchange(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimed")
	}
	if err := AbandonExchangeReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID); err != nil {
		t.Fatal(err)
	}
	got := readExchange(t, db, ctx, s.OrganizerID, s.ID)
	if got.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3: an undriven claim costs nothing", got.Attempts)
	}
	if got.ClaimID != nil || got.LeaseUntil != nil {
		t.Fatal("the lease was not released")
	}
	if _, ok := claimExchange(t, db, ctx, s.ID); !ok {
		t.Fatal("an abandoned row was not immediately reclaimable: an orderly shutdown must " +
			"differ from a crash only in latency")
	}
}

// Finish clears the lease, the budget and the error.
func TestFinishClearsTheReconciliationState(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()
	s := settledExchange(t, db, ctx, "finish", func(s *exchangeSeed) {
		s.SwitchedAt = &now
		s.Attempts = 5
	})

	c, ok := claimExchange(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimed")
	}
	if _, err := db.ExecContext(ctx, `UPDATE order_exchanges SET capacity_returned_at=now() WHERE organizer_id=$1 AND id=$2`,
		s.OrganizerID, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := FinishExchangeReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID); err != nil {
		t.Fatal(err)
	}
	got := readExchange(t, db, ctx, s.OrganizerID, s.ID)
	if got.Attempts != 0 || got.ClaimID != nil || got.LeaseUntil != nil {
		t.Fatalf("finish left state behind: attempts=%d claim=%v lease=%v",
			got.Attempts, got.ClaimID, got.LeaseUntil)
	}
}

// The backlog counts what the gauges report, and separates the two outstanding states: the
// sweep can drive a switched exchange's capacity, and can NEVER complete one awaiting its
// switch. An operator reading a stuck backlog needs to know which kind it is.
func TestTheBacklogSeparatesAwaitingSwitchFromOutstanding(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()

	before, err := ReadExchangeReversalBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	settledExchange(t, db, ctx, "backlog-awaiting", nil)                                           // no switch yet
	settledExchange(t, db, ctx, "backlog-capacity", func(s *exchangeSeed) { s.SwitchedAt = &now }) // capacity owed
	after, err := ReadExchangeReversalBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	if got := after.Outstanding - before.Outstanding; got != 2 {
		t.Fatalf("outstanding delta = %d, want 2", got)
	}
	if got := after.AwaitingSwitch - before.AwaitingSwitch; got != 1 {
		t.Fatalf("awaiting-switch delta = %d, want 1: the row the sweep can never complete on "+
			"its own must be counted separately — it is a different incident with a different "+
			"owner (an access consumer that stopped delivering, not inventory refusing)", got)
	}
}

// The age gauge measures from SETTLEMENT, not row creation. An exchange row is created at
// bind time, before settlement, so measuring from `created_at` would report an age for a row
// that owed nothing yet — and the gauge's justification (a small old backlog and a large
// fresh one are different incidents) only holds if the age means what an operator thinks.
func TestTheOldestAgeIsMeasuredFromSettlementNotCreation(t *testing.T) {
	db, ctx := outboxDB(t)
	old := time.Now().Add(-48 * time.Hour)
	recent := time.Now().Add(-1 * time.Minute)

	s := settledExchange(t, db, ctx, "age", func(s *exchangeSeed) { s.SettledAt = &recent })
	// Age the ROW far past its settlement. If the gauge read created_at it would now report
	// ~48h; settled_at says ~1m. Only one of those is the age of an obligation.
	if _, err := db.ExecContext(ctx, `UPDATE order_exchanges SET created_at=$1 WHERE organizer_id=$2 AND id=$3`,
		old, s.OrganizerID, s.ID); err != nil {
		t.Fatal(err)
	}

	b, err := ReadExchangeReversalBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if b.OldestAgeSeconds >= int64(24*time.Hour/time.Second) {
		t.Fatalf("oldest age = %ds, which is the ROW's age, not the OBLIGATION's: an exchange "+
			"bound long before it settled would inflate the gauge while owing nothing",
			b.OldestAgeSeconds)
	}
}
