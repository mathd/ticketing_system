//go:build smoke

package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/payments/internal/psp"
)

// TKT-525 S2 (ADR-087): POST /internal/psp/partial-refund/abandon through the production router
// (with response validation) and the REAL Stripe adapter, against the recording Stripe peer and
// a real database. The fixture is the failure-proof fixture: one bound 1000 EUR leg against a
// captured 2000 EUR charge, and a provider listing that holds the leg's stamped failed refund.

func (f *proofFixture) abandonBody(org, sourceKey, refundKey string, amount int64, currency string) string {
	return fmt.Sprintf(`{"organizer_id":%q,"idempotency_key":%q,"refund_key":%q,"amount":%d,"currency":%q}`,
		org, sourceKey, refundKey, amount, currency)
}

func (f *proofFixture) post(t *testing.T, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/psp/partial-refund/abandon", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	res := httptest.NewRecorder()
	f.h.ServeHTTP(res, req)
	return res
}

func (f *proofFixture) abandonValid(t *testing.T) *httptest.ResponseRecorder {
	return f.post(t, f.abandonBody(f.org.String(), f.sourceKey, f.refundKey, 1000, "EUR"), refundCredential)
}

type markerRow struct {
	count       int
	providerRef string
}

func (f *proofFixture) marker(t *testing.T) markerRow {
	t.Helper()
	var m markerRow
	if err := f.db.QueryRowContext(f.ctx, `
		SELECT count(*), coalesce(max(provider_ref),'') FROM payment_refund_leg_abandonments
		WHERE organizer_id=$1 AND source_idempotency_key=$2 AND refund_idempotency_key=$3`,
		f.org, f.sourceKey, f.refundKey).Scan(&m.count, &m.providerRef); err != nil {
		t.Fatal(err)
	}
	return m
}

// assertLegUntouched checks the parts of the leg the abandonment must never change: still
// bound, no journal entry, and no provider submit.
func (f *proofFixture) assertLegUntouched(t *testing.T) {
	t.Helper()
	calls, _ := f.peer.snapshot()
	for _, c := range calls {
		if c != "GET /v1/refunds" {
			t.Fatalf("the abandonment made %q; it may only list refunds", c)
		}
	}
	var journal int
	var status, providerKey string
	if err := f.db.QueryRowContext(f.ctx, `
		SELECT (SELECT count(*) FROM journal_entries WHERE organizer_id=$1),
		       status, provider_idempotency_key
		FROM payment_refund_legs WHERE organizer_id=$1 AND refund_idempotency_key=$2`,
		f.org, f.refundKey).Scan(&journal, &status, &providerKey); err != nil {
		t.Fatal(err)
	}
	if journal != 0 || status != "bound" || providerKey != f.legKey {
		t.Fatalf("leg after abandonment: journal=%d status=%q key=%q, want 0, bound, %q", journal, status, providerKey, f.legKey)
	}
}

// A proven terminal failure writes ONE marker carrying the provider's reference, and answers
// with it. A replay answers from the committed marker without asking the provider, even when
// the provider is down. The 255-byte case is the longest reference both services store (RA2-2):
// Commerce's recovery fixture covers its half, this one the Payments marker and its replay.
func TestAbandonMarksTheLegAfterAProof(t *testing.T) {
	for _, c := range []struct{ name, status, ref string }{
		{"failed", "failed", "re_ours_failed"},
		{"canceled", "canceled", "re_ours_canceled"},
		{"255-byte reference", "failed", "re_" + strings.Repeat("x", 252)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if len(c.ref) > 255 {
				t.Fatalf("fixture reference is %d bytes", len(c.ref))
			}
			f := newProofFixture(t, nil)
			f.peer.list = f.listWith(c.ref, c.status)
			want := `{"amount":1000,"currency":"EUR","provider_ref":"` + c.ref + `"}`
			assertProofResponse(t, f.abandonValid(t), 200, want)
			if calls, queries := f.peer.snapshot(); len(calls) != 1 || queries[0].Get("payment_intent") != f.pi {
				t.Fatalf("provider calls = %v %v, want one listing under the stored PaymentIntent", calls, queries)
			}
			if m := f.marker(t); m != (markerRow{1, c.ref}) {
				t.Fatalf("marker = %+v", m)
			}
			f.assertLegUntouched(t)

			f.peer.mu.Lock()
			f.peer.status, f.peer.list = 503, `{"error":{"message":"down","type":"api_error"}}`
			f.peer.mu.Unlock()
			assertProofResponse(t, f.abandonValid(t), 200, want)
			if calls, _ := f.peer.snapshot(); len(calls) != 1 {
				t.Fatalf("the replay asked the provider: %v", calls)
			}
			if m := f.marker(t); m.count != 1 {
				t.Fatalf("markers after replay = %d", m.count)
			}
		})
	}
}

// Requests refused before any provider call. Each varies ONE thing from the valid request.
func TestAbandonRefusesBeforeAskingTheProvider(t *testing.T) {
	f := newProofFixture(t, nil)
	org, src, rk := f.org.String(), f.sourceKey, f.refundKey
	for _, c := range []struct {
		name   string
		body   string
		token  string
		status int
		want   string
	}{
		{"no credential", f.abandonBody(org, src, rk, 1000, "EUR"), "", 401, `{"error":"unauthorized"}`},
		{"wrong credential", f.abandonBody(org, src, rk, 1000, "EUR"), "not-the-credential", 401, `{"error":"unauthorized"}`},
		{"uppercase UUID refund key", f.abandonBody(org, src, upper(rk), 1000, "EUR"), refundCredential, 400, `{"error":"refund_key must be a canonical UUID"}`},
		{"exchange downgrade key", f.abandonBody(org, src, "exchange-refund:"+rk, 1000, "EUR"), refundCredential, 400, ""},
		{"another organizer", f.abandonBody(uuid.NewString(), src, rk, 1000, "EUR"), refundCredential, 404, `{"error":"refund leg not found"}`},
		{"another charge", f.abandonBody(org, src+"-other", rk, 1000, "EUR"), refundCredential, 404, `{"error":"refund leg not found"}`},
		{"another leg", f.abandonBody(org, src, uuid.NewString(), 1000, "EUR"), refundCredential, 404, `{"error":"refund leg not found"}`},
		{"another amount", f.abandonBody(org, src, rk, 999, "EUR"), refundCredential, 409, `{"error":"refund leg money does not match"}`},
		{"another currency", f.abandonBody(org, src, rk, 1000, "USD"), refundCredential, 409, `{"error":"refund leg money does not match"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := f.post(t, c.body, c.token)
			if c.want == "" {
				if res.Code != c.status {
					t.Fatalf("status=%d body=%s, want %d", res.Code, res.Body.String(), c.status)
				}
			} else {
				assertProofResponse(t, res, c.status, c.want)
			}
			if calls, _ := f.peer.snapshot(); len(calls) != 0 {
				t.Fatalf("a refused request reached the provider: %v", calls)
			}
			if m := f.marker(t); m.count != 0 {
				t.Fatalf("a refused request wrote a marker: %+v", m)
			}
		})
	}
	f.assertLegUntouched(t)
}

// A completed leg is refused without a provider call. A marked leg answered for another
// organizer is still not found: the replay is scoped too.
func TestAbandonRefusesACompletedLegAndScopesTheReplay(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		f := newProofFixture(t, nil)
		if _, err := f.db.ExecContext(f.ctx, `UPDATE payment_refund_legs SET status='refunded',provider_ref='re_ok',fact_id=$2,
			completed_at=now(),confirmed_amount=1000,confirmed_currency='EUR' WHERE organizer_id=$1`, f.org, uuid.New()); err != nil {
			t.Fatal(err)
		}
		assertProofResponse(t, f.abandonValid(t), 409, `{"error":"refund leg completed"}`)
		if calls, _ := f.peer.snapshot(); len(calls) != 0 {
			t.Fatalf("a completed leg reached the provider: %v", calls)
		}
		if m := f.marker(t); m.count != 0 {
			t.Fatalf("marker on a completed leg: %+v", m)
		}
	})
	t.Run("replay scope", func(t *testing.T) {
		f := newProofFixture(t, nil)
		assertProofResponse(t, f.abandonValid(t), 200, `{"amount":1000,"currency":"EUR","provider_ref":"re_failed_ours"}`)
		assertProofResponse(t, f.post(t, f.abandonBody(uuid.NewString(), f.sourceKey, f.refundKey, 1000, "EUR"), refundCredential),
			404, `{"error":"refund leg not found"}`)
		assertProofResponse(t, f.post(t, f.abandonBody(f.org.String(), f.sourceKey, f.refundKey, 999, "EUR"), refundCredential),
			409, `{"error":"refund leg money does not match"}`)
		if calls, _ := f.peer.snapshot(); len(calls) != 1 {
			t.Fatalf("provider calls = %v, want only the first proof", calls)
		}
	})
}

// Provider evidence that is not a proof writes no marker.
func TestAbandonRefusesProviderEvidenceThatIsNotAProof(t *testing.T) {
	for _, c := range []struct {
		name   string
		list   func(f *proofFixture) string
		status int
		body   string
	}{
		{"the stamped refund succeeded", func(f *proofFixture) string { return f.listWith("re_ok", "succeeded") }, 409,
			`{"error":"provider evidence contradicts a failed refund"}`},
		{"the stamped refund is pending", func(f *proofFixture) string { return f.listWith("re_p", "pending") }, 502,
			`{"error":"refund failure proof unavailable"}`},
		{"no refund carries the stamp", func(f *proofFixture) string {
			return `{"object":"list","has_more":false,"data":[]}`
		}, 502, `{"error":"refund failure proof unavailable"}`},
		{"a malformed listing", func(*proofFixture) string { return `{"object":"list","data":[` }, 502,
			`{"error":"refund failure proof unavailable"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newProofFixture(t, nil)
			f.peer.list = c.list(f)
			assertProofResponse(t, f.abandonValid(t), c.status, c.body)
			if m := f.marker(t); m.count != 0 {
				t.Fatalf("marker without a proof: %+v", m)
			}
			f.assertLegUntouched(t)
		})
	}
	t.Run("the provider is unavailable", func(t *testing.T) {
		f := newProofFixture(t, nil)
		f.peer.status, f.peer.list = 503, `{"error":{"message":"down","type":"api_error"}}`
		assertProofResponse(t, f.abandonValid(t), 502, `{"error":"refund failure proof unavailable"}`)
		if m := f.marker(t); m.count != 0 {
			t.Fatalf("marker without a proof: %+v", m)
		}
	})
	// A provider without the RefundFailureProver capability (RA1-3): countingPSP embeds only
	// the PSP interface.
	t.Run("the provider has no proof capability", func(t *testing.T) {
		provider := &countingPSP{PSP: psp.NewFake()}
		f := newProofFixture(t, func(*stripePeer) psp.PSP { return provider })
		assertProofResponse(t, f.abandonValid(t), 502, `{"error":"refund failure proof unavailable"}`)
		if m := f.marker(t); m.count != 0 {
			t.Fatalf("marker without a proof: %+v", m)
		}
		f.assertLegUntouched(t)
		if n := provider.count(); n != 0 {
			t.Fatalf("the abandon submitted %d provider refunds", n)
		}
	})
}

// The short marker transaction rechecks, under the operation and leg locks, everything the
// lock-free proof relied on. A change landing inside the provider call refuses and writes no
// marker; a matching marker committed by a concurrent request is answered as a replay.
func TestAbandonRechecksTheLegAfterTheProvider(t *testing.T) {
	for _, c := range []struct {
		name    string
		change  string
		status  int
		body    string
		markers markerRow
	}{
		{"the leg completes", `UPDATE payment_refund_legs SET status='refunded',provider_ref='re_ok',fact_id=gen_random_uuid(),
			completed_at=now(),confirmed_amount=1000,confirmed_currency='EUR' WHERE organizer_id=$1`,
			409, `{"error":"refund leg completed"}`, markerRow{0, ""}},
		{"the charge reference changes", `UPDATE payment_operations SET provider_payment_ref='pi_replaced' WHERE organizer_id=$1`,
			409, `{"error":"refund leg changed during the proof"}`, markerRow{0, ""}},
		{"the leg's provider key changes", `UPDATE payment_refund_legs SET provider_idempotency_key='psp-refund-leg-v1:replaced' WHERE organizer_id=$1`,
			409, `{"error":"refund leg changed during the proof"}`, markerRow{0, ""}},
		{"a matching marker commits", `INSERT INTO payment_refund_leg_abandonments(organizer_id,source_idempotency_key,refund_idempotency_key,provider_ref)
			SELECT organizer_id,source_idempotency_key,refund_idempotency_key,'re_failed_ours' FROM payment_refund_legs WHERE organizer_id=$1`,
			200, `{"amount":1000,"currency":"EUR","provider_ref":"re_failed_ours"}`, markerRow{1, "re_failed_ours"}},
		{"a contradictory marker commits", `INSERT INTO payment_refund_leg_abandonments(organizer_id,source_idempotency_key,refund_idempotency_key,provider_ref)
			SELECT organizer_id,source_idempotency_key,refund_idempotency_key,'re_someone_else' FROM payment_refund_legs WHERE organizer_id=$1`,
			409, `{"error":"refund leg abandoned under other evidence"}`, markerRow{1, "re_someone_else"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newProofFixture(t, nil)
			f.peer.beforeAnswer = func() {
				if _, err := f.db.ExecContext(f.ctx, c.change, f.org); err != nil {
					t.Errorf("change inside the provider call: %v", err)
				}
			}
			assertProofResponse(t, f.abandonValid(t), c.status, c.body)
			if calls, _ := f.peer.snapshot(); len(calls) != 1 {
				t.Fatalf("provider calls = %v, want exactly the one listing", calls)
			}
			if m := f.marker(t); m != c.markers {
				t.Fatalf("marker = %+v, want %+v", m, c.markers)
			}
		})
	}
}

// dispatch sends the fixture leg's ordinary partial-refund request.
func (f *proofFixture) dispatch(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"organizer_id":%q,"idempotency_key":%q,"refund_key":%q,"amount":1000,"currency":"EUR"}`,
		f.org, f.sourceKey, f.refundKey)
	req := httptest.NewRequest(http.MethodPost, "/internal/psp/partial-refund", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", refundCredential)
	res := httptest.NewRecorder()
	f.h.ServeHTTP(res, req)
	return res
}

// COS 4, payments half. Ordinary dispatch of a marked leg answers the existing 409 and never
// reaches the provider: no listing, no POST.
//
// The mutation that removes the marker read is the case ADR-087's resolve-first argument covers:
// the same request then reaches the real Stripe adapter, which lists, finds the stamped FAILED
// refund and answers the terminal 422 without a POST. The status and the listing count make that
// mutation red; the POST count is zero either way.
func TestPartialRefundDispatchRefusesAnAbandonedLeg(t *testing.T) {
	f := newProofFixture(t, nil)
	assertProofResponse(t, f.abandonValid(t), 200, `{"amount":1000,"currency":"EUR","provider_ref":"re_failed_ours"}`)
	before, _ := f.peer.snapshot()

	res := f.dispatch(t)

	after, _ := f.peer.snapshot()
	posts := 0
	for _, c := range after {
		if c != "GET /v1/refunds" {
			posts++
		}
	}
	if posts != 0 {
		t.Fatalf("dispatch of an abandoned leg submitted to the provider: %v", after)
	}
	if res.Code != 409 || res.Body.String() != `{"error":"refund leg abandoned"}`+"\n" {
		t.Fatalf("status=%d body=%s, want 409 refund leg abandoned", res.Code, res.Body.String())
	}
	if len(after) != len(before) {
		t.Fatalf("dispatch of an abandoned leg reached the provider: %v", after[len(before):])
	}
	f.assertLegUntouched(t)
}

// A failed marker read fails closed (RA1-4). The leg is already bound, so the replay bind
// succeeds without reading the marker table; the dispatch read then fails. The answer is 500
// and the provider is not asked: no listing and no POST.
func TestPartialRefundDispatchFailsClosedWhenTheMarkerReadFails(t *testing.T) {
	f := newProofFixture(t, nil)
	if _, err := f.db.ExecContext(f.ctx, `ALTER TABLE payment_refund_leg_abandonments RENAME TO payment_refund_leg_abandonments_hidden`); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			if _, err := f.db.ExecContext(f.ctx, `ALTER TABLE payment_refund_leg_abandonments_hidden RENAME TO payment_refund_leg_abandonments`); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(restore)

	res := f.dispatch(t)
	restore()

	if res.Code != 500 || res.Body.String() != `{"error":"lookup refund leg abandonment"}`+"\n" {
		t.Fatalf("status=%d body=%s, want 500 lookup refund leg abandonment", res.Code, res.Body.String())
	}
	if calls, _ := f.peer.snapshot(); len(calls) != 0 {
		t.Fatalf("a failed marker read reached the provider: %v", calls)
	}
	f.assertLegUntouched(t)
}

const emptyRefundList = `{"object":"list","has_more":false,"data":[]}`

func (f *proofFixture) succeededRefund(id string) string {
	return f.refund(id, "succeeded", f.pi, f.legKey, 1000, "eur")
}

// ADR-087 §13: a dispatch admitted before a marker commits can make one POST, and it is the
// SAME request as the original — the stored leg provider key as both the Idempotency-Key header
// and the metadata stamp, with the same amount and PaymentIntent — so the provider deduplicates
// it within its key retention. The first POST's answer is lost (a provider 500); the replay
// must send byte-identical parameters.
func TestPartialRefundReplaySendsTheStoredKeyAsHeaderAndStamp(t *testing.T) {
	f := newProofFixture(t, nil)
	f.peer.list = emptyRefundList
	f.peer.submit = func() (int, string) {
		if len(f.peer.posts) == 1 {
			return 500, `{"error":{"message":"lost","type":"api_error"}}`
		}
		return 200, f.succeededRefund("re_replayed")
	}
	assertProofResponse(t, f.dispatch(t), 502, `{"error":"provider refund unresolved"}`)
	if res := f.dispatch(t); res.Code != 200 {
		t.Fatalf("replay: status=%d body=%s, want 200", res.Code, res.Body.String())
	}
	f.peer.mu.Lock()
	posts := append([]stripePost(nil), f.peer.posts...)
	f.peer.mu.Unlock()
	if len(posts) != 2 {
		t.Fatalf("refund POSTs = %d, want the original and the replay", len(posts))
	}
	for i, p := range posts {
		if p.idempotencyKey != f.legKey || p.form.Get("metadata[compensation_key]") != f.legKey {
			t.Fatalf("POST %d: header key %q, stamp %q, want the stored leg key %q as both",
				i+1, p.idempotencyKey, p.form.Get("metadata[compensation_key]"), f.legKey)
		}
		if p.form.Get("amount") != "1000" || p.form.Get("payment_intent") != f.pi {
			t.Fatalf("POST %d: amount %q, payment_intent %q, want 1000 and %q", i+1, p.form.Get("amount"), p.form.Get("payment_intent"), f.pi)
		}
	}
	if posts[0].form.Encode() != posts[1].form.Encode() {
		t.Fatalf("the replay changed the request: %q then %q", posts[0].form.Encode(), posts[1].form.Encode())
	}
}

// ADR-087 §13: the marker commits while the provider call runs, and the provider answers
// Refunded. Nothing is recorded: no payment.refunded fact, no completion, the existing 409.
func TestPartialRefundDispatchRecordsNothingWhenTheMarkerCommitsDuringTheProviderCall(t *testing.T) {
	f := newProofFixture(t, nil)
	f.peer.list = emptyRefundList
	f.peer.submit = func() (int, string) {
		if _, err := f.db.ExecContext(f.ctx, `INSERT INTO payment_refund_leg_abandonments(organizer_id,source_idempotency_key,refund_idempotency_key,provider_ref)
			VALUES($1,$2,$3,'re_failed_ours')`, f.org, f.sourceKey, f.refundKey); err != nil {
			t.Errorf("marker inside the provider call: %v", err)
		}
		return 200, f.succeededRefund("re_late")
	}
	assertProofResponse(t, f.dispatch(t), 409, `{"error":"refund leg abandoned"}`)
	if len(f.peer.posts) != 1 {
		t.Fatalf("refund POSTs = %d, want the one admitted before the marker", len(f.peer.posts))
	}
	assertLegStillBound(t, f.db, f.ctx, f.org)
	if m := f.marker(t); m != (markerRow{1, "re_failed_ours"}) {
		t.Fatalf("marker = %+v", m)
	}
}
