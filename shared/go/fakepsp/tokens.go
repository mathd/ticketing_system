// Package fakepsp defines the opaque payment tokens accepted by the local PSP port.
package fakepsp

import "errors"

const (
	TokenSuccess = "fake-ok"
	TokenDecline = "fake-decline"
	TokenTimeout = "fake-timeout"
)

// ErrUnknownToken reports a payment token outside the fake vocabulary. It lives here,
// beside the token constants, so the "what is a valid fake token" knowledge stays in one
// package; the payments PSP port re-exports it.
var ErrUnknownToken = errors.New("unknown fake payment token")

func ValidToken(token string) bool {
	switch token {
	case TokenSuccess, TokenDecline, TokenTimeout:
		return true
	default:
		return false
	}
}
