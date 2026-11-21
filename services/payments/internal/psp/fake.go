package psp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"ticketing/shared/fakepsp"
)

// Fake is the in-repo PSP used for local development and the offline gate. It reproduces
// exactly the outcomes the inline charge switch produced (server.go:169-187 before this
// refactor): fake-ok authorizes+captures, fake-decline declines, fake-timeout returns a
// status-proven no-side-effect timeout. It performs no I/O. It returns no ProviderRef,
// except for a fake-refund-fail-once charge.
//
// It is stateless for every token except fake-refund-fail-once (TKT-525). For a charge with
// that token, the fake keeps the refunds submitted against it in memory, the same way a
// provider keeps refunds against a PaymentIntent, so that it can fail the first one and later
// prove that failure (ProveRefundFailure). A process restart loses that memory; the proof is
// then inconclusive, which is the honest answer.
type Fake struct {
	mu sync.Mutex
	// refunds holds, for each fake-refund-fail-once charge reference, the refunds submitted
	// against it in submission order. The zero value is usable.
	refunds map[string][]fakeRefund
}

// fakeRefund is one refund the fake recorded against a fake-refund-fail-once charge.
type fakeRefund struct {
	id       string // re_…
	key      string // the compensation stamp: the provider idempotency key it was submitted under
	amount   int64
	currency string
	failed   bool
}

// NewFake returns the fake PSP.
func NewFake() *Fake { return &Fake{} }

// failOnceRefPrefix marks the charge reference of a fake-refund-fail-once charge. No other
// fake charge has a reference, so a refund against any other charge keeps the stateless path.
const failOnceRefPrefix = "pi_fake_refund_fail_once_"

func fakeDigest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:12])
}

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
	case fakepsp.TokenRefundFailsOnce:
		// Captured exactly like fake-ok, plus a charge reference derived from the operation's
		// identity. Payments stores it, and the refund and proof paths need it: it is what
		// identifies the charge whose first refund fails.
		return Result{Outcome: Captured, Captured: true, Authorized: true,
			ProviderRef: failOnceRefPrefix + fakeDigest(req.OrganizerID, req.IdempotencyKey),
			Confirmed:   &ConfirmedMoney{Amount: req.Amount, Currency: req.Currency}}, nil
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
//
// A fake-refund-fail-once charge is the exception (TKT-525). Its first refund fails
// terminally, in the shape the Stripe adapter gives a `failed` refund; every later refund
// under another key succeeds. A refund under a key already recorded answers what was recorded,
// the way a provider replays an idempotency key, and records nothing new.
func (f *Fake) Refund(_ context.Context, providerRef, idempotencyKey string, amount int64, currency string) (Result, error) {
	if !strings.HasPrefix(providerRef, failOnceRefPrefix) {
		return Result{Outcome: Refunded, Confirmed: &ConfirmedMoney{Amount: amount, Currency: currency}}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refunds == nil {
		f.refunds = map[string][]fakeRefund{}
	}
	recorded := f.refunds[providerRef]
	if rf, ok := findFakeRefund(recorded, idempotencyKey); ok {
		return rf.result()
	}
	rf := fakeRefund{id: "re_fake_" + fakeDigest(providerRef, idempotencyKey), key: idempotencyKey,
		amount: amount, currency: currency, failed: len(recorded) == 0}
	f.refunds[providerRef] = append(recorded, rf)
	return rf.result()
}

func (rf fakeRefund) result() (Result, error) {
	if rf.failed {
		return Result{Outcome: Unknown, ProviderRef: rf.id}, &RefundFailedError{ProviderRef: rf.id}
	}
	return Result{Outcome: Refunded, ProviderRef: rf.id, Confirmed: &ConfirmedMoney{Amount: rf.amount, Currency: rf.currency}}, nil
}

// findFakeRefund returns the one refund recorded under a key. Refund replays a recorded key
// instead of recording it again, so a key has at most one entry.
func findFakeRefund(recorded []fakeRefund, key string) (fakeRefund, bool) {
	for _, rf := range recorded {
		if rf.key == key {
			return rf, true
		}
	}
	return fakeRefund{}, false
}

// ProveRefundFailure is the fake's read-only proof (TKT-525, ADR-087). It proves only a refund
// that the fake itself recorded as failed. It looks the refund up by charge reference and key;
// a refund whose amount or currency differs does not corroborate it (inconclusive), a refund
// that succeeded is a contradiction, and no recorded refund is inconclusive.
//
// A fake charge with no reference (every token except fake-refund-fail-once) has nothing
// recorded, so it can never be proven. It never moves money and records nothing.
func (f *Fake) ProveRefundFailure(_ context.Context, providerRef, idempotencyKey string, amount int64, currency string) (RefundFailureProof, error) {
	if providerRef == "" || idempotencyKey == "" {
		return RefundFailureProof{}, fmt.Errorf("%w: no provider charge reference or refund key to look under", ErrRefundProofInconclusive)
	}
	f.mu.Lock()
	rf, ok := findFakeRefund(f.refunds[providerRef], idempotencyKey)
	f.mu.Unlock()
	switch {
	case !ok:
		return RefundFailureProof{}, fmt.Errorf("%w: no refund carries this key", ErrRefundProofInconclusive)
	case amount <= 0 || rf.amount != amount || rf.currency != currency:
		return RefundFailureProof{}, fmt.Errorf("%w: fake refund %q carries this key with evidence that does not corroborate it", ErrRefundProofInconclusive, rf.id)
	case !rf.failed:
		return RefundFailureProof{}, fmt.Errorf("%w: fake refund %q succeeded", ErrRefundProofContradicted, rf.id)
	}
	return RefundFailureProof{ProviderRef: rf.id}, nil
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
	case fakepsp.TokenRefundFailsOnce:
		// Captured like fake-ok. The charge reference is the persisted one: StatusRequest does
		// not carry the organizer the reference was derived from.
		return Result{Outcome: Captured, Captured: true, Authorized: true, ProviderRef: req.ProviderRef,
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
