package store

import (
	"testing"

	"github.com/google/uuid"
)

func historyOf(types ...string) []LifecycleEvent {
	out := make([]LifecycleEvent, 0, len(types))
	for _, typ := range types {
		out = append(out, LifecycleEvent{ID: uuid.New(), Type: typ})
	}
	return out
}

var (
	stillValid      = TicketValidity{Status: ValidityValid}
	voidedRefunded  = TicketValidity{Status: ValidityVoided, Reason: VoidRefunded}
	voidedExchanged = TicketValidity{Status: ValidityVoided, Reason: VoidExchanged}
)

// TKT-355. The rule, stated without naming the implementation: a recorded refund voids
// a ticket for the reason "refunded"; otherwise a recorded exchange voids it for the
// reason "exchanged"; nothing else changes the verdict, and nothing restores it. Each
// case below is there because one wrong rule passes every other case:
//   - a first-void rule fails "exchanged then refunded";
//   - a last-event rule fails "refunded then delivered" and "refunded then exchanged";
//   - a rule that reads only the newest void fails "refunded then exchanged".
func TestDeriveTicketValidity(t *testing.T) {
	cases := []struct {
		name    string
		history []LifecycleEvent
		want    TicketValidity
	}{
		{"issued only", historyOf("issued"), stillValid},
		{"every non-voiding type", historyOf("issued", "delivered", "redelivered", "redeemed", "entry", "exit", "duplicate_admit"), stillValid},
		{"refunded", historyOf("issued", "refunded"), voidedRefunded},
		{"exchanged", historyOf("issued", "exchanged"), voidedExchanged},
		{"exchanged then refunded", historyOf("issued", "exchanged", "refunded"), voidedRefunded},
		{"refunded then exchanged", historyOf("issued", "refunded", "exchanged"), voidedRefunded},
		{"refunded then later facts", historyOf("issued", "refunded", "delivered", "redelivered", "entry", "exit", "duplicate_admit", "redeemed"), voidedRefunded},
		{"exchanged then later facts", historyOf("issued", "exchanged", "delivered", "redelivered", "entry"), voidedExchanged},
		{"refunded before issued in slice order", historyOf("refunded", "issued"), voidedRefunded},
		// An unknown type keeps the verdict it finds, as the gate does: the gate voids on
		// refunded and exchanged only. The forcing test (validity_smoke_test.go) makes an
		// unclassified type unshippable, so these cases pin the fallback, not a policy.
		{"unknown type alone", historyOf("issued", "transferred"), stillValid},
		{"unknown type after a refund", historyOf("issued", "refunded", "reinstated"), voidedRefunded},
		{"unknown type after an exchange", historyOf("issued", "exchanged", "reinstated"), voidedExchanged},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveTicketValidity(c.history); got != c.want {
				t.Fatalf("DeriveTicketValidity(%v) = %+v, want %+v", typesOf(c.history), got, c.want)
			}
		})
	}
}

func typesOf(history []LifecycleEvent) []string {
	out := make([]string, 0, len(history))
	for _, e := range history {
		out = append(out, e.Type)
	}
	return out
}
