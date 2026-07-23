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

// Capture, Void, Refund and Status are the compensation/status surface wired in later
// slices. Slice 1 exposes them as unimplemented so nothing can silently depend on a fake
// that pretends to compensate.
func (f *Fake) Capture(context.Context, string, int64, string) (Result, error) {
	return Result{}, ErrNotImplemented
}
func (f *Fake) Void(context.Context, string, string) (Result, error) {
	return Result{}, ErrNotImplemented
}
func (f *Fake) Refund(context.Context, string, string, int64, string) (Result, error) {
	return Result{}, ErrNotImplemented
}
func (f *Fake) Status(context.Context, string) (Result, error) {
	return Result{}, ErrNotImplemented
}
