package psp

import (
	"context"
	"fmt"

	"ticketing/shared/fakepsp"
)

// Fake is the in-repo PSP used for local development and the offline gate. It reproduces
// exactly the outcomes the inline charge switch produced (server.go:169-187 before this
// refactor): fake-ok authorizes+captures, fake-decline declines, fake-timeout returns a
// status-proven no-side-effect timeout. It performs no I/O and returns no ProviderRef.
type Fake struct{}

// NewFake returns the fake PSP. It is stateless.
func NewFake() *Fake { return &Fake{} }

// Authorize maps each fake token to its normalized outcome. The mapping is the contract
// the fake_test.go table pins; changing it is a behavioural change, not a refactor.
func (f *Fake) Authorize(_ context.Context, req AuthorizeRequest) (Result, error) {
	switch req.PaymentToken {
	case fakepsp.TokenSuccess:
		return Result{Outcome: Captured, Captured: true, Authorized: true, TerminalNoSideEffect: false}, nil
	case fakepsp.TokenDecline:
		return Result{Outcome: Declined, TerminalNoSideEffect: true}, nil
	case fakepsp.TokenTimeout:
		return Result{Outcome: Timeout, TerminalNoSideEffect: true}, nil
	default:
		// Wrap the fake-specific error in the port-level sentinel so the handler checks a
		// PSP concept (ErrInvalidToken), not a fake one. errors.Is matches both.
		return Result{}, fmt.Errorf("%w: %w", ErrInvalidToken, fakepsp.ErrUnknownToken)
	}
}

// Capture, Void, Refund and Status are the compensation/status surface. The fake is
// deterministic and provider-reference-free: it reports success with a Result that obeys
// the same Validate() invariants Stripe must (TKT-114/S2). Durable operation state lives in
// the payments store, not the fake.

// Capture reports a successful capture of a prior authorization.
func (f *Fake) Capture(context.Context, string, int64, string) (Result, error) {
	return Result{Outcome: Captured, Captured: true, Authorized: true}, nil
}

// Void reports a successful void of an uncaptured authorization.
func (f *Fake) Void(context.Context, string, string) (Result, error) {
	return Result{Outcome: Voided, TerminalNoSideEffect: true}, nil
}

// Refund reports a successful refund of captured money.
func (f *Fake) Refund(context.Context, string, string, int64, string) (Result, error) {
	return Result{Outcome: Refunded}, nil
}

// Status resolves an operation. The fake has no provider to query, so it reports Unknown
// (the honest answer for a provider that keeps no state) — recovery treats Unknown as
// "not resolved", never releasing a claim on it.
func (f *Fake) Status(context.Context, StatusRequest) (Result, error) {
	return Result{Outcome: Unknown}, nil
}
