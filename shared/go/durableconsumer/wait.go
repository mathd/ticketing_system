// Package durableconsumer holds the one piece of durable-consumer *behaviour*
// that is identical across services: how a consumer distinguishes a clean
// shutdown from asynchronous termination, and what it owes the operator when it
// cannot tell the difference quietly.
//
// # Why this is in the shared kernel, and what it deliberately excludes
//
// shared/go is a shared kernel whose additions require an ADR
// (docs/architecture.md); ADR-034 is this package's. ADR-033 drew the line it
// sits behind: domainevent is shared *contract* (what an envelope IS), and it
// explicitly refused to become a consumer framework because "dispositions
// genuinely differ" — inventory quarantines and acks, access parks outstanding.
// It handed the run-loop half here, as shared *behaviour*, "deserving its own
// argument". This package is that argument, kept as small as it can be.
//
// NOT here, on purpose — all of it stays with the owning service:
//
//   - Durable configuration, stream lookup, handler registration and dispatch.
//     The two services' consumerConfig differ in filter subjects, MaxAckPending
//     and BackOff, and inventory deletes an orphaned durable on the way in.
//   - Disposition. Term, park, quarantine, NAK-with-delay and whether readiness
//     latches are service policy (ADR-033).
//   - Envelope decoding, per-subject known schema ranges, retry schedules,
//     quarantine persistence and dedup.
//   - Readiness SERIALIZATION. Inventory serializes its two readiness writers
//     behind an unexported readinessMu (TKT-90); access has no such race,
//     because it has no quarantine store to check against. Lifting that mutex
//     here was proposed and rejected at plan review: an unexported field in the
//     same package as its only two writers cannot be bypassed, while an exported
//     kernel type with a public Store would turn a structural guarantee into a
//     documented convention — a downgrade of one of the very guarantees TKT-127
//     was told to preserve. The latch itself needs no lifting: it is
//     sync/atomic.Bool, which already exists exactly once.
//
// The dividing line: this package owns when a consumer stops and what it says
// about it. Everything about what a consumer *does* stays with the service.
package durableconsumer

import (
	"context"
	"fmt"
	"sync/atomic"
)

// Wait blocks until either the parent context is cancelled (a clean shutdown)
// or the JetStream consume context closes underneath us (async termination: the
// durable was deleted, the subscription dropped, or the client library hit a
// terminal error and stopped the subscription itself).
//
// On clean shutdown it returns nil and leaves readiness alone — the caller's own
// `defer ready.Store(false)` owns that latch. On async termination it latches
// ready false *and* returns a non-nil error: the latch makes /readyz fail
// immediately, and the error propagates through main's consumerErr channel to
// tear the process down. Both signals are deliberate — under Compose a /readyz
// 503 is "a visible red dot and no more" (ADR-017 §236-238), so the process exit
// is the louder of the two.
//
// It never stores true: termination does not self-heal (ADR-017 §240-241).
//
// The `closed` channel is the value returned by jetstream ConsumeContext.Closed(),
// which the library *closes* (never sends to) when consuming is fully stopped;
// a receive on it therefore unblocks on that close. On a clean shutdown main
// cancels the root context, and the caller's deferred cc.Stop() — which would
// also close `closed` — runs only after Run returns, so the ctx.Done() arm
// reliably wins the ordinary path — an ordinary stop can never be misreported as
// a termination. If both are ready at once (a subscription killed at the exact
// instant of shutdown), **termination wins**; see the nested select below. That
// was deliberately unspecified until TKT-122 ("both answers are defensible, so
// callers must not depend on which") and stopped being defensible once the
// classification began carrying the operator's only evidence.
//
// The error string is a CONTRACT, not a log line. smoke/access_consumer_test.go
// (TKT-99) asserts it verbatim against a real broker after deleting a durable,
// and its comment calls that literal "the whole discriminator" — it is what ties
// an observed container restart to this function rather than to any other crash.
// TestWaitTerminationDiagnosticIsExact pins it here as a hand-written literal so
// a drift fails in milliseconds instead of after a container restart. Changing
// the wording means changing both, deliberately (and TKT-123, which wants a
// finer-grained cause, must do exactly that).
func Wait(ctx context.Context, closed <-chan struct{}, ready *atomic.Bool, name string) error {
	terminated := func() error {
		ready.Store(false)
		return fmt.Errorf("%s: consume context closed (durable deleted or subscription terminated)", name)
	}
	select {
	case <-ctx.Done():
		// Both arms can be ready at once — a durable deleted at the instant of
		// SIGTERM. Picking at random was defensible while the answer changed only
		// an exit code nothing reads. It stopped being defensible once TKT-122 made
		// this classification carry the operator's evidence: a nil here is filtered
		// by both mains as a clean stop, so the durable's death would leave no
		// error, no log line and no trace at all.
		//
		// So termination wins when it is observable. It is the more serious of the
		// two states, the only one carrying a diagnostic, and it never self-heals
		// (ADR-017 §240-241) — while "we were also asked to stop" is already
		// evident from the fact that we are stopping.
		select {
		case <-closed:
			return terminated()
		default:
			return nil
		}
	case <-closed:
		return terminated()
	}
}
