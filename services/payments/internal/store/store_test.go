package store

import (
	"strings"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestCanonicalHashAndPIIGuard(t *testing.T) {
	f := Fact{ID: uuid.New(), OrganizerID: uuid.New(), Type: "order.created", OccurredAt: time.Date(2026, 7, 12, 1, 2, 3, 4, time.UTC), BuyerID: uuid.New(), Amount: 1250, Currency: "EUR", Payload: map[string]string{"order_id": uuid.NewString()}}
	c1, _ := canonical(f, 1)
	c2, _ := canonical(f, 1)
	if string(c1) != string(c2) {
		t.Fatal("canonical bytes changed")
	}
	prev := make([]byte, 32)
	if got := hash(prev, c1); len(got) != 32 || string(got) != string(hash(prev, c2)) {
		t.Fatal("hash not deterministic")
	}
	f.Payload["email"] = "raw@example.test"
	if validate(f) == nil {
		t.Fatal("raw PII accepted")
	}
	f.Payload = map[string]string{"contact": "raw@example.test"}
	if validate(f) == nil {
		t.Fatal("arbitrary payload field accepted")
	}
}

// The compensating fact types (ADR-016 §Decision 4) must pass the allowlist so void/
// refund can be journalled as facts, never as mutations. TKT-56 Slice 1 adds the types;
// nothing emits them until the compensation slice. A hard-coded expected-accept set keeps
// this from silently widening — an unrelated new type must still be rejected.
func TestCompensatingFactTypesAreAccepted(t *testing.T) {
	base := func(typ string) Fact {
		return Fact{
			ID: uuid.New(), OrganizerID: uuid.New(), BuyerID: uuid.New(),
			Type: typ, Amount: 4200, Currency: "EUR", OccurredAt: time.Now().UTC(),
			Payload: map[string]string{"order_id": uuid.NewString()},
		}
	}
	for _, typ := range []string{"payment.voided", "payment.refunded"} {
		if err := validate(base(typ)); err != nil {
			t.Fatalf("compensating fact type %q must be accepted, got: %v", typ, err)
		}
	}
	// A neighbouring but unlisted type must still be rejected — the allowlist widened by
	// exactly two, not into a prefix wildcard.
	if err := validate(base("payment.reversed")); err == nil {
		t.Fatal("payment.reversed is not an allowed fact type but was accepted")
	}
}

// The compensation idempotency key is derived, versioned and bounded (plan-final /
// ADR-032 §Refund): the SAME (organizer, source key, kind) must always produce the SAME
// provider key — that determinism is what makes a crashed compensation replay hit
// Stripe's idempotency layer instead of issuing a second refund. NUL separators keep
// ("a","bc") and ("ab","c") from colliding.
func TestCompensationKeyDeterministic(t *testing.T) {
	org := uuid.MustParse("00000000-0000-0000-0000-000000000042")
	k1 := CompensationKey(org, "order-1-charge", "refund")
	k2 := CompensationKey(org, "order-1-charge", "refund")
	if k1 != k2 {
		t.Fatalf("compensation key not deterministic: %q vs %q", k1, k2)
	}
	if !strings.HasPrefix(k1, "psp-comp-v1:") {
		t.Fatalf("compensation key must be versioned: %q", k1)
	}
	// sha256 hex after the prefix: bounded length regardless of source key size.
	if len(k1) != len("psp-comp-v1:")+64 {
		t.Fatalf("compensation key not bounded: %d chars", len(k1))
	}
	if CompensationKey(org, "order-1-charge", "void") == k1 {
		t.Fatal("void and refund kinds must derive distinct keys")
	}
	if CompensationKey(uuid.MustParse("00000000-0000-0000-0000-000000000043"), "order-1-charge", "refund") == k1 {
		t.Fatal("distinct organizers must derive distinct keys")
	}
	// NUL separation: ambiguous concatenations must not collide.
	if CompensationKey(org, "ab", "refund") == CompensationKey(org, "a", "brefund") {
		t.Fatal("separator collision between source key and kind")
	}
}

// ai-review S13. Two assumptions the journal rested on without stating: that
// every currency has exponent 2, and that a fact claiming money moved actually
// moved some.
//
// Both are enforced HERE, at the durable boundary, and that placement is the
// point. The charge handler already hard-coded EUR while this accepted any
// three-letter code — so the assumption was checked where it could be bypassed
// and unchecked where it becomes permanent. The journal is append-only: a JPY
// amount written as if it were centimes is off by 100x and cannot be corrected,
// only compensated.
func TestValidateRefusesUnsupportedCurrencyAndZeroMoney(t *testing.T) {
	base := func() Fact {
		return Fact{
			ID: uuid.New(), OrganizerID: uuid.New(), BuyerID: uuid.New(),
			Type: "payment.captured", Amount: 1000, Currency: "EUR",
			Payload: map[string]string{"order_id": uuid.NewString()},
		}
	}
	if err := validate(base()); err != nil {
		t.Fatalf("an ordinary EUR capture must validate: %v", err)
	}

	// JPY is exponent 0 and KWD is 3. Admitting either without a per-currency
	// exponent is a ledger error, not a formatting one.
	for _, currency := range []string{"JPY", "KWD", "USD", "eur", "EU", "EURO", ""} {
		f := base()
		f.Currency = currency
		if err := validate(f); err == nil {
			t.Errorf("currency %q was accepted", currency)
		}
	}

	// A fact asserting money MOVED must carry some. The journal is the last place
	// a caller-side quantity guard that failed open can still be caught.
	for _, factType := range []string{"payment.authorized", "payment.captured", "payment.voided", "payment.refunded", "order.refunded"} {
		f := base()
		f.Type, f.Amount = factType, 0
		if err := validate(f); err == nil {
			t.Errorf("%s was accepted with a zero amount", factType)
		}
	}

	// ...and the types that are legitimately zero stay legitimate. A comp ticket
	// is a real order that moves no money, and a decline carries the amount that
	// was attempted rather than one that moved. Both exchange legs belong here:
	// exchanging a comp gives a zero reversal, and commerce writes the pair one
	// after the other — refusing the second leaves the first stranded forever.
	for _, factType := range []string{"order.created", "order.completed", "order.failed", "payment.declined", "payment.timeout", "order.exchange.reversed", "order.exchange.sold"} {
		f := base()
		f.Type, f.Amount = factType, 0
		if err := validate(f); err != nil {
			t.Errorf("%s must accept a zero amount: %v", factType, err)
		}
	}
}
