//go:build smoke

package api

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/payments/internal/psp"
	"ticketing/services/payments/internal/store"
)

// The partial-refund endpoint against a REAL store (TKT-156). The pure state-machine
// checks live in psp_test.go; what needs a database is the property the ticket is
// actually about — a replayed refund calls the provider ONCE.
//
// The fake PSP always answers Refunded and ignores the amount, so it cannot prove a call
// count on its own. countingPSP wraps it.

const refundCredential = "partial-refund-credential"

type countingPSP struct {
	psp.PSP
	mu         sync.Mutex
	calls      int
	authorizes int
	amount     int64
	key        string
	// confirmAmount/confirmCurrency override what this stub reports having returned. Zero
	// values mean "echo the request", which is the agreement path (TKT-257).
	//
	// State the limit plainly: an echoing stub CAN NEVER DISAGREE, so nothing in this file
	// proves the fail-closed guard — these are regression cover for the paths around it.
	// The divergence proofs are in provider_confirmation_smoke_test.go, where the override
	// makes the stub answer with money the caller did not ask for.
	confirmAmount   int64
	confirmCurrency string
}

func (c *countingPSP) Refund(_ context.Context, _, idempotencyKey string, amount int64, currency string) (psp.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.amount, c.key = amount, idempotencyKey
	confirmed := psp.ConfirmedMoney{Amount: amount, Currency: currency}
	if c.confirmAmount != 0 {
		confirmed.Amount = c.confirmAmount
	}
	if c.confirmCurrency != "" {
		confirmed.Currency = c.confirmCurrency
	}
	return psp.Result{Outcome: psp.Refunded, ProviderRef: "re_" + idempotencyKey, Confirmed: &confirmed}, nil
}

func (c *countingPSP) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

var (
	refundMigrateOnce sync.Once
	refundMigrateErr  error
)

func refundDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	// Its OWN database, not the store suite's. Journal.Verify scans the whole
	// journal_entries table, so an append here under a key that suite's ring does not
	// know fails seven of its tests — and `go test ./internal/...` runs the two packages
	// concurrently, so the append can land mid-scan. scripts/smoke.sh creates it.
	dsn := os.Getenv("PAYMENTS_API_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PAYMENTS_API_TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	refundMigrateOnce.Do(func() { refundMigrateErr = store.Migrate(ctx, db) })
	if refundMigrateErr != nil {
		t.Fatalf("migrate payments test database: %v", refundMigrateErr)
	}
	return db, ctx
}

// refundServer wires the real store and router against a counting PSP.
func refundServer(t *testing.T, org uuid.UUID, sourceKey string, captured int64) (http.Handler, *countingPSP) {
	t.Helper()
	db, ctx := refundDB(t)
	ring, err := store.NewKeyring("refund-key", []byte("payments-refund-leg-key-0123456"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_operations(organizer_id,idempotency_key,request_fingerprint,status,order_id,buyer_id,
		                               request_amount,request_currency,provider_payment_ref,provider_state,
		                               authorized_amount,captured_amount,provider_state_at)
		VALUES($1,$2,'fingerprint','captured',$3,$4,$5,'EUR','pi_test','captured',$5,$5,now())`,
		org, sourceKey, uuid.New(), uuid.New(), captured); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM payment_refund_legs WHERE organizer_id=$1`, org)
		_, _ = db.Exec(`DELETE FROM payment_operations WHERE organizer_id=$1`, org)
	})
	provider := &countingPSP{PSP: psp.NewFake()}
	return newTestServerWithPSP(store.New(db, ring), refundCredential, provider).Router(nil, true), provider
}

func postRefundLeg(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/psp/partial-refund", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", refundCredential)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

// AC3, payments half: a replay issues exactly one provider refund and appends exactly one
// compensating fact.
func TestPartialRefundReplayCallsProviderOnce(t *testing.T) {
	org, sourceKey := uuid.New(), "leg-replay"
	h, provider := refundServer(t, org, sourceKey, 2500)
	body := `{"organizer_id":"` + org.String() + `","idempotency_key":"` + sourceKey + `","refund_key":"refund-1","amount":1250,"currency":"EUR"}`

	first := postRefundLeg(t, h, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first refund: status=%d body=%s", first.Code, first.Body.String())
	}
	second := postRefundLeg(t, h, body)
	if second.Code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%s", second.Code, second.Body.String())
	}
	if provider.count() != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.count())
	}
	if provider.amount != 1250 {
		t.Fatalf("provider amount = %d, want the leg amount 1250", provider.amount)
	}
	if provider.key != store.RefundLegKey(org, sourceKey, "refund-1") {
		t.Fatalf("provider idempotency key = %q, want the derived leg key", provider.key)
	}

	db, ctx := refundDB(t)
	var facts int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM journal_entries WHERE organizer_id=$1 AND fact_type='payment.refunded'`, org).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if facts != 1 {
		t.Fatalf("payment.refunded facts = %d, want 1", facts)
	}
}

// Two legs against one charge are two provider refunds and two facts — the capability the
// whole-refund compensation path cannot express.
func TestTwoPartialRefundLegsAgainstOneCharge(t *testing.T) {
	org, sourceKey := uuid.New(), "leg-two"
	h, provider := refundServer(t, org, sourceKey, 2500)
	for _, leg := range []struct{ key, amount string }{{"refund-1", "1500"}, {"refund-2", "1000"}} {
		res := postRefundLeg(t, h, `{"organizer_id":"`+org.String()+`","idempotency_key":"`+sourceKey+`","refund_key":"`+leg.key+`","amount":`+leg.amount+`,"currency":"EUR"}`)
		if res.Code != http.StatusOK {
			t.Fatalf("leg %s: status=%d body=%s", leg.key, res.Code, res.Body.String())
		}
	}
	if provider.count() != 2 {
		t.Fatalf("provider calls = %d, want 2", provider.count())
	}
	// The third exceeds the capture and must be refused before any provider call.
	res := postRefundLeg(t, h, `{"organizer_id":"`+org.String()+`","idempotency_key":"`+sourceKey+`","refund_key":"refund-3","amount":1,"currency":"EUR"}`)
	if res.Code != http.StatusConflict {
		t.Fatalf("over-capture leg: status=%d body=%s", res.Code, res.Body.String())
	}
	if provider.count() != 2 {
		t.Fatalf("a refused leg must not reach the provider: calls = %d", provider.count())
	}
}

// The endpoint is internal: no token, no answer — and it answers 401 like every other
// payments internal operation, not 404 (that is commerce's convention).
func TestPartialRefundRequiresInternalToken(t *testing.T) {
	org, sourceKey := uuid.New(), "leg-auth"
	h, provider := refundServer(t, org, sourceKey, 2500)
	req := httptest.NewRequest(http.MethodPost, "/internal/psp/partial-refund",
		bytes.NewBufferString(`{"organizer_id":"`+org.String()+`","idempotency_key":"`+sourceKey+`","refund_key":"r","amount":1,"currency":"EUR"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", res.Code)
	}
	if provider.count() != 0 {
		t.Fatal("an unauthorized request must not reach the provider")
	}
}

// A leg whose amount would overflow the running total is refused at the endpoint as a
// CONFLICT, and never reaches the provider (TKT-297).
//
// The store test proves the ceiling holds; this proves the refusal survives the trip out.
// Both halves matter and neither implies the other. Unchecked, the addition wraps negative
// and the leg is ACCEPTED — 200, a provider refund for more money than was captured, and a
// payment.refunded fact to match. And a fix that refuses with any error other than
// ErrRefundExceedsCapture would be answered 500 by the handler's classifier rather than
// 409, turning a caller error into an apparent payments fault — which is why the status is
// asserted and not merely "not 200".
//
// The amount is MaxInt64, the largest value the contract admits: at the boundary rather
// than beyond it, so this exercises the store's guard and not the decoder's. A value above
// MaxInt64 fails to unmarshal into the int64 field and is answered 400 by decode() with or
// without any of this ticket's changes.
func TestPartialRefundOverflowIsRefusedNotAccepted(t *testing.T) {
	org, sourceKey := uuid.New(), "leg-overflow"
	h, provider := refundServer(t, org, sourceKey, 2500)

	first := postRefundLeg(t, h, `{"organizer_id":"`+org.String()+`","idempotency_key":"`+sourceKey+`","refund_key":"refund-1","amount":1250,"currency":"EUR"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first refund: status=%d body=%s", first.Code, first.Body.String())
	}

	// Three amounts that each break the rule, reaching it by two different routes. MaxInt64
	// and MaxInt64-1 overflow the running total, so the unchecked addition wraps and admits
	// them. MaxInt64-1250 does NOT overflow — added to the 1250 already bound it lands
	// exactly on MaxInt64 — so it is refused by the ceiling comparison itself, over a
	// capture of 2500. Both routes must give the same answer, and a guard special-casing the
	// largest values satisfies neither all three nor the pair that wraps.
	for _, amount := range []string{"9223372036854775807", "9223372036854775806", "9223372036854774557"} {
		res := postRefundLeg(t, h, `{"organizer_id":"`+org.String()+`","idempotency_key":"`+sourceKey+`","refund_key":"refund-2","amount":`+amount+`,"currency":"EUR"}`)
		if res.Code != http.StatusConflict {
			t.Fatalf("a leg of %s overflows the running total and must be refused with 409, "+
				"got status=%d body=%s", amount, res.Code, res.Body.String())
		}
	}
	if provider.count() != 1 {
		t.Fatalf("a refused leg must not reach the provider: calls = %d, want the 1 from the "+
			"legitimate leg", provider.count())
	}

	// The rule, read back: only the legitimate leg was committed, and no compensating fact
	// was appended for the refused one.
	db, ctx := refundDB(t)
	var total sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT sum(amount) FROM payment_refund_legs WHERE organizer_id=$1 AND source_idempotency_key=$2`, org, sourceKey).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total.Int64 != 1250 {
		t.Fatalf("bound legs total %d, want only the legitimate 1250", total.Int64)
	}
	var facts int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM journal_entries WHERE organizer_id=$1 AND fact_type='payment.refunded'`, org).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if facts != 1 {
		t.Fatalf("payment.refunded facts = %d, want 1", facts)
	}
}
