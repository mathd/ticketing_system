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
		// The fake ECHOES the request as its confirmation (TKT-257). That is honest for a
		// simulator — it "moved" exactly what it was asked to — and it keeps the offline
		// gate's agreement paths exercising the real settle-on-confirmation code.
		//
		// State the consequence plainly, because it is the reason this ticket existed: a
		// fake that echoes CAN NEVER DISAGREE, so no fake-backed test can demonstrate the
		// fail-closed guard. Every fake-backed assertion here is regression cover. The
		// divergence proofs run through the two seams where a provider's figure and the
		// request really can differ: `divergentPSP`, the port-level stub the api package's
		// provider_confirmation_smoke_test.go drives every write sink with, and the Stripe
		// adapter's httptest stub in stripe_confirmed_test.go. An earlier version of this
		// comment named the Stripe stub as the ONLY such seam; that was wrong when written,
		// and TKT-298 relied on the true answer — the fake's Status now echoes too, which
		// would have been a real loss of coverage had the claim been accurate.
		return Result{Outcome: Captured, Captured: true, Authorized: true, TerminalNoSideEffect: false,
			Confirmed: &ConfirmedMoney{Amount: req.Amount, Currency: req.Currency}}, nil
	case fakepsp.TokenDecline:
		return Result{Outcome: Declined, TerminalNoSideEffect: true}, nil
	case fakepsp.TokenTimeout:
		return Result{Outcome: Timeout, TerminalNoSideEffect: true}, nil
	case fakepsp.TokenAuthHold:
		// Authorized-only: the charge handler fails closed on this (its switch only
		// journals captured/declined/timeout), leaving the operation bound-unresolved —
		// the offline simulation of a crashed real-provider flow (payment_unknown).
		return Result{Outcome: Authorized, Authorized: true}, nil
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

// Capture reports a successful capture of a prior authorization, confirming the amount it
// was asked to capture (TKT-257; see Authorize on why an echo cannot prove the guard).
func (f *Fake) Capture(_ context.Context, _ string, amount int64, currency string) (Result, error) {
	return Result{Outcome: Captured, Captured: true, Authorized: true,
		Confirmed: &ConfirmedMoney{Amount: amount, Currency: currency}}, nil
}

// Void reports a successful void of an uncaptured authorization.
func (f *Fake) Void(context.Context, string, string) (Result, error) {
	return Result{Outcome: Voided, TerminalNoSideEffect: true}, nil
}

// Refund reports a successful refund of captured money, confirming the amount it was asked
// to return. Before TKT-257 this signature discarded its amount argument entirely.
func (f *Fake) Refund(_ context.Context, _, _ string, amount int64, currency string) (Result, error) {
	return Result{Outcome: Refunded, Confirmed: &ConfirmedMoney{Amount: amount, Currency: currency}}, nil
}

// Status resolves an operation deterministically from the replayed token — the same
// durable evidence the store carries in StatusRequest — mirroring Stripe's replay-under-
// the-same-key contract without hidden state. An empty/unknown token stays Unknown: no
// evidence, no resolution, and recovery never releases a claim on Unknown.
//
// The CAPTURED branch echoes the replayed request as its confirmation, exactly as
// Authorize, Capture and Refund do (TKT-298). It used to carry none, on the argument that
// echoing the stored request "would fabricate the evidence TKT-257 removes" — but that
// argument condemns this fake's other three methods equally, and they are the ones that
// are right: for a simulator, confirming the request IS the honest answer, because the
// simulator moved exactly what it was asked to.
//
// Withholding it was not neutral, it was broken. pspStatus refuses any Captured resolution
// whose confirmation does not Agrees (api/psp.go), and a nil confirmation never agrees, so
// an operation left bound-unresolved by a crash on a fake-ok charge could NEVER be
// status-resolved: payments answered 502 forever and commerce's runner parked the order
// permanently. A Status that cannot resolve its own success token is not a conservative
// Status, it is a false one — and this method's own doc line above promised otherwise.
//
// The other branches carry nothing, and that is not an oversight either: ConfirmedMoney is
// the captured figure, and an authorization, a decline or a timeout moved no money for a
// provider to confirm. Result.Validate enforces it.
//
// This costs no divergence coverage, which is the precondition that makes it safe. An echo
// can never disagree, so no fake-backed test could ever have demonstrated the fail-closed
// guard; the proofs run where a provider figure and the request really can differ —
// divergentPSP in the api package, and the Stripe httptest stub in stripe_confirmed_test.go.
func (f *Fake) Status(_ context.Context, req StatusRequest) (Result, error) {
	switch req.PaymentToken {
	case fakepsp.TokenSuccess:
		return Result{Outcome: Captured, Captured: true, Authorized: true,
			Confirmed: &ConfirmedMoney{Amount: req.Amount, Currency: req.Currency}}, nil
	case fakepsp.TokenAuthHold:
		return Result{Outcome: Authorized, Authorized: true}, nil
	case fakepsp.TokenDecline:
		return Result{Outcome: Declined, TerminalNoSideEffect: true}, nil
	case fakepsp.TokenTimeout:
		return Result{Outcome: Timeout, TerminalNoSideEffect: true}, nil
	default:
		return Result{Outcome: Unknown}, nil
	}
}
