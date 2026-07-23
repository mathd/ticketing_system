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
)

// ErrUnknownToken reports a payment token outside the fake vocabulary. It lives here,
// beside the token constants, so the "what is a valid fake token" knowledge stays in one
// package; the payments PSP port re-exports it.
var ErrUnknownToken = errors.New("unknown fake payment token")

func ValidToken(token string) bool {
	switch token {
	case TokenSuccess, TokenDecline, TokenTimeout, TokenAuthHold:
		return true
	default:
		return false
	}
}
