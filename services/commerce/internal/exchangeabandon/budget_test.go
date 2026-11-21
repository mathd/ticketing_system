package exchangeabandon

import (
	"testing"
	"time"
)

// R2-5. MaxRemoteCalls is pinned by enumerating the longest command, not by restating its
// formula: a fresh upgrade asks Access twice, reads the delta capture, inspects the replacement
// refunds three times with up to 50 pending refunds and one leg that stops the command, then
// reads its own leg, refunds, returns the target and journals.
func TestMaxRemoteCallsCoversTheLongestCommand(t *testing.T) {
	chain := []string{"access:source", "access:replacement", "payments:operation"}
	for inspection := 0; inspection < 3; inspection++ {
		for read := 0; read < 50+1; read++ {
			chain = append(chain, "payments:replacement-leg")
		}
	}
	chain = append(chain, "payments:abandonment-leg", "payments:refund", "inventory:return", "payments:fact")
	if MaxRemoteCalls != len(chain) {
		t.Fatalf("MaxRemoteCalls = %d, want the %d calls of the longest command", MaxRemoteCalls, len(chain))
	}
	const perCall = 10 * time.Second
	if got, want := CommandDeadline(perCall), time.Duration(len(chain))*perCall+2*time.Minute; got != want {
		t.Fatalf("CommandDeadline(%v) = %v, want %v", perCall, got, want)
	}
}
