//go:build smoke

package smoke_test

// Operator outcomes for failed post-purchase refunds, through the running stack (TKT-525,
// ADR-087).
//
// The fake PSP fails the FIRST refund of a `fake-refund-fail-once` charge and remembers it, so
// payments can prove that failure. Everything else is the real path: the staff refund, the
// operator command run inside the commerce container, both databases, the reversal reconciler,
// and the bulk cancellation runner.
//
// Coverage (ADR-030): the operator command calls payments from inside the stack, where the
// chokepoints cannot see it. So each test also calls the payments operation directly, through
// internalJSON, and that call is what records the operation.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// refundFailsOnceToken mirrors fakepsp.TokenRefundFailsOnce (shared/go/fakepsp). The smoke
// module does not import the shared module.
const refundFailsOnceToken = "fake-refund-fail-once"

// outcomeOrderAmount is two tickets at publishedSlot's 2500 EUR.
const outcomeOrderAmount = 5000

// failedRefund is a two-ticket order whose staff refund of both tickets the provider failed.
type failedRefund struct {
	slot, order string
	// sourceKey is the order's checkout key: the payments operation the leg refunds.
	sourceKey string
	// refundKey is the staff Idempotency-Key; refundID is the commerce refund, which is also
	// the payments leg's refund key.
	refundKey, refundID string
}

// failedStaffRefund buys two tickets with the fail-once token and refunds both. Payments
// answers the provider's terminal failure and commerce keeps the refund pending.
func failedStaffRefund(t *testing.T, tag string) failedRefund {
	t.Helper()
	slot, tt := publishedSlot(t, "Outcome Hall "+tag, 10)
	f := failedRefund{slot: slot, refundKey: "outcome-refund-" + slot}
	// buyOneWithToken waits for issuance, so the reversal later voids real tickets.
	f.order = buyOneWithToken(t, slot, tt, "outcome-"+tag+"-"+slot, 2, refundFailsOnceToken)

	code, body := commerceStaffCall(t, http.MethodPost, fmt.Sprintf("%s/internal/orders/%s/refunds", commerceURL, f.order),
		f.refundKey, staffHeaders(t, organizerID), map[string]any{"quantity": 2, "actor": "ops@example.test", "reason": "buyer asked"})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(body), `"provider_refund_failed"`) {
		t.Fatalf("staff refund of a fail-once charge = %d %s, want 422 provider_refund_failed", code, body)
	}

	ctx := context.Background()
	db := connectOutcomeDB(t, "commerce")
	var status string
	var amount int64
	if err := db.QueryRow(ctx, `SELECT o.idempotency_key, r.id::text, r.status, r.amount
		FROM order_refunds r JOIN orders o ON o.id = r.order_id
		WHERE r.organizer_id = $1 AND r.idempotency_key = $2`, organizerID, f.refundKey).
		Scan(&f.sourceKey, &f.refundID, &status, &amount); err != nil {
		t.Fatalf("read the failed refund: %v", err)
	}
	if status != "pending" || amount != outcomeOrderAmount {
		t.Fatalf("failed refund status=%q amount=%d, want pending %d", status, amount, outcomeOrderAmount)
	}
	return f
}

func connectOutcomeDB(t *testing.T, service string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := pgx.Connect(ctx, dsn(service, service))
	if err != nil {
		t.Fatalf("connect %s: %v", service, err)
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return db
}

// recordRefundOutcome runs the operator command the way an operator does: the commerce binary
// inside its own container, with that container's database and payments credentials.
func recordRefundOutcome(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", append([]string{"exec", project + "-commerce-1", "/app", "record-refund-outcome", organizerID}, args...)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// failureProofURL is payments' read-only proof for the leg of one commerce refund.
func failureProofURL(f failedRefund) string {
	q := url.Values{"organizer_id": {organizerID}, "idempotency_key": {f.sourceKey}, "refund_key": {f.refundID}}
	return paymentsURL + "/internal/psp/partial-refund/failure-proof?" + q.Encode()
}

type outcomeProof struct {
	ProviderRef string `json:"provider_ref"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
}

func decodeProof(t *testing.T, body []byte) outcomeProof {
	t.Helper()
	var p outcomeProof
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode proof %s: %v", body, err)
	}
	if !strings.HasPrefix(p.ProviderRef, "re_") || p.Amount != outcomeOrderAmount || p.Currency != "EUR" {
		t.Fatalf("proof = %+v, want a re_ reference for %d EUR", p, outcomeOrderAmount)
	}
	return p
}

// TestRefundedOutsideCompletesTheRefundAndKeepsBothCeilings is the `refunded_outside` outcome.
func TestRefundedOutsideCompletesTheRefundAndKeepsBothCeilings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := failedStaffRefund(t, "outside")

	// The proof the command relies on, driven directly so the coverage gate records it.
	code, body := internalJSON(t, http.MethodGet, failureProofURL(f), "", nil)
	if code != http.StatusOK {
		t.Fatalf("failure proof = %d %s, want 200", code, body)
	}
	proof := decodeProof(t, body)

	stdout, stderr, err := recordRefundOutcome(t, f.refundID, "refunded_outside", "Ada Operator", "WIRE-"+f.slot[:8], "card closed; wired back")
	if err != nil {
		t.Fatalf("record-refund-outcome refunded_outside: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "outcome=refunded_outside") || !strings.Contains(stdout, fmt.Sprintf("amount=%d currency=EUR", outcomeOrderAmount)) ||
		!strings.Contains(stdout, "replay=false") || !strings.Contains(stderr, proof.ProviderRef) {
		t.Fatalf("command output does not report the outcome on proof %s\nstdout=%s\nstderr=%s", proof.ProviderRef, stdout, stderr)
	}

	// The refund completed with the outcome as its evidence, and no payments fact.
	commerce := connectOutcomeDB(t, "commerce")
	var status, kind, externalRef string
	var paymentFact bool
	var outcomeAmount int64
	if err := commerce.QueryRow(ctx, `SELECT r.status, coalesce(r.outside_completion_kind,''), r.payment_fact_id IS NOT NULL,
			o.kind, o.external_ref, o.amount
		FROM order_refunds r JOIN refund_outcomes o
		  ON o.organizer_id = r.organizer_id AND o.subject_kind = 'order_refund' AND o.subject_id = r.id
		WHERE r.organizer_id = $1 AND r.id = $2`, organizerID, f.refundID).
		Scan(&status, &kind, &paymentFact, new(string), &externalRef, &outcomeAmount); err != nil {
		t.Fatalf("read the completed refund and its outcome: %v", err)
	}
	if status != "completed" || kind != "refunded_outside" || paymentFact || externalRef != "WIRE-"+f.slot[:8] || outcomeAmount != outcomeOrderAmount {
		t.Fatalf("refund status=%q outside_kind=%q payment_fact=%t external_ref=%q amount=%d, want completed outside on the typed reference",
			status, kind, paymentFact, externalRef, outcomeAmount)
	}

	// No money fact anywhere: not commerce's order.refunded, not a payments journal entry.
	var commerceFacts int
	if err := commerce.QueryRow(ctx, `SELECT count(*) FROM order_facts WHERE order_id = $1 AND fact_type = 'order.refunded'`, f.order).
		Scan(&commerceFacts); err != nil {
		t.Fatal(err)
	}
	payments := connectOutcomeDB(t, "payments")
	var journalFacts int
	if err := payments.QueryRow(ctx, `SELECT count(*) FROM journal_entries
		WHERE organizer_id = $1 AND payload->>'order_id' = $2 AND fact_type IN ('order.refunded','payment.refunded')`,
		organizerID, f.order).Scan(&journalFacts); err != nil {
		t.Fatal(err)
	}
	if commerceFacts != 0 || journalFacts != 0 {
		t.Fatalf("an outside refund wrote money facts: commerce order.refunded=%d, payments journal=%d", commerceFacts, journalFacts)
	}

	// Both ceilings stay counted. Commerce refuses another refund of the order...
	code, body = commerceStaffCall(t, http.MethodPost, fmt.Sprintf("%s/internal/orders/%s/refunds", commerceURL, f.order),
		"outcome-again-"+f.slot, staffHeaders(t, organizerID), map[string]any{"quantity": 1, "actor": "ops@example.test", "reason": "second refund"})
	if code != http.StatusConflict {
		t.Fatalf("second staff refund after refunded_outside = %d %s, want 409", code, body)
	}
	// ...and payments still counts the failed leg against the captured money.
	code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/psp/partial-refund", "",
		map[string]any{"organizer_id": organizerID, "idempotency_key": f.sourceKey,
			"refund_key": "outcome-direct-" + f.slot, "amount": 2500, "currency": "EUR"})
	if code != http.StatusConflict {
		t.Fatalf("payments leg past the ceiling after refunded_outside = %d %s, want 409", code, body)
	}

	// The reversal runs: the reconciler voids the tickets and then returns the capacity.
	interval, err := commerceReversalInterval(ctx)
	if err != nil {
		t.Fatal(err)
	}
	retry(t, max(8*interval, 30*time.Second), func() error {
		s, err := refundStateOf(ctx, f.refundID)
		if err != nil {
			return err
		}
		if !s.TicketsVoided || !s.CapacityReturned {
			return fmt.Errorf("reversal of the outside refund not complete: %+v", s)
		}
		return nil
	})
}

// TestAbandonedRefundReleasesBothAllowances is the `abandoned` outcome.
func TestAbandonedRefundReleasesBothAllowances(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := failedStaffRefund(t, "abandoned")

	stdout, stderr, err := recordRefundOutcome(t, f.refundID, "abandoned", "Ada Operator", "nobody will refund this attempt")
	if err != nil {
		t.Fatalf("record-refund-outcome abandoned: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "outcome=abandoned") || !strings.Contains(stdout, "replay=false") {
		t.Fatalf("command output does not report the abandonment\nstdout=%s\nstderr=%s", stdout, stderr)
	}

	// Commerce: the outcome carries the provider reference, and the refund stays pending.
	commerce := connectOutcomeDB(t, "commerce")
	var status, externalRef string
	if err := commerce.QueryRow(ctx, `SELECT r.status, o.external_ref
		FROM order_refunds r JOIN refund_outcomes o
		  ON o.organizer_id = r.organizer_id AND o.subject_kind = 'order_refund' AND o.subject_id = r.id AND o.kind = 'abandoned'
		WHERE r.organizer_id = $1 AND r.id = $2`, organizerID, f.refundID).Scan(&status, &externalRef); err != nil {
		t.Fatalf("read the abandoned refund and its outcome: %v", err)
	}
	if status != "pending" || !strings.HasPrefix(externalRef, "re_") {
		t.Fatalf("abandoned refund status=%q external_ref=%q, want pending with the provider reference", status, externalRef)
	}

	// Payments: exactly one marker, on the same provider reference.
	payments := connectOutcomeDB(t, "payments")
	var markers int
	var markerRef string
	if err := payments.QueryRow(ctx, `SELECT count(*), coalesce(max(provider_ref),'') FROM payment_refund_leg_abandonments
		WHERE organizer_id = $1 AND source_idempotency_key = $2 AND refund_idempotency_key = $3`,
		organizerID, f.sourceKey, f.refundID).Scan(&markers, &markerRef); err != nil {
		t.Fatal(err)
	}
	if markers != 1 || markerRef != externalRef {
		t.Fatalf("payments markers=%d ref=%q, want one marker on %q", markers, markerRef, externalRef)
	}

	// The abandon operation, driven directly so the coverage gate records it. A committed
	// marker answers as a replay with its own reference.
	code, body := internalJSON(t, http.MethodPost, paymentsURL+"/internal/psp/partial-refund/abandon", "",
		map[string]any{"organizer_id": organizerID, "idempotency_key": f.sourceKey, "refund_key": f.refundID,
			"amount": outcomeOrderAmount, "currency": "EUR"})
	if code != http.StatusOK {
		t.Fatalf("abandon replay = %d %s, want 200", code, body)
	}
	if p := decodeProof(t, body); p.ProviderRef != externalRef {
		t.Fatalf("abandon replay reference = %q, want the marker's %q", p.ProviderRef, externalRef)
	}

	// The old attempt is never re-submitted. Commerce refuses the staff replay of the old key...
	code, body = commerceStaffCall(t, http.MethodPost, fmt.Sprintf("%s/internal/orders/%s/refunds", commerceURL, f.order),
		f.refundKey, staffHeaders(t, organizerID), map[string]any{"quantity": 2, "actor": "ops@example.test", "reason": "buyer asked"})
	if code != http.StatusConflict || !strings.Contains(string(body), `"refund_abandoned"`) {
		t.Fatalf("replay of the abandoned refund = %d %s, want 409 refund_abandoned", code, body)
	}
	// ...and payments refuses the old leg before the provider. The fake would replay its
	// recorded failure as a 422; a 409 means the provider was not asked.
	code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/psp/partial-refund", "",
		map[string]any{"organizer_id": organizerID, "idempotency_key": f.sourceKey, "refund_key": f.refundID,
			"amount": outcomeOrderAmount, "currency": "EUR"})
	if code != http.StatusConflict || !strings.Contains(string(body), "refund leg abandoned") {
		t.Fatalf("replay of the abandoned leg = %d %s, want 409 refund leg abandoned", code, body)
	}

	// Both allowances are released: a fresh staff refund of BOTH tickets under a new key
	// passes commerce's ceiling and payments' ceiling, and the fake PSP refunds it.
	code, body = commerceStaffCall(t, http.MethodPost, fmt.Sprintf("%s/internal/orders/%s/refunds", commerceURL, f.order),
		"outcome-fresh-"+f.slot, staffHeaders(t, organizerID), map[string]any{"quantity": 2, "actor": "ops@example.test", "reason": "refund again"})
	if code != http.StatusOK {
		t.Fatalf("fresh staff refund after abandonment = %d %s, want 200", code, body)
	}
	var fresh struct {
		RefundID string `json:"refund_id"`
	}
	if err := json.Unmarshal(body, &fresh); err != nil || fresh.RefundID == "" || fresh.RefundID == f.refundID {
		t.Fatalf("fresh refund id = %q (err %v), want a new refund", fresh.RefundID, err)
	}
	var legStatus string
	var legAmount int64
	if err := payments.QueryRow(ctx, `SELECT status, amount FROM payment_refund_legs
		WHERE organizer_id = $1 AND source_idempotency_key = $2 AND refund_idempotency_key = $3`,
		organizerID, f.sourceKey, fresh.RefundID).Scan(&legStatus, &legAmount); err != nil {
		t.Fatalf("read the fresh leg: %v", err)
	}
	if legStatus != "refunded" || legAmount != outcomeOrderAmount {
		t.Fatalf("fresh leg status=%q amount=%d, want refunded %d", legStatus, legAmount, outcomeOrderAmount)
	}
}

// partialRefundPosts counts payments' request-log lines for POST /internal/psp/partial-refund.
// The log is append-only across the run, so a before/after count needs no clock agreement
// between the host and the containers. The exact path, closed by the next key, excludes the
// failure-proof and abandon routes.
func partialRefundPosts(t *testing.T, ctx context.Context) int {
	t.Helper()
	out, err := dockerRun(ctx, "compose", "--ansi", "never", "-p", project, "logs", "--no-color", "payments")
	if err != nil {
		t.Fatalf("compose logs payments: %v", err)
	}
	return strings.Count(out, `"method":"POST","path":"/internal/psp/partial-refund","status"`)
}

// TestBulkRunSkipsAnAbandonedCancellationRefund: a cancellation run whose refund an operator
// abandoned makes no payments call and reports refund_refused while quantity remains (owner
// decision 2026-10-12).
func TestBulkRunSkipsAnAbandonedCancellationRefund(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	slot, tt := publishedSlot(t, "Outcome Bulk Hall", 10)
	order := buyOneWithToken(t, slot, tt, "outcome-bulk-"+slot, 2, refundFailsOnceToken)

	// Run one: the provider fails the cancellation refund. This is also the positive control
	// for the log count below: it must see this run's payments call.
	before := partialRefundPosts(t, ctx)
	first := awaitReport(t, startRun(t, slot, "outcome-bulk-run-1-"+slot).RunID)
	if len(first.Orders) != 1 || first.Orders[0].Outcome != "failed" || first.Orders[0].Failure.Code != "refund_refused" {
		t.Fatalf("first run = %+v, want the order failed with refund_refused", first.Orders)
	}
	afterFirst := partialRefundPosts(t, ctx)
	if afterFirst <= before {
		t.Fatalf("the payments request log shows no partial-refund POST for the first run (%d -> %d): the count below cannot see a call", before, afterFirst)
	}

	commerce := connectOutcomeDB(t, "commerce")
	var refundID string
	if err := commerce.QueryRow(ctx, `SELECT id::text FROM order_refunds WHERE organizer_id = $1 AND idempotency_key = $2`,
		organizerID, "cancel:"+slot+":"+order).Scan(&refundID); err != nil {
		t.Fatalf("read the cancellation refund: %v", err)
	}
	if stdout, stderr, err := recordRefundOutcome(t, refundID, "abandoned", "Ada Operator", "event cancelled; card closed"); err != nil {
		t.Fatalf("record-refund-outcome abandoned: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}

	// Run two, under a new key, finds the same derived refund abandoned.
	beforeSecond := partialRefundPosts(t, ctx)
	second := awaitReport(t, startRun(t, slot, "outcome-bulk-run-2-"+slot).RunID)
	if len(second.Orders) != 1 {
		t.Fatalf("second run rows = %d, want 1", len(second.Orders))
	}
	o := second.Orders[0]
	if o.OrderID != order || o.Outcome != "failed" || o.Failure.Code != "refund_refused" || o.MoneyRefunded {
		t.Fatalf("second run row = %+v, want refund_refused with no money refunded", o)
	}
	if n := partialRefundPosts(t, ctx) - beforeSecond; n != 0 {
		t.Fatalf("the second run made %d partial-refund calls to payments, want 0", n)
	}
	// And payments holds no new leg for the charge: only the abandoned one.
	payments := connectOutcomeDB(t, "payments")
	var legs int
	if err := payments.QueryRow(ctx, `SELECT count(*) FROM payment_refund_legs l
		JOIN payment_operations p ON p.organizer_id = l.organizer_id AND p.idempotency_key = l.source_idempotency_key
		WHERE l.organizer_id = $1 AND p.order_id = $2`, organizerID, order).Scan(&legs); err != nil {
		t.Fatal(err)
	}
	if legs != 1 {
		t.Fatalf("payments legs for the order = %d, want only the abandoned one", legs)
	}
}
