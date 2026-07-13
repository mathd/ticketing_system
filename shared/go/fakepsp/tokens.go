// Package fakepsp defines the opaque payment tokens accepted by the local PSP port.
package fakepsp

const (
	TokenSuccess = "fake-ok"
	TokenDecline = "fake-decline"
	TokenTimeout = "fake-timeout"
)

func ValidToken(token string) bool {
	switch token {
	case TokenSuccess, TokenDecline, TokenTimeout:
		return true
	default:
		return false
	}
}
