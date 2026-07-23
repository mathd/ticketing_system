package store

import (
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
