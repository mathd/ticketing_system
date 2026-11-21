//go:build smoke

package refundoutcome

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
)

// TKT-525 S2 through the command coordinator, against real PostgreSQL and a STATEFUL payments
// peer that keeps the abandonment marker the way payments does: a marker committed by one
// request answers every later abandon as a replay and refuses every later failure proof.
//
// The order the plan requires is reserve, call, finish: Commerce commits an exclusive
// reservation, then payments commits its marker, then Commerce appends the outcome and
// finalizes the reservation. Each case asserts the exact Commerce rows and the quantity
// ceiling at each step, so a coordinator that called payments before its reservation
// committed, or released quantity before payments answered, is observed. The recovery through
// REAL payments is in realpayments_smoke_test.go.

type abandonMode int

const (
	abandonOK       abandonMode = iota // commit the marker, answer 200
	abandonDrop                        // commit the marker, lose the response
	abandonConflict                    // conclusive 409, no marker
	abandonNotFound                    // conclusive 404, no marker
	abandonBadMoney                    // commit the marker, answer 200 for other money
)

const peerProviderRef = "re_failed_1"

type statefulPayments struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []string
	bodies   []map[string]any
	mode     abandonMode
	marker   string
	ref      string
	amount   int64
	// steps answer the next abandon POSTs in order; mode answers the rest.
	steps []func(w http.ResponseWriter)
	// proofHeld, when set, holds each failure-proof GET until proofRelease closes.
	proofHeld, proofRelease chan struct{}
}

func newStatefulPayments(t *testing.T, amount int64) *statefulPayments {
	t.Helper()
	p := &statefulPayments{amount: amount, ref: peerProviderRef}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		p.mu.Lock()
		p.requests = append(p.requests, r.Method+" "+r.URL.Path)
		p.bodies = append(p.bodies, body)
		var step func(http.ResponseWriter)
		if r.Method == http.MethodPost && len(p.steps) > 0 {
			step, p.steps = p.steps[0], p.steps[1:]
		}
		mode, held, release := p.mode, p.proofHeld, p.proofRelease
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /internal/psp/partial-refund/failure-proof":
			if held != nil {
				held <- struct{}{}
				<-release
			}
			if p.markerRef() != "" {
				w.WriteHeader(409)
				_, _ = w.Write([]byte(`{"error":"refund leg abandoned"}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"provider_ref":%q,"amount":%d,"currency":"EUR"}`, p.ref, amount)
		case "POST /internal/psp/partial-refund/abandon":
			if step != nil {
				step(w)
				return
			}
			p.answer(t, w, mode)
		default:
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"peer: unexpected route"}`))
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// answer is one abandon POST in a mode, the way payments answers it.
func (p *statefulPayments) answer(t *testing.T, w http.ResponseWriter, mode abandonMode) {
	switch mode {
	case abandonNotFound:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"refund leg not found"}`))
		return
	case abandonConflict:
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"provider evidence contradicts a failed refund"}`))
		return
	}
	p.commitMarker()
	switch mode {
	case abandonDrop:
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("peer cannot drop a response")
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	case abandonBadMoney:
		_, _ = fmt.Fprintf(w, `{"provider_ref":%q,"amount":%d,"currency":"EUR"}`, p.ref, p.amount-1)
	default:
		_, _ = fmt.Fprintf(w, `{"provider_ref":%q,"amount":%d,"currency":"EUR"}`, p.ref, p.amount)
	}
}

func (p *statefulPayments) commitMarker() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.marker == "" {
		p.marker = p.ref
	}
}

func (p *statefulPayments) setMode(m abandonMode) {
	p.mu.Lock()
	p.mode = m
	p.mu.Unlock()
}

func (p *statefulPayments) script(steps ...func(http.ResponseWriter)) {
	p.mu.Lock()
	p.steps = append(p.steps, steps...)
	p.mu.Unlock()
}

func (p *statefulPayments) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.requests...)
}

func (p *statefulPayments) count(route string) int {
	n := 0
	for _, r := range p.seen() {
		if r == route {
			n++
		}
	}
	return n
}

func (p *statefulPayments) markerRef() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.marker
}

// gate holds a peer request until the test releases it. Cleanup also releases it, and runs
// before the peer server closes (cleanups run last-in first-out), so a failed assertion cannot
// leave a handler blocked and the server's Close waiting for it.
func gate(t *testing.T) (chan struct{}, func()) {
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return ch, release
}

const (
	abandonRoute = "POST /internal/psp/partial-refund/abandon"
	proofRoute   = "GET /internal/psp/partial-refund/failure-proof"
)

func abandonRequest(s seeded, refund uuid.UUID) store.RefundOutcomeRequest {
	return store.RefundOutcomeRequest{
		OrganizerID: s.org, RefundID: refund, Kind: store.OutcomeAbandoned,
		OperatorName: "Ada Operator", Reason: "buyer agreed nobody will refund this attempt",
	}
}

func abandonService(db *sql.DB, p *statefulPayments) *AbandonService {
	return NewAbandon(db, NewHTTPAbandoner(p.srv.URL, "payments-token", 2*time.Second))
}

func outsideService(db *sql.DB, p *statefulPayments) *Service {
	return New(db, NewHTTPPayments(p.srv.URL, "payments-token", 2*time.Second))
}

// quantityFree reports whether `quantity` more units fit the order's quantity ceiling. Probing
// inside a rolled-back transaction is not possible through BindOrderRefund, so the probe binds
// and then deletes its own row.
func quantityFree(t *testing.T, ctx context.Context, db *sql.DB, s seeded, quantity int32) bool {
	t.Helper()
	key := "probe-" + uuid.NewString()
	r, err := store.BindOrderRefund(ctx, db, store.RefundRequest{
		OrderID: s.order, OrganizerID: s.org, Quantity: quantity, IdempotencyKey: key,
		Actor: "probe@example.test", Reason: "ceiling probe",
	})
	if errors.Is(err, store.ErrRefundExceedsOrder) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM order_refunds WHERE organizer_id=$1 AND id=$2`, s.org, r.ID); err != nil {
		t.Fatal(err)
	}
	return true
}

// reservation is the refund's abandonment reservation as stored, or count 0.
type reservation struct {
	count            int
	state            string
	operator, reason string
}

func readReservation(t *testing.T, ctx context.Context, db *sql.DB, s seeded, refund uuid.UUID) reservation {
	t.Helper()
	var r reservation
	if err := db.QueryRowContext(ctx, `
		SELECT count(*), coalesce(max(state),''), coalesce(max(operator_name),''), coalesce(max(reason),'')
		FROM order_refund_abandonment_dispositions WHERE organizer_id=$1 AND refund_id=$2`, s.org, refund).
		Scan(&r.count, &r.state, &r.operator, &r.reason); err != nil {
		t.Fatal(err)
	}
	return r
}

func pendingReservation(in store.RefundOutcomeRequest) reservation {
	return reservation{1, store.DispositionPending, in.OperatorName, in.Reason}
}

func finalizedReservation(in store.RefundOutcomeRequest) reservation {
	return reservation{1, store.DispositionFinalized, in.OperatorName, in.Reason}
}

var untouched = refundState{"pending", sql.NullString{}, 0, 0, 0, 0, "none"}

// COS 3: Commerce reserves, payments marks and returns the provider's reference, Commerce
// appends the outcome with it and finalizes the reservation, keeps the refund pending and the
// projection untouched, and releases the refund's quantity. A replay answers from Commerce
// alone.
func TestAbandonedRecordsAfterPaymentsMarksTheLeg(t *testing.T) {
	db, ctx := outcomeDB(t)
	s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
	r := bindStaffRefund(t, ctx, db, s, "abandon-1", 2)
	peer := newStatefulPayments(t, 2000)
	svc := abandonService(db, peer)
	in := abandonRequest(s, r.ID)

	got, err := svc.RecordAbandoned(ctx, in)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if got.Replay || got.Outcome.Kind != store.OutcomeAbandoned || got.Outcome.ExternalRef != peerProviderRef ||
		got.Outcome.Amount != 2000 || got.Outcome.Currency != "EUR" || got.Proof.ProviderRef != peerProviderRef {
		t.Fatalf("result = %+v", got)
	}
	if req := peer.seen(); len(req) != 1 || req[0] != abandonRoute {
		t.Fatalf("payments requests = %v, want one abandon", req)
	}
	body := peer.bodies[0]
	if body["organizer_id"] != s.org.String() || body["idempotency_key"] != s.chargeKey || body["refund_key"] != r.ID.String() ||
		body["amount"] != float64(2000) || body["currency"] != "EUR" || len(body) != 5 {
		t.Fatalf("abandon body = %v, want the stored identities and money", body)
	}
	if st := readRefundState(t, ctx, db, s, r.ID); st != (refundState{"pending", sql.NullString{}, 1, 0, 0, 0, "none"}) {
		t.Fatalf("after abandon: %+v", st)
	}
	if got := readReservation(t, ctx, db, s, r.ID); got != finalizedReservation(in) {
		t.Fatalf("reservation = %+v, want finalized with the command's attribution", got)
	}
	if !quantityFree(t, ctx, db, s, 2) {
		t.Fatal("the abandoned refund's two units are still reserved")
	}

	again, err := svc.RecordAbandoned(ctx, in)
	if err != nil || !again.Replay || !again.Outcome.RecordedAt.Equal(got.Outcome.RecordedAt) || again.Outcome.ExternalRef != peerProviderRef {
		t.Fatalf("exact repeat: %+v err=%v", again, err)
	}
	other := in
	other.Reason = "a different reason"
	if _, err := svc.RecordAbandoned(ctx, other); !errors.Is(err, store.ErrRefundOutcomeConflict) {
		t.Fatalf("different repeat: err = %v, want conflict", err)
	}
	if req := peer.seen(); len(req) != 1 {
		t.Fatalf("payments requests after repeats = %v, want still one", req)
	}
}

// A conclusive refusal describes ONE abandon request. The reservation stays pending, so the
// quantity stays reserved and an outside completion stays refused, with no payments call. The
// same command run again finishes once payments can prove the failure.
func TestAbandonedRefusalKeepsTheReservationPending(t *testing.T) {
	for name, mode := range map[string]abandonMode{"404": abandonNotFound, "409": abandonConflict} {
		t.Run(name, func(t *testing.T) {
			db, ctx := outcomeDB(t)
			s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
			r := bindStaffRefund(t, ctx, db, s, "abandon-refused", 2)
			peer := newStatefulPayments(t, 2000)
			peer.setMode(mode)
			in := abandonRequest(s, r.ID)
			if _, err := abandonService(db, peer).RecordAbandoned(ctx, in); !errors.Is(err, ErrAbandonRefused) {
				t.Fatalf("err = %v, want ErrAbandonRefused", err)
			}
			if got := readReservation(t, ctx, db, s, r.ID); got != pendingReservation(in) {
				t.Fatalf("reservation after the refusal = %+v, want pending", got)
			}
			if st := readRefundState(t, ctx, db, s, r.ID); st != untouched {
				t.Fatalf("after the refusal: %+v", st)
			}
			if quantityFree(t, ctx, db, s, 1) {
				t.Fatal("a refused abandonment released quantity")
			}
			if _, err := outsideService(db, peer).RecordRefundedOutside(ctx, outsideRequest(s, r.ID)); !errors.Is(err, store.ErrAbandonmentReserved) {
				t.Fatalf("outside completion over the pending reservation: err = %v, want ErrAbandonmentReserved", err)
			}
			if n := peer.count(proofRoute); n != 0 {
				t.Fatalf("the refused outside completion read the proof %d times", n)
			}

			peer.setMode(abandonOK)
			if got, err := abandonService(db, peer).RecordAbandoned(ctx, in); err != nil || got.Replay {
				t.Fatalf("the same command after the refusal cleared: %+v err=%v", got, err)
			}
			if got := readReservation(t, ctx, db, s, r.ID); got != finalizedReservation(in) {
				t.Fatalf("reservation after the finish = %+v", got)
			}
		})
	}
}

// P4-1: a reservation committed with no payments call (a crash, or a commit error the caller
// could not tell apart). The outside completion refuses before its proof read: zero GETs, no
// outcome and no projection change.
func TestRefundedOutsideRefusesAPendingReservationBeforeTheProof(t *testing.T) {
	db, ctx := outcomeDB(t)
	s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
	r := bindStaffRefund(t, ctx, db, s, "outside-after-reservation", 2)
	in := abandonRequest(s, r.ID)
	target, err := store.InspectRefundOutcomeTarget(ctx, db, s.org, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, existing, err := store.ReserveAbandonment(ctx, db, in, target); err != nil || existing != nil {
		t.Fatalf("reserve: existing=%v err=%v", existing, err)
	}
	peer := newStatefulPayments(t, 2000)
	if _, err := outsideService(db, peer).RecordRefundedOutside(ctx, outsideRequest(s, r.ID)); !errors.Is(err, store.ErrAbandonmentReserved) {
		t.Fatalf("err = %v, want ErrAbandonmentReserved", err)
	}
	if req := peer.seen(); len(req) != 0 {
		t.Fatalf("payments requests = %v, want none", req)
	}
	if st := readRefundState(t, ctx, db, s, r.ID); st != untouched {
		t.Fatalf("after the refusal: %+v", st)
	}
}

// injectCommitFailure makes the next Commerce commit that inserts a row into `table` for
// `refund` fail AT COMMIT, through a deferred constraint trigger. It returns the function that
// removes it. `column` names the table's refund column.
func injectCommitFailure(t *testing.T, ctx context.Context, db *sql.DB, table, column string, refund uuid.UUID) func() {
	t.Helper()
	name := "tkt525_fail_" + strings.ReplaceAll(refund.String(), "-", "")
	for _, stmt := range []string{
		`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN IF NEW.` + column + ` = '` + refund.String() + `' THEN RAISE EXCEPTION 'injected commit failure'; END IF; RETURN NEW; END $$`,
		`CREATE CONSTRAINT TRIGGER ` + name + ` AFTER INSERT ON ` + table + ` DEFERRABLE INITIALLY DEFERRED
		 FOR EACH ROW EXECUTE FUNCTION ` + name + `()`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	removed := false
	remove := func() {
		if removed {
			return
		}
		removed = true
		_, _ = db.Exec(`DROP TRIGGER ` + name + ` ON ` + table)
		_, _ = db.Exec(`DROP FUNCTION ` + name + `()`)
	}
	t.Cleanup(remove)
	return remove
}

// P4-2: the reservation must be durable before the abandon POST. A commit failure on the
// reservation transaction stops the command before any HTTP call, with no reservation. An
// exact retry then inserts exactly one pending reservation, keeps it through a refusal, and a
// third run finishes it.
func TestAbandonedStopsBeforePaymentsWhenTheReservationCommitFails(t *testing.T) {
	db, ctx := outcomeDB(t)
	s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
	r := bindStaffRefund(t, ctx, db, s, "reserve-commit-fails", 2)
	peer := newStatefulPayments(t, 2000)
	svc := abandonService(db, peer)
	in := abandonRequest(s, r.ID)

	remove := injectCommitFailure(t, ctx, db, "order_refund_abandonment_dispositions", "refund_id", r.ID)
	if _, err := svc.RecordAbandoned(ctx, in); err == nil || !strings.Contains(err.Error(), "injected commit failure") {
		t.Fatalf("err = %v, want the injected commit failure", err)
	}
	if req := peer.seen(); len(req) != 0 {
		t.Fatalf("payments requests after a failed reservation commit = %v, want none", req)
	}
	if got := readReservation(t, ctx, db, s, r.ID); got.count != 0 {
		t.Fatalf("reservation after the failed commit = %+v, want none", got)
	}
	remove()

	peer.setMode(abandonConflict)
	if _, err := svc.RecordAbandoned(ctx, in); !errors.Is(err, ErrAbandonRefused) {
		t.Fatalf("retry: err = %v, want the scripted refusal", err)
	}
	if got := readReservation(t, ctx, db, s, r.ID); got != pendingReservation(in) {
		t.Fatalf("reservation after the retry = %+v, want exactly one pending", got)
	}
	peer.setMode(abandonOK)
	if _, err := svc.RecordAbandoned(ctx, in); err != nil {
		t.Fatalf("final run: %v", err)
	}
	if got := readReservation(t, ctx, db, s, r.ID); got != finalizedReservation(in) {
		t.Fatalf("reservation after the finish = %+v", got)
	}
	if n := peer.count(abandonRoute); n != 2 {
		t.Fatalf("abandon POSTs = %d, want 2 (none for the failed reservation)", n)
	}
}

// Two abandon requests overlap on one reservation. The first is refused; the second commits the
// marker and its 200 is held. During that gap: one pending reservation, one marker, no outcome,
// and an outside completion is refused. The held 200 is then lost, and the original command
// finishes from the marker with the original attribution.
func TestOverlappingAbandonRequestsKeepTheReservation(t *testing.T) {
	db, ctx := outcomeDB(t)
	s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
	r := bindStaffRefund(t, ctx, db, s, "abandon-overlap", 2)
	peer := newStatefulPayments(t, 2000)
	in := abandonRequest(s, r.ID)

	firstArrived, committed := make(chan struct{}), make(chan struct{})
	firstRelease, releaseFirst := gate(t)
	secondRelease, releaseSecond := gate(t)
	peer.script(
		func(w http.ResponseWriter) { // the first request: its proof is refused
			close(firstArrived)
			<-firstRelease
			peer.answer(t, w, abandonConflict)
		},
		func(w http.ResponseWriter) { // the second request: marker committed, 200 held, then lost
			peer.commitMarker()
			close(committed)
			<-secondRelease
			peer.answer(t, w, abandonDrop)
		},
	)
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := abandonService(db, peer).RecordAbandoned(ctx, in); first <- err }()
	<-firstArrived
	go func() { _, err := abandonService(db, peer).RecordAbandoned(ctx, in); second <- err }()
	<-committed
	releaseFirst()
	if err := <-first; !errors.Is(err, ErrAbandonRefused) {
		t.Fatalf("first request: err = %v, want ErrAbandonRefused", err)
	}

	if got := readReservation(t, ctx, db, s, r.ID); got != pendingReservation(in) {
		t.Fatalf("reservation during the gap = %+v, want one pending", got)
	}
	if peer.markerRef() == "" {
		t.Fatal("fixture: the second request did not commit the marker")
	}
	if st := readRefundState(t, ctx, db, s, r.ID); st != untouched {
		t.Fatalf("Commerce during the gap: %+v", st)
	}
	if _, err := outsideService(db, peer).RecordRefundedOutside(ctx, outsideRequest(s, r.ID)); !errors.Is(err, store.ErrAbandonmentReserved) {
		t.Fatalf("outside completion during the gap: err = %v, want ErrAbandonmentReserved", err)
	}

	releaseSecond()
	if err := <-second; !errors.Is(err, ErrAbandonUncertain) {
		t.Fatalf("second request: err = %v, want ErrAbandonUncertain", err)
	}
	got, err := abandonService(db, peer).RecordAbandoned(ctx, in)
	if err != nil || got.Replay || got.Outcome.OperatorName != in.OperatorName || got.Outcome.Reason != in.Reason || got.Outcome.ExternalRef != peerProviderRef {
		t.Fatalf("retry of the original command = %+v err=%v", got, err)
	}
	if got := readReservation(t, ctx, db, s, r.ID); got != finalizedReservation(in) {
		t.Fatalf("reservation after the retry = %+v, want one finalized", got)
	}
	if st := readRefundState(t, ctx, db, s, r.ID); st.outcomes != 1 || st.status != "pending" {
		t.Fatalf("after the retry: %+v", st)
	}
}

// Commit order 1: the abandonment reserves while an outside completion's proof is in flight.
// The outside write rechecks under its locks and refuses; the abandonment then finishes once.
func TestAbandonmentReservedDuringAnOutsideProofWins(t *testing.T) {
	db, ctx := outcomeDB(t)
	s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
	r := bindStaffRefund(t, ctx, db, s, "order-one", 2)
	peer := newStatefulPayments(t, 2000)
	var releaseProof func()
	peer.proofHeld = make(chan struct{}, 1)
	peer.proofRelease, releaseProof = gate(t)
	postArrived := make(chan struct{})
	postRelease, releasePost := gate(t)
	peer.script(func(w http.ResponseWriter) {
		close(postArrived)
		<-postRelease
		peer.answer(t, w, abandonOK)
	})
	in := abandonRequest(s, r.ID)

	outside, abandon := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := outsideService(db, peer).RecordRefundedOutside(ctx, outsideRequest(s, r.ID))
		outside <- err
	}()
	<-peer.proofHeld
	go func() { _, err := abandonService(db, peer).RecordAbandoned(ctx, in); abandon <- err }()
	<-postArrived
	if got := readReservation(t, ctx, db, s, r.ID); got != pendingReservation(in) {
		t.Fatalf("reservation before the abandon POST = %+v, want pending", got)
	}

	releaseProof()
	if err := <-outside; !errors.Is(err, store.ErrAbandonmentReserved) {
		t.Fatalf("outside completion: err = %v, want ErrAbandonmentReserved", err)
	}
	if st := readRefundState(t, ctx, db, s, r.ID); st != untouched {
		t.Fatalf("after the refused outside completion: %+v", st)
	}
	releasePost()
	if err := <-abandon; err != nil {
		t.Fatalf("abandonment: %v", err)
	}
	if st := readRefundState(t, ctx, db, s, r.ID); st != (refundState{"pending", sql.NullString{}, 1, 0, 0, 0, "none"}) {
		t.Fatalf("after the abandonment: %+v", st)
	}
	if got := readReservation(t, ctx, db, s, r.ID); got != finalizedReservation(in) {
		t.Fatalf("reservation = %+v", got)
	}
}

// waitForLockWaiters waits until n backends in this database wait on a lock.
func waitForLockWaiters(t *testing.T, ctx context.Context, db *sql.DB, n int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d backends waited on a lock, want %d", waiting, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// R1-4: payments accepts a provider reference up to 255 bytes. The response for one is lost,
// and the same command finishes from the marker and stores the whole reference.
func TestAbandonedRecoversWithAProviderReferenceLongerThan200Bytes(t *testing.T) {
	db, ctx := outcomeDB(t)
	s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 1, 1000)
	r := bindStaffRefund(t, ctx, db, s, "abandon-long-ref", 1)
	peer := newStatefulPayments(t, 1000)
	peer.ref = "re_" + strings.Repeat("l", 250-3)
	svc := abandonService(db, peer)
	peer.setMode(abandonDrop)
	if _, err := svc.RecordAbandoned(ctx, abandonRequest(s, r.ID)); !errors.Is(err, ErrAbandonUncertain) {
		t.Fatalf("lost response: err = %v, want ErrAbandonUncertain", err)
	}
	peer.setMode(abandonOK)
	got, err := svc.RecordAbandoned(ctx, abandonRequest(s, r.ID))
	if err != nil || got.Outcome.ExternalRef != peer.ref {
		t.Fatalf("finish = %+v err=%v, want the 250-byte reference stored", got, err)
	}
}

// A 200 for other money is not evidence about this refund: nothing is recorded, and the
// reservation stays pending.
func TestAbandonedRefusesEvidenceForOtherMoney(t *testing.T) {
	db, ctx := outcomeDB(t)
	s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
	r := bindStaffRefund(t, ctx, db, s, "abandon-money", 2)
	peer := newStatefulPayments(t, 2000)
	peer.setMode(abandonBadMoney)
	in := abandonRequest(s, r.ID)
	if _, err := abandonService(db, peer).RecordAbandoned(ctx, in); !errors.Is(err, ErrAbandonUncertain) {
		t.Fatalf("err = %v, want ErrAbandonUncertain", err)
	}
	if st := readRefundState(t, ctx, db, s, r.ID); st != untouched {
		t.Fatalf("after other money: %+v", st)
	}
	if got := readReservation(t, ctx, db, s, r.ID); got != pendingReservation(in) {
		t.Fatalf("reservation = %+v, want pending", got)
	}
}

// Answers Commerce gives alone never reach payments: missing and foreign refunds (one answer),
// an ordinarily completed refund, and a refund with another outcome.
func TestAbandonedAnswersLocallyWithoutAskingPayments(t *testing.T) {
	db, ctx := outcomeDB(t)
	s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 3, 1000)
	foreign := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 1, 1000)
	foreignRefund := bindStaffRefund(t, ctx, db, foreign, "foreign", 1)
	completed := bindStaffRefund(t, ctx, db, s, "completed", 1)
	if err := store.CompleteOrderRefund(ctx, db, s.org, completed.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	outside := bindStaffRefund(t, ctx, db, s, "outside", 1)
	proofPeer := newPaymentsPeer(t, 200, `{"provider_ref":"re_failed_1","amount":1000,"currency":"EUR"}`)
	if _, err := New(db, NewHTTPPayments(proofPeer.srv.URL, "t", time.Second)).RecordRefundedOutside(ctx, outsideRequest(s, outside.ID)); err != nil {
		t.Fatal(err)
	}
	peer := newStatefulPayments(t, 1000)
	svc := abandonService(db, peer)
	var missingErr, foreignErr error
	for name, c := range map[string]struct {
		refund uuid.UUID
		want   error
		got    *error
	}{
		"missing":   {uuid.New(), store.ErrRefundOutcomeNotFound, &missingErr},
		"foreign":   {foreignRefund.ID, store.ErrRefundOutcomeNotFound, &foreignErr},
		"completed": {completed.ID, store.ErrRefundOutcomeIneligible, nil},
		"outside":   {outside.ID, store.ErrRefundOutcomeConflict, nil},
	} {
		_, err := svc.RecordAbandoned(ctx, abandonRequest(s, c.refund))
		if !errors.Is(err, c.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, c.want)
		}
		if c.got != nil {
			*c.got = err
		}
	}
	if missingErr.Error() != foreignErr.Error() {
		t.Fatalf("a foreign refund answered %q and a missing one %q", foreignErr, missingErr)
	}
	if req := peer.seen(); len(req) != 0 {
		t.Fatalf("local answers reached payments: %v", req)
	}
}
