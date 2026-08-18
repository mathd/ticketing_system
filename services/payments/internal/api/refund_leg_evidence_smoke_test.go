//go:build smoke

package api

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/payments/internal/store"
)

// Read-only refund-leg evidence (TKT-168). The write path (TKT-156, ADR-037) already
// records a leg's amount, currency and completion; nothing could READ it, so a downgrade
// whose refund call was replaced by a successful no-op passed every assertion.
//
// Same tier argument as evidence_smoke_test.go: the projection from a durable row to the
// wire is the mechanism, so the store is real.

// seedRefundLeg inserts one leg directly. The completed shape carries completed_at and
// fact_id because the table's CHECK constraint refuses a 'refunded' row without them —
// "completed" may never mean money moved with nothing in the trail saying so.
func seedRefundLeg(t *testing.T, db *sql.DB, org uuid.UUID, sourceKey, refundKey string, amount int64, currency string, completed bool) {
	t.Helper()
	status, providerRef := "bound", "re_must_not_leak_"+refundKey
	var factID any
	var completedAt any
	if completed {
		status, factID, completedAt = "refunded", uuid.New(), "now()"
	}
	// completed_at is a literal now() or NULL; it cannot be parameterized alongside the
	// CHECK without a CASE, so the two shapes are two statements.
	q := `INSERT INTO payment_refund_legs(organizer_id,source_idempotency_key,refund_idempotency_key,
	                                      provider_idempotency_key,amount,currency,status,provider_ref,fact_id,completed_at)
	      VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULL)`
	if completedAt != nil {
		q = strings.Replace(q, "NULL)", "now())", 1)
	}
	if _, err := db.ExecContext(context.Background(), q, org, sourceKey, refundKey,
		store.RefundLegKey(org, sourceKey, refundKey), amount, currency, status, providerRef, factID); err != nil {
		t.Fatal(err)
	}
}

// A refund leg reports how much it gave back, in which currency, and whether it has
// actually settled.
//
// Two legs are seeded with DIFFERENT amounts and opposite completion, and each is asserted
// separately: an implementation that reads the wrong row, or derives completion from the
// wrong column, changes exactly one of these answers. Deleting either seed turns its own
// case into a 404, so neither seed is decoration.
func TestRefundLegEvidenceReportsAmountCurrencyAndCompletion(t *testing.T) {
	org, sourceKey := uuid.New(), "leg-evidence"
	h, db := evidenceServer(t, evidenceOperation{org: org, key: sourceKey, request: 2000, captured: 2000,
		currency: "EUR", state: "captured"})
	seedRefundLeg(t, db, org, sourceKey, "settled", 1250, "EUR", true)
	seedRefundLeg(t, db, org, sourceKey, "in-flight", 300, "EUR", false)

	for _, tc := range []struct {
		refundKey string
		amount    float64
		completed bool
	}{
		{"settled", 1250, true},
		{"in-flight", 300, false},
	} {
		res := getEvidence(t, h, refundLegPath(org, sourceKey, tc.refundKey), evidenceCredential)
		if res.Code != http.StatusOK {
			t.Fatalf("leg %s: status=%d body=%s", tc.refundKey, res.Code, res.Body.String())
		}
		body := decodeEvidence(t, res)
		if body["amount"] != tc.amount {
			t.Fatalf("leg %s amount = %v, want %v", tc.refundKey, body["amount"], tc.amount)
		}
		if body["currency"] != "EUR" {
			t.Fatalf("leg %s currency = %v, want EUR", tc.refundKey, body["currency"])
		}
		if body["completed"] != tc.completed {
			t.Fatalf("leg %s completed = %v, want %v", tc.refundKey, body["completed"], tc.completed)
		}
	}
}

// Refund-leg evidence names money; it never names the provider operation that moved it.
//
// The leg's provider_idempotency_key and provider_ref are the two provider-identifying
// columns on the row, and BOTH are filled with markers before the read — the position that
// could leak is the position under test.
func TestRefundLegEvidenceCarriesNoProviderIdentity(t *testing.T) {
	org, sourceKey, refundKey := uuid.New(), "leg-neutral", "settled"
	h, db := evidenceServer(t, evidenceOperation{org: org, key: sourceKey, request: 2000, captured: 2000,
		currency: "EUR", state: "captured"})
	seedRefundLeg(t, db, org, sourceKey, refundKey, 1250, "EUR", true)

	res := getEvidence(t, h, refundLegPath(org, sourceKey, refundKey), evidenceCredential)
	if res.Code != http.StatusOK {
		t.Fatalf("leg evidence: status=%d body=%s", res.Code, res.Body.String())
	}
	for _, secret := range []string{"re_must_not_leak_" + refundKey, store.RefundLegKey(org, sourceKey, refundKey)} {
		if strings.Contains(res.Body.String(), secret) {
			t.Fatalf("provider identity %q crossed the evidence boundary: %s", secret, res.Body.String())
		}
	}
}

// A refund leg is answered only when the organizer, the source key and the refund key all
// name the same row.
//
// Three cases, one per predicate, each satisfying the other two — so deleting any single
// predicate independently turns a 404 into a 200. A leg is the only record here addressed
// by three parts, which is why it needs three cases rather than the operation's two.
func TestRefundLegEvidenceIsScopedByAllThreeKeys(t *testing.T) {
	org, sourceKey, refundKey := uuid.New(), "leg-scope", "settled"
	other := uuid.New()
	h, db := evidenceServer(t, evidenceOperation{org: org, key: sourceKey, request: 2000, captured: 2000,
		currency: "EUR", state: "captured"})
	seedRefundLeg(t, db, org, sourceKey, refundKey, 1250, "EUR", true)

	for _, tc := range []struct {
		name string
		path string
	}{
		{"another organizer, both keys correct", refundLegPath(other, sourceKey, refundKey)},
		{"correct organizer and refund key, wrong source key", refundLegPath(org, sourceKey+"-absent", refundKey)},
		{"correct organizer and source key, wrong refund key", refundLegPath(org, sourceKey, refundKey+"-absent")},
	} {
		res := getEvidence(t, h, tc.path, evidenceCredential)
		if res.Code != http.StatusNotFound {
			t.Fatalf("%s: status=%d body=%s, want 404", tc.name, res.Code, res.Body.String())
		}
	}
}
