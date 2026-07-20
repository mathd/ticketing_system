package consumer

import (
	"context"
	"fmt"
	"sync/atomic"
)

// waitConsume blocks until either the parent context is cancelled (a clean
// shutdown) or the JetStream consume context closes underneath us (async
// termination: the durable was deleted, the subscription dropped, or the
// client library hit a terminal error and stopped the subscription itself).
//
// On clean shutdown it returns nil and leaves readiness alone — Run's own
// `defer c.ready.Store(false)` owns that latch. On async termination it latches
// ready false *and* returns a non-nil error: the latch makes /readyz fail
// immediately, and the error propagates through main's shared consumerErr
// channel to tear the process down. Both signals are deliberate — under Compose
// a /readyz 503 is "a visible red dot and no more" (ADR-017 §236-238), so the
// process exit is the louder of the two.
//
// It never stores true: termination does not self-heal (ADR-017 §240-241).
//
// The `closed` channel is the value returned by jetstream ConsumeContext.Closed(),
// which the library *closes* (never sends to) when consuming is fully stopped;
// a receive on it therefore unblocks on that close. On a clean shutdown main
// cancels the root context, and Run's deferred cc.Stop() — which would also
// close `closed` — runs only after Run returns, so the ctx.Done() arm reliably
// wins the ordinary path. If both are ready at once (a subscription killed at
// the exact instant of shutdown), the select picks either arm; both answers are
// defensible, so callers must not depend on which.
func waitConsume(ctx context.Context, closed <-chan struct{}, ready *atomic.Bool, name string) error {
	select {
	case <-ctx.Done():
		return nil
	case <-closed:
		ready.Store(false)
		return fmt.Errorf("%s: consume context closed (durable deleted or subscription terminated)", name)
	}
}
