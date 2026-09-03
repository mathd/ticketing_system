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
)

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
	seedRejectedPrefix(t, db, ctx, "refund-prefix", 50000, 32)

	node := explainQueueScan(t, db, ctx, claimOutstandingReversalsSQL,
		"order_refunds_reversal_queue_idx", 16)

	assertBoundedByBatch(t, node, 16)
}

// The exchange twin. Written out rather than shared with the refund case: ADR-062 §1's
// copy-don't-share decision governs these two queues, and the fixtures differ (an exchange
// needs settled_at and tickets_exchanged_at, a refund needs status and the void marker).
func TestTheExchangeClaimsWorkIsBoundedByTheBatchNotTheRejectedPrefix(t *testing.T) {
	db, ctx := outboxDB(t)
	seedExchangeRejectedPrefix(t, db, ctx, "exchange-prefix", 50000, 32)

	node := explainQueueScan(t, db, ctx, claimOutstandingExchangeReversalsSQL,
		"order_exchanges_reversal_queue_idx", 16)

	assertBoundedByBatch(t, node, 16)
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
// The malformed state is built DIRECTLY — the queue row's organizer is moved away from its
// source reservation's after insert — because no code path creates it. That is also why the
// migration deliberately ships no CHECK constraint: a constraint making this unrepresentable
// would make this fixture unbuildable, and the test would silently become about nothing.
func seedRejectedPrefix(t *testing.T, db *sql.DB, ctx context.Context, key string, malformed, valid int) {
	t.Helper()
	org, order, _ := seedPrefixParent(t, db, ctx, key)

	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(id,order_id,organizer_id,idempotency_key,request_fingerprint,
		                          quantity,unit_amount,amount,currency,actor,reason,status,
		                          completed_at,payment_fact_id,reversal_next_attempt_at)
		SELECT gen_random_uuid(),$1,$2,'k-'||$3||'-'||g,'f-'||$3||'-'||g,1,100,100,'EUR',
		       'ops@example.test','prefix fixture','completed',now(),gen_random_uuid(),
		       now() - interval '30 days' + (g * interval '1 second')
		FROM generate_series(1,$4) g`, order, org, key, malformed); err != nil {
		t.Fatal(err)
	}
	// Make them malformed: the queue row's organizer no longer matches the source
	// reservation's. The trigger owns source_organizer_id, so moving organizer_id is what
	// creates the mismatch.
	if _, err := db.ExecContext(ctx,
		`UPDATE order_refunds SET organizer_id=gen_random_uuid() WHERE organizer_id=$1`, org); err != nil {
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
	// bulk alongside them, all on the same organizer, which is what the mismatch update below
	// then moves the queue rows away from.
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
		SELECT $1,gen_random_uuid(),
		       ('00000000-0000-4001-8000-' || lpad(g::text,12,'0'))::uuid,
		       ('00000000-0000-4001-8000-' || lpad(g::text,12,'0'))::uuid,
		       gen_random_uuid(),'k-'||$2||'-'||g,'f-'||$2||'-'||g,
		       2,2500,2500,2500,0,1250,gen_random_uuid(),
		       ('00000000-0000-4000-8000-' || lpad(g::text,12,'0'))::uuid,
		       gen_random_uuid(),now(),
		       'EUR','ops@example.test','prefix fixture',now(),now(),
		       now() - interval '30 days' + (g * interval '1 second')
		FROM generate_series(1,$3) g`, org, key, malformed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE order_exchanges SET organizer_id=gen_random_uuid() WHERE organizer_id=$1`, org); err != nil {
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
