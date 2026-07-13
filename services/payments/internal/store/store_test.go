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
