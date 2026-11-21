package psp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ticketing/shared/fakepsp"
)

// TKT-525 (ADR-087): the fake's fail-once refund and its read-only proof. The proof follows the
// Stripe adapter's rule; each refusal case below differs from the accepted one in ONE input, so
// each predicate is reached on its own.

const failOnceOrg, failOnceChargeKey = "11111111-1111-1111-1111-111111111111", "checkout-fail-once"

// failOnceCharge authorizes a fake-refund-fail-once charge and returns its reference.
func failOnceCharge(t *testing.T, f *Fake) string {
	t.Helper()
	got, err := f.Authorize(context.Background(), AuthorizeRequest{
		OrganizerID: failOnceOrg, OrderID: "o", BuyerID: "b", Amount: 2000, Currency: "EUR",
		PaymentToken: fakepsp.TokenRefundFailsOnce, IdempotencyKey: failOnceChargeKey,
	})
	if err != nil {
		t.Fatalf("Authorize(fail-once): %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("fail-once result invalid: %v", err)
	}
	if got.Outcome != Captured || !got.Confirmed.Agrees(2000, "EUR") {
		t.Fatalf("Authorize(fail-once) = %+v, want a capture confirming the request", got)
	}
	if !strings.HasPrefix(got.ProviderRef, failOnceRefPrefix) {
		t.Fatalf("fail-once charge reference = %q, want the %q prefix", got.ProviderRef, failOnceRefPrefix)
	}
	return got.ProviderRef
}

func TestFakeFailOnceFailsTheFirstRefundAndReplaysIt(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	pi := failOnceCharge(t, f)

	_, err := f.Refund(ctx, pi, "leg-1", 1000, "EUR")
	var failed *RefundFailedError
	if !errors.As(err, &failed) || !strings.HasPrefix(failed.ProviderRef, "re_") || len(failed.ProviderRef) <= len("re_") {
		t.Fatalf("first refund err = %v, want a terminal RefundFailedError with a re_ reference", err)
	}
	// The same key replays the recorded failure; it does not become a second refund.
	if _, again := f.Refund(ctx, pi, "leg-1", 1000, "EUR"); !errors.As(again, &failed) {
		t.Fatalf("replayed first refund err = %v, want the same terminal failure", again)
	}
	// A later refund under another key succeeds.
	res, err := f.Refund(ctx, pi, "leg-2", 1000, "EUR")
	if err != nil || res.Outcome != Refunded || !res.Confirmed.Agrees(1000, "EUR") || res.ProviderRef == "" {
		t.Fatalf("second refund = %+v err=%v, want a confirmed success with a reference", res, err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("second refund result invalid: %v", err)
	}
	if n := len(f.refunds[pi]); n != 2 {
		t.Fatalf("recorded refunds = %d, want 2 (the replay records nothing)", n)
	}
}

// Every other fake charge keeps its stateless refund: a success, nothing recorded, nothing
// to prove.
func TestFakeOrdinaryChargeRefundStaysStatelessAndUnprovable(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	got, err := f.Authorize(ctx, AuthorizeRequest{OrganizerID: failOnceOrg, Amount: 2000, Currency: "EUR",
		PaymentToken: fakepsp.TokenSuccess, IdempotencyKey: failOnceChargeKey})
	if err != nil || got.ProviderRef != "" {
		t.Fatalf("fake-ok Authorize = %+v err=%v, want no reference", got, err)
	}
	res, err := f.Refund(ctx, got.ProviderRef, "leg-1", 1000, "EUR")
	if err != nil || res.Outcome != Refunded || res.ProviderRef != "" {
		t.Fatalf("fake-ok refund = %+v err=%v, want the unchanged stateless success", res, err)
	}
	if len(f.refunds) != 0 {
		t.Fatalf("an ordinary refund was recorded: %v", f.refunds)
	}
	if _, err := f.ProveRefundFailure(ctx, got.ProviderRef, "leg-1", 1000, "EUR"); !errors.Is(err, ErrRefundProofInconclusive) {
		t.Fatalf("proof for an ordinary charge err = %v, want inconclusive", err)
	}
}

func TestFakeProveRefundFailureAcceptsItsOwnFailedRefund(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	pi := failOnceCharge(t, f)
	_, err := f.Refund(ctx, pi, "leg-1", 1000, "EUR")
	var failed *RefundFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("first refund err = %v, want a terminal failure", err)
	}
	got, err := f.ProveRefundFailure(ctx, pi, "leg-1", 1000, "EUR")
	if err != nil || got.ProviderRef != failed.ProviderRef {
		t.Fatalf("proof = %+v err=%v, want the failed refund's own reference %q", got, err, failed.ProviderRef)
	}
	// A later success under another key beside it does not disturb the proof.
	if _, err := f.Refund(ctx, pi, "leg-2", 1000, "EUR"); err != nil {
		t.Fatal(err)
	}
	if again, err := f.ProveRefundFailure(ctx, pi, "leg-1", 1000, "EUR"); err != nil || again != got {
		t.Fatalf("proof beside a foreign success = %+v err=%v, want %+v", again, err, got)
	}
	// Read-only: the proof recorded nothing.
	if n := len(f.refunds[pi]); n != 2 {
		t.Fatalf("recorded refunds = %d after the proofs, want 2", n)
	}
}

// A success under the stamp, reached through the public API: the second refund of a fail-once
// charge succeeds, and proving ITS key is a contradiction.
func TestFakeProveRefundFailureRefusesASucceededRefund(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	pi := failOnceCharge(t, f)
	_, _ = f.Refund(ctx, pi, "leg-1", 1000, "EUR")
	if _, err := f.Refund(ctx, pi, "leg-2", 1000, "EUR"); err != nil {
		t.Fatal(err)
	}
	if got, err := f.ProveRefundFailure(ctx, pi, "leg-2", 1000, "EUR"); !errors.Is(err, ErrRefundProofContradicted) || got.ProviderRef != "" {
		t.Fatalf("proof of a succeeded refund = %+v err=%v, want a contradiction", got, err)
	}
}

// The remaining predicates, from recorded state. Two of them (two refunds under one stamp, and
// a corroborated match beside an uncorroborated one) cannot be reached through Refund, which
// replays a recorded key; the proof still refuses them, as the Stripe adapter does.
func TestFakeProveRefundFailureRefusesEverythingElse(t *testing.T) {
	const pi = failOnceRefPrefix + "proof"
	ours := fakeRefund{id: "re_fake_ours", key: "leg-1", amount: 1000, currency: "EUR", failed: true}
	for _, c := range []struct {
		name     string
		recorded []fakeRefund
		pi, key  string
		amount   int64
		currency string
		want     error
	}{
		// Refund never records under an empty reference, so this case also seeds one there:
		// the guard must refuse on its own, not through "no stamped refund".
		{"no charge reference", []fakeRefund{ours}, "", "leg-1", 1000, "EUR", ErrRefundProofInconclusive},
		{"no stamp", []fakeRefund{ours, {id: "re_fake_unstamped", key: "", amount: 1000, currency: "EUR", failed: true}}, pi, "", 1000, "EUR", ErrRefundProofInconclusive},
		{"a different charge reference", []fakeRefund{ours}, failOnceRefPrefix + "other", "leg-1", 1000, "EUR", ErrRefundProofInconclusive},
		{"no refund carries the stamp", []fakeRefund{{id: "re_fake_x", key: "leg-other", amount: 1000, currency: "EUR", failed: true}}, pi, "leg-1", 1000, "EUR", ErrRefundProofInconclusive},
		{"a different amount", []fakeRefund{ours}, pi, "leg-1", 999, "EUR", ErrRefundProofInconclusive},
		{"a different currency", []fakeRefund{ours}, pi, "leg-1", 1000, "USD", ErrRefundProofInconclusive},
		{"a non-positive amount", []fakeRefund{{id: "re_fake_zero", key: "leg-1", amount: 0, currency: "EUR", failed: true}}, pi, "leg-1", 0, "EUR", ErrRefundProofInconclusive},
		{"two refunds carry the stamp", []fakeRefund{ours, {id: "re_fake_second", key: "leg-1", amount: 1000, currency: "EUR", failed: true}}, pi, "leg-1", 1000, "EUR", ErrRefundProofContradicted},
		{"a corroborated match beside an uncorroborated one", []fakeRefund{ours, {id: "re_fake_odd", key: "leg-1", amount: 5, currency: "EUR", failed: true}}, pi, "leg-1", 1000, "EUR", ErrRefundProofInconclusive},
		{"the stamped refund succeeded", []fakeRefund{{id: "re_fake_ok", key: "leg-1", amount: 1000, currency: "EUR"}}, pi, "leg-1", 1000, "EUR", ErrRefundProofContradicted},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &Fake{refunds: map[string][]fakeRefund{pi: c.recorded}}
			if c.pi == "" {
				f.refunds[""] = c.recorded
			}
			got, err := f.ProveRefundFailure(context.Background(), c.pi, c.key, c.amount, c.currency)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v (proof %+v), want %v", err, got, c.want)
			}
			if got.ProviderRef != "" {
				t.Fatalf("a refused proof returned a reference %q", got.ProviderRef)
			}
		})
	}
}

// Status resolves a fail-once charge like fake-ok and keeps its persisted reference.
func TestFakeStatusResolvesAFailOnceCharge(t *testing.T) {
	res, err := NewFake().Status(context.Background(), StatusRequest{ProviderRef: failOnceRefPrefix + "x",
		PaymentToken: fakepsp.TokenRefundFailsOnce, Amount: 2000, Currency: "EUR"})
	if err != nil || res.Outcome != Captured || res.ProviderRef != failOnceRefPrefix+"x" || !res.Confirmed.Agrees(2000, "EUR") {
		t.Fatalf("Status(fail-once) = %+v err=%v, want a confirmed capture keeping the reference", res, err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("Status(fail-once) invalid: %v", err)
	}
}
