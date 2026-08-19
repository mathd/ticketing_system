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
	// The row must satisfy the table's own shape, not just this file's interests, and 0010's
	// CHECKs are strict about it. `order_exchanges_basis_shape` is ALL-OR-NOTHING across
	// seven columns (basis_at, target_hold_id, replacement_reservation_id, target_total,
	// delta_amount, target_unit_amount, target_slot_id); `order_exchanges_settlement_shape`
	// additionally requires a replacement order and a basis before settled_at may be set;
	// `_delta_is_the_difference` and `_total_is_the_product` then pin the arithmetic. Setting
	// a convenient subset is how a fixture fails for a reason the test is not about — this
	// one did, on the first gate run, in eleven tests at once.
	//
	// The numbers are an EVEN exchange, chosen so the money arithmetic is trivially valid:
	// quantity × unit = target_total = source_total, so delta is zero. No money moves here.
	var replacementOrder, replacementReservation, targetHold, targetSlot, basisAt any
	var targetTotal, delta, targetUnit any
	if s.SettledAt != nil {
		const unit = int64(1250)
		replacementOrder, replacementReservation = c.OrderID, c.ReservationID
		targetHold, targetSlot = uuid.New(), c.SlotID
		basisAt = *s.SettledAt
		targetUnit = unit
		targetTotal = unit * int64(s.Quantity)
		delta = targetTotal.(int64) - int64(2500)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_exchanges(organizer_id,id,source_order_id,replacement_order_id,target_ticket_type_id,
		                            idempotency_key,request_fingerprint,quantity,source_total,source_gross_total,
		                            target_total,delta_amount,target_unit_amount,target_hold_id,
		                            replacement_reservation_id,target_slot_id,basis_at,
		                            currency,actor,reason,settled_at,
		                            tickets_exchanged_at,capacity_returned_at,
		                            reversal_parked_at,reversal_next_attempt_at,reversal_attempts,
		                            reversal_claim_id,reversal_lease_until)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,2500,2500,$9,$10,$11,$12,$13,$14,$15,
		       'EUR','ops@example.test','exchange sweep test',
		       $16,$17,$18,$19,coalesce($20,now()),$21,$22,$23)`,
		s.OrganizerID, s.ID, s.OrderID, replacementOrder, uuid.New(),
		"xkey-"+key, "xfingerprint-"+key, s.Quantity,
		targetTotal, delta, targetUnit, targetHold, replacementReservation, targetSlot, basisAt,
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
// AI-REVIEW F2. The composite-key fence, tested so it can actually FAIL.
//
// The first version of this test made the other tenant's row unclaimable to isolate the
// release — and thereby left its `reversal_claim_id` NULL, so the claim-token predicate
// (`AND reversal_claim_id=$3`) rejected it on its own. Deleting `organizer_id=$1` changed
// nothing and the test stayed green: a fixture that cannot reach the failing state, produced
// while fixing a different failure.
//
// The fix is to give both tenants' rows the SAME live claim token. Then the token predicate
// no longer discriminates and the organizer predicate is the only thing standing between one
// tenant's write and another's row — which is the claim under test. Setting the token
// directly is legitimate here: this file's subject is which rows a statement MATCHES, and the
// fixture must be able to express a collision the claim path would never hand out.
//
// One test per fenced statement, because each carries its own WHERE clause and deleting the
// predicate from one is invisible to a test that exercises another.
func exchangePairSharingAClaim(t *testing.T, db *sql.DB, ctx context.Context, key string) (mine, theirs exchangeSeed, claim uuid.UUID) {
	t.Helper()
	now := time.Now()
	shared := uuid.New()
	claim = uuid.New()
	lease := now.Add(time.Hour)

	mine = settledExchange(t, db, ctx, key+"-mine", func(s *exchangeSeed) {
		s.ID = shared
		s.SwitchedAt = &now
		s.ClaimID, s.LeaseUntil = &claim, &lease
	})
	theirs = settledExchange(t, db, ctx, key+"-theirs", func(s *exchangeSeed) {
		s.ID = shared // same exchange id, same claim token, different organizer
		s.SwitchedAt = &now
		s.Attempts = 7
		s.ClaimID, s.LeaseUntil = &claim, &lease
	})
	if mine.OrganizerID == theirs.OrganizerID {
		t.Fatal("fixture is broken: the two rows must belong to different tenants")
	}
	return mine, theirs, claim
}

func TestReleaseIsFencedByTheFullCompositeKey(t *testing.T) {
	db, ctx := outboxDB(t)
	mine, theirs, claim := exchangePairSharingAClaim(t, db, ctx, "fence-release")

	if err := ReleaseExchangeReversalClaim(ctx, db, mine.OrganizerID, mine.ID, claim,
		true, false, "capacity return outstanding"); err != nil {
		t.Fatal(err)
	}

	// The other tenant's row is untouched. Its attempt count is the sharpest witness:
	// release is the statement that increments it, so an id-only match charges this
	// tenant's failure to a stranger's budget and eventually parks a row that never failed.
	other := readExchange(t, db, ctx, theirs.OrganizerID, theirs.ID)
	if other.Attempts != 7 {
		t.Fatalf("the other tenant's attempts moved 7 -> %d: ReleaseExchangeReversalClaim "+
			"matched on `id` alone and reached across the tenant boundary", other.Attempts)
	}
	if other.ClaimID == nil || other.LeaseUntil == nil {
		t.Fatal("the other tenant's lease was released by this tenant's call")
	}
	// And this tenant's row DID move, so the statement ran at all.
	if got := readExchange(t, db, ctx, mine.OrganizerID, mine.ID); got.Attempts != 1 || got.ClaimID != nil {
		t.Fatalf("this tenant's row was not released (attempts=%d claim=%v); the assertions "+
			"above would then prove nothing", got.Attempts, got.ClaimID)
	}
}

func TestFinishIsFencedByTheFullCompositeKey(t *testing.T) {
	db, ctx := outboxDB(t)
	mine, theirs, claim := exchangePairSharingAClaim(t, db, ctx, "fence-finish")

	if err := FinishExchangeReversalClaim(ctx, db, mine.OrganizerID, mine.ID, claim); err != nil {
		t.Fatal(err)
	}

	other := readExchange(t, db, ctx, theirs.OrganizerID, theirs.ID)
	if other.Attempts != 7 || other.ClaimID == nil {
		t.Fatalf("FinishExchangeReversalClaim cleared another tenant's reconciliation state "+
			"(attempts %d, claim %v): finishing one tenant's exchange must not tell another "+
			"tenant's row that its obligation is discharged", other.Attempts, other.ClaimID)
	}
	if got := readExchange(t, db, ctx, mine.OrganizerID, mine.ID); got.ClaimID != nil || got.Attempts != 0 {
		t.Fatalf("this tenant's row was not finished (claim=%v attempts=%d)", got.ClaimID, got.Attempts)
	}
}

func TestAbandonIsFencedByTheFullCompositeKey(t *testing.T) {
	db, ctx := outboxDB(t)
	mine, theirs, claim := exchangePairSharingAClaim(t, db, ctx, "fence-abandon")

	if err := AbandonExchangeReversalClaim(ctx, db, mine.OrganizerID, mine.ID, claim); err != nil {
		t.Fatal(err)
	}

	other := readExchange(t, db, ctx, theirs.OrganizerID, theirs.ID)
	if other.ClaimID == nil || other.LeaseUntil == nil {
		t.Fatal("AbandonExchangeReversalClaim released another tenant's lease: a shutdown in " +
			"one tenant's pass would hand another tenant's in-flight rows to a competing claimant")
	}
	if got := readExchange(t, db, ctx, mine.OrganizerID, mine.ID); got.ClaimID != nil {
		t.Fatal("this tenant's claim was not abandoned")
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

// AI-REVIEW F1. An exchange awaiting its switch must NEVER park, however many passes see it.
//
// The defect this pins: the sweep claims every settled outstanding exchange, including ones
// access has not confirmed, and DriveExchange correctly does nothing with them. If that
// release charged an attempt, the row would park after MaxExchangeReversalAttempts passes —
// and the claim predicate excludes parked rows, so when access finally confirmed the switch
// and its own capacity return failed, the sweep could never reclaim it. The capacity would be
// stranded permanently BY THE MECHANISM ADDED TO PREVENT THAT.
//
// One pass cannot see this. The single-pass safety test is green either way, which is why
// this test drives the budget to exhaustion.
func TestAnExchangeAwaitingItsSwitchNeverParksHoweverManyPasses(t *testing.T) {
	db, ctx := outboxDB(t)
	s := settledExchange(t, db, ctx, "awaiting-never-parks", nil) // settled, switch unconfirmed

	for pass := 0; pass < MaxExchangeReversalAttempts+3; pass++ {
		// The row is due on a flat interval, so make it due again rather than waiting.
		if _, err := db.ExecContext(ctx, `UPDATE order_exchanges SET reversal_next_attempt_at=now()-interval '1 second' WHERE organizer_id=$1 AND id=$2`,
			s.OrganizerID, s.ID); err != nil {
			t.Fatal(err)
		}
		c, ok := claimExchange(t, db, ctx, s.ID)
		if !ok {
			t.Fatalf("pass %d: the row was not claimable; an awaiting-switch exchange must stay "+
				"visible to the sweep so its gauge stays honest", pass)
		}
		if err := ReleaseExchangeReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID,
			c.Exchange.TicketsExchanged, c.Exchange.CapacityReturned, "awaiting access switch confirmation"); err != nil {
			t.Fatal(err)
		}
		got := readExchange(t, db, ctx, s.OrganizerID, s.ID)
		if got.ParkedAt != nil {
			t.Fatalf("pass %d: the row PARKED while waiting for access. Parking is for work "+
				"that failed against a downstream; this row asked nobody. Once parked it is "+
				"excluded from the claim predicate forever, so a later switch confirmation "+
				"whose capacity return fails can never be swept", pass)
		}
		if got.Attempts != 0 {
			t.Fatalf("pass %d: attempts = %d, want 0: waiting on another service's event is "+
				"not a failed attempt, and spending the budget on it is what leads to parking",
				pass, got.Attempts)
		}
	}
}

// The other half of F1, and what makes the test above prove something: once access DOES
// confirm the switch, the row is swept normally — with its full budget, and a genuine
// capacity failure now does charge.
func TestOnceAccessConfirmsTheSwitchTheRowIsSweptNormally(t *testing.T) {
	db, ctx := outboxDB(t)
	s := settledExchange(t, db, ctx, "late-switch", nil)

	// Several passes while access is silent.
	for pass := 0; pass < 3; pass++ {
		if _, err := db.ExecContext(ctx, `UPDATE order_exchanges SET reversal_next_attempt_at=now()-interval '1 second' WHERE organizer_id=$1 AND id=$2`,
			s.OrganizerID, s.ID); err != nil {
			t.Fatal(err)
		}
		c, ok := claimExchange(t, db, ctx, s.ID)
		if !ok {
			t.Fatalf("pass %d: not claimable", pass)
		}
		if err := ReleaseExchangeReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID,
			c.Exchange.TicketsExchanged, c.Exchange.CapacityReturned, "awaiting access switch confirmation"); err != nil {
			t.Fatal(err)
		}
	}

	// Access confirms.
	if _, err := db.ExecContext(ctx, `UPDATE order_exchanges SET tickets_exchanged_at=now(), reversal_next_attempt_at=now()-interval '1 second' WHERE organizer_id=$1 AND id=$2`,
		s.OrganizerID, s.ID); err != nil {
		t.Fatal(err)
	}
	c, ok := claimExchange(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("a switched exchange with capacity outstanding was not claimable: this is the " +
			"state the whole sweep exists to drive")
	}
	if !c.Exchange.TicketsExchanged {
		t.Fatal("the claim did not observe the switch")
	}
	// Now a real capacity failure — and THIS one charges, because it asked inventory.
	if err := ReleaseExchangeReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID,
		c.Exchange.TicketsExchanged, c.Exchange.CapacityReturned, "capacity return outstanding"); err != nil {
		t.Fatal(err)
	}
	if got := readExchange(t, db, ctx, s.OrganizerID, s.ID); got.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1: the budget must be intact when the row becomes "+
			"actionable, and a genuine inventory failure must still be charged — otherwise "+
			"nothing ever parks and a permanently refused return spins forever", got.Attempts)
	}
}
