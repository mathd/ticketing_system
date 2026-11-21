//go:build smoke

package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/payments/internal/psp"
	"ticketing/services/payments/internal/store"
)

// TKT-525 (ADR-087): GET /internal/psp/partial-refund/failure-proof through the production
// router (with response validation) and the REAL Stripe adapter, against a recording Stripe
// peer and a real database. Every case asserts the declared status and body, and that the
// provider saw no POST, no marker was written and no journal entry was appended.

// stripePeer is a recording Stripe. It answers the refund listing with `list` and fails every
// other route, so any submit is both recorded and visible.
type stripePeer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []string
	query  []url.Values
	status int
	list   string
	// beforeAnswer runs inside the listing request, after the handler's first read and before
	// its second: the window a concurrent writer has.
	beforeAnswer func()
}

func newStripePeer(t *testing.T) *stripePeer {
	t.Helper()
	p := &stripePeer{status: 200}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.calls = append(p.calls, r.Method+" "+r.URL.Path)
		p.query = append(p.query, r.URL.Query())
		status, list, hook := p.status, p.list, p.beforeAnswer
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || r.URL.Path != "/v1/refunds" {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":{"message":"peer: unexpected route","type":"api_error"}}`))
			return
		}
		if hook != nil {
			hook()
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(list))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *stripePeer) snapshot() ([]string, []url.Values) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...), append([]url.Values(nil), p.query...)
}

type proofFixture struct {
	db        *sql.DB
	ctx       context.Context
	h         http.Handler
	peer      *stripePeer
	org       uuid.UUID
	sourceKey string
	refundKey string
	legKey    string
	pi        string
}

// newProofFixture binds one 1000 EUR leg against a captured 2000 EUR charge through the real
// store, so the leg's provider key is the one the production code derived.
func newProofFixture(t *testing.T, provider func(*stripePeer) psp.PSP) *proofFixture {
	t.Helper()
	db, ctx := refundDB(t)
	f := &proofFixture{db: db, ctx: ctx, org: uuid.New(), refundKey: uuid.NewString()}
	f.sourceKey = "proof-charge-" + f.org.String()
	f.pi = "pi_proof_" + f.org.String()[:8]
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_operations(organizer_id,idempotency_key,request_fingerprint,status,order_id,buyer_id,
		                               request_amount,request_currency,provider_payment_ref,provider_state,
		                               authorized_amount,captured_amount,provider_state_at)
		VALUES($1,$2,'fingerprint','captured',$3,$4,2000,'EUR',$5,'captured',2000,2000,now())`,
		f.org, f.sourceKey, uuid.New(), uuid.New(), f.pi); err != nil {
		t.Fatal(err)
	}
	ring, err := store.NewKeyring("proof-key", []byte("payments-refund-proof-key-012345"), "")
	if err != nil {
		t.Fatal(err)
	}
	journal := store.New(db, ring)
	leg, err := journal.BindRefundLeg(ctx, f.org, f.sourceKey, f.refundKey, 1000, "EUR")
	if err != nil {
		t.Fatal(err)
	}
	f.legKey = leg.ProviderKey
	f.peer = newStripePeer(t)
	f.peer.list = f.listWith("re_failed_ours", "failed")
	var p psp.PSP = psp.NewStripe("sk_test_dummy", f.peer.srv.URL, f.peer.srv.Client())
	if provider != nil {
		p = provider(f.peer)
	}
	f.h = newTestServerWithPSP(journal, refundCredential, p).Router(nil, true)
	return f
}

func (f *proofFixture) refund(id, status, pi, key string, amount int64, currency string) string {
	return fmt.Sprintf(`{"id":%q,"object":"refund","amount":%d,"currency":%q,"status":%q,"payment_intent":%q,"metadata":{"compensation_key":%q}}`,
		id, amount, currency, status, pi, key)
}

func (f *proofFixture) listWith(id, status string) string {
	return `{"object":"list","has_more":false,"data":[` + f.refund(id, status, f.pi, f.legKey, 1000, "eur") + `]}`
}

func (f *proofFixture) get(t *testing.T, org, sourceKey, refundKey, token string) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{}
	q.Set("organizer_id", org)
	q.Set("idempotency_key", sourceKey)
	q.Set("refund_key", refundKey)
	req := httptest.NewRequest(http.MethodGet, "/internal/psp/partial-refund/failure-proof?"+q.Encode(), nil)
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	res := httptest.NewRecorder()
	f.h.ServeHTTP(res, req)
	return res
}

func (f *proofFixture) getValid(t *testing.T) *httptest.ResponseRecorder {
	return f.get(t, f.org.String(), f.sourceKey, f.refundKey, refundCredential)
}

// assertNothingWritten checks that the proof left payments exactly as it found it and made no
// provider submit.
func (f *proofFixture) assertNothingWritten(t *testing.T) {
	t.Helper()
	calls, _ := f.peer.snapshot()
	for _, c := range calls {
		if c != "GET /v1/refunds" {
			t.Fatalf("the proof made %q; it may only list refunds", c)
		}
	}
	var markers, journal int
	var status string
	if err := f.db.QueryRowContext(f.ctx, `
		SELECT (SELECT count(*) FROM payment_refund_leg_abandonments WHERE organizer_id=$1),
		       (SELECT count(*) FROM journal_entries WHERE organizer_id=$1),
		       (SELECT status FROM payment_refund_legs WHERE organizer_id=$1 AND refund_idempotency_key=$2)`,
		f.org, f.refundKey).Scan(&markers, &journal, &status); err != nil {
		t.Fatal(err)
	}
	if journal != 0 || status != "bound" {
		t.Fatalf("after the proof: journal entries=%d leg status=%q, want 0 and bound", journal, status)
	}
	if markers > 1 {
		t.Fatalf("the proof wrote a marker: %d", markers)
	}
}

func assertProofResponse(t *testing.T, res *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if res.Code != status || res.Body.String() != body+"\n" {
		t.Fatalf("status=%d body=%s, want %d %s", res.Code, res.Body.String(), status, body)
	}
}

func TestFailureProofReturnsTheMatchingTerminalRefund(t *testing.T) {
	for _, status := range []string{"failed", "canceled"} {
		t.Run(status, func(t *testing.T) {
			f := newProofFixture(t, nil)
			f.peer.list = f.listWith("re_ours_"+status, status)
			assertProofResponse(t, f.getValid(t), 200, `{"amount":1000,"currency":"EUR","provider_ref":"re_ours_`+status+`"}`)
			calls, queries := f.peer.snapshot()
			if len(calls) != 1 {
				t.Fatalf("provider calls = %v, want one listing", calls)
			}
			// The charge reference came from the stored operation; the stamp matched only
			// because the adapter looked for the stored leg's provider key.
			if queries[0].Get("payment_intent") != f.pi {
				t.Fatalf("listing scoped to %q, want the stored PaymentIntent %q", queries[0].Get("payment_intent"), f.pi)
			}
			if f.legKey != store.RefundLegKey(f.org, f.sourceKey, f.refundKey) {
				t.Fatalf("fixture leg key %q is not the derived key", f.legKey)
			}
			f.assertNothingWritten(t)
		})
	}
}

// Requests refused before any provider call: credential, input, and scope. Each varies ONE
// identifier from the valid request.
func TestFailureProofRefusesBeforeAskingTheProvider(t *testing.T) {
	f := newProofFixture(t, nil)
	other := uuid.NewString()
	for _, c := range []struct {
		name                             string
		org, sourceKey, refundKey, token string
		status                           int
		body                             string
	}{
		{"no credential", f.org.String(), f.sourceKey, f.refundKey, "", 401, `{"error":"unauthorized"}`},
		{"wrong credential", f.org.String(), f.sourceKey, f.refundKey, "not-the-credential", 401, `{"error":"unauthorized"}`},
		{"uppercase UUID refund key", f.org.String(), f.sourceKey, upper(f.refundKey), refundCredential, 400, `{"error":"refund_key must be a canonical UUID"}`},
		{"exchange downgrade key", f.org.String(), f.sourceKey, "exchange-refund:" + f.refundKey, refundCredential, 400, ""},
		{"another organizer", uuid.NewString(), f.sourceKey, f.refundKey, refundCredential, 404, `{"error":"refund leg not found"}`},
		{"another charge", f.org.String(), f.sourceKey + "-other", f.refundKey, refundCredential, 404, `{"error":"refund leg not found"}`},
		{"another leg", f.org.String(), f.sourceKey, other, refundCredential, 404, `{"error":"refund leg not found"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := f.get(t, c.org, c.sourceKey, c.refundKey, c.token)
			if c.body == "" {
				// The request validator answers a non-UUID before the handler runs; the
				// status is what is declared, the body is the validator's.
				if res.Code != c.status {
					t.Fatalf("status=%d body=%s, want %d", res.Code, res.Body.String(), c.status)
				}
			} else {
				assertProofResponse(t, res, c.status, c.body)
			}
			if calls, _ := f.peer.snapshot(); len(calls) != 0 {
				t.Fatalf("a refused request reached the provider: %v", calls)
			}
		})
	}
	f.assertNothingWritten(t)
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}

// A completed leg or a marked leg is refused conclusively, without a provider call.
func TestFailureProofRefusesACompletedOrMarkedLeg(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		f := newProofFixture(t, nil)
		if _, err := f.db.ExecContext(f.ctx, `UPDATE payment_refund_legs SET status='refunded',provider_ref='re_ok',fact_id=$2,
			completed_at=now(),confirmed_amount=1000,confirmed_currency='EUR' WHERE organizer_id=$1`, f.org, uuid.New()); err != nil {
			t.Fatal(err)
		}
		assertProofResponse(t, f.getValid(t), 409, `{"error":"refund leg completed"}`)
		if calls, _ := f.peer.snapshot(); len(calls) != 0 {
			t.Fatalf("a completed leg reached the provider: %v", calls)
		}
	})
	t.Run("marked", func(t *testing.T) {
		f := newProofFixture(t, nil)
		if _, err := f.db.ExecContext(f.ctx, `INSERT INTO payment_refund_leg_abandonments(organizer_id,source_idempotency_key,refund_idempotency_key,provider_ref)
			VALUES($1,$2,$3,'re_failed_ours')`, f.org, f.sourceKey, f.refundKey); err != nil {
			t.Fatal(err)
		}
		assertProofResponse(t, f.getValid(t), 409, `{"error":"refund leg abandoned"}`)
		if calls, _ := f.peer.snapshot(); len(calls) != 0 {
			t.Fatalf("a marked leg reached the provider: %v", calls)
		}
		f.assertNothingWritten(t)
	})
}

// Provider evidence that is not a proof. Conclusive contradictions are 409; everything that
// only fails to prove is 502.
func TestFailureProofRefusesProviderEvidenceThatIsNotAProof(t *testing.T) {
	for _, c := range []struct {
		name   string
		list   func(f *proofFixture) string
		status int
		body   string
	}{
		{"the stamped refund succeeded", func(f *proofFixture) string { return f.listWith("re_ok", "succeeded") }, 409,
			`{"error":"provider evidence contradicts a failed refund"}`},
		{"two refunds carry the stamp", func(f *proofFixture) string {
			return `{"object":"list","has_more":false,"data":[` + f.refund("re_a", "failed", f.pi, f.legKey, 1000, "eur") + `,` +
				f.refund("re_b", "failed", f.pi, f.legKey, 1000, "eur") + `]}`
		}, 409, `{"error":"provider evidence contradicts a failed refund"}`},
		{"the stamped refund is pending", func(f *proofFixture) string { return f.listWith("re_p", "pending") }, 502,
			`{"error":"refund failure proof unavailable"}`},
		{"no refund carries the stamp", func(f *proofFixture) string {
			return `{"object":"list","has_more":false,"data":[{"id":"re_x","object":"refund","amount":1000,"currency":"eur","status":"failed","payment_intent":"` + f.pi + `","metadata":{}}]}`
		}, 502, `{"error":"refund failure proof unavailable"}`},
		{"a different PaymentIntent", func(f *proofFixture) string {
			return `{"object":"list","has_more":false,"data":[` + f.refund("re_x", "failed", "pi_other", f.legKey, 1000, "eur") + `]}`
		}, 502, `{"error":"refund failure proof unavailable"}`},
		{"a different amount", func(f *proofFixture) string {
			return `{"object":"list","has_more":false,"data":[` + f.refund("re_x", "failed", f.pi, f.legKey, 999, "eur") + `]}`
		}, 502, `{"error":"refund failure proof unavailable"}`},
		{"a malformed listing", func(*proofFixture) string { return `{"object":"list","data":[` }, 502,
			`{"error":"refund failure proof unavailable"}`},
		{"an incomplete listing", func(f *proofFixture) string {
			return `{"object":"list","data":[` + f.refund("re_x", "failed", f.pi, f.legKey, 1000, "eur") + `]}`
		}, 502, `{"error":"refund failure proof unavailable"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newProofFixture(t, nil)
			f.peer.list = c.list(f)
			assertProofResponse(t, f.getValid(t), c.status, c.body)
			f.assertNothingWritten(t)
		})
	}
	t.Run("the provider is unavailable", func(t *testing.T) {
		f := newProofFixture(t, nil)
		f.peer.status, f.peer.list = 503, `{"error":{"message":"down","type":"api_error"}}`
		assertProofResponse(t, f.getValid(t), 502, `{"error":"refund failure proof unavailable"}`)
		f.assertNothingWritten(t)
	})
	// The fake PSP proves only refunds it itself failed (TKT-525); it never recorded this one.
	t.Run("the fake PSP never recorded the refund", func(t *testing.T) {
		f := newProofFixture(t, func(*stripePeer) psp.PSP { return psp.NewFake() })
		assertProofResponse(t, f.getValid(t), 502, `{"error":"refund failure proof unavailable"}`)
		f.assertNothingWritten(t)
	})
	t.Run("the charge has no provider reference", func(t *testing.T) {
		f := newProofFixture(t, nil)
		if _, err := f.db.ExecContext(f.ctx, `UPDATE payment_operations SET provider_payment_ref=NULL WHERE organizer_id=$1`, f.org); err != nil {
			t.Fatal(err)
		}
		assertProofResponse(t, f.getValid(t), 502, `{"error":"refund failure proof unavailable"}`)
		if calls, _ := f.peer.snapshot(); len(calls) != 0 {
			t.Fatalf("a charge with no reference reached the provider: %v", calls)
		}
	})
}

// The leg is read again after the provider answers. A completion, a marker, or a change to
// the evidence the proof was built on, landing inside the provider call, refuses the proof.
func TestFailureProofRechecksTheLegAfterTheProvider(t *testing.T) {
	for _, c := range []struct {
		name   string
		change string
		body   string
	}{
		{"the leg completes", `UPDATE payment_refund_legs SET status='refunded',provider_ref='re_ok',fact_id=gen_random_uuid(),
			completed_at=now(),confirmed_amount=1000,confirmed_currency='EUR' WHERE organizer_id=$1`, `{"error":"refund leg completed"}`},
		{"a marker commits", `INSERT INTO payment_refund_leg_abandonments(organizer_id,source_idempotency_key,refund_idempotency_key,provider_ref)
			SELECT organizer_id,source_idempotency_key,refund_idempotency_key,'re_failed_ours' FROM payment_refund_legs WHERE organizer_id=$1`,
			`{"error":"refund leg abandoned"}`},
		{"the charge reference changes", `UPDATE payment_operations SET provider_payment_ref='pi_replaced' WHERE organizer_id=$1`,
			`{"error":"refund leg changed during the proof"}`},
		{"the leg's provider key changes", `UPDATE payment_refund_legs SET provider_idempotency_key='psp-refund-leg-v1:replaced' WHERE organizer_id=$1`,
			`{"error":"refund leg changed during the proof"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newProofFixture(t, nil)
			f.peer.beforeAnswer = func() {
				if _, err := f.db.ExecContext(f.ctx, c.change, f.org); err != nil {
					t.Errorf("change inside the provider call: %v", err)
				}
			}
			assertProofResponse(t, f.getValid(t), 409, c.body)
			if calls, _ := f.peer.snapshot(); len(calls) != 1 {
				t.Fatalf("provider calls = %v, want exactly the one listing", calls)
			}
		})
	}
}
