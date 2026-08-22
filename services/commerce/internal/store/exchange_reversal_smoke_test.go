//go:build smoke

package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Exchange reversal reconciliation, SQL half (TKT-259, ADR-063).
//
// Everything here is a claim about a PREDICATE, which is why it lives against real
// PostgreSQL rather than against the runner's fakes: the six claim conjuncts, the
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

// ONE CASE PER CONJUNCT. The claim predicate has six independent conjuncts and an earlier
// refusal short-circuits the rest, so a single "the wrong row was not claimed" case proves
// one predicate and is silent about the other five (AGENTS.md). Each case below satisfies
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

// Conjunct 2: capacity still outstanding. A fully discharged exchange must never be claimed
// again — re-driving it would ask inventory to return capacity a second time.
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
// AI-REVIEW pass 3, F7. The column/observation pairing, tested so it can actually fail.
//
// `progressed` is
//
//	(tickets_exchanged_at IS NOT NULL AND NOT $7) OR (capacity_returned_at IS NOT NULL AND NOT $8)
//
// with $7 = switchedAtClaim and $8 = capacityAtClaim. Crossing the two type-checks, runs,
// and reports progress backwards.
//
// DISCRIMINATING THE CROSSING TAKES A SPECIFIC FIXTURE, and two earlier attempts at this
// test could not do it — each was written, looked right, and survived the mutant:
//
//   - `false, false` against switched=SET, capacity=NULL: both arms read the same flag, so
//     correct and crossed both compute true.
//   - `true, false` against switched=SET, capacity=SET: both columns are set, so each arm
//     can rescue the other and both compute true.
//
// What discriminates is exactly ONE column set, with the observations ASYMMETRIC and matched
// to that column: switched=SET, capacity=NULL, $7=true, $8=false. Correct pairing asks
// "did the switch move since I saw it?" — no, it was already set — and reports NO progress.
// Crossed pairing tests the switch against $8=false and reports progress that never happened,
// resetting a budget that should have been spent.
//
// The generalisable rule, which is the reason for this comment: a test for a SWAP needs the
// two swapped things to be distinguishable in every position, or it proves only that the
// query runs.
func TestProgressReadsEachColumnAgainstItsOwnClaimTimeObservation(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()
	// Switched at claim time, capacity outstanding, and NOTHING moves during the drive.
	s := settledExchange(t, db, ctx, "pairing", func(s *exchangeSeed) {
		s.SwitchedAt = &now
		s.Attempts = 4
	})

	c, ok := claimExchange(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimed")
	}
	if !c.Exchange.TicketsExchanged || c.Exchange.CapacityReturned {
		t.Fatal("fixture: the claim must observe switched=true, capacity=false, or the swap " +
			"is invisible to this test")
	}

	if err := ReleaseExchangeReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID,
		c.Exchange.TicketsExchanged /* $7 = true */, c.Exchange.CapacityReturned /* $8 = false */, "capacity return outstanding"); err != nil {
		t.Fatal(err)
	}

	if got := readExchange(t, db, ctx, s.OrganizerID, s.ID); got.Attempts != 5 {
		t.Fatalf("attempts = %d, want 5: nothing moved during this drive — the switch was "+
			"ALREADY set when the row was claimed, so it is not progress. Reading 0 here means "+
			"the switch column was compared against the CAPACITY observation, which is the "+
			"$7/$8 crossing: it type-checks, runs, and hands a failing row a fresh budget "+
			"every pass, so it can never park", got.Attempts)
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
// TKT-266 retarget. The expectation is derived WITHOUT re-running the predicate under test,
// and a SECOND reservation on the SAME organizer makes the wrong answer constructible.
//
// The previous version derived its expected hold with
// `SELECT r.hold_id ... WHERE o.id=$1 AND r.organizer_id=$2` — the claim's own join predicate
// spelled out again. It compared the query's answer against a value obtained by asking the
// same question, so it would have stayed green against a claim that sourced its hold from
// anywhere. That is the precondition-that-cannot-fail shape (AGENTS.md), and it is a worse
// defect than the thin fixture the ticket was filed about: seeding a second TENANT does not
// fix it, and — see below — would not have been the right fixture anyway.
//
// WHAT THIS CAN AND CANNOT CATCH, stated precisely, because the obvious version of this test
// is unfalsifiable. `orders.reservation_id` is `UNIQUE REFERENCES reservations(id)` and
// `reservations.id` is the primary key (migration 0001), so `res.id = o.reservation_id` is
// FK-to-PK and SINGLE-VALUED. Deleting `AND res.organizer_id = c.organizer_id` therefore
// cannot make the join select a DIFFERENT reservation — it can only turn a mismatched row
// into zero rows. An assertion of the form "the returned hold is not the other tenant's" can
// never fire, whatever fixture backs it, and asserting it would be decoration.
//
// What CAN go wrong is the join losing its `id` correlation — `JOIN reservations res ON
// res.organizer_id = c.organizer_id`, the plausible fat-finger of the very predicate this
// test is about. That IS multi-valued, and it hands back an arbitrary reservation of the
// right organizer. Catching it needs one organizer owning TWO reservations, which is what
// this fixture builds and what the single-tenant original could not express.
func TestTheClaimedHoldBelongsToTheClaimingTenant(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()

	mine := settledExchange(t, db, ctx, "hold-scope", func(s *exchangeSeed) { s.SwitchedAt = &now })

	// A SECOND reservation owned by the SAME organizer, with its own hold — the decoy. It is
	// a legitimate row (organizers own many reservations); it is simply not this exchange's.
	decoyHold := uuid.New()
	decoyReservation := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,
		                         quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,2,1250,2500,2500,'EUR','completed')`,
		decoyReservation, mine.OrganizerID, decoyHold, uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM reservations WHERE id=$1`, decoyReservation) })

	// The expected hold comes from the reservation THIS ORDER points at, read by reservation
	// id alone — no organizer predicate, so the lookup cannot launder the property under test.
	var wantHold uuid.UUID
	if err := db.QueryRowContext(ctx, `
		SELECT r.hold_id FROM orders o JOIN reservations r ON r.id=o.reservation_id
		WHERE o.id=$1`, mine.OrderID).Scan(&wantHold); err != nil {
		t.Fatal(err)
	}
	if wantHold == decoyHold {
		t.Fatal("fixture is broken: the decoy reservation shares a hold_id with the real one, " +
			"so a join that picked the wrong row would be indistinguishable from one that " +
			"picked the right row")
	}

	// Scan EVERY returned row, not the first match. A multi-valued join does not replace the
	// right answer with the wrong one — it returns BOTH, and a helper that stops at the first
	// hit finds the correct row and reports success while the claim has silently emitted the
	// same obligation twice. The duplication is the defect: two rows means the sweep drives one
	// exchange twice in a pass and returns its capacity twice.
	claimed, err := ClaimOutstandingExchangeReversals(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var got []uuid.UUID
	for _, c := range claimed {
		if c.Exchange.ID == mine.ID {
			got = append(got, c.Exchange.SourceHoldID)
		}
	}
	if len(got) != 1 {
		t.Fatalf("the claim returned %d rows for one exchange (holds %v). The join lost its "+
			"`res.id = o.reservation_id` correlation and is matching on organizer alone, so "+
			"every reservation that organizer owns produces a row: the sweep would drive one "+
			"obligation once per row and return its capacity that many times", len(got), got)
	}
	if got[0] == decoyHold {
		t.Fatalf("the claim returned a DIFFERENT reservation's hold (%v) belonging to the same "+
			"organizer. The capacity leg returns seats to this hold, so it would give capacity "+
			"back against a reservation the buyer never held", decoyHold)
	}
	if got[0] != wantHold {
		t.Fatalf("hold = %v, want %v: the source hold must come from the reservation this "+
			"order actually points at", got[0], wantHold)
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

// AI-REVIEW F1/F4. A settled exchange awaiting its switch is NOT CLAIMED AT ALL, however
// long it waits — and it therefore cannot be charged, cannot record an error, and cannot
// park.
//
// The history is worth keeping, because two passes were needed to get here. The first
// version claimed such rows and charged them an attempt, so after
// MaxExchangeReversalAttempts passes the row PARKED — and the claim predicate excludes
// parked rows, so a later switch confirmation whose capacity return failed could never be
// swept. The capacity would have been stranded permanently by the mechanism added to
// prevent that. The second version stopped charging them but still claimed them, which left
// two defects: an error string written for work never attempted (blocking 0022's rollback,
// F3) and a large awaiting-switch backlog filling every LIMIT-ed, next-attempt-ordered batch
// and pushing actionable capacity returns past the runner's per-pass bound (F4).
//
// Excluding them costs nothing: DriveExchange refuses an unswitched row anyway. They are
// monitored by the awaiting_switch gauge, not by the sweep.
func TestAnExchangeAwaitingItsSwitchIsNeverClaimedAndAccruesNoState(t *testing.T) {
	db, ctx := outboxDB(t)
	s := settledExchange(t, db, ctx, "awaiting-not-claimed", nil) // settled, switch unconfirmed

	for pass := 0; pass < MaxExchangeReversalAttempts+3; pass++ {
		if _, ok := claimExchange(t, db, ctx, s.ID); ok {
			t.Fatalf("pass %d: a settled exchange awaiting its switch was CLAIMED. Commerce "+
				"can do nothing with it — the marker is access's fact — and letting these "+
				"into a bounded, next-attempt-ordered queue starves the actionable rows the "+
				"sweep exists to drive", pass)
		}
	}

	got := readExchange(t, db, ctx, s.OrganizerID, s.ID)
	if got.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0: waiting on another service's event is not a failed "+
			"attempt, and spending the budget on it is what leads to parking", got.Attempts)
	}
	if got.ParkedAt != nil {
		t.Fatal("the row PARKED while waiting for access. Once parked it is excluded from the " +
			"claim predicate forever, so a later switch confirmation whose capacity return " +
			"fails could never be swept")
	}
	// F3: the rollback guard. 0022's Down refuses on ANY non-null last error, so writing a
	// cause here would make a routine wait for access permanently unrollbackable.
	var lastError *string
	if err := db.QueryRowContext(ctx, `SELECT reversal_last_error FROM order_exchanges WHERE organizer_id=$1 AND id=$2`,
		s.OrganizerID, s.ID).Scan(&lastError); err != nil {
		t.Fatal(err)
	}
	if lastError != nil {
		t.Fatalf("reversal_last_error = %q for a row that never attempted anything. 0022's "+
			"rollback guard refuses on any non-NULL error, so the ordinary transient state of "+
			"waiting for access would make the migration unrollbackable", *lastError)
	}
}

// The other half, and what makes the test above prove something: once access confirms the
// switch the row becomes claimable IMMEDIATELY, with its full budget, and a genuine capacity
// failure now does charge and can eventually park.
func TestOnceAccessConfirmsTheSwitchTheRowBecomesActionable(t *testing.T) {
	db, ctx := outboxDB(t)
	s := settledExchange(t, db, ctx, "late-switch", nil)

	if _, ok := claimExchange(t, db, ctx, s.ID); ok {
		t.Fatal("claimable before the switch; this fixture cannot show the transition")
	}

	// Access confirms. Nothing else changes — in particular next_attempt_at is untouched,
	// because the row was never released and so was never pushed into the future.
	if _, err := db.ExecContext(ctx, `UPDATE order_exchanges SET tickets_exchanged_at=now() WHERE organizer_id=$1 AND id=$2`,
		s.OrganizerID, s.ID); err != nil {
		t.Fatal(err)
	}

	c, ok := claimExchange(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("a switched exchange with capacity outstanding was not claimable: this is the " +
			"exact state the sweep exists to drive, and it must become actionable the moment " +
			"access confirms rather than after any backoff")
	}
	if !c.Exchange.TicketsExchanged {
		t.Fatal("the claim did not observe the switch")
	}
	// A real capacity failure — and THIS one charges, because it asked inventory.
	if err := ReleaseExchangeReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID,
		c.Exchange.TicketsExchanged, c.Exchange.CapacityReturned, "capacity return outstanding"); err != nil {
		t.Fatal(err)
	}
	got := readExchange(t, db, ctx, s.OrganizerID, s.ID)
	if got.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1: the budget must be intact when the row becomes "+
			"actionable, and a genuine inventory failure must still be charged — otherwise "+
			"nothing ever parks and a permanently refused return spins forever", got.Attempts)
	}
}

// F4 directly: a backlog of awaiting-switch rows larger than one batch must not keep an
// actionable row out of the claim. Ordered by next_attempt_at, the awaiting rows are OLDER
// here, so an unfiltered queue would return them first and the actionable row would be
// outside the LIMIT.
func TestAnActionableRowIsClaimedDespiteAnOlderAwaitingSwitchBacklog(t *testing.T) {
	db, ctx := outboxDB(t)
	old := time.Now().Add(-24 * time.Hour)
	now := time.Now()

	// More awaiting-switch rows than the claim limit used below, all older.
	for i := 0; i < 12; i++ {
		settledExchange(t, db, ctx, fmt.Sprintf("f4-awaiting-%d", i), func(s *exchangeSeed) {
			s.NextAttemptAt = &old
		})
	}
	// One actionable row, newer than every one of them.
	actionable := settledExchange(t, db, ctx, "f4-actionable", func(s *exchangeSeed) {
		s.SwitchedAt = &now
		s.NextAttemptAt = &now
	})

	claimed, err := ClaimOutstandingExchangeReversals(ctx, db, 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range claimed {
		if c.Exchange.ID == actionable.ID {
			found = true
		}
		if !c.Exchange.TicketsExchanged {
			t.Fatalf("the claim returned an unswitched exchange (%v): commerce can do nothing "+
				"with it, and every slot it occupies is one an actionable row does not get",
				c.Exchange.ID)
		}
	}
	if !found {
		t.Fatal("a switched, capacity-outstanding exchange was crowded out of the claim by " +
			"older rows awaiting their switch — head-of-line blocking that delays exactly the " +
			"work the sweep exists to do, while the capacity under-sells")
	}
}

// Conjunct 6 (TKT-267): the source reservation must belong to the exchange's own organizer,
// and the check must happen BEFORE the lease — which is why this is a claim-query test and
// not another assertion on the final join.
//
// The final join has always been organizer-scoped, so no foreign hold_id ever escaped. The
// defect was one stage earlier: `claimable` selected and `claimed` LEASED before that join
// ran, so a malformed row — settled, switched, capacity outstanding, but whose source
// reservation belongs to another organizer — took a lease and a claim slot and then vanished
// at the join. Never returned means never released and never abandoned: nothing charged an
// attempt, nothing parked it, nothing cleared the lease. It re-took a slot on every lease
// expiry while the function reported no error.
//
// WHY THE LEASE ASSERTION COMES FIRST, and why the malformed row is the OLDEST. Under
// `ORDER BY reversal_next_attempt_at LIMIT 1` a test that only asserted "the valid row came
// back" could pass by ordering luck with the predicate deleted. Making the malformed row
// older removes the luck — an unfiltered query takes it first by construction — and asserting
// the untouched lease BEFORE the return means a deleted predicate goes red on the write,
// which is the thing this conjunct is about (AGENTS.md: assert what the write left behind).
//
// This is honest-writer consistency, not tamper-evidence (ADR-021). No code path writes a
// mismatched pair today; a writer with commerce database access still can, and this predicate
// constrains that writer not at all. What it removes is the sweep's inability to make progress
// once such a row exists.
func TestConjunct6AnExchangeWhoseSourceReservationIsAnotherTenantsIsNotLeased(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()
	older, newer := now.Add(-time.Hour), now

	// Malformed AND oldest: an unfiltered claim orders it first and spends the only slot on it.
	malformed := settledExchange(t, db, ctx, "cross-tenant-source", func(s *exchangeSeed) {
		s.SwitchedAt = &newer
		s.NextAttemptAt = &older
		s.OrganizerID = uuid.New() // diverges from the source reservation's organizer
	})
	valid := settledExchange(t, db, ctx, "same-tenant-source", func(s *exchangeSeed) {
		s.SwitchedAt = &newer
		s.NextAttemptAt = &newer
	})

	// limit=1 is the point: one slot, and the malformed row is first in the ordering.
	claimed, err := ClaimOutstandingExchangeReversals(ctx, db, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// FIRST, and deliberately: the write is what this conjunct guards.
	got := readExchange(t, db, ctx, malformed.OrganizerID, malformed.ID)
	if got.LeaseUntil != nil || got.ClaimID != nil {
		t.Fatalf("the malformed row was LEASED (lease_until=%v claim_id=%v). It is dropped by "+
			"the final join, so it is never returned, never released and never abandoned: "+
			"nothing charges an attempt, nothing parks it, nothing clears the lease. It "+
			"re-takes a claim slot on every lease expiry, forever, and the function reports "+
			"no error", got.LeaseUntil, got.ClaimID)
	}

	// The mirror. Without it, a claim query that returned nothing at all would satisfy the
	// assertion above and starve the sweep completely.
	var found bool
	for _, c := range claimed {
		if c.Exchange.ID == valid.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("the valid exchange was not claimed: the only slot went to a row the query " +
			"can never drive, which is the head-of-line blocking this predicate exists to stop")
	}
}

// The claim locks the QUEUE ROW ONLY, and two claimants never lease the same row
// (ai-review of TKT-267, F1).
//
// Conjunct 6 put a correlated EXISTS over orders and reservations inside the locking CTE, and
// the tests above cannot tell a narrow lock from a wide one: they run one claim and inspect
// the result, so a regression to a clause that also locked the joined rows would keep every
// one of them green while making this hot sweep contend with checkout, refund and exchange
// writes on the reservation table.
//
// So assert the two things only a second connection can see. `FOR UPDATE OF oe` names its
// target, and a mistyped alias is a parse error rather than a silently wider lock — but that
// is an argument, and this is the test.
func TestTheClaimLocksTheQueueRowOnlyAndNeverTwice(t *testing.T) {
	db, ctx := outboxDB(t)
	now := time.Now()
	a := settledExchange(t, db, ctx, "lockscope-a", func(s *exchangeSeed) { s.SwitchedAt = &now })

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	// The shipped function, inside a transaction, so its row locks are still held below.
	// *sql.Tx satisfies OutboxDB, so this is the production query and not a copy of it.
	claimed, err := ClaimOutstandingExchangeReversals(ctx, tx, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	for _, c := range claimed {
		if c.Exchange.ID == a.ID {
			got = true
		}
	}
	if !got {
		t.Fatal("the fixture row was not claimed; this test cannot show what it is about")
	}

	// (1) The source reservation the EXISTS traversed must NOT be locked. A separate
	// connection writes it under a short timeout: if the claim widened its lock, this blocks
	// and fails, which is precisely the production symptom — sweep replicas serialising
	// against ordinary checkout traffic.
	other, err := sql.Open("pgx", os.Getenv("COMMERCE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()

	// A PINNED connection, not the pool. `SET lock_timeout` is session state on one physical
	// connection, and *sql.DB is a pool that may run the next statement on a different one —
	// so setting it on the pool and writing through the pool can leave the write with no
	// timeout at all. It would then block on the 30s context deadline instead of failing in
	// two seconds: the test still goes red on a widened lock, but slowly, and a reader would
	// credit the timeout that never applied. This is the harness-pinning shape, so bind both
	// statements to one connection (ai-review pass 2, F3).
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `SET lock_timeout='2s'`); err != nil {
		t.Fatal(err)
	}

	// Both joined tables, not just the reservation. The property is "the queue row ONLY", and
	// a regression that took row locks on `orders` while leaving `reservations` writable would
	// pass a reservation-only assertion (pass 2, F4). `SET hold_id=hold_id` and
	// `SET status=status` are real UPDATEs and take real row locks — PostgreSQL writes a new
	// tuple version rather than optimising a self-assignment away.
	for _, w := range []struct{ what, stmt string }{
		{"SOURCE RESERVATION", `UPDATE reservations SET hold_id=hold_id
		                        WHERE id=(SELECT reservation_id FROM orders WHERE id=$1)`},
		{"SOURCE ORDER", `UPDATE orders SET status=status WHERE id=$1`},
	} {
		res, err := conn.ExecContext(ctx, w.stmt, a.OrderID)
		if err != nil {
			t.Fatalf("a concurrent write to the claim's %s was blocked (%v). The claim must "+
				"lock its own queue row and nothing else: locking the joined rows would make "+
				"every sweep pass contend with checkout and refund writes on those tables",
				w.what, err)
		}
		// COUNT THE ROW, or this whole assertion is decoration. An UPDATE whose WHERE matches
		// nothing succeeds and returns no error, so a fixture that stopped producing the
		// expected row — or a subselect that silently found none — would sail through the
		// check above having taken no lock and contended with nothing.
		n, err := res.RowsAffected()
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("the concurrent write to the claim's %s touched %d rows, want 1: it took "+
				"no row lock, so passing the block check above proves nothing about what the "+
				"claim locks", w.what, n)
		}
	}

	// (2) SKIP LOCKED still fences: a second claimant must not re-lease the row this
	// transaction holds. Without this half, a query that locked nothing at all would satisfy
	// the assertion above and let two replicas drive one obligation.
	second, err := ClaimOutstandingExchangeReversals(ctx, conn, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range second {
		if c.Exchange.ID == a.ID {
			t.Fatal("a second claimant re-leased a row already locked by an open claim: " +
				"SKIP LOCKED is not fencing, and two replicas would drive one obligation")
		}
	}
}
