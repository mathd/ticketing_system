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
	}{
		{
			name:  "success authorizes and captures, no terminal-no-side-effect",
			token: fakepsp.TokenSuccess,
			want:  Result{Outcome: Captured, Captured: true, Authorized: true, TerminalNoSideEffect: false},
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
			if got != tc.want {
				t.Fatalf("Authorize(%s)\n got=%+v\nwant=%+v", tc.token, got, tc.want)
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

// Slice 1 wires only Authorize; the compensation/status surface must announce itself as
// unimplemented rather than silently succeed, so a later slice can't accidentally rely on
// a fake that pretends to void/refund.
func TestFakeCompensationSurfaceUnimplementedInSlice1(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	if _, err := f.Capture(ctx, "ref", 1, "EUR"); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Capture: want ErrNotImplemented, got %v", err)
	}
	if _, err := f.Void(ctx, "ref", "idem"); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Void: want ErrNotImplemented, got %v", err)
	}
	if _, err := f.Refund(ctx, "ref", "idem", 1, "EUR"); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Refund: want ErrNotImplemented, got %v", err)
	}
	if _, err := f.Status(ctx, "ref"); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Status: want ErrNotImplemented, got %v", err)
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
