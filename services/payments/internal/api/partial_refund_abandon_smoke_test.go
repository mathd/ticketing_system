//go:build smoke

package api

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// waitForLockWaiters waits until n backends of this test database wait on a lock, or until
// stop has a value (the request finished without waiting). It fails after ten seconds.
func (f *proofFixture) waitForLockWaiters(t *testing.T, n int, stop ...chan *httptest.ResponseRecorder) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := f.db.QueryRowContext(f.ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		for _, c := range stop {
			if len(c) > 0 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock waiters = %d after 10s, want %d", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *proofFixture) refundFacts(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM journal_entries WHERE organizer_id=$1 AND fact_type='payment.refunded'`, f.org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// D34, ADR-087 §13 (R3-1). The marker transaction starts during the provider call and takes the
// locks AbandonRefundLeg takes, in its order: the operation row, then the leg row, then the
// marker insert. The provider answers Refunded while that transaction is still open. Dispatch
// then waits for the operation lock, and the marker commits only after dispatch waits: after
// the Refunded answer and before dispatch holds the leg lock. Dispatch must read the marker
// under the lock: 409, the leg still bound, zero refund facts.
func TestPartialRefundDispatchRecordsNothingWhenTheMarkerCommitsAfterTheProviderAnswer(t *testing.T) {
	f := newProofFixture(t, nil)
	f.peer.list = emptyRefundList
	opened := make(chan *sql.Tx, 1)
	f.peer.submit = func() (int, string) {
		tx, err := f.db.BeginTx(f.ctx, nil)
		if err != nil {
			t.Errorf("begin marker transaction: %v", err)
			close(opened)
			return 500, `{"error":{"message":"fixture","type":"api_error"}}`
		}
		for _, q := range []string{
			`SELECT 1 FROM payment_operations WHERE organizer_id=$1 AND idempotency_key=$2 FOR UPDATE`,
			`SELECT 1 FROM payment_refund_legs WHERE organizer_id=$1 AND source_idempotency_key=$2 AND refund_idempotency_key=$3 FOR UPDATE`,
			`INSERT INTO payment_refund_leg_abandonments(organizer_id,source_idempotency_key,refund_idempotency_key,provider_ref)
			 VALUES($1,$2,$3,'re_failed_ours')`,
		} {
			args := []any{f.org, f.sourceKey, f.refundKey}
			if strings.Contains(q, "payment_operations") {
				args = args[:2]
			}
			if _, err := tx.ExecContext(f.ctx, q, args...); err != nil {
				t.Errorf("marker transaction: %v", err)
			}
		}
		opened <- tx
		return 200, f.succeededRefund("re_late")
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.dispatch(t) }()

	tx, ok := <-opened
	if !ok {
		t.Fatal("the marker transaction did not open")
	}
	f.waitForLockWaiters(t, 1, done)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	res := <-done

	if res.Code != 409 || res.Body.String() != `{"error":"refund leg abandoned"}`+"\n" {
		t.Fatalf("status=%d body=%s, want 409 refund leg abandoned", res.Code, res.Body.String())
	}
	if len(f.peer.posts) != 1 {
		t.Fatalf("refund POSTs = %d, want the one admitted before the marker", len(f.peer.posts))
	}
	assertLegStillBound(t, f.db, f.ctx, f.org)
	if n := f.refundFacts(t); n != 0 {
		t.Fatalf("payment.refunded facts = %d, want 0", n)
	}
	if m := f.marker(t); m != (markerRow{1, "re_failed_ours"}) {
		t.Fatalf("marker = %+v", m)
	}
}

// lockedBuffer is a log sink that the handler goroutine and the test can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog sends the default logger to a buffer for one test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// RA4-2 (D34, ADR-087 §13). The marker commits during the provider call, and the provider then
// answers Refunded with money the leg did not bind: a different amount, no confirmed money at
// all, or an amount with no currency, which the port's validation rejects (R5-1). The marker
// answers first: 409, both references in the log, the leg still bound and zero refund facts.
// The money-mismatch 502 and the invalid-result 500 are only for a leg with no marker; the
// provider confirmation tests keep the first, TestPartialRefundRefusesAnInvalidConfirmationWithoutAMarker
// the second.
//
// In the "in flight" cases the marker transaction holds the operation and leg locks when the
// provider answers, and commits only after dispatch waits for them: a marker read that does not
// take those locks misses it.
func TestPartialRefundLateMarkerWinsOverAConfirmedMoneyMismatch(t *testing.T) {
	for _, c := range []struct {
		name     string
		amount   int64
		cur      string
		inFlight bool
		// body, when set, replaces the refund the provider answers with.
		body func(f *proofFixture) string
	}{
		{"a different confirmed amount", 999, "eur", false, nil},
		{"no confirmed money", 0, "", false, nil},
		{"an amount without a currency", 0, "", false, refundWithoutCurrency},
		{"a different confirmed amount, marker in flight", 999, "eur", true, nil},
		{"no confirmed money, marker in flight", 0, "", true, nil},
		{"an amount without a currency, marker in flight", 0, "", true, refundWithoutCurrency},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := captureLog(t)
			f := newProofFixture(t, nil)
			f.peer.list = emptyRefundList
			opened := make(chan *sql.Tx, 1)
			f.peer.submit = func() (int, string) {
				tx, err := f.db.BeginTx(f.ctx, nil)
				if err != nil {
					t.Errorf("begin marker transaction: %v", err)
					close(opened)
					return 500, `{"error":{"message":"fixture","type":"api_error"}}`
				}
				for _, q := range []string{
					`SELECT 1 FROM payment_operations WHERE organizer_id=$1 AND idempotency_key=$2 FOR UPDATE`,
					`SELECT 1 FROM payment_refund_legs WHERE organizer_id=$1 AND source_idempotency_key=$2 AND refund_idempotency_key=$3 FOR UPDATE`,
					`INSERT INTO payment_refund_leg_abandonments(organizer_id,source_idempotency_key,refund_idempotency_key,provider_ref)
					 VALUES($1,$2,$3,'re_failed_ours')`,
				} {
					args := []any{f.org, f.sourceKey, f.refundKey}
					if strings.Contains(q, "payment_operations") {
						args = args[:2]
					}
					if _, err := tx.ExecContext(f.ctx, q, args...); err != nil {
						t.Errorf("marker transaction: %v", err)
					}
				}
				if !c.inFlight {
					if err := tx.Commit(); err != nil {
						t.Errorf("commit the marker during the provider call: %v", err)
					}
				}
				opened <- tx
				if c.body != nil {
					return 200, c.body(f)
				}
				return 200, f.refund("re_late", "succeeded", f.pi, f.legKey, c.amount, c.cur)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- f.dispatch(t) }()
			tx, ok := <-opened
			if !ok {
				t.Fatal("the marker transaction did not open")
			}
			if c.inFlight {
				f.waitForLockWaiters(t, 1, done)
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			res := <-done

			if res.Code != 409 || res.Body.String() != `{"error":"refund leg abandoned"}`+"\n" {
				t.Fatalf("status=%d body=%s, want 409 refund leg abandoned", res.Code, res.Body.String())
			}
			out := logs.String()
			if !strings.Contains(out, "marker_provider_ref=re_failed_ours") || !strings.Contains(out, "result_provider_ref=re_late") {
				t.Fatalf("log does not carry both references:\n%s", out)
			}
			assertLegStillBound(t, f.db, f.ctx, f.org)
			if n := f.refundFacts(t); n != 0 {
				t.Fatalf("payment.refunded facts = %d, want 0", n)
			}
		})
	}
}

// refundWithoutCurrency is a succeeded refund re_late for the leg's amount whose `currency` key
// is absent. The adapter maps it to Refunded with a confirmation that fails the port's validation
// (R5-1).
func refundWithoutCurrency(f *proofFixture) string {
	return fmt.Sprintf(`{"id":"re_late","object":"refund","amount":1000,"status":"succeeded","payment_intent":%q,"metadata":{"compensation_key":%q}}`, f.pi, f.legKey)
}

// R5-1, the control. Without a marker, a Refunded answer whose confirmation fails validation
// keeps its existing refusal: 500, nothing recorded, the leg still bound.
func TestPartialRefundRefusesAnInvalidConfirmationWithoutAMarker(t *testing.T) {
	f := newProofFixture(t, nil)
	f.peer.list = emptyRefundList
	f.peer.submit = func() (int, string) { return 200, refundWithoutCurrency(f) }
	assertProofResponse(t, f.dispatch(t), 500, `{"error":"invalid provider result"}`)
	assertLegStillBound(t, f.db, f.ctx, f.org)
	if n := f.refundFacts(t); n != 0 {
		t.Fatalf("payment.refunded facts = %d, want 0", n)
	}
	if m := f.marker(t); m != (markerRow{0, ""}) {
		t.Fatalf("marker = %+v, want none", m)
	}
}

// RA5-2 (ADR-087 §13). A POST answer is not current evidence: within the key retention Stripe
// replays the original response body for the same key. The schedule: dispatch lists (nothing
// yet) and POSTs; while the POST is in flight the refund under the leg's key has failed, and the
// failure proof finds it failed; then the POST answer arrives, the cached `succeeded` body.
// Payments must re-read the refund and record nothing: no payment.refunded fact, the leg still
// bound, and the dispatch answer is the current failed state.
func TestPartialRefundDispatchRecordsNothingWhenTheCachedSuccessIsStale(t *testing.T) {
	f := newProofFixture(t, nil)
	f.peer.list = emptyRefundList
	var proof *httptest.ResponseRecorder
	f.peer.submit = func() (int, string) {
		f.peer.mu.Lock()
		f.peer.list = f.listWith("re_same", "failed")
		f.peer.mu.Unlock()
		proof = f.getValid(t)
		return 200, f.succeededRefund("re_same")
	}
	f.peer.retrieve = func(id string) (int, string) {
		return 200, f.refund(id, "failed", f.pi, f.legKey, 1000, "eur")
	}

	res := f.dispatch(t)

	if proof == nil {
		t.Fatal("the failure proof did not run during the provider call")
	}
	assertProofResponse(t, proof, 200, `{"amount":1000,"currency":"EUR","provider_ref":"re_same"}`)
	assertProofResponse(t, res, 422, `{"code":"provider_refund_failed","error":"provider refund failed","provider_ref":"re_same"}`)
	if len(f.peer.posts) != 1 {
		t.Fatalf("refund POSTs = %d, want 1", len(f.peer.posts))
	}
	if calls, _ := f.peer.snapshot(); calls[len(calls)-1] != "GET /v1/refunds/re_same" {
		t.Fatalf("provider calls = %v, want the POST answer re-read last", calls)
	}
	assertLegStillBound(t, f.db, f.ctx, f.org)
	if n := f.refundFacts(t); n != 0 {
		t.Fatalf("payment.refunded facts = %d, want 0", n)
	}
	if m := f.marker(t); m != (markerRow{0, ""}) {
		t.Fatalf("marker = %+v, want none: the outside path writes no marker", m)
	}
}

// The reverse order (D34). Dispatch holds the operation and leg locks in its completion
// transaction; the test holds the organizer's journal head, so dispatch pauses there before its
// append. The abandonment's lock-free proof then finds the stamped failed refund, and its marker
// transaction waits for the operation lock. Dispatch commits first: one fact, the leg
// refunded. The abandonment then finds a completed leg: 409 and no marker.
func TestAbandonIsRefusedWhenDispatchCommitsFirst(t *testing.T) {
	f := newProofFixture(t, nil)
	f.peer.list = emptyRefundList
	f.peer.submit = func() (int, string) { return 200, f.succeededRefund("re_ok") }
	if _, err := f.db.ExecContext(f.ctx, `INSERT INTO journal_heads(organizer_id) VALUES($1) ON CONFLICT DO NOTHING`, f.org); err != nil {
		t.Fatal(err)
	}
	head, err := f.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = head.Rollback() })
	if _, err := head.ExecContext(f.ctx, `SELECT 1 FROM journal_heads WHERE organizer_id=$1 FOR UPDATE`, f.org); err != nil {
		t.Fatal(err)
	}

	dispatched := make(chan *httptest.ResponseRecorder, 1)
	go func() { dispatched <- f.dispatch(t) }()
	f.waitForLockWaiters(t, 1, dispatched)

	f.peer.mu.Lock()
	f.peer.list = f.listWith("re_failed_ours", "failed")
	f.peer.mu.Unlock()
	abandoned := make(chan *httptest.ResponseRecorder, 1)
	go func() { abandoned <- f.abandonValid(t) }()
	f.waitForLockWaiters(t, 2, dispatched, abandoned)

	if err := head.Rollback(); err != nil {
		t.Fatal(err)
	}
	d, a := <-dispatched, <-abandoned

	if d.Code != 200 || !strings.Contains(d.Body.String(), `"status":"refunded"`) {
		t.Fatalf("dispatch: status=%d body=%s, want 200 refunded", d.Code, d.Body.String())
	}
	assertProofResponse(t, a, 409, `{"error":"refund leg completed"}`)
	if m := f.marker(t); m != (markerRow{0, ""}) {
		t.Fatalf("marker = %+v, want none", m)
	}
	if n := f.refundFacts(t); n != 1 {
		t.Fatalf("payment.refunded facts = %d, want 1", n)
	}
	var status string
	if err := f.db.QueryRowContext(f.ctx, `SELECT status FROM payment_refund_legs WHERE organizer_id=$1`, f.org).Scan(&status); err != nil || status != "refunded" {
		t.Fatalf("leg status = %q err=%v, want refunded", status, err)
	}
}
