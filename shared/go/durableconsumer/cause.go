package durableconsumer

import "sync/atomic"

// TerminationCause carries what the BROKER said, from the goroutine that heard it
// to the one that reports it.
//
// jetstream distinguishes a deleted durable (ErrConsumerDeleted, a server 409
// status message) from every other reason a subscription stops, but it delivers
// that only through a ConsumeErrHandler callback — while Wait learns of the stop
// through ConsumeContext.Closed(), which carries no cause at all. Before TKT-123
// neither service registered a handler, so the distinction existed in the library
// and nowhere else: TKT-99's broker-level test could prove a durable was deleted
// and that something terminated the consumer, but not that the first caused the
// second.
//
// The ordering that makes an atomic sufficient is a real dependency on library
// internals, so it is written down rather than assumed: on a terminal status
// nats.go calls ErrHandler and only THEN Stop() (jetstream/pull.go:277-280), and
// Stop is what eventually closes Closed(). A Wait that observes Closed() therefore
// always observes a store that happened before it. If a nats.go upgrade reorders
// those two calls, this degrades to the generic diagnostic — it does not lie.
//
// An atomic rather than a channel because the callback must never block, and
// rather than atomic.Pointer[error] because only one bit of classification is
// wanted: exactly one cause is actionable, and holding the error would invite
// callers to render a library string an operator has no contract with.
type TerminationCause struct{ deleted atomic.Bool }

// MarkConsumerDeleted records that the broker confirmed the durable is gone.
// Safe to call from the ConsumeErrHandler goroutine, and idempotent.
func (c *TerminationCause) MarkConsumerDeleted() {
	if c != nil {
		c.deleted.Store(true)
	}
}

// consumerDeleted reports whether the broker confirmed deletion. A nil cause is
// "no evidence", so callers that do not register a handler keep the old
// behaviour rather than crashing.
func (c *TerminationCause) consumerDeleted() bool { return c != nil && c.deleted.Load() }
