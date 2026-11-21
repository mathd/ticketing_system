//go:build smoke

package api

import (
	"net/http"
	"testing"

	commercestore "ticketing/services/commerce/internal/store"
)

// TKT-498 (D24). A replay of the original exchange request after the exchange's abandonment.
//
// A terminal abandonment answers HTTP 200 with status `abandoned`: the exchange will never
// switch, so `switch_pending` would report work that is no longer owed. The original exchange
// and money fields stay as history, and the replay does not repair the switch outbox. A started
// abandonment that is not terminal keeps today's answer, `switch_pending`, and its repair.
func TestAnExchangeReplayAfterTheAbandonmentReportsItsState(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "abandoned-replay", 2, 2500)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(2500)
	s := exchangeStackFor(t, db, f, policy)
	id := settleAnExchange(t, s, f, "abandoned-replay")

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
		body["delta_amount"] != float64(e.DeltaAmount) || body["target_total"] != float64(e.TargetTotal) ||
		body["tickets_exchanged"] != false || body["capacity_returned"] != false {
		t.Fatalf("terminal replay = %v, want the original exchange and money fields", body)
	}
	if n := outbox(); n != 0 {
		t.Fatalf("a terminal abandonment's replay repaired %d switch outbox row(s), want 0", n)
	}
}
