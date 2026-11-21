//go:build smoke

package refundoutcome

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/commerce/internal/bulkrefund"
	"ticketing/services/commerce/internal/refunds"
	"ticketing/services/commerce/internal/reversal"
	"ticketing/services/commerce/internal/store"
)

// TKT-525 S1 through the command coordinator, against real PostgreSQL and a recording payments
// peer that speaks the declared failure-proof contract. The peer counts EVERY request, so "the
// command makes zero writing HTTP calls" is an assertion about what reached the wire.

var (
	migrateOnce sync.Once
	migrateErr  error
)

// outcomeDB opens this package's OWN database: `go test ./internal/...` runs packages as
// concurrent binaries, and a second package migrating a shared database races the first
// (TKT-198). scripts/smoke.sh creates commerce_refundoutcome_smoke.
func outcomeDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("COMMERCE_REFUNDOUTCOME_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("COMMERCE_REFUNDOUTCOME_TEST_DATABASE_URL is not set")
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

type peerRequest struct {
	method, path string
	query        url.Values
}

// paymentsPeer is a recording payments that answers the failure proof with a fixed response.
type paymentsPeer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []peerRequest
	status   int
	body     string
}

func newPaymentsPeer(t *testing.T, status int, body string) *paymentsPeer {
	t.Helper()
	p := &paymentsPeer{status: status, body: body}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.requests = append(p.requests, peerRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query()})
		status, body := p.status, p.body
		p.mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/internal/psp/partial-refund/failure-proof" {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"peer: unexpected route"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *paymentsPeer) seen() []peerRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]peerRequest(nil), p.requests...)
}

func (p *paymentsPeer) assertOnlyProofReads(t *testing.T, want int) {
	t.Helper()
	got := p.seen()
	if len(got) != want {
		t.Fatalf("payments requests = %d (%+v), want %d", len(got), got, want)
	}
	for _, r := range got {
		if r.method != http.MethodGet || r.path != "/internal/psp/partial-refund/failure-proof" {
			t.Fatalf("the command made %s %s; it may only read the failure proof", r.method, r.path)
		}
	}
}

type seeded struct {
	org, order, hold, slot uuid.UUID
	chargeKey              string
}

func seedOrder(t *testing.T, ctx context.Context, db *sql.DB, org, slot uuid.UUID, quantity int32, unit int64) seeded {
	t.Helper()
	return seedOrderWithChargeKey(t, ctx, db, org, slot, quantity, unit, "checkout-"+uuid.NewString())
}

// seedOrderWithChargeKey seeds a completed order whose stored checkout key is chargeKey. Two
// orders can store the same key (TKT-586's checkout and exchange alias).
func seedOrderWithChargeKey(t *testing.T, ctx context.Context, db *sql.DB, org, slot uuid.UUID, quantity int32, unit int64, chargeKey string) seeded {
	t.Helper()
	s := seeded{org: org, order: uuid.New(), hold: uuid.New(), slot: slot, chargeKey: chargeKey}
	res := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,'EUR','completed')`,
		res, org, s.hold, slot, uuid.New(), uuid.New(), quantity, unit, unit*int64(quantity)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref)
		VALUES($1,$2,'completed',$3,'fp',$4)`, s.order, res, s.chargeKey, uuid.New()); err != nil {
		t.Fatal(err)
	}
	return s
}

func bindStaffRefund(t *testing.T, ctx context.Context, db *sql.DB, s seeded, key string, quantity int32) store.Refund {
	t.Helper()
	r, err := store.BindOrderRefund(ctx, db, store.RefundRequest{
		OrderID: s.order, OrganizerID: s.org, Quantity: quantity, IdempotencyKey: key,
		Actor: "support@example.test", Reason: "buyer cannot attend",
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func outsideRequest(s seeded, refund uuid.UUID) store.RefundOutcomeRequest {
	return store.RefundOutcomeRequest{
		OrganizerID: s.org, RefundID: refund, Kind: store.OutcomeRefundedOutside,
		OperatorName: "Ada Operator", ExternalRef: "SEPA-0001", Reason: "card closed; refunded by bank transfer",
	}
}

const proof2000 = `{"provider_ref":"re_failed_1","amount":2000,"currency":"EUR"}`

type refundState struct {
	status   string
	outside  sql.NullString
	outcomes int
	facts    int
	qty      int32
	amount   int64
	rstatus  string
}

func readRefundState(t *testing.T, ctx context.Context, db *sql.DB, s seeded, refund uuid.UUID) refundState {
	t.Helper()
	var st refundState
	if err := db.QueryRowContext(ctx, `
		SELECT rf.status, rf.outside_completion_kind,
		       (SELECT count(*) FROM refund_outcomes WHERE organizer_id=$1 AND subject_id=$2),
		       (SELECT count(*) FROM order_facts WHERE order_id=$3 AND fact_type='order.refunded'),
		       o.refunded_quantity, o.refunded_amount, o.refund_status
		FROM order_refunds rf JOIN orders o ON o.id=rf.order_id
		WHERE rf.organizer_id=$1 AND rf.id=$2`, s.org, refund, s.order).
		Scan(&st.status, &st.outside, &st.outcomes, &st.facts, &st.qty, &st.amount, &st.rstatus); err != nil {
		t.Fatal(err)
	}
	return st
}

// COS 1: one proof read with the stored identities, one outcome, completion with no payments
// fact and no Commerce order.refunded fact, one projection advance. An exact repeat answers
// from Commerce alone; a different repeat conflicts, also without asking payments.
func TestRefundedOutsideRecordsAfterAReadOnlyProof(t *testing.T) {
	db, ctx := outcomeDB(t)
	s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
	r := bindStaffRefund(t, ctx, db, s, "staff-1", 2)
	peer := newPaymentsPeer(t, 200, proof2000)
	svc := New(db, NewHTTPPayments(peer.srv.URL, "payments-token", time.Second))

	got, err := svc.RecordRefundedOutside(ctx, outsideRequest(s, r.ID))
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if got.Replay || got.Outcome.Amount != 2000 || got.Outcome.Currency != "EUR" || got.Outcome.ExternalRef != "SEPA-0001" {
		t.Fatalf("result = %+v", got)
	}
	peer.assertOnlyProofReads(t, 1)
	q := peer.seen()[0].query
	if q.Get("organizer_id") != s.org.String() || q.Get("idempotency_key") != s.chargeKey || q.Get("refund_key") != r.ID.String() {
		t.Fatalf("proof query = %v, want the organizer, the order's stored charge key and the refund id", q)
	}
	if st := readRefundState(t, ctx, db, s, r.ID); st != (refundState{"completed", sql.NullString{String: "refunded_outside", Valid: true}, 1, 0, 2, 2000, "full"}) {
		t.Fatalf("after record: %+v", st)
	}

	again, err := svc.RecordRefundedOutside(ctx, outsideRequest(s, r.ID))
	if err != nil || !again.Replay || !again.Outcome.RecordedAt.Equal(got.Outcome.RecordedAt) {
		t.Fatalf("exact repeat: %+v err=%v", again, err)
	}
	different := outsideRequest(s, r.ID)
	different.OperatorName = "Grace Operator"
	if _, err := svc.RecordRefundedOutside(ctx, different); !errors.Is(err, store.ErrRefundOutcomeConflict) {
		t.Fatalf("different repeat: err = %v, want conflict", err)
	}
	peer.assertOnlyProofReads(t, 1)
	if st := readRefundState(t, ctx, db, s, r.ID); st.qty != 2 || st.amount != 2000 || st.outcomes != 1 {
		t.Fatalf("after repeats: %+v", st)
	}
}

// Without a valid proof nothing changes in Commerce. Each payments answer is one the plan names:
// a leg payments already completed (provider success before Commerce recorded it), a missing
// leg, a leg with an abandonment marker, an inconclusive provider, and a proof for other money.
func TestRefundedOutsideRefusesWithoutAValidProof(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"payments completed the leg", 409, `{"error":"refund leg completed"}`, ErrProofRefused},
		{"payments holds an abandonment marker", 409, `{"error":"refund leg abandoned"}`, ErrProofRefused},
		{"payments has no such leg", 404, `{"error":"refund leg not found"}`, ErrProofRefused},
		{"the provider is inconclusive", 502, `{"error":"refund failure proof unavailable"}`, ErrProofUnavailable},
		{"the proof is for a different amount", 200, `{"provider_ref":"re_failed_1","amount":1999,"currency":"EUR"}`, ErrProofMismatch},
		{"the proof is for a different currency", 200, `{"provider_ref":"re_failed_1","amount":2000,"currency":"USD"}`, ErrProofMismatch},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, ctx := outcomeDB(t)
			s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
			r := bindStaffRefund(t, ctx, db, s, "staff-refused", 2)
			peer := newPaymentsPeer(t, c.status, c.body)
			_, err := New(db, NewHTTPPayments(peer.srv.URL, "t", time.Second)).RecordRefundedOutside(ctx, outsideRequest(s, r.ID))
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			peer.assertOnlyProofReads(t, 1)
			if st := readRefundState(t, ctx, db, s, r.ID); st != (refundState{"pending", sql.NullString{}, 0, 0, 0, 0, "none"}) {
				t.Fatalf("Commerce changed without a proof: %+v", st)
			}
		})
	}
}

// Answers Commerce can give alone never reach payments: a missing refund and a foreign one
// (the same refusal), an ordinarily completed refund, and a refund with another outcome.
func TestRefundedOutsideAnswersLocallyWithoutAskingPayments(t *testing.T) {
	db, ctx := outcomeDB(t)
	s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 3, 1000)
	foreign := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 1, 1000)
	foreignRefund := bindStaffRefund(t, ctx, db, foreign, "foreign", 1)
	completed := bindStaffRefund(t, ctx, db, s, "completed", 1)
	if err := store.CompleteOrderRefund(ctx, db, s.org, completed.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	abandoned := bindStaffRefund(t, ctx, db, s, "abandoned", 1)
	if _, err := db.ExecContext(ctx, `INSERT INTO refund_outcomes(organizer_id,subject_kind,subject_id,kind,amount,currency,external_ref,operator_name,session_user_name,reason)
		VALUES($1,'order_refund',$2,'abandoned',1000,'EUR','re_x','Ada',session_user,'nobody will refund')`, s.org, abandoned.ID); err != nil {
		t.Fatal(err)
	}
	peer := newPaymentsPeer(t, 200, proof2000)
	svc := New(db, NewHTTPPayments(peer.srv.URL, "t", time.Second))

	var missingErr, foreignErr error
	for name, c := range map[string]struct {
		refund uuid.UUID
		want   error
		got    *error
	}{
		"missing":   {uuid.New(), store.ErrRefundOutcomeNotFound, &missingErr},
		"foreign":   {foreignRefund.ID, store.ErrRefundOutcomeNotFound, &foreignErr},
		"completed": {completed.ID, store.ErrRefundOutcomeIneligible, nil},
		"abandoned": {abandoned.ID, store.ErrRefundOutcomeConflict, nil},
	} {
		_, err := svc.RecordRefundedOutside(ctx, outsideRequest(s, c.refund))
		if !errors.Is(err, c.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, c.want)
		}
		if c.got != nil {
			*c.got = err
		}
	}
	if missingErr.Error() != foreignErr.Error() {
		t.Fatalf("a foreign refund answered %q and a missing one %q; they must be indistinguishable", foreignErr, missingErr)
	}
	peer.assertOnlyProofReads(t, 0)
	var foreignStatus string
	if err := db.QueryRowContext(ctx, `SELECT status FROM order_refunds WHERE organizer_id=$1 AND id=$2`, foreign.org, foreignRefund.ID).Scan(&foreignStatus); err != nil || foreignStatus != "pending" {
		t.Fatalf("the foreign refund changed: %q %v", foreignStatus, err)
	}
}

// recordingCaller is the refund service's transport. It records every call, fails the money
// leg terminally (the failed bulk refund this ticket is about), and fails the first
// `accessFailures` ticket voids for the watched order.
type recordingCaller struct {
	mu             sync.Mutex
	calls          []string
	watchOrder     uuid.UUID
	accessFailures int
}

func (c *recordingCaller) call(_ context.Context, method, u, _ string, in any, _ bool) (int, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	body, _ := json.Marshal(in)
	c.calls = append(c.calls, method+" "+u+" "+string(body))
	switch {
	case strings.HasSuffix(u, "/internal/psp/partial-refund"):
		return 422, []byte(`{"code":"provider_refund_failed","error":"provider refund failed","provider_ref":"re_failed_1"}`), nil
	case strings.HasSuffix(u, "/internal/facts"):
		return 200, []byte(`{}`), nil
	case strings.Contains(u, "/internal/orders/") && strings.HasSuffix(u, "/refunds"):
		if strings.Contains(u, c.watchOrder.String()) && c.accessFailures > 0 {
			c.accessFailures--
			return 503, []byte(`{"error":"issuance has not caught up"}`), nil
		}
		return 200, []byte(`{}`), nil
	case strings.HasSuffix(u, "/refund-capacity"):
		return 200, []byte(`{}`), nil
	}
	return 500, nil, nil
}

func (c *recordingCaller) matching(substrings ...string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, call := range c.calls {
		ok := true
		for _, s := range substrings {
			if !strings.Contains(call, s) {
				ok = false
			}
		}
		if ok {
			out = append(out, call)
		}
	}
	return out
}

func (c *recordingCaller) indexOf(substrings ...string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, call := range c.calls {
		ok := true
		for _, s := range substrings {
			if !strings.Contains(call, s) {
				ok = false
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// COS 2, end to end: a bulk cancellation refund fails terminally at the provider; the operator
// records refunded_outside; the existing reconciler claims the outside completion, fails its
// first ticket void, makes no capacity call on that pass, then voids and only then returns
// capacity; a second bulk run over the slot uses the ORIGINAL refund key, moves no money and
// reports already_refunded. An ordinary pending refund on another order is never reversed.
func TestOutsideCompletionIsReversedAndABulkRerunReportsAlreadyRefunded(t *testing.T) {
	db, ctx := outcomeDB(t)
	org, slot := uuid.New(), uuid.New()
	s := seedOrder(t, ctx, db, org, slot, 2, 1000)
	// An ordinary pending staff refund on another order of the same organizer: never eligible.
	other := seedOrder(t, ctx, db, org, uuid.New(), 1, 1000)
	pending := bindStaffRefund(t, ctx, db, other, "ordinary-pending", 1)

	caller := &recordingCaller{watchOrder: s.order}
	svcRefunds := refunds.New(db, caller.call, "http://payments.test", "http://access.test", "http://inventory.test")
	bulkKeyRefund := store.RefundID(org, store.CancellationRefundKey(slot, s.order))

	// The first bulk run: the provider fails the refund terminally.
	if _, err := store.BindCancellationRun(ctx, db, store.CancellationRunRequest{
		OrganizerID: org, SlotID: slot, IdempotencyKey: "cancel-1", Actor: "ops@example.test", Reason: "event cancelled",
	}); err != nil {
		t.Fatal(err)
	}
	bulkrefund.New(bulkrefund.DBStore{DB: db}, svcRefunds, time.Minute, 10, time.Minute).RunOnce(ctx)
	if n := len(caller.matching("POST http://payments.test/internal/psp/partial-refund", bulkKeyRefund.String())); n != 1 {
		t.Fatalf("first bulk run: %d money legs for the refund, want 1 (calls %v)", n, caller.calls)
	}
	if st := readRefundState(t, ctx, db, s, bulkKeyRefund); st.status != "pending" {
		t.Fatalf("the failed bulk refund is %q, want pending", st.status)
	}

	// The operator records the outside refund.
	peer := newPaymentsPeer(t, 200, proof2000)
	if _, err := New(db, NewHTTPPayments(peer.srv.URL, "t", time.Second)).RecordRefundedOutside(ctx, outsideRequest(s, bulkKeyRefund)); err != nil {
		t.Fatalf("record outside: %v", err)
	}
	peer.assertOnlyProofReads(t, 1)
	moneyCalls := len(caller.matching("http://payments.test"))

	// Reconciler pass 1: the void fails, so capacity must not be returned.
	caller.mu.Lock()
	caller.accessFailures = 1
	caller.mu.Unlock()
	rev := reversal.New(reversal.DBStore{DB: db}, svcRefunds, time.Minute, 100, time.Minute, nil)
	rev.RunOnce(ctx)
	if n := len(caller.matching("http://access.test/internal/orders/" + s.order.String())); n != 1 {
		t.Fatalf("pass 1: %d void calls for the order, want 1", n)
	}
	if n := len(caller.matching("refund-capacity", s.hold.String())); n != 0 {
		t.Fatalf("pass 1 returned capacity after a FAILED void (%d calls): the seat is free while the ticket admits\n%s", n, strings.Join(caller.calls, "\n"))
	}
	var voided, returned sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT tickets_voided_at, capacity_returned_at FROM order_refunds WHERE organizer_id=$1 AND id=$2`, org, bulkKeyRefund).
		Scan(&voided, &returned); err != nil || voided.Valid || returned.Valid {
		t.Fatalf("after the failed void: voided=%v returned=%v err=%v, want both outstanding", voided, returned, err)
	}

	// Make the retry due now; the backoff is the reconciler's, not this test's subject.
	if _, err := db.ExecContext(ctx, `UPDATE order_refunds SET reversal_next_attempt_at=now() WHERE organizer_id=$1 AND id=$2`, org, bulkKeyRefund); err != nil {
		t.Fatal(err)
	}
	rev.RunOnce(ctx)
	voidAt := caller.indexOf("http://access.test/internal/orders/"+s.order.String(), `"refund_id":"`+bulkKeyRefund.String())
	capacityAt := caller.indexOf("refund-capacity", s.hold.String())
	if capacityAt < 0 {
		t.Fatal("pass 2 never returned capacity")
	}
	if n := len(caller.matching("http://access.test/internal/orders/" + s.order.String())); n != 2 {
		t.Fatalf("void calls = %d, want the failed one and the retry", n)
	}
	if capacityAt < voidAt {
		t.Fatalf("capacity (call %d) was returned before the tickets were voided (call %d)", capacityAt, voidAt)
	}
	if err := db.QueryRowContext(ctx, `SELECT tickets_voided_at, capacity_returned_at FROM order_refunds WHERE organizer_id=$1 AND id=$2`, org, bulkKeyRefund).
		Scan(&voided, &returned); err != nil || !voided.Valid || !returned.Valid {
		t.Fatalf("after the retry: voided=%v returned=%v err=%v, want both discharged", voided, returned, err)
	}
	// The ordinary pending refund was never reversed.
	if n := len(caller.matching("http://access.test/internal/orders/" + other.order.String())); n != 0 {
		t.Fatalf("the reconciler voided tickets for a pending refund (%d calls)", n)
	}
	if err := db.QueryRowContext(ctx, `SELECT tickets_voided_at FROM order_refunds WHERE organizer_id=$1 AND id=$2`, org, pending.ID).Scan(&voided); err != nil || voided.Valid {
		t.Fatalf("pending refund voided=%v err=%v", voided, err)
	}

	// A second bulk run over the slot: the original key, no money, already_refunded.
	run2, err := store.BindCancellationRun(ctx, db, store.CancellationRunRequest{
		OrganizerID: org, SlotID: slot, IdempotencyKey: "cancel-2", Actor: "ops@example.test", Reason: "event cancelled",
	})
	if err != nil {
		t.Fatal(err)
	}
	bulkrefund.New(bulkrefund.DBStore{DB: db}, svcRefunds, time.Minute, 10, time.Minute).RunOnce(ctx)
	if n := len(caller.matching("http://payments.test")); n != moneyCalls {
		t.Fatalf("the second bulk run made %d payments calls; the refund is complete and none is owed", n-moneyCalls)
	}
	report, err := store.CancellationReport(ctx, db, org, run2.ID, 100, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	var row *store.CancellationOrderOutcome
	for i := range report.Orders {
		if report.Orders[i].OrderID == s.order {
			row = &report.Orders[i]
		}
	}
	if row == nil {
		t.Fatalf("the second run has no row for the order: %+v", report.Orders)
	}
	if row.Outcome != "already_refunded" || !row.RefundID.Valid || row.RefundID.UUID != bulkKeyRefund ||
		!row.MoneyRefunded || !row.TicketsVoided || !row.CapacityReturned || row.RefundedQuantity != 2 || row.RefundedAmount != 2000 {
		t.Fatalf("second run verdict = %+v, want already_refunded under the original key %s", *row, bulkKeyRefund)
	}
	if st := readRefundState(t, ctx, db, s, bulkKeyRefund); st.facts != 0 || st.outcomes != 1 {
		t.Fatalf("after the whole flow: %+v, want one outcome and no order.refunded fact", st)
	}
	if n := len(caller.matching("/internal/facts")); n != 0 {
		t.Fatalf("%d facts were forwarded to the payments journal; an outside completion forwards none", n)
	}
}
