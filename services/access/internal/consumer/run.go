package consumer

import (
	"context"
	"sync/atomic"

	"ticketing/shared/durableconsumer"
)

// waitConsume is the package-local seam onto durableconsumer.Wait, which owns
// the behaviour and the contract (see that package's doc comment; ADR-034).
// TKT-127 moved the body out; the symbol and its signature stay.
//
// The delegate is deliberate, not a compatibility shim. TKT-97's four tests in
// run_test.go call this symbol, and both production call sites — Consumer.Run
// ("access-ticket-issuer") and PolicyConsumer.Run ("access-slot-policy") — go
// through it too. Point those call sites straight at durableconsumer.Wait and
// the tests would still pass while testing nothing that ships: a façade. Keeping
// one symbol for both is what keeps the guarantee tested on the real path.
// cause carries what the BROKER said (TKT-123): a durable the server confirmed
// deleted reads differently from any other reason consuming stopped, and only a
// ConsumeErrHandler ever hears that. Pass nil for no handler — the diagnostic
// degrades to the generic form rather than lying.
func waitConsume(ctx context.Context, closed <-chan struct{}, ready *atomic.Bool, name string, cause *durableconsumer.TerminationCause) error {
	return durableconsumer.WaitWithCause(ctx, closed, ready, name, cause)
}
