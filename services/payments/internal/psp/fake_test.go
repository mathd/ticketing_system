package psp

import (
	"context"
	"errors"
	"testing"

	"ticketing/shared/fakepsp"
)

// The expected outcomes are a hand-written table, NOT derived from the Fake's own logic:
// it encodes what the inline charge handler (server.go:169-187) did before the refactor,
// so a behavioural drift in the refactor is caught rather than co-defined by the impl.
func TestFakeAuthorizeNormalizesEachToken(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  Result
		// nil = the fake must confirm NO money for this token.
		wantConfirmed *ConfirmedMoney
	}{
		{
			name:  "success authorizes and captures, no terminal-no-side-effect",
			token: fakepsp.TokenSuccess,
			want:  Result{Outcome: Captured, Captured: true, Authorized: true, TerminalNoSideEffect: false},
			// The fake echoes the request as its confirmation (TKT-257). Echoed, so this
			// pins the plumbing, never the guard — the fake cannot disagree with itself.
			wantConfirmed: &ConfirmedMoney{Amount: 1250, Currency: "EUR"},
		},
		{
			name:  "decline is terminal-no-side-effect, no capture",
			token: fakepsp.TokenDecline,
			want:  Result{Outcome: Declined, Captured: false, Authorized: false, TerminalNoSideEffect: true},
		},
		{
			name:  "timeout (status-proven) is terminal-no-side-effect, no capture",
			token: fakepsp.TokenTimeout,
			want:  Result{Outcome: Timeout, Captured: false, Authorized: false, TerminalNoSideEffect: true},
		},
	}
	f := NewFake()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.Authorize(context.Background(), AuthorizeRequest{
				OrganizerID:  "11111111-1111-1111-1111-111111111111",
				OrderID:      "22222222-2222-2222-2222-222222222222",
				BuyerID:      "33333333-3333-3333-3333-333333333333",
				Amount:       1250,
				Currency:     "EUR",
				PaymentToken: tc.token,
			})
			if err != nil {
				t.Fatalf("Authorize(%s) unexpected error: %v", tc.token, err)
			}
			// Compared with Confirmed cleared, then by VALUE: Result carries a pointer
			// (TKT-257), and struct equality on it compares addresses.
			gotSansMoney := got
			gotSansMoney.Confirmed = nil
			if gotSansMoney != tc.want {
				t.Fatalf("Authorize(%s)\n got=%+v\nwant=%+v", tc.token, gotSansMoney, tc.want)
			}
			switch {
			case tc.wantConfirmed == nil && got.Confirmed != nil:
				t.Fatalf("Authorize(%s) must confirm no money, got %+v", tc.token, got.Confirmed)
			case tc.wantConfirmed != nil && got.Confirmed == nil:
				t.Fatalf("Authorize(%s) must confirm %+v, got none", tc.token, tc.wantConfirmed)
			case tc.wantConfirmed != nil && *got.Confirmed != *tc.wantConfirmed:
				t.Fatalf("Authorize(%s) confirmed %+v, want %+v", tc.token, *got.Confirmed, *tc.wantConfirmed)
			}
		})
	}
}

func TestFakeAuthorizeRejectsUnknownToken(t *testing.T) {
	f := NewFake()
	_, err := f.Authorize(context.Background(), AuthorizeRequest{
		OrganizerID: "11111111-1111-1111-1111-111111111111",
		OrderID:     "22222222-2222-2222-2222-222222222222",
		BuyerID:     "33333333-3333-3333-3333-333333333333",
		Amount:      1250, Currency: "EUR", PaymentToken: "not-a-fake-token",
	})
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken (port-level) for an unknown token, got %v", err)
	}
	// It must also still wrap the fake-specific cause, so a fake-aware caller can unwrap it.
	if !errors.Is(err, fakepsp.ErrUnknownToken) {
		t.Fatalf("want the wrapped fakepsp.ErrUnknownToken cause, got %v", err)
	}
}

// TKT-114/S2 wires the fake's compensation surface: Capture/Void/Refund report deterministic
// success and Status resolves deterministically from the replayed token (Unknown when there
// is none to replay). Every result must obey the same Validate() invariants Stripe's does — a fake that produced an invalid Result would let an
// invalid shape reach the money-path boundary in local/gate runs.
func TestFakeCompensationSurfaceIsDeterministicAndValid(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	checks := []struct {
		name string
		call func() (Result, error)
		want Outcome
	}{
		{"capture", func() (Result, error) { return f.Capture(ctx, "ref", 1, "EUR") }, Captured},
		{"void", func() (Result, error) { return f.Void(ctx, "ref", "idem") }, Voided},
		{"refund", func() (Result, error) { return f.Refund(ctx, "ref", "idem", 1, "EUR") }, Refunded},
		{"status", func() (Result, error) { return f.Status(ctx, StatusRequest{ProviderRef: "ref"}) }, Unknown},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.call()
			if err != nil {
				t.Fatalf("%s: unexpected error %v", c.name, err)
			}
			if got.Outcome != c.want {
				t.Fatalf("%s: want outcome %q, got %+v", c.name, c.want, got)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("%s: fake produced an invalid Result: %v", c.name, err)
			}
		})
	}
}

// The Fake must satisfy the PSP interface — a compile-time assertion, red until Fake exists.
var _ PSP = (*Fake)(nil)

// Result.Validate is the money-path boundary guard: the handler journals from a Result, so
// a provider (a future Stripe adapter) that returns a self-contradictory Result must be
// rejected before anything is written, not trusted. These tuples are HAND-WRITTEN
// contradictions — deliberately NOT produced by any adapter — so the test proves the guard
// rejects impossible states rather than co-defining them with an implementation.
func TestResultValidateRejectsContradictions(t *testing.T) {
	valid := []Result{
		{Outcome: Captured, Captured: true, Authorized: true},
		{Outcome: Declined, TerminalNoSideEffect: true},
		{Outcome: Timeout, TerminalNoSideEffect: true},
		{Outcome: Authorized, Authorized: true, TerminalNoSideEffect: false}, // auth-only: authorized, not captured, not terminal
		{Outcome: Unknown}, // undetermined: not captured/authorized/terminal
		// Compensation outcomes (TKT-114/S2). Voided: the hold is released, nothing moved
		// on the ledger — genuinely no side effect. Refunded: money moved and came back —
		// there WAS a side effect, so it is NOT terminal-no-side-effect (recovery must
		// never read a refund as "no side effect" and release a claim on it).
		{Outcome: Voided, TerminalNoSideEffect: true},
		{Outcome: Refunded, TerminalNoSideEffect: false},
	}
	for _, r := range valid {
		if err := r.Validate(); err != nil {
			t.Fatalf("valid Result %+v rejected: %v", r, err)
		}
	}

	invalid := []struct {
		name string
		r    Result
	}{
		{"captured outcome but Captured=false", Result{Outcome: Captured, Captured: false, Authorized: true}},
		{"captured outcome but Authorized=false", Result{Outcome: Captured, Captured: true, Authorized: false}},
		{"declined but claims capture", Result{Outcome: Declined, Captured: true, TerminalNoSideEffect: true}},
		{"declined but claims authorization", Result{Outcome: Declined, Authorized: true, TerminalNoSideEffect: true}},
		{"declined but not terminal-no-side-effect", Result{Outcome: Declined, TerminalNoSideEffect: false}},
		{"timeout but claims authorization", Result{Outcome: Timeout, Authorized: true, TerminalNoSideEffect: true}},
		{"timeout but not terminal-no-side-effect", Result{Outcome: Timeout, TerminalNoSideEffect: false}},
		{"unknown but terminal-no-side-effect", Result{Outcome: Unknown, TerminalNoSideEffect: true}},
		{"authorized-only but also captured", Result{Outcome: Authorized, Authorized: true, Captured: true}},
		{"voided but claims capture", Result{Outcome: Voided, Captured: true, TerminalNoSideEffect: true}},
		{"voided but claims authorization", Result{Outcome: Voided, Authorized: true, TerminalNoSideEffect: true}},
		{"voided but not terminal-no-side-effect", Result{Outcome: Voided, TerminalNoSideEffect: false}},
		{"refunded but claims capture", Result{Outcome: Refunded, Captured: true}},
		{"refunded but claims authorization", Result{Outcome: Refunded, Authorized: true}},
		{"refunded but claims terminal-no-side-effect", Result{Outcome: Refunded, TerminalNoSideEffect: true}},
		{"empty/zero outcome", Result{}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.r.Validate(); err == nil {
				t.Fatalf("contradictory Result %+v must be rejected by Validate()", tc.r)
			}
		})
	}
}

// The fake's own Authorize outputs must all pass Validate — a provider that produces an
// invalid Result is itself the bug the guard exists to catch.
func TestFakeAuthorizeOutputsAreValid(t *testing.T) {
	f := NewFake()
	for _, tok := range []string{fakepsp.TokenSuccess, fakepsp.TokenDecline, fakepsp.TokenTimeout} {
		got, err := f.Authorize(context.Background(), AuthorizeRequest{
			OrganizerID: "o", OrderID: "o2", BuyerID: "b", Amount: 1, Currency: "EUR",
			IdempotencyKey: "idem-1", PaymentToken: tok,
		})
		if err != nil {
			t.Fatalf("Authorize(%s): %v", tok, err)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("fake Authorize(%s) produced an invalid Result %+v: %v", tok, got, err)
		}
	}
}

// The auth-hold token (TKT-114/S2) is the offline simulation of a crashed/interrupted
// Stripe flow: Authorize reports Authorized-only (the charge handler fails closed and the
// operation stays unresolved), and Status later resolves it deterministically from the
// durable token the store replays. This is what makes the void happy path — and S3's
// payment_unknown recovery — drivable against the fake.
func TestFakeAuthHoldAndDeterministicStatus(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	got, err := f.Authorize(ctx, AuthorizeRequest{
		OrganizerID: "o", OrderID: "o2", BuyerID: "b", Amount: 1250, Currency: "EUR",
		IdempotencyKey: "idem-hold", PaymentToken: fakepsp.TokenAuthHold,
	})
	if err != nil {
		t.Fatalf("Authorize(auth-hold): %v", err)
	}
	want := Result{Outcome: Authorized, Authorized: true}
	if got != want {
		t.Fatalf("Authorize(auth-hold)\n got=%+v\nwant=%+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("auth-hold result invalid: %v", err)
	}
	// Status is deterministic on the replayed token — the same durable evidence the store
	// carries in StatusRequest — never on hidden state.
	statuses := map[string]Outcome{
		fakepsp.TokenSuccess:  Captured,
		fakepsp.TokenAuthHold: Authorized,
		fakepsp.TokenDecline:  Declined,
		fakepsp.TokenTimeout:  Timeout,
		"":                    Unknown,
	}
	for token, wantOutcome := range statuses {
		res, err := f.Status(ctx, StatusRequest{PaymentToken: token, IdempotencyKey: "idem-hold", Amount: 1250, Currency: "EUR"})
		if err != nil {
			t.Fatalf("Status(%q): %v", token, err)
		}
		if res.Outcome != wantOutcome {
			t.Fatalf("Status(%q) = %+v, want outcome %q", token, res, wantOutcome)
		}
		if err := res.Validate(); err != nil {
			t.Fatalf("Status(%q) produced invalid Result: %v", token, err)
		}
	}
}
