//go:build smoke

package exchangeabandon

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/commerce/internal/accessadmission"
	"ticketing/services/commerce/internal/exchangeunwind"
	"ticketing/services/commerce/internal/store"
)

// The test world of the abandonment service: a real, migrated Commerce database and strict,
// stateful fakes of Access, payments and inventory.
//
// The fakes are strict. Each call checks its organizer, its order or hold, and its key against
// the fixture and fails the test on any other value, so a wrong lookup cannot pass on a fixed
// answer. They are stateful where the real service converges on a key: payments keeps one leg
// per (charge key, refund key) and one journal entry per fact id, and inventory one receipt per
// refund id. Effects are counted as the real services would count them, not only as calls.

var (
	migrateOnce sync.Once
	migrateErr  error
)

func abandonDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("COMMERCE_EXCHANGEABANDON_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("COMMERCE_EXCHANGEABANDON_TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	migrateOnce.Do(func() { migrateErr = store.Migrate(ctx, db) })
	if migrateErr != nil {
		t.Fatalf("migrate: %v", migrateErr)
	}
	return db, ctx
}

const (
	quantity       = int32(2)
	sourceUnit     = int64(1500)
	retainedFee    = int64(200)
	upgradeUnit    = int64(2000) // delta +1000
	downgradeUnit  = int64(1100) // delta -800
	evenUnit       = sourceUnit  // delta 0
	operatorName   = "Ada Operator"
	operatorReason = "source admitted before the switch"
)

// fixture is one settled, unswitched exchange with its two gross facts, as settlement leaves it.
type fixture struct {
	Org, Exchange                              uuid.UUID
	SourceOrder, SourceReservation, SourceHold uuid.UUID
	ReplacementOrder, ReplacementReservation   uuid.UUID
	TargetHold, Buyer                          uuid.UUID
	SourceTotal, SourceGross                   int64
	TargetTotal, TargetGross, Delta            int64
	SourceKey, ReplacementKey                  string
	Quantity                                   int32
}

func (f fixture) receipt() uuid.UUID         { return store.ExchangeAbandonmentRefundID(f.Org, f.Exchange) }
func (f fixture) chargeKey() string          { return exchangeunwind.ChargeKey(f.Exchange) }
func (f fixture) downgradeRefundKey() string { return exchangeunwind.RefundKey(f.Exchange) }
func (f fixture) factID() uuid.UUID          { return store.ExchangeAbandonedFactID(f.Org, f.Exchange) }
func (f fixture) attribution() store.AbandonmentAttribution {
	return store.AbandonmentAttribution{Operator: operatorName, Reason: operatorReason}
}

func seed(t *testing.T, db *sql.DB, ctx context.Context, targetUnit int64) fixture {
	t.Helper()
	return seedQuantity(t, db, ctx, targetUnit, quantity)
}

// seedQuantity is seed with another order quantity.
func seedQuantity(t *testing.T, db *sql.DB, ctx context.Context, targetUnit int64, quantity int32) fixture {
	t.Helper()
	f := fixture{
		Org: uuid.New(), Exchange: uuid.New(),
		SourceOrder: uuid.New(), SourceReservation: uuid.New(), SourceHold: uuid.New(),
		ReplacementOrder: uuid.New(), ReplacementReservation: uuid.New(), TargetHold: uuid.New(), Buyer: uuid.New(),
		Quantity: quantity,
	}
	f.SourceTotal = sourceUnit * int64(quantity)
	f.SourceGross = f.SourceTotal + retainedFee
	f.TargetTotal = targetUnit * int64(quantity)
	f.TargetGross = f.TargetTotal + retainedFee
	f.Delta = f.TargetTotal - f.SourceTotal
	f.SourceKey = "src-" + f.SourceOrder.String()
	f.ReplacementKey = "exchange:" + f.Exchange.String()
	settledAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	targetSlot, targetType := uuid.New(), uuid.New()
	exec(`INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'EUR','completed')`,
		f.SourceReservation, f.Org, f.SourceHold, uuid.New(), uuid.New(), f.Buyer, quantity, sourceUnit, f.SourceGross, f.SourceTotal)
	exec(`INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref)
		VALUES($1,$2,'completed',$3,'fingerprint',$4)`, f.SourceOrder, f.SourceReservation, f.SourceKey, uuid.New())
	exec(`INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'EUR','completed')`,
		f.ReplacementReservation, f.Org, f.TargetHold, targetSlot, targetType, f.Buyer, quantity, targetUnit, f.TargetGross, f.TargetTotal)
	exec(`INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
		VALUES($1,$2,'completed',$3,'exchange')`, f.ReplacementOrder, f.ReplacementReservation, f.ReplacementKey)
	exec(`INSERT INTO order_exchanges(organizer_id,id,source_order_id,replacement_order_id,target_ticket_type_id,
		                            idempotency_key,request_fingerprint,quantity,source_total,source_gross_total,
		                            target_total,delta_amount,target_unit_amount,target_hold_id,
		                            replacement_reservation_id,target_slot_id,basis_at,
		                            currency,actor,reason,created_at,settled_at)
		VALUES($1,$2,$3,$4,$5,$6,'fp',$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'EUR','ops','buyer asked',$16,$17)`,
		f.Org, f.Exchange, f.SourceOrder, f.ReplacementOrder, targetType, "xkey-"+f.Exchange.String(),
		quantity, f.SourceTotal, f.SourceGross, f.TargetTotal, f.Delta, targetUnit, f.TargetHold,
		f.ReplacementReservation, targetSlot, settledAt.Add(-time.Minute), settledAt)
	for _, leg := range []struct {
		id     uuid.UUID
		typ    string
		amount int64
	}{
		{store.ExchangeReversedFactID(f.Exchange), "order.exchange.reversed", f.SourceGross},
		{store.ExchangeSoldFactID(f.Exchange), "order.exchange.sold", f.TargetGross},
	} {
		exec(`INSERT INTO order_facts(fact_id,order_id,organizer_id,buyer_id,fact_type,amount,currency,occurred_at)
			VALUES($1,$2,$3,$4,$5,$6,'EUR',$7)`, leg.id, f.SourceOrder, f.Org, f.Buyer, leg.typ, leg.amount, settledAt.Add(-time.Minute))
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM order_exchanges WHERE organizer_id=$1`, f.Org)
		_, _ = db.Exec(`DELETE FROM order_facts WHERE organizer_id=$1`, f.Org)
		_, _ = db.Exec(`DELETE FROM order_refunds WHERE organizer_id=$1`, f.Org)
		_, _ = db.Exec(`DELETE FROM orders WHERE reservation_id IN (SELECT id FROM reservations WHERE organizer_id=$1)`, f.Org)
		_, _ = db.Exec(`DELETE FROM reservations WHERE organizer_id=$1`, f.Org)
	})
	return f
}

// leg is one payments refund leg.
type leg struct {
	amount    int64
	currency  string
	completed bool
	factID    uuid.UUID
}

// world holds the fakes. Hooks run inside a call and may fail it, before or after the fake's
// own durable effect, which is how a lost answer is modelled.
type world struct {
	t *testing.T
	f fixture

	mu    sync.Mutex
	calls []string

	// Access.
	admission   map[uuid.UUID]accessadmission.Admission
	accessErr   map[uuid.UUID]error
	accessHook  func(order uuid.UUID)
	accessOrder []uuid.UUID

	// Payments evidence.
	capture          ChargeEvidence
	captureKey       string
	downgrade        LegEvidence
	legErr           func(source, refund string) error
	replacementLegs  map[string]LegEvidence
	onReplacementLeg func(refund string)

	// Payments refund legs under the charge key, keyed by refund key.
	legs            map[string]*leg
	captured        int64 // the capture the legs draw on
	otherLegs       int64 // legs bound by others against the same capture
	refundBefore    func(n int) error
	refundAfterBind func(n int) error
	refundAfterDone func(n int) error
	refundAnswer    func(RefundAnswer) RefundAnswer
	refundCalls     int
	movements       int

	// Payments journal.
	journal      map[uuid.UUID]store.ExchangeAbandonedFact
	appendBefore func(n int) error
	appendAfter  func(n int) error
	appendCalls  int

	// Inventory receipts, keyed by refund id.
	receipts     map[uuid.UUID]int32
	returnBefore func(n int) error
	returnAfter  func(n int) error
	returnAnswer func(CapacityReturn) CapacityReturn
	returnCalls  int
}

func newWorld(t *testing.T, f fixture) *world {
	w := &world{
		t: t, f: f,
		admission: map[uuid.UUID]accessadmission.Admission{
			f.SourceOrder:      {Admitted: true, IssuedCount: f.Quantity},
			f.ReplacementOrder: {Admitted: false, IssuedCount: 0},
		},
		accessErr:       map[uuid.UUID]error{},
		captureKey:      f.chargeKey(),
		replacementLegs: map[string]LegEvidence{},
		legs:            map[string]*leg{},
		journal:         map[uuid.UUID]store.ExchangeAbandonedFact{},
		receipts:        map[uuid.UUID]int32{},
	}
	if f.Delta > 0 {
		amount, currency := f.Delta, "EUR"
		w.capture = ChargeEvidence{Found: true, Resolved: true, Status: "captured", CapturedAmount: &amount, Currency: &currency}
		w.captured = f.Delta
	}
	if f.Delta < 0 {
		w.downgrade = LegEvidence{Found: true, Completed: true, Amount: -f.Delta, Currency: "EUR"}
	}
	return w
}

func (w *world) service(db *sql.DB) *Service { return New(db, w, w, w) }

func (w *world) note(call string) {
	w.calls = append(w.calls, call)
}

// count reports the calls recorded under one name since the last reset.
func (w *world) count(name string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.calls {
		if c == name {
			n++
		}
	}
	return n
}

func (w *world) total() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}

func (w *world) resetCalls() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls, w.accessOrder = nil, nil
}

func (w *world) Admission(_ context.Context, org, order uuid.UUID) (accessadmission.Admission, error) {
	w.mu.Lock()
	if org != w.f.Org {
		w.t.Errorf("access admission read for organizer %s, want %s", org, w.f.Org)
	}
	if order != w.f.SourceOrder && order != w.f.ReplacementOrder {
		w.t.Errorf("access admission read for order %s, which is neither the source nor the replacement", order)
	}
	name := "access:source"
	if order == w.f.ReplacementOrder {
		name = "access:replacement"
	}
	w.note(name)
	w.accessOrder = append(w.accessOrder, order)
	hook, answer, err := w.accessHook, w.admission[order], w.accessErr[order]
	w.mu.Unlock()
	if hook != nil {
		hook(order)
		w.mu.Lock()
		answer, err = w.admission[order], w.accessErr[order]
		w.mu.Unlock()
	}
	return answer, err
}

func (w *world) ChargeOperation(_ context.Context, org uuid.UUID, key string) (ChargeEvidence, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if org != w.f.Org || key != w.f.chargeKey() {
		w.t.Errorf("operation read for (%s, %q), want (%s, %q)", org, key, w.f.Org, w.f.chargeKey())
	}
	w.note("payments:operation")
	if key != w.captureKey {
		return ChargeEvidence{}, nil
	}
	return w.capture, nil
}

func (w *world) RefundLeg(_ context.Context, org uuid.UUID, source, refund string) (LegEvidence, error) {
	w.mu.Lock()
	if org != w.f.Org {
		w.t.Errorf("refund-leg read for organizer %s, want %s", org, w.f.Org)
	}
	var name string
	switch {
	case source == w.f.SourceKey && refund == w.f.downgradeRefundKey():
		name = "payments:downgrade-leg"
	case source == w.f.chargeKey() && refund == w.f.receipt().String():
		name = "payments:abandonment-leg"
	case source == w.f.ReplacementKey:
		name = "payments:replacement-leg"
	default:
		w.t.Errorf("refund-leg read for (%q, %q) matches no leg of exchange %s", source, refund, w.f.Exchange)
	}
	w.note(name)
	if w.legErr != nil {
		if err := w.legErr(source, refund); err != nil {
			w.mu.Unlock()
			return LegEvidence{}, err
		}
	}
	hook := w.onReplacementLeg
	var out LegEvidence
	switch name {
	case "payments:downgrade-leg":
		out = w.downgrade
	case "payments:abandonment-leg":
		if l, ok := w.legs[refund]; ok {
			out = LegEvidence{Found: true, Completed: l.completed, Amount: l.amount, Currency: l.currency}
		}
	case "payments:replacement-leg":
		out = w.replacementLegs[refund]
	}
	w.mu.Unlock()
	if name == "payments:replacement-leg" && hook != nil {
		hook(refund)
	}
	return out, nil
}

// PartialRefund models payments' leg binder: an existing leg is returned before its amount is
// rechecked, a new leg must fit the capture, and a completed leg replays its fact.
func (w *world) PartialRefund(_ context.Context, r RefundRequest) (RefundAnswer, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.refundCalls++
	n := w.refundCalls
	w.note("payments:refund")
	if r.OrganizerID != w.f.Org || r.ChargeKey != w.f.chargeKey() || r.RefundKey != w.f.receipt().String() ||
		r.Amount != w.f.Delta || r.Currency != "EUR" {
		w.t.Errorf("partial refund %+v, want (%s, %q, %q, %d EUR)", r, w.f.Org, w.f.chargeKey(), w.f.receipt(), w.f.Delta)
	}
	if w.refundBefore != nil {
		if err := w.refundBefore(n); err != nil {
			return RefundAnswer{}, err
		}
	}
	l, ok := w.legs[r.RefundKey]
	if !ok {
		if w.otherLegs+r.Amount > w.captured {
			return RefundAnswer{}, fmt.Errorf("%w: refund legs would exceed the captured amount", ErrRefused)
		}
		l = &leg{amount: r.Amount, currency: r.Currency}
		w.legs[r.RefundKey] = l
	}
	if w.refundAfterBind != nil {
		if err := w.refundAfterBind(n); err != nil {
			return RefundAnswer{}, err
		}
	}
	if !l.completed {
		l.completed, l.factID = true, uuid.NewSHA1(uuid.NameSpaceOID, []byte("payment-leg:"+r.ChargeKey+":"+r.RefundKey))
		w.movements++
	}
	if w.refundAfterDone != nil {
		if err := w.refundAfterDone(n); err != nil {
			return RefundAnswer{}, err
		}
	}
	answer := RefundAnswer{FactID: l.factID, Amount: l.amount, Currency: l.currency}
	if w.refundAnswer != nil {
		answer = w.refundAnswer(answer)
	}
	return answer, nil
}

// AppendFact models the journal: one entry per fact id, an identical replay is accepted, a
// different fact under the same id is refused.
func (w *world) AppendFact(_ context.Context, f store.ExchangeAbandonedFact) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.appendCalls++
	n := w.appendCalls
	w.note("payments:fact")
	if f.ID != w.f.factID() || f.OrganizerID != w.f.Org || f.OrderID != w.f.SourceOrder || f.ExchangeID != w.f.Exchange {
		w.t.Errorf("fact %+v does not name exchange %s of organizer %s", f, w.f.Exchange, w.f.Org)
	}
	if w.appendBefore != nil {
		if err := w.appendBefore(n); err != nil {
			return err
		}
	}
	if prior, ok := w.journal[f.ID]; ok && !equalFacts(prior, f) {
		return fmt.Errorf("%w: fact id reused with different content", ErrRefused)
	}
	w.journal[f.ID] = f
	if w.appendAfter != nil {
		return w.appendAfter(n)
	}
	return nil
}

func equalFacts(a, b store.ExchangeAbandonedFact) bool {
	return a.ID == b.ID && a.Amount == b.Amount && a.Currency == b.Currency && a.OccurredAt.Equal(b.OccurredAt) &&
		fmt.Sprint(a.Payload()) == fmt.Sprint(b.Payload()) && a.BuyerID == b.BuyerID
}

// ReturnCapacity models inventory's receipt: one return per refund id, replayed after.
func (w *world) ReturnCapacity(_ context.Context, org, hold, receipt uuid.UUID, q int32) (CapacityReturn, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.returnCalls++
	n := w.returnCalls
	w.note("inventory:return")
	if org != w.f.Org || hold != w.f.TargetHold || receipt != w.f.receipt() || q != w.f.Quantity {
		w.t.Errorf("capacity return (%s, hold %s, receipt %s, %d), want (%s, target hold %s, receipt %s, %d)",
			org, hold, receipt, q, w.f.Org, w.f.TargetHold, w.f.receipt(), w.f.Quantity)
	}
	if w.returnBefore != nil {
		if err := w.returnBefore(n); err != nil {
			return CapacityReturn{}, err
		}
	}
	w.receipts[receipt] = q
	if w.returnAfter != nil {
		if err := w.returnAfter(n); err != nil {
			return CapacityReturn{}, err
		}
	}
	out := CapacityReturn{HoldID: hold, Quantity: q, Unreturned: 0}
	if w.returnAnswer != nil {
		out = w.returnAnswer(out)
	}
	return out, nil
}

// once fails only the first call.
func once(err error) func(int) error {
	return func(n int) error {
		if n == 1 {
			return err
		}
		return nil
	}
}

// always fails every call.
func always(err error) func(int) error { return func(int) error { return err } }

// row reads the abandonment columns directly, so assertions do not depend on the code under
// test to read them back.
type row struct {
	StartedAt                     sql.NullTime
	Operator, SessionUser, Reason sql.NullString
	RefundFact                    uuid.NullUUID
	FailedRef                     sql.NullString
	ReturnedAt, AbandonedAt       sql.NullTime
	WrittenOff                    sql.NullInt64
}

func readRow(t *testing.T, db *sql.DB, ctx context.Context, f fixture) row {
	t.Helper()
	var r row
	if err := db.QueryRowContext(ctx, `
		SELECT abandonment_started_at, abandonment_operator_name, abandonment_session_user, abandonment_reason,
		       abandonment_refund_fact_id, abandonment_refund_failed_ref, abandonment_target_returned_at,
		       abandoned_at, abandonment_written_off_amount
		FROM order_exchanges WHERE organizer_id=$1 AND id=$2`, f.Org, f.Exchange).
		Scan(&r.StartedAt, &r.Operator, &r.SessionUser, &r.Reason, &r.RefundFact, &r.FailedRef,
			&r.ReturnedAt, &r.AbandonedAt, &r.WrittenOff); err != nil {
		t.Fatal(err)
	}
	return r
}

func countRows(t *testing.T, db *sql.DB, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func abandonedFacts(t *testing.T, db *sql.DB, ctx context.Context, f fixture) int {
	return countRows(t, db, ctx, `SELECT count(*) FROM order_facts WHERE organizer_id=$1 AND fact_type='order.exchange.abandoned'`, f.Org)
}
