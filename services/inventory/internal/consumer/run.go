package consumer

import (
	"context"
	"sync/atomic"

	"ticketing/shared/durableconsumer"
)

// waitConsume is inventory's seam onto durableconsumer.Wait, the same shape
// access uses (ADR-034). Run calls this symbol and so do run_test.go's tests,
// so the guarantee is tested on the path that ships rather than on a façade.
//
// Adopting it is a BEHAVIOUR ADDITION for inventory, not a refactor: before
// TKT-127, Run blocked on <-ctx.Done() alone and never observed
// ConsumeContext.Closed(). A durable deleted underneath a live consumer left the
// process up and READY with nothing consuming — precisely the silent stall
// ADR-017 §236-241 rules out. Access has had the guarantee since TKT-97;
// inventory had the same loop and not the same protection, which is the
// duplication TKT-127 exists to end.
// cause carries what the BROKER said (TKT-123): a durable the server confirmed
// deleted reads differently from any other reason consuming stopped, and only a
// ConsumeErrHandler ever hears that. Pass nil for no handler — the diagnostic
// degrades to the generic form rather than lying.
func waitConsume(ctx context.Context, closed <-chan struct{}, ready *atomic.Bool, name string, cause *durableconsumer.TerminationCause) error {
	return durableconsumer.WaitWithCause(ctx, closed, ready, name, cause)
}
