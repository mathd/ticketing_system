//go:build smoke

package api

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	commercestore "ticketing/services/commerce/internal/store"
)

// TKT-498 (D24). A replay of the original exchange request after the exchange's abandonment.
//
// A terminal abandonment answers HTTP 200 with status `abandoned`: the exchange will never
// switch, so `switch_pending` would report work that is no longer owed. The original exchange
// and money fields stay as history, and the replay does not repair the switch outbox. A started
// abandonment that is not terminal keeps today's answer, `switch_pending`, and its repair. The
// exchange is a downgrade, so the replayed money fields are not zero.
func TestAnExchangeReplayAfterTheAbandonmentReportsItsState(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "abandoned-replay", 2, 2500)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(2000)
	s := exchangeStackFor(t, db, f, policy)
	id := settleAnExchange(t, s, f, "abandoned-replay")

	e, err := commercestore.LoadSettledExchange(ctx, db, f.organizer, id)
	if err != nil {
		t.Fatal(err)
	}
	if e.DeltaAmount != -1000 {
		t.Fatalf("settled delta = %d, want the downgrade's -1000", e.DeltaAmount)
	}
	attribution, err := commercestore.NewAbandonmentAttribution("Ada Operator", "source admitted before the switch")
	if err != nil {
		t.Fatal(err)
	}
	if _, reserved, err := commercestore.ReserveExchangeAbandonment(ctx, db, commercestore.AbandonmentReservation{
		Expected: e, Attribution: attribution}); err != nil || !reserved {
		t.Fatalf("reserve: reserved=%v err=%v", reserved, err)
	}
	outbox := func() int {
		t.Helper()
		return countCommerceRows(t, ctx, db, `SELECT count(*) FROM completion_outbox WHERE order_id=$1`, e.ReplacementOrderID)
	}
	dropOutbox := func() {
		t.Helper()
		if _, err := db.ExecContext(ctx, `DELETE FROM completion_outbox WHERE order_id=$1`, e.ReplacementOrderID); err != nil {
			t.Fatal(err)
		}
	}

	// Started, not terminal: today's answer, and the replay still repairs the switch obligation.
	dropOutbox()
	code, body := s.exchange(t, f, "abandoned-replay")
	if code != http.StatusOK || body["status"] != "switch_pending" || body["replay"] != true {
		t.Fatalf("replay of a started abandonment = %d %v, want 200 switch_pending as a replay", code, body)
	}
	if n := outbox(); n != 1 {
		t.Fatalf("a started abandonment's replay left %d switch outbox row(s), want the repaired 1", n)
	}

	if _, err := commercestore.RecordExchangeAbandonmentTargetReturned(ctx, db, f.organizer, id); err != nil {
		t.Fatal(err)
	}
	cur, err := commercestore.LoadSettledExchange(ctx, db, f.organizer, id)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := commercestore.BuildExchangeAbandonedFact(cur)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := commercestore.FinishExchangeAbandonment(ctx, db, fact); err != nil {
		t.Fatal(err)
	}

	// Terminal: `abandoned`, the original fields as history, and no switch repair.
	dropOutbox()
	code, body = s.exchange(t, f, "abandoned-replay")
	if code != http.StatusOK || body["status"] != "abandoned" || body["replay"] != true {
		t.Fatalf("replay of a terminal abandonment = %d %v, want 200 abandoned as a replay", code, body)
	}
	if body["exchange_id"] != id.String() || body["replacement_order_id"] != e.ReplacementOrderID.String() ||
		body["source_order_id"] != f.order.String() || body["delta_amount"] != float64(e.DeltaAmount) ||
		body["source_total"] != float64(e.SourceTotal) || body["target_total"] != float64(e.TargetTotal) ||
		body["tickets_exchanged"] != false || body["capacity_returned"] != false {
		t.Fatalf("terminal replay = %v, want the original exchange and money fields", body)
	}
	if n := outbox(); n != 0 {
		t.Fatalf("a terminal abandonment's replay repaired %d switch outbox row(s), want 0", n)
	}
}

// RA2-2. The terminal commit wins the exchange row lock after the replay's first read. The
// replay read a started, unfinished abandonment, so its snapshot says `switch_pending`; under
// the settlement lock it must see abandoned_at, answer `abandoned` and write no outbox row.
//
// The schedule is fixed by PostgreSQL's FIFO tuple-lock queue: a holder transaction locks the
// row; FinishExchangeAbandonment queues first; the replay reads (no lock) and queues second;
// the holder ends; the terminal transaction commits before the replay's settlement lock.
func TestAReplayThatLosesTheLockToTheTerminalCommitAnswersAbandoned(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "abandon-race", 2, 2500)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(2500)
	s := exchangeStackFor(t, db, f, policy)
	id := settleAnExchange(t, s, f, "abandon-race")
	e, err := commercestore.LoadSettledExchange(ctx, db, f.organizer, id)
	if err != nil {
		t.Fatal(err)
	}
	attribution, err := commercestore.NewAbandonmentAttribution("Ada Operator", "source admitted before the switch")
	if err != nil {
		t.Fatal(err)
	}
	if _, reserved, err := commercestore.ReserveExchangeAbandonment(ctx, db, commercestore.AbandonmentReservation{
		Expected: e, Attribution: attribution}); err != nil || !reserved {
		t.Fatalf("reserve: reserved=%v err=%v", reserved, err)
	}
	if _, err := commercestore.RecordExchangeAbandonmentTargetReturned(ctx, db, f.organizer, id); err != nil {
		t.Fatal(err)
	}
	cur, err := commercestore.LoadSettledExchange(ctx, db, f.organizer, id)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := commercestore.BuildExchangeAbandonedFact(cur)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM completion_outbox WHERE order_id=$1`, e.ReplacementOrderID); err != nil {
		t.Fatal(err)
	}

	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `SELECT 1 FROM order_exchanges WHERE organizer_id=$1 AND id=$2 FOR UPDATE`, f.organizer, id); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, _, err := commercestore.FinishExchangeAbandonment(ctx, db, fact)
		finished <- err
	}()
	waitForExchangeLockWaiters(t, ctx, db, 1)
	type answer struct {
		code int
		body map[string]any
	}
	replayed := make(chan answer, 1)
	go func() {
		code, body := s.exchange(t, f, "abandon-race")
		replayed <- answer{code, body}
	}()
	waitForExchangeLockWaiters(t, ctx, db, 2)
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatalf("finish the abandonment: %v", err)
	}
	got := <-replayed
	if got.code != http.StatusOK || got.body["status"] != "abandoned" || got.body["replay"] != true {
		t.Fatalf("replay that lost the lock to the terminal commit = %d %v, want 200 abandoned as a replay", got.code, got.body)
	}
	if n := countCommerceRows(t, ctx, db, `SELECT count(*) FROM completion_outbox WHERE order_id=$1`, e.ReplacementOrderID); n != 0 {
		t.Fatalf("the replay wrote %d switch outbox row(s) after the terminal commit, want 0", n)
	}
}

// waitForExchangeLockWaiters waits until n sessions of this database wait on a lock.
func waitForExchangeLockWaiters(t *testing.T, ctx context.Context, db *sql.DB, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d session(s) wait on a lock, want %d", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
