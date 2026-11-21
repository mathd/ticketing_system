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
// The order the plan requires is Payments first, then Commerce. Each recovery case below
// asserts the exact Commerce rows and the quantity ceiling after the failure AND after the
// recovery, so a coordinator that wrote Commerce first, or released quantity before payments
// answered, is observed.

type abandonMode int

const (
	abandonOK       abandonMode = iota // commit the marker, answer 200
	abandonDrop                        // commit the marker, lose the response
	abandonNotFound                    // conclusive 404, no marker
	abandonConflict                    // conclusive 409, no marker
	abandonBadMoney                    // commit the marker, answer 200 for other money
)

type statefulPayments struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []string
	bodies   []map[string]any
	mode     abandonMode
	marker   string
	amount   int64
}

func newStatefulPayments(t *testing.T, amount int64) *statefulPayments {
	t.Helper()
	p := &statefulPayments{amount: amount}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		p.mu.Lock()
		p.requests = append(p.requests, r.Method+" "+r.URL.Path)
		p.bodies = append(p.bodies, body)
		mode := p.mode
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /internal/psp/partial-refund/failure-proof":
			p.mu.Lock()
			marked := p.marker != ""
			p.mu.Unlock()
			if marked {
				w.WriteHeader(409)
				_, _ = w.Write([]byte(`{"error":"refund leg abandoned"}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"provider_ref":"re_failed_1","amount":%d,"currency":"EUR"}`, amount)
		case "POST /internal/psp/partial-refund/abandon":
			switch mode {
			case abandonNotFound:
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"error":"refund leg not found"}`))
				return
			case abandonConflict:
				w.WriteHeader(409)
				_, _ = w.Write([]byte(`{"error":"refund leg completed"}`))
				return
			}
			p.mu.Lock()
			if p.marker == "" {
				p.marker = "re_failed_1"
			}
			p.mu.Unlock()
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
				_, _ = fmt.Fprintf(w, `{"provider_ref":"re_failed_1","amount":%d,"currency":"EUR"}`, amount-1)
			default:
				_, _ = fmt.Fprintf(w, `{"provider_ref":"re_failed_1","amount":%d,"currency":"EUR"}`, amount)
			}
		default:
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"peer: unexpected route"}`))
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *statefulPayments) setMode(m abandonMode) {
	p.mu.Lock()
	p.mode = m
	p.mu.Unlock()
}

func (p *statefulPayments) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.requests...)
}

func (p *statefulPayments) markerRef() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.marker
}

func abandonRequest(s seeded, refund uuid.UUID) store.RefundOutcomeRequest {
	return store.RefundOutcomeRequest{
		OrganizerID: s.org, RefundID: refund, Kind: store.OutcomeAbandoned,
		OperatorName: "Ada Operator", Reason: "buyer agreed nobody will refund this attempt",
	}
}

func abandonService(db *sql.DB, p *statefulPayments) *AbandonService {
	return NewAbandon(db, NewHTTPAbandoner(p.srv.URL, "payments-token", time.Second))
}

// quantityReserved reports whether the order's quantity ceiling still counts the refund: one
// more unit than the remaining allowance must be refused, probing inside a rolled-back
// transaction is not possible through BindOrderRefund, so the probe binds and then deletes its
// own row.
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

func countOutcomes(t *testing.T, ctx context.Context, db *sql.DB, s seeded, refund uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM refund_outcomes WHERE organizer_id=$1 AND subject_id=$2`, s.org, refund).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// COS 3: payments marks first and returns the provider's reference; Commerce appends the
// outcome with it, keeps the refund pending and the projection untouched, and releases the
// refund's quantity. A replay answers from Commerce alone.
func TestAbandonedRecordsAfterPaymentsMarksTheLeg(t *testing.T) {
	db, ctx := outcomeDB(t)
	s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
	r := bindStaffRefund(t, ctx, db, s, "abandon-1", 2)
	peer := newStatefulPayments(t, 2000)
	svc := abandonService(db, peer)

	got, err := svc.RecordAbandoned(ctx, abandonRequest(s, r.ID))
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if got.Replay || got.Outcome.Kind != store.OutcomeAbandoned || got.Outcome.ExternalRef != "re_failed_1" ||
		got.Outcome.Amount != 2000 || got.Outcome.Currency != "EUR" || got.Proof.ProviderRef != "re_failed_1" {
		t.Fatalf("result = %+v", got)
	}
	if req := peer.seen(); len(req) != 1 || req[0] != "POST /internal/psp/partial-refund/abandon" {
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
	if !quantityFree(t, ctx, db, s, 2) {
		t.Fatal("the abandoned refund's two units are still reserved")
	}

	again, err := svc.RecordAbandoned(ctx, abandonRequest(s, r.ID))
	if err != nil || !again.Replay || !again.Outcome.RecordedAt.Equal(got.Outcome.RecordedAt) || again.Outcome.ExternalRef != "re_failed_1" {
		t.Fatalf("exact repeat: %+v err=%v", again, err)
	}
	other := abandonRequest(s, r.ID)
	other.Reason = "a different reason"
	if _, err := svc.RecordAbandoned(ctx, other); !errors.Is(err, store.ErrRefundOutcomeConflict) {
		t.Fatalf("different repeat: err = %v, want conflict", err)
	}
	if req := peer.seen(); len(req) != 1 {
		t.Fatalf("payments requests after repeats = %v, want still one", req)
	}
}

// A conclusive payments refusal leaves both services without a new record. The refund stays
// available for its next valid disposition: here, the outside completion that follows.
func TestAbandonedConclusiveRefusalLeavesTheRefundAvailable(t *testing.T) {
	for name, mode := range map[string]abandonMode{"404": abandonNotFound, "409": abandonConflict} {
		t.Run(name, func(t *testing.T) {
			db, ctx := outcomeDB(t)
			s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
			r := bindStaffRefund(t, ctx, db, s, "abandon-refused", 2)
			peer := newStatefulPayments(t, 2000)
			peer.setMode(mode)
			if _, err := abandonService(db, peer).RecordAbandoned(ctx, abandonRequest(s, r.ID)); !errors.Is(err, ErrAbandonRefused) {
				t.Fatalf("err = %v, want ErrAbandonRefused", err)
			}
			if peer.markerRef() != "" || countOutcomes(t, ctx, db, s, r.ID) != 0 {
				t.Fatalf("a conclusive refusal left a record: marker=%q outcomes=%d", peer.markerRef(), countOutcomes(t, ctx, db, s, r.ID))
			}
			if quantityFree(t, ctx, db, s, 1) {
				t.Fatal("a refused abandonment released quantity")
			}
			if _, err := New(db, NewHTTPPayments(peer.srv.URL, "t", time.Second)).RecordRefundedOutside(ctx, outsideRequest(s, r.ID)); err != nil {
				t.Fatalf("the next valid disposition was refused: %v", err)
			}
			if st := readRefundState(t, ctx, db, s, r.ID); st.status != "completed" || st.outcomes != 1 {
				t.Fatalf("after the outside completion: %+v", st)
			}
		})
	}
}

// injectCommitFailure makes the next Commerce commit that carries an outcome for `refund` fail
// AT COMMIT, through a deferred constraint trigger. It returns the function that removes it.
func injectCommitFailure(t *testing.T, ctx context.Context, db *sql.DB, refund uuid.UUID) func() {
	t.Helper()
	name := "tkt525_fail_" + strings.ReplaceAll(refund.String(), "-", "")
	for _, stmt := range []string{
		`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN IF NEW.subject_id = '` + refund.String() + `' THEN RAISE EXCEPTION 'injected commit failure'; END IF; RETURN NEW; END $$`,
		`CREATE CONSTRAINT TRIGGER ` + name + ` AFTER INSERT ON refund_outcomes DEFERRABLE INITIALLY DEFERRED
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
		_, _ = db.Exec(`DROP TRIGGER ` + name + ` ON refund_outcomes`)
		_, _ = db.Exec(`DROP FUNCTION ` + name + `()`)
	}
	t.Cleanup(remove)
	return remove
}

// Pre-mortem 2: the response is lost after payments released its allowance. Commerce records
// nothing and keeps the quantity reserved; an outside completion is refused because payments'
// proof sees the marker; a Commerce commit failure on the retry still leaves nothing; and the
// same command then finishes from the stored marker.
func TestAbandonedRecoversFromALostResponseAndACommitFailure(t *testing.T) {
	db, ctx := outcomeDB(t)
	s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
	r := bindStaffRefund(t, ctx, db, s, "abandon-lost", 2)
	peer := newStatefulPayments(t, 2000)
	svc := abandonService(db, peer)

	peer.setMode(abandonDrop)
	if _, err := svc.RecordAbandoned(ctx, abandonRequest(s, r.ID)); !errors.Is(err, ErrAbandonUncertain) {
		t.Fatalf("lost response: err = %v, want ErrAbandonUncertain", err)
	}
	if peer.markerRef() == "" {
		t.Fatal("fixture: the peer did not commit its marker")
	}
	if n := countOutcomes(t, ctx, db, s, r.ID); n != 0 {
		t.Fatalf("Commerce recorded %d outcomes on a lost payments response", n)
	}
	if quantityFree(t, ctx, db, s, 1) {
		t.Fatal("quantity was released before payments' answer was known")
	}

	// S1 after the marker commit: payments refuses the proof, Commerce stays as it was.
	if _, err := New(db, NewHTTPPayments(peer.srv.URL, "t", time.Second)).RecordRefundedOutside(ctx, outsideRequest(s, r.ID)); !errors.Is(err, ErrProofRefused) {
		t.Fatalf("outside completion after the marker: err = %v, want ErrProofRefused", err)
	}
	if st := readRefundState(t, ctx, db, s, r.ID); st != (refundState{"pending", sql.NullString{}, 0, 0, 0, 0, "none"}) {
		t.Fatalf("after the refused outside completion: %+v", st)
	}

	// The retry reaches payments (a replay there) and fails at the Commerce commit.
	peer.setMode(abandonOK)
	remove := injectCommitFailure(t, ctx, db, r.ID)
	if _, err := svc.RecordAbandoned(ctx, abandonRequest(s, r.ID)); err == nil {
		t.Fatal("the injected commit failure was not reported")
	}
	if n := countOutcomes(t, ctx, db, s, r.ID); n != 0 {
		t.Fatalf("outcomes after the failed commit = %d", n)
	}
	if quantityFree(t, ctx, db, s, 1) {
		t.Fatal("quantity was released by a commit that failed")
	}
	remove()

	got, err := svc.RecordAbandoned(ctx, abandonRequest(s, r.ID))
	if err != nil || got.Replay || got.Outcome.ExternalRef != "re_failed_1" {
		t.Fatalf("final retry: %+v err=%v", got, err)
	}
	if st := readRefundState(t, ctx, db, s, r.ID); st != (refundState{"pending", sql.NullString{}, 1, 0, 0, 0, "none"}) {
		t.Fatalf("after recovery: %+v", st)
	}
	if !quantityFree(t, ctx, db, s, 2) {
		t.Fatal("the recovered abandonment did not release the quantity")
	}
	want := []string{"POST /internal/psp/partial-refund/abandon", "GET /internal/psp/partial-refund/failure-proof",
		"POST /internal/psp/partial-refund/abandon", "POST /internal/psp/partial-refund/abandon"}
	if got := peer.seen(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("payments requests = %v, want %v", got, want)
	}
}

// A 200 for other money is not evidence about this refund: nothing is recorded.
func TestAbandonedRefusesEvidenceForOtherMoney(t *testing.T) {
	db, ctx := outcomeDB(t)
	s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
	r := bindStaffRefund(t, ctx, db, s, "abandon-money", 2)
	peer := newStatefulPayments(t, 2000)
	peer.setMode(abandonBadMoney)
	if _, err := abandonService(db, peer).RecordAbandoned(ctx, abandonRequest(s, r.ID)); !errors.Is(err, ErrAbandonUncertain) {
		t.Fatalf("err = %v, want ErrAbandonUncertain", err)
	}
	if n := countOutcomes(t, ctx, db, s, r.ID); n != 0 {
		t.Fatalf("outcomes = %d", n)
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
