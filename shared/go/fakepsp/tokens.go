// Package fakepsp defines the opaque payment tokens accepted by the local PSP port.
package fakepsp

import "errors"

const (
	TokenSuccess = "fake-ok"
	TokenDecline = "fake-decline"
	TokenTimeout = "fake-timeout"
	// TokenAuthHold simulates an interrupted real-provider flow (TKT-114/S2): the charge
	// authorizes but never captures, leaving the payment operation unresolved — the
	// payment_unknown case. A later PSP status resolves it as authorized-uncaptured, which
	// is what makes void (and S3's recovery) drivable offline.
	TokenAuthHold = "fake-auth-hold"
	// TokenRefundFailsOnce captures like TokenSuccess, but the FIRST refund submitted against
	// the charge fails terminally at the provider, and every later refund under another key
	// succeeds (TKT-525). It is the offline way to reach a failed post-purchase refund and the
	// operator outcomes that resolve one (ADR-087).
	//
	// The guarantee holds only for a capture whose provider reference payments persisted, and
	// only while the payments process that answered the refund keeps running. A capture that
	// status recovery confirmed without a stored reference has no fake charge identity: its
	// refunds take the stateless success path. A restart loses the recorded refunds, and the
	// proof then answers inconclusive.
	TokenRefundFailsOnce = "fake-refund-fail-once"
)

// ErrUnknownToken reports a payment token outside the fake vocabulary. It lives here,
// beside the token constants, so the "what is a valid fake token" knowledge stays in one
// package; the payments PSP port re-exports it.
var ErrUnknownToken = errors.New("unknown fake payment token")

func ValidToken(token string) bool {
	switch token {
	case TokenSuccess, TokenDecline, TokenTimeout, TokenAuthHold, TokenRefundFailsOnce:
		return true
	default:
		return false
	}
}
