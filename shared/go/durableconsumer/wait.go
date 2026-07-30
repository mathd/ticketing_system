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
// a receive on it therefore unblocks on that close.
//
// # Arbitration when both are ready (TKT-122)
//
// `Closed()` does not encode its CAUSE, and after cancellation this process closes
// subscriptions itself: both service mains `defer nc.Close()` without joining
// their consumer goroutines. So a cancelled context plus a closed subscription is
// genuinely ambiguous — it is an ordinary stop far more often than a dead durable.
//
// **Shutdown wins when both are ready**, deterministically. Preferring termination
// would emit a durable-deletion diagnostic on every clean stop that lost that
// race, which is worse than missing a rare real one.
//
// The consequence, stated because it is easy to miss: **a durable that genuinely
// dies as SIGTERM lands is suppressed entirely.** Wait returns nil, so no error
// reaches the caller and nothing is logged — this residual is *quieter* than the
// shutdown-drain residual it is often lumped with (ADR-034), where an error is
// produced and logged and only the exit code is given up. Distinguishing the two
// cases needs a bounded join of every consumer before `nc.Close()`, which TKT-122
// weighed and declined.
//
// Before cancellation — a durable deleted under a live consumer, the case that
// actually matters — nothing is ambiguous and termination is always reported.
//
// (This paragraph previously claimed cc.Stop's ordering made an ordinary stop
// impossible to misreport, and later that termination wins. Both were wrong; the
// first survived a review pass before a second disproved it.)
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
		return nil
	case <-closed:
		// Both arms can be ready at once, and after cancellation a closed
		// subscription is AMBIGUOUS: we close it ourselves. Both mains
		// `defer nc.Close()` without joining their consumer goroutines, so on an
		// ordinary stop the connection close races a goroutine that has not yet
		// arbitrated its already-cancelled context — and `Closed()` does not encode
		// its cause.
		//
		// So shutdown wins, deterministically. Reporting termination here would
		// emit a durable-deletion diagnostic on ordinary stops, which is strictly
		// worse than missing a rare real one: it would corrupt the very operator
		// evidence TKT-122's producer-side logging exists to preserve, and it would
		// do so on every clean shutdown that lost the race.
		//
		// A durable that genuinely dies inside this window is therefore not
		// distinguished. That is the SAME accepted residual as the shutdown drain
		// (ADR-034 §"The shutdown drain stays a snapshot"): once SIGTERM has
		// landed, this process no longer tries to classify late consumer events,
		// because doing so correctly needs a bounded join of every consumer before
		// nc.Close() — the lifecycle coupling TKT-122 weighed and declined to buy.
		//
		// Before cancellation — the case that actually matters, a durable deleted
		// under a live consumer — nothing is ambiguous and termination is reported
		// exactly as before.
		select {
		case <-ctx.Done():
			return nil
		default:
			return terminated()
		}
	}
}
