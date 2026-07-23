// Package psp defines the provider-agnostic payment-service-provider port and its
// implementations. The port normalizes every provider's authorize/capture/void/refund/
// status semantics into a single vocabulary payments can journal against, so the money
// journal and the recovery state machine never learn a provider's dialect (ADR-032).
//
// Slice 1 (TKT-56) lands the port contract and the fake implementation refactored out of
// the inline charge handler. The Stripe implementation, and the wiring of Capture/Void/
// Refund/Status into recovery, arrive in later slices; the interface is defined in full
// here so those slices extend a fixed contract rather than reshape it.
package psp

import (
	"context"
	"errors"
	"fmt"
)

// Outcome is the normalized result vocabulary. It is deliberately provider-neutral: a
// Stripe decline and a fake decline are both Declined. The recovery state machine and the
// journal fact types are derived from this, never from a provider status string.
type Outcome string

const (
	// Authorized: funds are held but not captured. Distinct from Captured because
	// compensation for an authorization is a void, for a capture a refund (ADR-016 §Dec4).
	Authorized Outcome = "authorized"
	// Captured: money has moved. The happy path for the current immediate-capture flow.
	Captured Outcome = "captured"
	// Declined: the provider refused. A terminal answer proving no side effect.
	Declined Outcome = "declined"
	// Timeout: the provider answer proves no side effect (ADR-016 §Dec3's fake-PSP
	// timeout whose status proves no capture). NOT a transport timeout — that is Unknown.
	Timeout Outcome = "timeout"
	// Unknown: the side effect is genuinely undetermined (transport failure before a
	// provider answer). Reserved for the Stripe status path (later slice); the fake never
	// returns it. Present in the contract so recovery is written against the full set.
	Unknown Outcome = "unknown"
)

// ErrNotImplemented is returned by an implementation for a port operation a given slice
// has not wired yet. Slice 1 wires only Authorize (the charge path); Capture/Void/Refund/
// Status exist on the interface but the fake returns this until their slices land.
var ErrNotImplemented = errors.New("psp: operation not implemented")

// ErrInvalidToken reports a payment token/method the provider cannot accept. It is a
// port-level sentinel so the handler checks a PSP concept, not a fake-specific one: the
// fake wraps fakepsp.ErrUnknownToken with it, and a future Stripe adapter maps its own
// "no such payment method" error to it. The charge handler answers 400 on this.
var ErrInvalidToken = errors.New("psp: invalid payment token")

// AuthorizeRequest is the provider-neutral charge input. Money is integer minor units +
// ISO currency; floats are banned on money paths.
type AuthorizeRequest struct {
	OrganizerID string
	OrderID     string
	BuyerID     string
	Amount      int64
	Currency    string
	// PaymentToken is the opaque provider payment-method reference. For the fake PSP it is
	// one of the fakepsp tokens; for Stripe it is a PaymentMethod/PaymentIntent reference.
	PaymentToken string
	// IdempotencyKey is the payment operation's stable idempotency key (the same key the
	// handler binds the operation under). A provider must submit under this key so a retry
	// after a lost response cannot create a second authorization — the fake ignores it, but
	// the contract carries it from Slice 1 so the Stripe adapter (Slice 2) never has to
	// reshape this struct to become crash-safe. See ADR-032 §Status/replay.
	IdempotencyKey string
}

// Result is the normalized outcome of an authorize. The handler derives the journal fact
// type and HTTP status from it; the port never touches the journal.
type Result struct {
	Outcome Outcome
	// Captured reports whether money moved (Outcome == Captured). Convenience for the
	// handler; always consistent with Outcome.
	Captured bool
	// Authorized reports whether a distinct authorization was established before capture.
	// The immediate-capture fake sets this true on success (it appends payment.authorized
	// then payment.captured); a hypothetical auth-only flow would set it without Captured.
	Authorized bool
	// TerminalNoSideEffect is true when the outcome proves no money moved and never will
	// for this attempt (Declined or a status-proven Timeout) — ADR-016 §Dec3. Recovery may
	// release the claim on this; it must NOT release on Unknown.
	TerminalNoSideEffect bool
	// ProviderRef is the provider's durable identity for the operation (Stripe charge id).
	// Empty for the fake PSP. Stored on the operation, never in the journal payload (S2).
	ProviderRef string
}

// Validate rejects a self-contradictory Result. The charge handler journals from a Result,
// so a provider that returns an impossible combination (a captured outcome that did not
// capture, a decline that also claims money moved) must be caught at the money-path
// boundary before anything is written — the fake always produces consistent Results, but
// the interface cannot assume every future adapter does. The handler calls this fail-closed
// before it appends any fact.
func (r Result) Validate() error {
	switch r.Outcome {
	case Captured:
		if !r.Captured || !r.Authorized || r.TerminalNoSideEffect {
			return fmt.Errorf("psp: captured outcome must be captured+authorized and not terminal-no-side-effect: %+v", r)
		}
	case Authorized:
		if !r.Authorized || r.Captured || r.TerminalNoSideEffect {
			return fmt.Errorf("psp: authorized-only outcome must be authorized, not captured, not terminal-no-side-effect: %+v", r)
		}
	case Declined, Timeout:
		if r.Captured || r.Authorized || !r.TerminalNoSideEffect {
			return fmt.Errorf("psp: %s must prove no side effect (not captured, not authorized, terminal-no-side-effect): %+v", r.Outcome, r)
		}
	case Unknown:
		if r.Captured || r.Authorized || r.TerminalNoSideEffect {
			return fmt.Errorf("psp: unknown outcome cannot claim capture/authorization or terminal-no-side-effect: %+v", r)
		}
	default:
		return fmt.Errorf("psp: unrecognized outcome %q", r.Outcome)
	}
	return nil
}

// PSP is the provider-agnostic port. Slice 1 defines all five operations but only
// Authorize is exercised; the rest are the compensation/status surface later slices wire
// into recovery. An implementation that has not wired an operation returns ErrNotImplemented.
type PSP interface {
	// Authorize submits a charge. For the immediate-capture flow it authorizes and captures
	// in one step, reflected in the Result (Authorized && Captured on success).
	Authorize(ctx context.Context, req AuthorizeRequest) (Result, error)
	// Capture captures a prior authorization. Unwired in Slice 1.
	Capture(ctx context.Context, providerRef string, amount int64, currency string) (Result, error)
	// Void cancels an uncaptured authorization. Unwired in Slice 1.
	Void(ctx context.Context, providerRef, idempotencyKey string) (Result, error)
	// Refund refunds a captured charge. Unwired in Slice 1.
	Refund(ctx context.Context, providerRef, idempotencyKey string, amount int64, currency string) (Result, error)
	// Status retrieves the provider's current view of an operation. Unwired in Slice 1.
	//
	// NOTE (TKT-56 S2): the crash-safe recovery path (ADR-032 §Status/replay) needs Status
	// to resolve an operation that timed out *before* a ProviderRef was persisted, by
	// replaying the original request under the same IdempotencyKey. A bare providerRef
	// cannot express that. The Stripe slice replaces this signature with a request carrying
	// both an optional providerRef and the durable original request + idempotency key; the
	// interface is intentionally minimal in S1 (Status is not yet called anywhere).
	Status(ctx context.Context, providerRef string) (Result, error)
}
