//go:build smoke

package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// TKT-319. Checkout resumed an order whose recovery claim had EXPIRED, and did not fence it off.
//
// Before the fix, claimOrder refused only a LIVE lease (server.go:1392-1394). An expired lease
// left recovery_claim_id set. The claim-fenced writes compare the claim id, not the lease:
// ReleaseStuckOrder matches `id AND recovery_claim_id` (store/recovery.go:152), and so do other
// statements in store/recovery.go, including ParkForReconciliation (:197). Some recovery writes
// check no claim at all: RecordOrderFact and CompleteOrder (store/recovery.go:357-364;
// store/store.go:118, :132). So a worker whose lease had lapsed could still park the order after
// checkout had read it as unparked and committed. The fix clears such a claim under the same row
// lock (server.go:1400-1404), so the stale worker's claim-fenced writes match no row (ADR-016, TKT-319).
//
// THE SCHEDULE IS FORCED. afterCheckoutClaim parks checkout after claimOrder has committed and
// before the park-marker guard (server.go:1813). The test then makes the stale claimant's write
// with the OLD token, reads the row, and only then lets checkout continue. Nothing sleeps. The
// waits in the test body have deadlines. Cleanup cancels the request and joins it before the
// database closes.
//
// THE TIER IS THE POINT. The row lock, the claim fencing and the park marker exist only in
// PostgreSQL. These tests run the real handler and the real store function against a real
// database. A fake would not show whether the stale write matched a row.

// Buyer fields for every checkout in this file. The fingerprint is built from them, so each
// request must send these exact values.
const (
	staleBuyerName  = "Stale Buyer"
	staleBuyerEmail = "stale@example.test"
	staleBuyerToken = "pm_stale_instrument"
)

// staleOrder is one order that checkout is about to resume, with a recovery claim on it.
type staleOrder struct {
	order, claim uuid.UUID
	key          string
}

// staleLeaseShape is how the claim's lease is left on the row.
type staleLeaseShape struct {
	name  string
	lease sql.NullTime
}

// staleLeaseShapes are the two unleased states the steal must take: a lease that has lapsed, and
// a NULL lease. The writers in store/recovery.go set or clear the lease in the same statement as
// the claim (ClaimStuckOrders at :90-91, ReleaseStuckOrder at :146-147), so the NULL-lease case
// is defensive. recovery's own claim predicate admits it (store/recovery.go:81).
func staleLeaseShapes() []staleLeaseShape {
	return []staleLeaseShape{
		{name: "expired lease", lease: expiredLease()},
		{name: "NULL lease", lease: sql.NullTime{}},
	}
}

// expiredLease is a lease that lapsed a minute ago by the Go clock. claimOrder compares the lease
// with time.Now(), so the fixture uses the same clock.
func expiredLease() sql.NullTime {
	return sql.NullTime{Time: time.Now().Add(-time.Minute), Valid: true}
}

// TKT-319 COS2. A stale claimant's write must not match an order that checkout has taken over.
//
// The row has an expired or NULL lease, an attempt budget one short of parking, and no park
// marker. While checkout is parked at the seam, the stale claimant releases with the old token.
// Before the fix that release matches and parks the row. After the steal the claim is already gone, so
// the release matches nothing and returns ErrRecoveryConflict.
func TestAStaleClaimantCannotParkAnOrderCheckoutTookOverFromAnExpiredClaim(t *testing.T) {
	for _, status := range parkableResumableStatuses {
		for _, shape := range staleLeaseShapes() {
			t.Run(status+" "+shape.name, func(t *testing.T) {
				db, ctx := exchangeAPIDB(t)
				_, reservation := seedCheckoutable(t, db, ctx)
				inventory, payments := staleStubs(t)
				srv := newTestServer(db, http.DefaultClient, "", inventory.server.URL, payments.server.URL, "tok")
				so := seedStaleClaim(t, db, ctx, srv, reservation, status, shape.lease, commercestore.MaxRecoveryAttempts-1, false)
				seeded := readRecoveryRow(t, db, ctx, so.order)
				assertStaleSeed(t, seeded, status, so.claim, shape.lease, commercestore.MaxRecoveryAttempts-1, false)

				// The request runs under its own context. Cleanup cancels it, so a request that is still
				// running stops at its next call that takes the request context.
				reqCtx, cancelReq := context.WithCancel(context.Background())

				// The seam. Checkout signals once after claimOrder commits, then waits for permission.
				// The wait also ends when the request is cancelled.
				reached := make(chan struct{}, 1)
				proceed := make(chan struct{})
				var openOnce sync.Once
				openSeam := func() { openOnce.Do(func() { close(proceed) }) }
				srv.afterCheckoutClaim = func() {
					select {
					case reached <- struct{}{}:
					default:
					}
					select {
					case <-proceed:
					case <-reqCtx.Done():
					}
				}

				r := chi.NewRouter()
				r.Post("/reservations/{id}/checkout", srv.checkout)
				var rec *httptest.ResponseRecorder
				done := make(chan struct{})
				// Registered after the stubs and the fixtures, so it runs before them. It cancels the request
				// and then joins it. The join has no deadline on purpose. The checkout path passes the request
				// context to its database and HTTP calls, so a cancelled request returns at its next such call,
				// and the fixtures must not be torn down while a handler may still be running.
				t.Cleanup(func() {
					cancelReq()
					<-done
				})
				go func() {
					defer close(done)
					rec = staleCheckout(reqCtx, r, reservation, so.key)
				}()

				// The waits in the test body have deadlines.
				waitCtx, cancelWait := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancelWait()
				select {
				case <-reached:
				case <-done:
					t.Fatalf("checkout answered %d before the seam: %s", rec.Code, rec.Body.String())
				case <-waitCtx.Done():
					t.Fatal("checkout never reached the seam")
				}

				// Parked after claimOrder committed and before the guard. The budget must be untouched:
				// taking a claim spends no attempt and moves no schedule.
				atSeam := readRecoveryRow(t, db, ctx, so.order)
				if changed := budgetChanges(seeded, atSeam); len(changed) > 0 || atSeam.status != status {
					t.Fatalf("claimOrder changed the row before the guard: status %q, budget %v", atSeam.status, changed)
				}

				// The stale claimant. It holds the OLD token, and this release is the write that parks.
				parked, err := commercestore.ReleaseStuckOrder(ctx, db, so.order, so.claim,
					errors.New("stale claimant: lease lapsed during an external call"))
				afterStale := readRecoveryRow(t, db, ctx, so.order)
				openSeam()

				if !errors.Is(err, commercestore.ErrRecoveryConflict) {
					t.Fatalf("a release with the OLD claim token returned err=%v (parked=%v), want ErrRecoveryConflict. "+
						"Checkout had committed claimOrder while the expired claim was still on the row, "+
						"so the stale claimant still owned it.", err, parked)
				}
				if parked {
					t.Fatal("the stale release parked an order checkout had taken over; a parked row is never claimed again (store/recovery.go:79)")
				}
				if afterStale.claim.Valid || afterStale.lease.Valid {
					t.Fatalf("the expired claim survived checkout: claim=%v lease=%v", afterStale.claim, afterStale.lease)
				}
				if changed := budgetChanges(atSeam, afterStale); len(changed) > 0 {
					t.Fatalf("the stale release changed a row it did not own: %v", changed)
				}

				// The stale claimant was refused, so the buyer's checkout must complete the order.
				select {
				case <-done:
				case <-waitCtx.Done():
					t.Fatal("checkout did not finish after the seam was released")
				}
				if rec.Code != http.StatusOK {
					t.Fatalf("checkout answered %d, want 200: %s", rec.Code, rec.Body.String())
				}
				if got := inventory.count("finalize"); got != 1 {
					t.Fatalf("inventory finalizes = %d, want 1", got)
				}
				if got := inventory.count("confirm"); got != 1 {
					t.Fatalf("inventory confirms = %d, want 1", got)
				}
				if got := payments.count("charges"); got != 1 {
					t.Fatalf("payment charges = %d, want 1", got)
				}
				assertOrderState(t, db, ctx, so.order, "completed", false)
				assertResumeWrites(t, db, ctx, so.order, reservation)
			})
		}
	}
}

// TKT-319 COS4. An expired claim left by a worker that died must not refuse the buyer. No stale
// writer runs here. Checkout's own claimOrder takes the claim, and checkout still completes the
// order, as it did before this ticket.
func TestAnExpiredClaimFromADeadWorkerStillResumesCheckout(t *testing.T) {
	for _, status := range parkableResumableStatuses {
		for _, shape := range staleLeaseShapes() {
			t.Run(status+" "+shape.name, func(t *testing.T) {
				db, ctx := exchangeAPIDB(t)
				_, reservation := seedCheckoutable(t, db, ctx)
				inventory, payments := staleStubs(t)
				srv := newTestServer(db, http.DefaultClient, "", inventory.server.URL, payments.server.URL, "tok")
				so := seedStaleClaim(t, db, ctx, srv, reservation, status, shape.lease, commercestore.MaxRecoveryAttempts-1, false)
				assertStaleSeed(t, readRecoveryRow(t, db, ctx, so.order), status, so.claim, shape.lease, commercestore.MaxRecoveryAttempts-1, false)

				r := chi.NewRouter()
				r.Post("/reservations/{id}/checkout", srv.checkout)
				rec := submitCheckout(t, r, reservation, staleBuyerName, staleBuyerEmail, staleBuyerToken, so.key)

				if rec.Code != http.StatusOK {
					t.Fatalf("a dead worker's expired claim refused the buyer: checkout answered %d, want 200: %s",
						rec.Code, rec.Body.String())
				}
				if got := inventory.count("finalize"); got != 1 {
					t.Fatalf("inventory finalizes = %d, want 1", got)
				}
				if got := inventory.count("confirm"); got != 1 {
					t.Fatalf("inventory confirms = %d, want 1", got)
				}
				if got := payments.count("charges"); got != 1 {
					t.Fatalf("payment charges = %d, want 1", got)
				}
				assertOrderState(t, db, ctx, so.order, "completed", false)
			})
		}
	}
}

// TKT-319 COS3. A LIVE lease still refuses checkout, and the steal does not touch it.
//
// The steal takes only a claim whose lease has lapsed. A claimant that is still working keeps its
// order. The buyer gets the existing 409, and neither inventory nor payments is called.
func TestALiveRecoveryLeaseStillRefusesCheckoutAndKeepsItsClaim(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	_, reservation := seedCheckoutable(t, db, ctx)
	inventory, payments := staleStubs(t)
	srv := newTestServer(db, http.DefaultClient, "", inventory.server.URL, payments.server.URL, "tok")
	live := sql.NullTime{Time: time.Now().Add(10 * time.Minute), Valid: true}
	so := seedStaleClaim(t, db, ctx, srv, reservation, "created", live, 0, false)
	before := readRecoveryRow(t, db, ctx, so.order)
	assertStaleSeed(t, before, "created", so.claim, live, 0, false)

	r := chi.NewRouter()
	r.Post("/reservations/{id}/checkout", srv.checkout)
	rec := submitCheckout(t, r, reservation, staleBuyerName, staleBuyerEmail, staleBuyerToken, so.key)

	if rec.Code != http.StatusConflict {
		t.Fatalf("checkout on a live recovery lease answered %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var answer map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("response body is not JSON: %s", rec.Body.String())
	}
	if got := answer["error"]; got != "this order is being recovered; retry shortly" {
		t.Fatalf("the live-lease refusal answered %v, want the recovery message: %s", got, rec.Body.String())
	}
	if got := inventory.count("any"); got != 0 {
		t.Fatalf("inventory received %d requests for an order a live lease holds; the refusal must precede every call", got)
	}
	if got := payments.count("any"); got != 0 {
		t.Fatalf("payments received %d requests for an order a live lease holds", got)
	}
	after := readRecoveryRow(t, db, ctx, so.order)
	if after.claim != before.claim || !after.lease.Valid || !after.lease.Time.Equal(before.lease.Time) {
		t.Fatalf("a live claim was disturbed: claim %v -> %v, lease %v -> %v",
			before.claim, after.claim, before.lease, after.lease)
	}
	if changed := budgetChanges(before, after); len(changed) > 0 || after.status != "created" {
		t.Fatalf("the refused checkout changed the row: status %q, budget %v", after.status, changed)
	}
}

// TKT-319. An already-parked order keeps its park marker when checkout takes its expired claim.
//
// The steal clears the claim and the lease and nothing else. The parked check that follows still
// answers 409 before any money call (TKT-292), as it did before the fix.
//
// The claim on a parked row is a fixture. The writers that park a row also clear the claim in the
// same statement (ReleaseStuckOrder at store/recovery.go:147, ParkForReconciliation at :193), so
// a parked row with a claim is not a state the runner produces. The fixture reaches the steal
// branch for a parked row, which is the case the ticket names.
func TestAParkedOrderKeepsItsMarkerWhenCheckoutTakesItsExpiredClaim(t *testing.T) {
	for _, status := range parkableResumableStatuses {
		t.Run(status, func(t *testing.T) {
			db, ctx := exchangeAPIDB(t)
			_, reservation := seedCheckoutable(t, db, ctx)
			inventory, payments := staleStubs(t)
			srv := newTestServer(db, http.DefaultClient, "", inventory.server.URL, payments.server.URL, "tok")
			lease := expiredLease()
			so := seedStaleClaim(t, db, ctx, srv, reservation, status, lease, commercestore.MaxRecoveryAttempts, true)
			before := readRecoveryRow(t, db, ctx, so.order)
			assertStaleSeed(t, before, status, so.claim, lease, commercestore.MaxRecoveryAttempts, true)

			r := chi.NewRouter()
			r.Post("/reservations/{id}/checkout", srv.checkout)
			rec := submitCheckout(t, r, reservation, staleBuyerName, staleBuyerEmail, staleBuyerToken, so.key)

			if got := inventory.count("any"); got != 0 {
				t.Fatalf("inventory received %d requests for a PARKED %s order", got, status)
			}
			if got := payments.count("any"); got != 0 {
				t.Fatalf("payments received %d requests for a PARKED %s order", got, status)
			}
			if rec.Code != http.StatusConflict {
				t.Fatalf("a parked %s order answered %d, want 409: %s", status, rec.Code, rec.Body.String())
			}
			var answer map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
				t.Fatalf("response body is not JSON: %s", rec.Body.String())
			}
			if got := answer["error"]; got != "order awaiting payment reconciliation" {
				t.Fatalf("a parked order must tell the buyer a human must act, got %v: %s", got, rec.Body.String())
			}
			if got := answer["status"]; got != status {
				t.Fatalf("the answer must echo the durable status %q, got %v", status, got)
			}
			after := readRecoveryRow(t, db, ctx, so.order)
			// The steal runs for a parked row too, so the claim id and the lease must be NULL after the refusal.
			if after.claim.Valid {
				t.Fatalf("the claim id survived checkout on a parked order: %v; want NULL", after.claim.UUID)
			}
			if after.lease.Valid {
				t.Fatalf("the recovery lease survived checkout on a parked order: %v; want NULL", after.lease.Time)
			}
			if !after.parked.Valid {
				t.Fatal("the steal cleared the park marker of a parked order")
			}
			if changed := budgetChanges(before, after); len(changed) > 0 || after.status != status {
				t.Fatalf("the parked refusal changed the row: status %q, budget %v", after.status, changed)
			}
		})
	}
}

// recoveryRow is the recovery state of one order, read back from the database.
type recoveryRow struct {
	status      string
	claim       uuid.NullUUID
	lease       sql.NullTime
	attempts    int
	nextAttempt time.Time
	lastError   sql.NullString
	parked      sql.NullTime
}

func readRecoveryRow(t *testing.T, db *sql.DB, ctx context.Context, order uuid.UUID) recoveryRow {
	t.Helper()
	var r recoveryRow
	if err := db.QueryRowContext(ctx, `
		SELECT status,recovery_claim_id,recovery_lease_until,recovery_attempts,
		       recovery_next_attempt_at,recovery_last_error,recovery_parked_at
		FROM orders WHERE id=$1`, order).
		Scan(&r.status, &r.claim, &r.lease, &r.attempts, &r.nextAttempt, &r.lastError, &r.parked); err != nil {
		t.Fatal(err)
	}
	return r
}

// budgetChanges names the fields of the attempt budget that differ between two reads: the attempt
// count, the schedule, the last error and the park marker. The claim and the lease are left out,
// because the steal is allowed to change them. Taking a claim spends no attempt (ClaimStuckOrders
// sets only the lease and the claim, store/recovery.go:89-91).
func budgetChanges(before, after recoveryRow) []string {
	var out []string
	if before.attempts != after.attempts {
		out = append(out, fmt.Sprintf("recovery_attempts %d -> %d", before.attempts, after.attempts))
	}
	if !before.nextAttempt.Equal(after.nextAttempt) {
		out = append(out, fmt.Sprintf("recovery_next_attempt_at %v -> %v", before.nextAttempt, after.nextAttempt))
	}
	if before.lastError != after.lastError {
		out = append(out, fmt.Sprintf("recovery_last_error %q -> %q", before.lastError.String, after.lastError.String))
	}
	if before.parked.Valid != after.parked.Valid || !before.parked.Time.Equal(after.parked.Time) {
		out = append(out, fmt.Sprintf("recovery_parked_at %v -> %v", before.parked, after.parked))
	}
	return out
}

// staleFingerprint is executeCheckout's fingerprint for the buyer above (server.go, executeCheckout).
// claimOrder compares it on a replay, so the seeded order must carry the value the request sends.
func staleFingerprint(reservation uuid.UUID) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%s\n%s",
		reservation, staleBuyerName, strings.ToLower(staleBuyerEmail), staleBuyerToken))))
}

// staleCheckout sends one checkout through the real router under ctx. A test that may stop early
// passes a cancellable context, so cleanup can end a request that is still running. The buyer fields
// are the ones the seeded fingerprint was built from.
func staleCheckout(ctx context.Context, r chi.Router, reservation uuid.UUID, key string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"reservation_id":%q,"name":%q,"email":%q,"payment_token":%q}`,
		reservation, staleBuyerName, staleBuyerEmail, staleBuyerToken)
	req := httptest.NewRequest(http.MethodPost, "/reservations/"+reservation.String()+"/checkout", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

// seedStaleClaim mints the order through the REAL claimOrder, then writes the recovery state a
// claimant leaves behind: a claim, a lease, an attempt count and an optional park marker. claimOrder
// is called directly, not through the handler, so no orchestration runs and no stub sees a request.
// The caller reads the row back and proves each field before checkout runs (assertStaleSeed).
func seedStaleClaim(t *testing.T, db *sql.DB, ctx context.Context, srv *Server, reservation uuid.UUID,
	status string, lease sql.NullTime, attempts int, parked bool) staleOrder {
	t.Helper()
	x, err := srv.load(ctx, reservation)
	if err != nil {
		t.Fatal(err)
	}
	so := staleOrder{key: "stale-" + uuid.NewString(), claim: uuid.New()}
	so.order, _, _, _, err = srv.claimOrder(ctx, x, so.key, staleFingerprint(reservation), uuid.NullUUID{})
	if err != nil {
		t.Fatalf("claimOrder minted no order: %v", err)
	}
	var parkedAt sql.NullTime
	if parked {
		parkedAt = sql.NullTime{Time: time.Now(), Valid: true}
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE orders SET status=$2, recovery_claim_id=$3, recovery_lease_until=$4,
		    recovery_attempts=$5, recovery_last_error='prior attempt lost its lease',
		    recovery_next_attempt_at=$6, recovery_parked_at=$7
		WHERE id=$1`,
		so.order, status, so.claim, lease, attempts, time.Now().Add(-3*time.Minute), parkedAt); err != nil {
		t.Fatal(err)
	}
	return so
}

// assertStaleSeed reads the seeded state back and fails unless it is the state the test names. A
// seed that wrote nothing would let every later assertion pass for the wrong reason.
func assertStaleSeed(t *testing.T, row recoveryRow, status string, claim uuid.UUID, lease sql.NullTime, attempts int, parked bool) {
	t.Helper()
	if row.status != status {
		t.Fatalf("fixture status = %q, want %q", row.status, status)
	}
	if !row.claim.Valid || row.claim.UUID != claim {
		t.Fatalf("fixture claim = %v, want %v", row.claim, claim)
	}
	if row.lease.Valid != lease.Valid {
		t.Fatalf("fixture lease valid = %v, want %v", row.lease.Valid, lease.Valid)
	}
	if lease.Valid && row.lease.Time.Before(time.Now()) != lease.Time.Before(time.Now()) {
		t.Fatalf("fixture lease %v is on the wrong side of now", row.lease.Time)
	}
	if row.attempts != attempts {
		t.Fatalf("fixture recovery_attempts = %d, want %d", row.attempts, attempts)
	}
	if row.parked.Valid != parked {
		t.Fatalf("fixture recovery_parked_at valid = %v, want %v", row.parked.Valid, parked)
	}
}

// staleStubs answers inventory and payments with success and counts what reached them. The "any"
// counters are the ones a refusal must leave at zero.
func staleStubs(t *testing.T) (inventory, payments *countingStub) {
	t.Helper()
	inventory = newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
		c.hit("any")
		switch {
		case strings.HasSuffix(r.URL.Path, "/finalize"):
			c.hit("finalize")
		case strings.HasSuffix(r.URL.Path, "/confirm"):
			c.hit("confirm")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	payments = newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
		c.hit("any")
		if strings.HasSuffix(r.URL.Path, "/internal/charges") {
			c.hit("charges")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"captured","fact_id":"` + uuid.NewString() + `"}`))
	})
	return inventory, payments
}

// assertResumeWrites reads back the rows a resumed checkout leaves behind: the order.created fact
// and the buyer's PII. The stub counts show that calls were made. These show what was written.
func assertResumeWrites(t *testing.T, db *sql.DB, ctx context.Context, order, reservation uuid.UUID) {
	t.Helper()
	var created int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM order_facts WHERE order_id=$1 AND fact_type='order.created'`, order).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("order.created facts = %d, want 1", created)
	}
	var pii int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM buyer_pii
		WHERE buyer_id=(SELECT buyer_id FROM reservations WHERE id=$1) AND name=$2 AND email=$3`,
		reservation, staleBuyerName, staleBuyerEmail).Scan(&pii); err != nil {
		t.Fatal(err)
	}
	if pii != 1 {
		t.Fatalf("buyer_pii rows for the buyer = %d, want 1", pii)
	}
}
