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
	if !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("want ErrUnknownToken for an unknown token, got %v", err)
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
