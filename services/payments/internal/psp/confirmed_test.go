package psp

import "testing"

// TKT-257. `psp.Result` gained an OPTIONAL provider-confirmed money figure: what the
// provider says it moved, as distinct from what we asked it to move. These tests pin the
// three properties that make it safe to record as settlement evidence.
//
// Why optional rather than required on Captured/Refunded, which is the obvious reading of
// the ticket: two callers legitimately reconstruct a Captured Result from STORED columns
// with no provider answer in hand — `resolvedResult` and `providerStateResult`
// (services/payments/internal/api/psp.go:57,212). A mandatory field would force those to
// forge one, which is the exact defect this ticket exists to remove. So the invariant is
// "absent, or well-formed and consistent with the outcome", and the fail-closed comparison
// lives at the four write sinks, where a real provider answer is genuinely in hand.

func TestConfirmedMoneyPresence(t *testing.T) {
	amt := int64(1250)
	m := &ConfirmedMoney{Amount: amt, Currency: "EUR"}

	if (Result{Outcome: Captured, Captured: true, Authorized: true}).Confirmed != nil {
		t.Fatal("a Result with no provider money must report absent, never a zero value")
	}
	got := Result{Outcome: Captured, Captured: true, Authorized: true, Confirmed: m}
	if got.Confirmed == nil || got.Confirmed.Amount != amt || got.Confirmed.Currency != "EUR" {
		t.Fatalf("confirmed money not carried: %+v", got.Confirmed)
	}
}

// Absence and zero are different states and must stay different. Stripe reports
// `amount_received: 0` on a requires_capture PaymentIntent — a REAL zero, not a missing
// value — so a type that collapses the two would make "the provider confirmed nothing"
// indistinguishable from "the provider confirmed zero".
func TestConfirmedMoneyAbsentIsNotZero(t *testing.T) {
	absent := Result{Outcome: Captured, Captured: true, Authorized: true}
	zero := Result{Outcome: Captured, Captured: true, Authorized: true, Confirmed: &ConfirmedMoney{Amount: 0, Currency: "EUR"}}
	if absent.Confirmed != nil {
		t.Fatal("absent confirmation must be nil")
	}
	if zero.Confirmed == nil {
		t.Fatal("a confirmed zero must be representable and distinguishable from absence")
	}
	// The guard rejects a confirmed zero on an outcome that claims money MOVED; that is
	// TestResultValidateRejectsMalformedConfirmedMoney's job, not this one's. Here we only
	// pin that the type can express both states.
}

// Validate is the money-path boundary. A confirmed figure that is present must be
// self-consistent AND consistent with the outcome it accompanies. Hand-written
// contradictions, deliberately not produced by any adapter, so this proves the guard
// rejects impossible states rather than co-defining them with an implementation.
func TestResultValidateRejectsMalformedConfirmedMoney(t *testing.T) {
	valid := []struct {
		name string
		r    Result
	}{
		{"captured with confirmed money", Result{Outcome: Captured, Captured: true, Authorized: true, Confirmed: &ConfirmedMoney{Amount: 1250, Currency: "EUR"}}},
		{"captured without confirmed money (reconstructed from stored columns)", Result{Outcome: Captured, Captured: true, Authorized: true}},
		{"refunded with confirmed money", Result{Outcome: Refunded, Confirmed: &ConfirmedMoney{Amount: 500, Currency: "EUR"}}},
		{"refunded without confirmed money", Result{Outcome: Refunded}},
	}
	for _, c := range valid {
		if err := c.r.Validate(); err != nil {
			t.Fatalf("%s: rejected but valid: %v", c.name, err)
		}
	}

	invalid := []struct {
		name string
		r    Result
	}{
		// Malformed in itself.
		{"confirmed amount negative", Result{Outcome: Captured, Captured: true, Authorized: true, Confirmed: &ConfirmedMoney{Amount: -1, Currency: "EUR"}}},
		{"confirmed currency empty", Result{Outcome: Captured, Captured: true, Authorized: true, Confirmed: &ConfirmedMoney{Amount: 1250}}},
		{"confirmed currency not ISO-4217 shaped", Result{Outcome: Captured, Captured: true, Authorized: true, Confirmed: &ConfirmedMoney{Amount: 1250, Currency: "eur"}}},
		// Money moved, but the provider confirmed zero: a self-contradiction. `Captured`
		// asserts money moved; a confirmed zero says none did.
		{"captured but confirmed zero", Result{Outcome: Captured, Captured: true, Authorized: true, Confirmed: &ConfirmedMoney{Amount: 0, Currency: "EUR"}}},
		{"refunded but confirmed zero", Result{Outcome: Refunded, Confirmed: &ConfirmedMoney{Amount: 0, Currency: "EUR"}}},
		// Outcomes that prove NO money moved must never carry a confirmation. Declined and
		// Timeout are terminal-no-side-effect; Voided released a hold that never captured.
		{"declined carries confirmed money", Result{Outcome: Declined, TerminalNoSideEffect: true, Confirmed: &ConfirmedMoney{Amount: 1250, Currency: "EUR"}}},
		{"timeout carries confirmed money", Result{Outcome: Timeout, TerminalNoSideEffect: true, Confirmed: &ConfirmedMoney{Amount: 1250, Currency: "EUR"}}},
		{"voided carries confirmed money", Result{Outcome: Voided, TerminalNoSideEffect: true, Confirmed: &ConfirmedMoney{Amount: 1250, Currency: "EUR"}}},
		{"authorized-only carries confirmed money", Result{Outcome: Authorized, Authorized: true, Confirmed: &ConfirmedMoney{Amount: 1250, Currency: "EUR"}}},
		// Unknown is the genuinely-undetermined outcome, and it is what a PENDING refund
		// returns (mapRefundStatus -> Unknown + ErrRefundPending). Stripe's refund object
		// carries an `amount` while pending, but the money has NOT come back: attaching it
		// would let a pending refund be recorded as settled evidence.
		{"unknown carries confirmed money", Result{Outcome: Unknown, Confirmed: &ConfirmedMoney{Amount: 1250, Currency: "EUR"}}},
	}
	for _, c := range invalid {
		t.Run(c.name, func(t *testing.T) {
			if err := c.r.Validate(); err == nil {
				t.Fatalf("Validate must reject %+v", c.r)
			}
		})
	}
}

// ConfirmedMoney.Agrees is the single comparison the four write sinks share. It is one
// function rather than four hand-written comparisons because the likeliest way this ticket
// ships wrong is COVERAGE — three of four sinks guarded, which looks complete from inside
// each file. With a shared helper the missing sink is a missing CALL, which a test names.
//
// Two independent predicates, amount and currency. Each case below satisfies the other
// predicate, so neither can be proven by a case that short-circuits on the first.
func TestConfirmedMoneyAgrees(t *testing.T) {
	cases := []struct {
		name           string
		confirmed      *ConfirmedMoney
		wantAmount     int64
		wantCurrency   string
		agrees         bool
	}{
		{"exact agreement", &ConfirmedMoney{Amount: 1250, Currency: "EUR"}, 1250, "EUR", true},
		{"amount differs, currency matches", &ConfirmedMoney{Amount: 1249, Currency: "EUR"}, 1250, "EUR", false},
		{"currency differs, amount matches", &ConfirmedMoney{Amount: 1250, Currency: "USD"}, 1250, "EUR", false},
		{"both differ", &ConfirmedMoney{Amount: 999, Currency: "USD"}, 1250, "EUR", false},
		// Absent is NOT agreement. A provider that told us nothing has confirmed nothing,
		// and treating silence as assent is precisely the fail-open this ticket closes.
		{"absent does not agree", nil, 1250, "EUR", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.confirmed.Agrees(c.wantAmount, c.wantCurrency); got != c.agrees {
				t.Fatalf("Agrees(%d,%q) on %+v = %v, want %v", c.wantAmount, c.wantCurrency, c.confirmed, got, c.agrees)
			}
		})
	}
}
