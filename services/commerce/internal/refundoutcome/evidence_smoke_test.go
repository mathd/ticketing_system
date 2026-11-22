//go:build smoke

package refundoutcome

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
)

// TKT-594 COS 1 and 4, against real PostgreSQL and through the real clients. A redirect or an
// answer that is not exactly payments' evidence changes nothing in Commerce: an outside
// completion records nothing, and an abandonment keeps its reservation pending and its
// quantity counted. The same command then finishes when payments answers properly.
//
// Each refused answer would otherwise be accepted: the redirect target answers evidence for the
// refund's own money, and each malformed body carries that money where a loose decoder reads it.
//
// Each case reads the persisted Commerce state even when the error differs from the expected
// one, so a change that both returns another error and writes to Commerce shows both failures.

// switchablePayments answers the failure proof with the current handler and counts requests.
type switchablePayments struct {
	srv      *httptest.Server
	mu       sync.Mutex
	handler  http.HandlerFunc
	requests int
}

func newSwitchablePayments(t *testing.T, h http.HandlerFunc) *switchablePayments {
	t.Helper()
	p := &switchablePayments{handler: h}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.requests++
		h := p.handler
		p.mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/internal/psp/partial-refund/failure-proof" {
			w.WriteHeader(500)
			return
		}
		h(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *switchablePayments) set(h http.HandlerFunc) {
	p.mu.Lock()
	p.handler = h
	p.mu.Unlock()
}

func (p *switchablePayments) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests
}

func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func redirectTo(target string, status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target+r.URL.RequestURI())
		w.WriteHeader(status)
	}
}

// Malformed evidence for 2000 EUR with the stored reference. A decoder that keeps the last
// duplicate, matches names without regard to case, or ignores the reference limit would accept
// each one.
func malformedEvidence2000(ref string) map[string]string {
	return map[string]string{
		"duplicate amount":       fmt.Sprintf(`{"provider_ref":%q,"amount":1999,"amount":2000,"currency":"EUR"}`, ref),
		"case-variant currency":  fmt.Sprintf(`{"provider_ref":%q,"amount":2000,"Currency":"EUR"}`, ref),
		"256-byte reference":     fmt.Sprintf(`{"provider_ref":%q,"amount":2000,"currency":"EUR"}`, "re_"+strings.Repeat("x", 253)),
		"null reference":         `{"provider_ref":null,"amount":2000,"currency":"EUR"}`,
		"trailing second answer": fmt.Sprintf(`{"provider_ref":%q,"amount":2000,"currency":"EUR"}{"provider_ref":%q,"amount":2000,"currency":"EUR"}`, ref, ref),
	}
}

func TestRefundedOutsideRecordsNothingOnARedirectOrInvalidProof(t *testing.T) {
	type refusal struct {
		first  func(target string) http.HandlerFunc
		detail string
	}
	cases := map[string]refusal{
		"302 to another host": {func(target string) http.HandlerFunc { return redirectTo(target, http.StatusFound) },
			"refund failure proof unavailable: payments redirect refused (HTTP 302)"},
		"307 to another host": {func(target string) http.HandlerFunc { return redirectTo(target, http.StatusTemporaryRedirect) },
			"refund failure proof unavailable: payments redirect refused (HTTP 307)"},
	}
	for name, body := range malformedEvidence2000("re_failed_1") {
		cases[name] = refusal{func(string) http.HandlerFunc { return answer(200, body) },
			"refund failure proof unavailable: payments returned invalid refund evidence"}
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			db, ctx := outcomeDB(t)
			s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
			r := bindStaffRefund(t, ctx, db, s, "outside-refused-594", 2)
			target := newRedirectTarget(t, proof2000)
			peer := newSwitchablePayments(t, c.first(target.url))
			svc := New(db, NewHTTPPayments(peer.srv.URL, "payments-token", 5*time.Second))

			_, err := svc.RecordRefundedOutside(ctx, outsideRequest(s, r.ID))
			if !errors.Is(err, ErrProofUnavailable) || err.Error() != c.detail {
				t.Errorf("err = %v, want %q", err, c.detail)
			}
			if seen := target.requests(); len(seen) != 0 {
				t.Errorf("the redirect target received %v, want nothing", seen)
			}
			if st := readRefundState(t, ctx, db, s, r.ID); st != untouched {
				t.Errorf("Commerce changed on a refused proof: %+v", st)
			}
			if t.Failed() {
				t.FailNow()
			}

			peer.set(answer(200, proof2000))
			got, err := svc.RecordRefundedOutside(ctx, outsideRequest(s, r.ID))
			if err != nil || got.Replay || got.Proof.ProviderRef != "re_failed_1" {
				t.Fatalf("the same command with a valid proof: %+v err=%v", got, err)
			}
			if st := readRefundState(t, ctx, db, s, r.ID); st != (refundState{"completed", sql.NullString{String: "refunded_outside", Valid: true}, 1, 0, 2, 2000, "full"}) {
				t.Fatalf("after the valid proof: %+v", st)
			}
			if n := peer.count(); n != 2 {
				t.Fatalf("payments proof reads = %d, want 2", n)
			}
		})
	}
}

func TestAbandonedStaysPendingOnARedirectOrInvalidAnswer(t *testing.T) {
	type refusal struct {
		// first answers the first abandon POST. marker reports whether payments committed its
		// marker before that answer.
		first  func(p *statefulPayments, target string) func(http.ResponseWriter)
		marker bool
		detail string
		// replay, when set, answers the second abandon POST. Otherwise the peer's default answer
		// does. wantRef is the reference the finished outcome records.
		replay  func(p *statefulPayments) func(http.ResponseWriter)
		wantRef string
	}
	cases := map[string]refusal{
		"307 to another host": {func(_ *statefulPayments, target string) func(http.ResponseWriter) {
			return func(w http.ResponseWriter) {
				w.Header().Set("Location", target+"/internal/psp/partial-refund/abandon")
				w.WriteHeader(http.StatusTemporaryRedirect)
			}
		}, false, "payments abandonment answer unknown: payments redirect refused (HTTP 307)", nil, peerProviderRef},
		"308 to another host": {func(_ *statefulPayments, target string) func(http.ResponseWriter) {
			return func(w http.ResponseWriter) {
				w.Header().Set("Location", target+"/internal/psp/partial-refund/abandon")
				w.WriteHeader(http.StatusPermanentRedirect)
			}
		}, false, "payments abandonment answer unknown: payments redirect refused (HTTP 308)", nil, peerProviderRef},
	}
	// RA1-2. In these cases payments commits its marker with savedMarkerRef before the malformed
	// answer. The retry answers from that saved marker only, the way payments' replay does
	// (partial_refund_abandon.go), and it refuses when no marker is saved. A finished outcome with
	// savedMarkerRef therefore proves that the retry finished from the marker of the first run.
	for name, body := range malformedEvidence2000(savedMarkerRef) {
		cases["marker committed, "+name] = refusal{func(p *statefulPayments, _ string) func(http.ResponseWriter) {
			return func(w http.ResponseWriter) {
				p.commitMarkerRef(savedMarkerRef)
				_, _ = w.Write([]byte(body))
			}
		}, true, "payments abandonment answer unknown: payments returned invalid refund evidence",
			func(p *statefulPayments) func(http.ResponseWriter) { return p.replayFromMarker }, savedMarkerRef}
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			db, ctx := outcomeDB(t)
			s := seedOrder(t, ctx, db, uuid.New(), uuid.New(), 2, 1000)
			r := bindStaffRefund(t, ctx, db, s, "abandon-refused-594", 2)
			target := newRedirectTarget(t, proof2000)
			peer := newStatefulPayments(t, 2000)
			peer.script(c.first(peer, target.url))
			if c.replay != nil {
				peer.script(c.replay(peer))
			}
			in := abandonRequest(s, r.ID)

			_, err := abandonService(db, peer).RecordAbandoned(ctx, in)
			if !errors.Is(err, ErrAbandonUncertain) || err.Error() != c.detail {
				t.Errorf("err = %v, want %q", err, c.detail)
			}
			if seen := target.requests(); len(seen) != 0 {
				t.Errorf("the redirect target received %v, want nothing", seen)
			}
			if got := (peer.markerRef() != ""); got != c.marker {
				t.Errorf("payments marker = %t, want %t", got, c.marker)
			}
			if got := readReservation(t, ctx, db, s, r.ID); got != pendingReservation(in) {
				t.Errorf("reservation = %+v, want pending", got)
			}
			if st := readRefundState(t, ctx, db, s, r.ID); st != untouched {
				t.Errorf("Commerce changed on an uncertain answer: %+v", st)
			}
			if quantityFree(t, ctx, db, s, 1) {
				t.Error("an uncertain abandonment released quantity")
			}
			if _, err := outsideService(db, peer).RecordRefundedOutside(ctx, outsideRequest(s, r.ID)); !errors.Is(err, store.ErrAbandonmentReserved) {
				t.Errorf("outside completion over the pending reservation: err = %v, want ErrAbandonmentReserved", err)
			}
			if t.Failed() {
				t.FailNow()
			}

			got, err := abandonService(db, peer).RecordAbandoned(ctx, in)
			if err != nil || got.Replay || got.Outcome.ExternalRef != c.wantRef || got.Outcome.Amount != 2000 {
				t.Fatalf("the same command with a valid answer: %+v err=%v, want reference %q", got, err, c.wantRef)
			}
			if ref := readAbandonedRef(t, ctx, db, s, r.ID); ref != c.wantRef {
				t.Fatalf("persisted abandoned outcome reference = %q, want %q", ref, c.wantRef)
			}
			if got := readReservation(t, ctx, db, s, r.ID); got != finalizedReservation(in) {
				t.Fatalf("reservation after the finish = %+v, want finalized", got)
			}
			if !quantityFree(t, ctx, db, s, 2) {
				t.Fatal("the finished abandonment kept its quantity")
			}
			if n, proofs := peer.count(abandonRoute), peer.count(proofRoute); n != 2 || proofs != 0 {
				t.Fatalf("payments abandon POSTs = %d and proof reads = %d, want 2 and 0", n, proofs)
			}
		})
	}
}

// savedMarkerRef is the reference in the marker that payments commits before a malformed answer.
// It differs from peerProviderRef, the reference that the peer's default answer returns.
const savedMarkerRef = "re_saved_marker_594"

// commitMarkerRef commits the abandonment marker with ref, unless a marker exists.
func (p *statefulPayments) commitMarkerRef(ref string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.marker == "" {
		p.marker = ref
	}
}

// replayFromMarker answers one abandon POST from the saved marker only. With no saved marker it
// refuses, and it never creates one.
func (p *statefulPayments) replayFromMarker(w http.ResponseWriter) {
	ref := p.markerRef()
	if ref == "" {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"peer: no saved marker to replay"}`))
		return
	}
	_, _ = fmt.Fprintf(w, `{"provider_ref":%q,"amount":%d,"currency":"EUR"}`, ref, p.amount)
}

// readAbandonedRef reads the persisted reference of the refund's abandoned outcome, or "".
func readAbandonedRef(t *testing.T, ctx context.Context, db *sql.DB, s seeded, refund uuid.UUID) string {
	t.Helper()
	var ref string
	if err := db.QueryRowContext(ctx, `
		SELECT coalesce(max(external_ref),'') FROM refund_outcomes
		WHERE organizer_id=$1 AND subject_kind='order_refund' AND subject_id=$2 AND kind='abandoned'`, s.org, refund).
		Scan(&ref); err != nil {
		t.Fatal(err)
	}
	return ref
}
