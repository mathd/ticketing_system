//go:build smoke

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// rejectedPrefixRows is the malformed backlog both acceptance tests seed.
//
// TKT-268's ticket names a 50,000-row fixture, which is the size the original measurements used.
// These tests seed 5,000, and the reduction costs nothing THIS TEST asserts, because the
// assertion is on the SHAPE of the plan rather than on a magnitude: with the predicate in the
// partial index the malformed rows are not in the index at all, so `Rows Removed by Filter` is
// zero and the buffer count is flat in the prefix — 2 buffers at 5,000 and 2 at 50,000. Removing
// the equality from the index gives `Rows Removed by Filter: 5000` here and 50,000 there; both
// fail the assertion just as loudly.
//
// What the reduction buys is staying inside outboxDB's 30s per-test budget when the whole store
// package runs against one shared database. At 50,000 the exchange fixture exceeded it under that
// contention while passing in isolation, which is a flake waiting to be muted rather than a
// stronger test.
const rejectedPrefixRows = 5000

// The reversal claims' work is bounded by the BATCH, not by the rejected prefix (TKT-268).
//
// TKT-267 filtered malformed rows — a queue row whose source reservation belongs to another
// organizer — with a correlated EXISTS over orders/reservations. Correct, and unindexable:
// the EXISTS cannot be part of the partial queue index, so PostgreSQL evaluated it per
// candidate row and the LIMIT could not stop early on the rows it rejected. Cost was linear
// in the REJECTED PREFIX. Measured during TKT-267's review at 50,000 malformed rows, batch
// 16: 263ms over 350,585 buffers, against ~0.34ms for the unfiltered query.
//
// WHY THIS ASSERTS ON THE PLAN AND NOT THE CLOCK. A wall-clock threshold on a loaded gate
// machine is a flake, and a flake in a performance test gets muted rather than fixed. The
// plan says the same thing and says it deterministically: with the predicate in the partial
// index, the malformed rows are NOT IN the index at all, so the scan reads only index
// entries it can return.
//
// WHY NAMING THE INDEX WOULD NOT BE ENOUGH — the trap voided_feed_smoke_test.go records for
// catalog. A scan can MENTION the queue index while walking every one of its entries and
// filtering afterwards, which satisfies "the index appears" and "no Seq Scan" while being
// exactly the unbounded read this test exists to refuse. What separates the two states is
// `Rows Removed by Filter` on the queue node: zero when the index carries the predicate,
// 50,000 when it does not. Both mutations below move that number.
//
// It EXPLAINs claimOutstandingReversalsSQL — the shipped constant — rather than a copy. A
// copied query drifts from production and keeps asserting about the copy.
func TestTheRefundClaimsWorkIsBoundedByTheBatchNotTheRejectedPrefix(t *testing.T) {
	db, ctx := outboxDB(t)
	seedRejectedPrefix(t, db, ctx, "refund-prefix", rejectedPrefixRows, 32)

	node := explainQueueScan(t, db, ctx, claimOutstandingReversalsSQL,
		"order_refunds_reversal_queue_idx", 16)

	assertBoundedByBatch(t, node, 16)
}

// The exchange twin. Written out rather than shared with the refund case: ADR-062 §1's
// copy-don't-share decision governs these two queues, and the fixtures differ (an exchange
// needs settled_at and tickets_exchanged_at, a refund needs status and the void marker).
func TestTheExchangeClaimsWorkIsBoundedByTheBatchNotTheRejectedPrefix(t *testing.T) {
	db, ctx := outboxDB(t)
	seedExchangeRejectedPrefix(t, db, ctx, "exchange-prefix", rejectedPrefixRows, 32)

	node := explainQueueScan(t, db, ctx, claimOutstandingExchangeReversalsSQL,
		"order_exchanges_reversal_queue_idx", 16)

	assertBoundedByBatch(t, node, 16)
}

// A row UPDATEd out of the partial index is still claimed correctly (TKT-268, ai-review F3).
//
// The two tests above deliberately insert their malformed rows malformed, because that is what
// makes their PLAN assertions honest. This one covers the path they therefore stop covering, and
// it is a real production path: an operator repairing a mis-tenanted row, or the parent-side
// triggers moving a derived organizer, both move rows across the predicate by UPDATE.
//
// What is asserted here is CORRECTNESS, not physical work. The dead index entries such an UPDATE
// leaves behind are a vacuum concern, not a wrong answer: the claim must still refuse the row that
// became malformed and still lease the one that became valid. Asserting buffers here instead would
// pin PostgreSQL's vacuum timing, which is exactly the flake this file avoids elsewhere.
func TestAQueueRowMovedAcrossThePredicateByUpdateIsClaimedCorrectly(t *testing.T) {
	db, ctx := outboxDB(t)

	// Starts valid, becomes malformed.
	fell := completedRefund(t, db, ctx, "moved-out", func(s *refundSeed) {
		s.NextAttemptAt = reversalAgo(2 * time.Hour)
	})
	if _, err := db.ExecContext(ctx,
		`UPDATE order_refunds SET organizer_id=gen_random_uuid() WHERE id=$1`, fell.ID); err != nil {
		t.Fatal(err)
	}

	// Starts malformed, becomes valid: the repair direction.
	rose := completedRefund(t, db, ctx, "moved-in", func(s *refundSeed) {
		s.NextAttemptAt = reversalAgo(time.Hour)
		s.OrganizerID = uuid.New()
	})
	if _, err := db.ExecContext(ctx,
		`UPDATE order_refunds SET organizer_id=source_organizer_id WHERE id=$1`, rose.ID); err != nil {
		t.Fatal(err)
	}

	claimed, err := ClaimOutstandingReversals(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var gotFell, gotRose bool
	for _, c := range claimed {
		if c.Refund.ID == fell.ID {
			gotFell = true
		}
		if c.Refund.ID == rose.ID {
			gotRose = true
		}
	}
	if gotFell {
		t.Error("a refund that became malformed by UPDATE was still claimed: the index entry it " +
			"left behind is dead, but the row must not be leased")
	}
	if !gotRose {
		t.Error("a refund REPAIRED by UPDATE was not claimed. Moving a row back into the " +
			"predicate must make it claimable, or a repair leaves the obligation stranded")
	}
}

// assertBoundedByBatch is the whole claim of both tests, in one place so the two queues
// cannot drift into asserting different things.
func assertBoundedByBatch(t *testing.T, node map[string]any, batch int) {
	t.Helper()

	// (1) No correlated subplan. Its presence IS the TKT-267 shape.
	if _, ok := node["Plans"]; ok {
		for _, sub := range node["Plans"].([]any) {
			if pt, _ := sub.(map[string]any)["Parent Relationship"].(string); pt == "SubPlan" {
				t.Errorf("the queue scan carries a correlated SubPlan, which is the per-candidate "+
					"evaluation TKT-268 removed: %v", sub)
			}
		}
	}

	// (2) The malformed rows were never READ. This is the assertion that separates a partial
	// index carrying the predicate from one that merely gets named.
	if removed := num(node["Rows Removed by Filter"]); removed != 0 {
		t.Errorf("the queue scan filtered %.0f rows out after reading them. The predicate is "+
			"not in the partial index, so the malformed prefix is still walked and the LIMIT "+
			"cannot stop early — the exact cost TKT-268 removed", removed)
	}

	// (3) It returned no more than the batch and ran once.
	if loops := num(node["Actual Loops"]); loops != 1 {
		t.Errorf("the queue scan ran %.0f times, want 1", loops)
	}
	if rows := num(node["Actual Rows"]); rows > float64(batch) {
		t.Errorf("the queue scan returned %.0f rows for a batch of %d", rows, batch)
	}

	// (4) PHYSICAL work, not just logical. (1)-(3) are all counts of rows the executor
	// RETURNED or FILTERED, and dead index entries appear in neither: an UPDATE that moves a
	// row out of a partial index leaves its entry behind until vacuum, and a scan walking
	// thousands of them satisfies every assertion above (ai-review F3). Buffers are what
	// notice. The ceiling is generous — a handful of B-tree levels plus the batch's heap
	// pages — because the claim is "bounded by the batch", not a tuned number: the failing
	// shapes miss it by one to two orders of magnitude (2 vs 102 vs 915 measured).
	const ceiling = 64
	if b := num(node["Shared Hit Blocks"]) + num(node["Shared Read Blocks"]); b > ceiling {
		t.Errorf("the queue scan touched %.0f buffers for a batch of %d (ceiling %d). The row "+
			"counts above can all pass while the scan walks a prefix of DEAD index entries, "+
			"which never reach the filter and are never returned — physical work is what sees "+
			"them", b, batch, ceiling)
	}
}

// explainQueueScan EXPLAINs sql with a rolled-back transaction and returns the plan node
// for the queue index scan.
//
// EXPLAIN (ANALYZE) EXECUTES the statement, and this one LEASES rows. That is why it runs
// inside a transaction that always rolls back: the lease writes must not survive into the
// next test's fixture.
func explainQueueScan(t *testing.T, db *sql.DB, ctx context.Context, sql, index string, batch int) map[string]any {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	var raw string
	if err := tx.QueryRowContext(ctx,
		"EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, batch, 60.0, claimUUID()).Scan(&raw); err != nil {
		t.Fatalf("explain the shipped claim statement: %v", err)
	}
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &plans); err != nil {
		t.Fatalf("parse plan JSON: %v", err)
	}
	node := findNode(plans[0].Plan, index)
	if node == nil {
		t.Fatalf("the plan does not reach the queue through %s, so this test cannot say what "+
			"it is about. Plan:\n%s", index, raw)
	}
	if strings.Contains(raw, `"Node Type": "Seq Scan"`) {
		t.Errorf("the plan contains a sequential scan:\n%s", raw)
	}
	return node
}

// findNode walks the plan tree for the node whose Index Name is index.
func findNode(n map[string]any, index string) map[string]any {
	if name, _ := n["Index Name"].(string); name == index {
		return n
	}
	subs, ok := n["Plans"].([]any)
	if !ok {
		return nil
	}
	for _, s := range subs {
		if m, ok := s.(map[string]any); ok {
			if got := findNode(m, index); got != nil {
				return got
			}
		}
	}
	return nil
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func claimUUID() string { return "00000000-0000-0000-0000-0000000000ff" }

// seedRejectedPrefix writes malformed refund queue rows that are OLDER than the valid ones,
// so an unbounded scan must walk all of them before reaching anything claimable.
//
// Set-based, not row-by-row: 50,000 round trips would dominate the suite's runtime, and the
// fixture's size is the point of the test.
//
// The malformed rows are INSERTED malformed, never inserted valid and then updated out of the
// partial index. That distinction is the whole difference between this test working and this test
// lying, and it cost a review round to find (ai-review F3).
//
// PostgreSQL's MVCC leaves the OLD index entry in place when an UPDATE moves a row out of a
// partial index's predicate, until vacuum removes it. A scan still walks those dead entries, and
// because dead tuples never reach the filter they do NOT increment `Rows Removed by Filter`, and
// they do not appear in `Actual Rows` either. So the fixture's first version built its 5,000
// malformed rows by UPDATE, and every assertion here passed while the scan did 102 buffers of
// work instead of 2. Measured: 2 buffers inserted-malformed, 102 updated-out-of-index, 3 after a
// VACUUM. Inserting them malformed is what makes the assertions describe the work.
//
// The trigger is what makes this possible: source_organizer_id is derived from the ORDER's
// reservation, so giving the queue row a different organizer_id at INSERT time produces the
// mismatch at birth.
//
// The mismatch is built directly because no code path creates it. That is also why the migration
// deliberately ships no CHECK constraint: a constraint making this state unrepresentable would
// make this fixture unbuildable, and the test would silently become about nothing.
func seedRejectedPrefix(t *testing.T, db *sql.DB, ctx context.Context, key string, malformed, valid int) {
	t.Helper()
	_, order, _ := seedPrefixParent(t, db, ctx, key)

	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(id,order_id,organizer_id,idempotency_key,request_fingerprint,
		                          quantity,unit_amount,amount,currency,actor,reason,status,
		                          completed_at,payment_fact_id,reversal_next_attempt_at)
		SELECT gen_random_uuid(),$1,gen_random_uuid(),'k-'||$2||'-'||g,'f-'||$2||'-'||g,1,100,100,'EUR',
		       'ops@example.test','prefix fixture','completed',now(),gen_random_uuid(),
		       now() - interval '30 days' + (g * interval '1 second')
		FROM generate_series(1,$3) g`, order, key, malformed); err != nil {
		t.Fatal(err)
	}

	// Valid rows, newer, on their own parents.
	for i := 0; i < valid; i++ {
		completedRefund(t, db, ctx, fmt.Sprintf("%s-valid-%d", key, i), nil)
	}
	analyze(t, db, ctx, "order_refunds")
}

func seedExchangeRejectedPrefix(t *testing.T, db *sql.DB, ctx context.Context, key string, malformed, valid int) {
	t.Helper()
	org, _, _ := seedPrefixParent(t, db, ctx, key)

	// UNLIKE THE REFUND SIDE, every malformed row needs its OWN source order:
	// `order_exchanges_one_per_source` is UNIQUE on source_order_id, so 50,000 exchanges on
	// one order is not a state the schema can hold. The reservations and orders are seeded in
	// bulk alongside them on one organizer; each queue row then gets its OWN random
	// organizer_id at insert time, so it is malformed at birth (see seedRejectedPrefix on why
	// that matters).
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,
		                         quantity,unit_amount,total_amount,face_value_amount,currency,status)
		SELECT ('00000000-0000-4000-8000-' || lpad(g::text,12,'0'))::uuid,$1,gen_random_uuid(),
		       gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),2,1250,2500,1250,'EUR','completed'
		FROM generate_series(1,$2) g`, org, malformed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
		SELECT ('00000000-0000-4001-8000-' || lpad(g::text,12,'0'))::uuid,
		       ('00000000-0000-4000-8000-' || lpad(g::text,12,'0'))::uuid,
		       'completed','ok-'||$1||'-'||g,'of-'||$1||'-'||g
		FROM generate_series(1,$2) g`, key, malformed); err != nil {
		t.Fatal(err)
	}

	// 0010's CHECKs are strict and all-or-nothing: a settled exchange needs a replacement
	// order, a full basis (seven columns), and arithmetic satisfying _delta_is_the_difference
	// and _total_is_the_product. An even exchange keeps the money trivially valid —
	// quantity x unit = target_total = source_total, delta zero. No money moves here.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_exchanges(organizer_id,id,source_order_id,replacement_order_id,
		                            target_ticket_type_id,idempotency_key,request_fingerprint,
		                            quantity,source_total,source_gross_total,target_total,
		                            delta_amount,target_unit_amount,target_hold_id,
		                            replacement_reservation_id,target_slot_id,basis_at,
		                            currency,actor,reason,settled_at,tickets_exchanged_at,
		                            reversal_next_attempt_at)
		SELECT gen_random_uuid(),gen_random_uuid(),
		       ('00000000-0000-4001-8000-' || lpad(g::text,12,'0'))::uuid,
		       ('00000000-0000-4001-8000-' || lpad(g::text,12,'0'))::uuid,
		       gen_random_uuid(),'k-'||$1||'-'||g,'f-'||$1||'-'||g,
		       2,2500,2500,2500,0,1250,gen_random_uuid(),
		       ('00000000-0000-4000-8000-' || lpad(g::text,12,'0'))::uuid,
		       gen_random_uuid(),now(),
		       'EUR','ops@example.test','prefix fixture',now(),now(),
		       now() - interval '30 days' + (g * interval '1 second')
		FROM generate_series(1,$2) g`, key, malformed); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < valid; i++ {
		settledExchange(t, db, ctx, fmt.Sprintf("%s-valid-%d", key, i), func(s *exchangeSeed) {
			s.SwitchedAt = reversalAgo(time.Minute)
		})
	}
	analyze(t, db, ctx, "order_exchanges")
}

func seedPrefixParent(t *testing.T, db *sql.DB, ctx context.Context, key string) (org, order, reservation any) {
	t.Helper()
	c, _ := seedCompleted(t, db, ctx, key+"-parent", 2, 1250)
	return c.OrganizerID, c.OrderID, c.ReservationID
}

func analyze(t *testing.T, db *sql.DB, ctx context.Context, table string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, "ANALYZE "+table); err != nil {
		t.Fatal(err)
	}
}
