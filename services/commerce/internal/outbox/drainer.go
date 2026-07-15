// Package outbox drains owed completion events to JetStream.
//
// This is commerce's first background worker: until TKT-43 the service only ever did
// work with an inbound request on the stack. The drainer closes the window between
// CompleteOrder's commit and publication (ADR-016 §Decision 6) — a crash there used to
// leave a paid order whose ticket was never issued, recoverable only by an exact
// checkout replay that nothing generated.
package outbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
)

// Publisher transmits an already-serialized envelope. The drainer never rebuilds the
// payload: the frozen bytes from the outbox row are what goes on the wire.
type Publisher interface {
	PublishRaw(ctx context.Context, subject string, eventID uuid.UUID, envelope []byte) error
}

type Drainer struct {
	db        store.OutboxDB
	publisher Publisher
	interval  time.Duration
	batch     int
	log       *slog.Logger
}

func New(db store.OutboxDB, publisher Publisher, interval time.Duration, batch int, log *slog.Logger) *Drainer {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if batch <= 0 {
		batch = 32
	}
	if log == nil {
		log = slog.Default()
	}
	return &Drainer{db: db, publisher: publisher, interval: interval, batch: batch, log: log}
}

// Run drains until ctx is cancelled. It drains once immediately: on restart, rows
// owed by the process that died are the whole point, and waiting a full interval to
// notice them would leave tickets unissued for no reason.
func (d *Drainer) Run(ctx context.Context) {
	d.DrainOnce(ctx)
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Shutdown: stop claiming. In-flight rows keep their lease and are picked
			// up by the next drainer once it lapses — at-least-once holds across a
			// restart because publication is only retired after an ack.
			return
		case <-t.C:
			d.DrainOnce(ctx)
		}
	}
}

// DrainOnce claims a batch and publishes it. Returns the number of messages
// successfully published, for tests and for callers that want to drain to quiescence.
func (d *Drainer) DrainOnce(ctx context.Context) int {
	msgs, err := store.ClaimOutbox(ctx, d.db, d.batch)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			d.log.ErrorContext(ctx, "claim completion outbox", "err", err)
		}
		return 0
	}
	var published int
	for _, m := range msgs {
		if ctx.Err() != nil {
			return published
		}
		if err := d.publisher.PublishRaw(ctx, m.Subject, m.EventID, m.Envelope); err != nil {
			// Publication failed: return the row to the claimable set with the cause
			// recorded. Marking it published here would silently drop the ticket.
			if relErr := store.ReleaseOutbox(ctx, d.db, m.EventID, err); relErr != nil {
				d.log.ErrorContext(ctx, "release completion outbox", "event_id", m.EventID, "err", relErr)
			}
			// A row that keeps failing is a poison row — surface it rather than
			// retrying in silence.
			d.log.WarnContext(ctx, "publish owed completion event",
				"event_id", m.EventID, "order_id", m.OrderID, "attempts", m.Attempts, "err", err)
			continue
		}
		// Ack happened (PublishRaw is synchronous), so retiring the row is safe.
		if err := store.MarkPublished(ctx, d.db, m.EventID); err != nil {
			// Published but not retired: the duplicate on retry is deduped by the
			// deterministic Nats-Msg-Id, so this is safe, just noisy.
			d.log.ErrorContext(ctx, "mark completion event published", "event_id", m.EventID, "err", err)
			continue
		}
		published++
	}
	return published
}
