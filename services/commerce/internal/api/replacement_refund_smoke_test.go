//go:build smoke

package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// TKT-498 (plan v2 Build order 4, D10, D14, D18). Staff refunds of an exchange REPLACEMENT order.
//
// A replacement refund can reach money only through the TKT-586 alias: payments binds the
// replacement's checkout key `exchange:<id>` to the exchange's own charge. The shared refund
// service therefore requires Access to report issued replacement tickets before it calls
// payments, and the binder refuses once the exchange's abandonment has started. These tests
// drive the real staff handler, the real refund service and a real Commerce database. Access,
// payments and inventory are one recording stub, so every call is asserted with its scope.

type replacementFixture struct {
	organizer, exchange                      uuid.UUID
	sourceOrder, sourceReservation           uuid.UUID
	replacementOrder, replacementReservation uuid.UUID
	targetHold, targetSlot                   uuid.UUID
	quantity                                 int32
	unit                                     int64
}

// seedSettledReplacement writes a settled, unswitched exchange the way settlement leaves it:
// the source order, the completed replacement order under `exchange:<id>` on its own
// reservation and target hold, and the exchange row naming both.
func seedSettledReplacement(t *testing.T, db *sql.DB, ctx context.Context) replacementFixture {
	t.Helper()
	f := replacementFixture{
		organizer: uuid.New(), exchange: uuid.New(),
		sourceOrder: uuid.New(), sourceReservation: uuid.New(),
		replacementOrder: uuid.New(), replacementReservation: uuid.New(),
		targetHold: uuid.New(), targetSlot: uuid.New(), quantity: 2, unit: 1500,
	}
	buyer := uuid.New()
	sourceTotal, targetTotal := int64(1000)*int64(f.quantity), f.unit*int64(f.quantity)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,1000,$8,$8,'EUR','completed')`,
		f.sourceReservation, f.organizer, uuid.New(), uuid.New(), uuid.New(), buyer, f.quantity, sourceTotal)
	exec(`INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref)
		VALUES($1,$2,'completed',$3,'fingerprint',$4)`, f.sourceOrder, f.sourceReservation, "src-"+f.sourceOrder.String(), uuid.New())
	targetType := uuid.New()
	exec(`INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,'EUR','completed')`,
		f.replacementReservation, f.organizer, f.targetHold, f.targetSlot, targetType, buyer, f.quantity, f.unit, targetTotal)
	exec(`INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
		VALUES($1,$2,'completed',$3,'exchange')`, f.replacementOrder, f.replacementReservation, "exchange:"+f.exchange.String())
	exec(`INSERT INTO order_exchanges(organizer_id,id,source_order_id,replacement_order_id,target_ticket_type_id,
		                            idempotency_key,request_fingerprint,quantity,source_total,source_gross_total,
		                            target_total,delta_amount,target_unit_amount,target_hold_id,
		                            replacement_reservation_id,target_slot_id,basis_at,
		                            currency,actor,reason,created_at,settled_at)
		VALUES($1,$2,$3,$4,$5,$6,'fp',$7,$8,$8,$9,$10,$11,$12,$13,$14,now(),'EUR','ops','buyer asked',now(),now())`,
		f.organizer, f.exchange, f.sourceOrder, f.replacementOrder, targetType, "xkey-"+f.exchange.String(),
		f.quantity, sourceTotal, targetTotal, targetTotal-sourceTotal, f.unit, f.targetHold,
		f.replacementReservation, f.targetSlot)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM order_exchanges WHERE organizer_id=$1`, f.organizer)
		_, _ = db.Exec(`DELETE FROM order_facts WHERE organizer_id=$1`, f.organizer)
		_, _ = db.Exec(`DELETE FROM order_refunds WHERE organizer_id=$1`, f.organizer)
		_, _ = db.Exec(`DELETE FROM orders WHERE reservation_id IN (SELECT id FROM reservations WHERE organizer_id=$1)`, f.organizer)
		_, _ = db.Exec(`DELETE FROM reservations WHERE organizer_id=$1`, f.organizer)
	})
	return f
}

// dependencyStub is Access, payments and inventory behind one server, each under its own path
// prefix so the refund service's token rule still sees a distinct payments URL. It records
// every call; `pause`, when set, blocks the FIRST call of the refund dispatch until released.
type dependencyStub struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []stubCall
	// issued is the replacement's ticket count Access reports; admissionStatus its status.
	issued          int32
	admissionStatus int
	pause           chan struct{}
	paused          chan struct{}
}

type stubCall struct {
	method, path, organizer string
	body                    map[string]any
}

func newDependencyStub(t *testing.T) *dependencyStub {
	t.Helper()
	s := &dependencyStub{admissionStatus: http.StatusOK}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *dependencyStub) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	s.mu.Lock()
	s.calls = append(s.calls, stubCall{method: r.Method, path: r.URL.Path, organizer: r.URL.Query().Get("organizer_id"), body: body})
	pause, paused := s.pause, s.paused
	s.pause = nil
	issued, status := s.issued, s.admissionStatus
	s.mu.Unlock()
	if pause != nil {
		close(paused)
		<-pause
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasPrefix(r.URL.Path, "/access/internal/orders/") && strings.HasSuffix(r.URL.Path, "/admission"):
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"admitted":false,"issued_count":` + strconv.Itoa(int(issued)) + `}`))
	case r.URL.Path == "/payments/internal/psp/partial-refund":
		// The TKT-586 alias: payments resolves `exchange:<id>` to a captured charge and refunds.
		_, _ = w.Write([]byte(`{"status":"refunded","fact_id":"` + uuid.NewString() + `","amount":1,"currency":"EUR","replay":false}`))
	case r.URL.Path == "/payments/internal/facts":
		_, _ = w.Write([]byte(`{"sequence":1,"hash":"00","replay":false}`))
	default:
		_, _ = w.Write([]byte(`{}`))
	}
}

func (s *dependencyStub) recorded() []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubCall(nil), s.calls...)
}

func (s *dependencyStub) count(method, path string) int {
	n := 0
	for _, c := range s.recorded() {
		if c.method == method && c.path == path {
			n++
		}
	}
	return n
}

func (s *dependencyStub) set(issued int32, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issued, s.admissionStatus = issued, status
}

// pauseNext blocks the next call until the returned release runs, and returns a channel that
// closes when that call arrives.
func (s *dependencyStub) pauseNext() (arrived <-chan struct{}, release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pause, s.paused = make(chan struct{}), make(chan struct{})
	p := s.pause
	var once sync.Once
	return s.paused, func() { once.Do(func() { close(p) }) }
}

func replacementRefundServer(t *testing.T, db *sql.DB, stub *dependencyStub) *Server {
	t.Helper()
	return New(ServerConfig{
		DB: db, Client: stub.server.Client(),
		AccessURL: stub.server.URL + "/access", PaymentsURL: stub.server.URL + "/payments",
		InventoryURL: stub.server.URL + "/inventory", InternalToken: internalTok,
	}).WithPaymentsToken("payments-token-for-tests-0123456789").
		WithStaffWriteCredential(staffTok).WithOrganizerAssertionVerifier(testOrgVerifier(t))
}

// staffRefund answers the status and the decoded body, or a nil body when it is not JSON. It
// does not fail the test itself, so a goroutine can call it.
func staffRefund(t *testing.T, s *Server, org, order uuid.UUID, key string) (int, map[string]any) {
	req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+order.String()+"/refunds",
		bytes.NewBufferString(`{"quantity":2,"actor":"support","reason":"target slot cancelled"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Commerce-Staff-Write-Token", staffTok)
	req.Header.Set(organizerAssertionHeader, mintTestAssertion(t, testOrgKey(), org.String(), time.Now().Add(time.Hour)))
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	s.Router(nil, true).ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func refundStatus(t *testing.T, db *sql.DB, ctx context.Context, org, id uuid.UUID) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM order_refunds WHERE organizer_id=$1 AND id=$2`, org, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// reserveReplacementAbandonment reserves the exchange's abandonment with the replacement's
// refunds as they stand, which is what the command writes after payments answered 404 for each
// pending leg.
func reserveReplacementAbandonment(t *testing.T, db *sql.DB, ctx context.Context, f replacementFixture) {
	t.Helper()
	e, err := commercestore.LoadSettledExchange(ctx, db, f.organizer, f.exchange)
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := commercestore.ListReplacementRefunds(ctx, db, f.organizer, f.replacementOrder)
	if err != nil {
		t.Fatal(err)
	}
	attribution, err := commercestore.NewAbandonmentAttribution("Ada Operator", "source admitted before the switch")
	if err != nil {
		t.Fatal(err)
	}
	if _, reserved, err := commercestore.ReserveExchangeAbandonment(ctx, db, commercestore.AbandonmentReservation{
		Expected: e, Attribution: attribution, InspectedRefunds: inspected}); err != nil || !reserved {
		t.Fatalf("reserve abandonment: reserved=%v err=%v", reserved, err)
	}
}

// With no issued replacement ticket, a staff refund of the replacement reaches no payments
// route: Access is asked once, scoped to the organizer and the replacement order, and the
// refund stays pending behind the existing 502. Each Access answer that is not a well-formed
// positive count gets the same treatment. Once Access reports the switched replacement's
// tickets, the same key reaches payments under the replacement's own checkout key.
func TestStaffRefundOfAnUnissuedReplacementNeverReachesPayments(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedSettledReplacement(t, db, ctx)
	stub := newDependencyStub(t)
	s := replacementRefundServer(t, db, stub)
	const key = "staff-replacement-1"
	refundID := commercestore.RefundID(f.organizer, key)
	admissionPath := "/access/internal/orders/" + f.replacementOrder.String() + "/admission"

	for _, tc := range []struct {
		name   string
		issued int32
		status int
	}{
		{"zero issued tickets", 0, http.StatusOK},
		{"access unavailable", 2, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub.set(tc.issued, tc.status)
			before := stub.count(http.MethodGet, admissionPath)
			code, body := staffRefund(t, s, f.organizer, f.replacementOrder, key)
			if code != http.StatusBadGateway || body["error"] != "refund unresolved" {
				t.Fatalf("staff refund = %d %v, want 502 refund unresolved", code, body)
			}
			if n := stub.count(http.MethodPost, "/payments/internal/psp/partial-refund"); n != 0 {
				t.Fatalf("the refund reached payments %d time(s) with no issued replacement ticket", n)
			}
			if n := stub.count(http.MethodGet, admissionPath) - before; n != 1 {
				t.Fatalf("access was asked %d time(s) about the replacement, want 1", n)
			}
			for _, c := range stub.recorded() {
				if c.path == admissionPath && c.organizer != f.organizer.String() {
					t.Fatalf("the admission read was scoped to organizer %q, want %s", c.organizer, f.organizer)
				}
			}
			if got := refundStatus(t, db, ctx, f.organizer, refundID); got != "pending" {
				t.Fatalf("refund status = %q, want pending", got)
			}
		})
	}

	// A switched replacement: Access reports its tickets, so the refund proceeds to payments.
	stub.set(f.quantity, http.StatusOK)
	if code, body := staffRefund(t, s, f.organizer, f.replacementOrder, key); code != http.StatusOK {
		t.Fatalf("staff refund of a switched replacement = %d %v, want 200", code, body)
	}
	var refunds []stubCall
	for _, c := range stub.recorded() {
		if c.path == "/payments/internal/psp/partial-refund" {
			refunds = append(refunds, c)
		}
	}
	if len(refunds) != 1 {
		t.Fatalf("payments received %d partial refunds, want 1", len(refunds))
	}
	b := refunds[0].body
	if b["organizer_id"] != f.organizer.String() || b["idempotency_key"] != "exchange:"+f.exchange.String() ||
		b["refund_key"] != refundID.String() {
		t.Fatalf("partial refund body = %v, want the replacement's key and the refund's own id", b)
	}
}

// After the abandonment reservation, the binder refuses both a fresh key and the replay of a
// refund bound before it. The staff answer is the existing 409 (D18), and nothing downstream
// is called.
func TestStaffRefundOfAReplacementAfterTheAbandonmentReservationAnswers409(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedSettledReplacement(t, db, ctx)
	stub := newDependencyStub(t)
	s := replacementRefundServer(t, db, stub)
	stub.set(0, http.StatusOK)
	const bound = "staff-replacement-bound"
	if code, _ := staffRefund(t, s, f.organizer, f.replacementOrder, bound); code != http.StatusBadGateway {
		t.Fatalf("first attempt = %d, want 502 with no issued tickets", code)
	}
	reserveReplacementAbandonment(t, db, ctx, f)
	calls := len(stub.recorded())

	for _, key := range []string{bound, "staff-replacement-fresh"} {
		code, body := staffRefund(t, s, f.organizer, f.replacementOrder, key)
		if code != http.StatusConflict || body["error"] != "only a completed order can be refunded" {
			t.Fatalf("key %s after the reservation = %d %v, want 409 only a completed order can be refunded", key, code, body)
		}
	}
	if n := len(stub.recorded()) - calls; n != 0 {
		t.Fatalf("a refused refund made %d dependency call(s)", n)
	}
	if n := countCommerceRows(t, ctx, db, `SELECT count(*) FROM order_refunds WHERE organizer_id=$1`, f.organizer); n != 1 {
		t.Fatalf("%d refund rows, want only the one bound before the reservation", n)
	}
}

// The TKT-586 schedule (plan v2 Verification, replacement refunds). A staff refund binds before
// the abandonment, then pauses at its first dispatch call. The abandonment reserves. Dispatch
// resumes with payments able to refund the alias. Access still reports no replacement ticket,
// so the refund stops before payments: removing the issuance guard makes the paused call the
// payments POST itself, and this test then sees it.
func TestAReplacementRefundPausedAfterBindingStopsBeforePaymentsOnceAbandonmentReserves(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedSettledReplacement(t, db, ctx)
	stub := newDependencyStub(t)
	s := replacementRefundServer(t, db, stub)
	stub.set(0, http.StatusOK)
	const key = "staff-replacement-paused"

	arrived, release := stub.pauseNext()
	defer release()
	type answer struct {
		code int
		body map[string]any
	}
	done := make(chan answer, 1)
	go func() {
		code, body := staffRefund(t, s, f.organizer, f.replacementOrder, key)
		done <- answer{code, body}
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the refund never reached its first dispatch call")
	}
	if got := refundStatus(t, db, ctx, f.organizer, commercestore.RefundID(f.organizer, key)); got != "pending" {
		t.Fatalf("refund status at the pause = %q, want a bound pending refund", got)
	}
	reserveReplacementAbandonment(t, db, ctx, f)
	release()

	var a answer
	select {
	case a = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the paused refund never answered")
	}
	if n := stub.count(http.MethodPost, "/payments/internal/psp/partial-refund"); n != 0 {
		t.Fatalf("the paused replacement refund reached payments %d time(s) after the abandonment reserved", n)
	}
	if a.code != http.StatusBadGateway || a.body["error"] != "refund unresolved" {
		t.Fatalf("paused refund answered %d %v, want 502 refund unresolved", a.code, a.body)
	}
	if got := refundStatus(t, db, ctx, f.organizer, commercestore.RefundID(f.organizer, key)); got != "pending" {
		t.Fatalf("refund status = %q, want pending", got)
	}
}
