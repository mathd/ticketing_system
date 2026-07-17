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
	lease     time.Duration
	backfill  func(context.Context) (int, error)
	log       *slog.Logger
}

// New's backfill, when non-nil, is one-shot data repair run at the top of Run —
// before the initial drain, so repaired rows publish immediately instead of
// waiting a full interval. It lives here rather than on the startup path so its
// cost and its failures belong to a background worker, never to readiness
// (TKT-71): an error is logged and the drainer keeps draining; the next boot
// retries by construction.
func New(db store.OutboxDB, publisher Publisher, interval time.Duration, batch int, backfill func(context.Context) (int, error), log *slog.Logger) *Drainer {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if batch <= 0 {
		batch = 32
	}
	if log == nil {
		log = slog.Default()
	}
	// The whole batch shares one lease but publishes sequentially, so the lease must
	// cover the slowest plausible batch, not a single publish. Sized from the batch
	// with a floor; too short and a drainer's own later rows get stolen mid-pass.
	lease := time.Duration(batch)*2*time.Second + 30*time.Second
	return &Drainer{db: db, publisher: publisher, interval: interval, batch: batch, lease: lease, backfill: backfill, log: log}
}

// Run drains until ctx is cancelled. It drains once immediately: on restart, rows
// owed by the process that died are the whole point, and waiting a full interval to
// notice them would leave tickets unissued for no reason.
func (d *Drainer) Run(ctx context.Context) {
	if d.backfill != nil {
		if owed, err := d.backfill(ctx); err != nil {
			// Same convention as the claim path below: a shutdown that lands
			// mid-backfill is a normal stop, not a failure worth an ERROR line.
			if !errors.Is(err, context.Canceled) {
				d.log.ErrorContext(ctx, "backfill completion outbox", "err", err)
			}
		} else if owed > 0 {
			d.log.InfoContext(ctx, "backfilled owed completion events", "count", owed)
		}
	}
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
	msgs, err := store.ClaimOutbox(ctx, d.db, d.batch, d.lease)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			d.log.ErrorContext(ctx, "claim completion outbox", "err", err)
		}
		return 0
	}
	var published int
	for _, m := range msgs {
		if ctx.Err() != nil {
			// Shutting down: leave the rest leased. They are re-claimed once the lease
			// lapses, which is why the lease exists.
			return published
		}
		if err := d.publisher.PublishRaw(ctx, m.Subject, m.EventID, m.Envelope); err != nil {
			// Return the row with the cause recorded and a backoff applied. Marking it
			// published here would silently drop the ticket.
			if relErr := store.ReleaseOutbox(ctx, d.db, m.EventID, m.ClaimID, err); relErr != nil {
				d.log.ErrorContext(ctx, "release completion outbox", "event_id", m.EventID, "err", relErr)
			}
			if m.Attempts >= store.MaxOutboxAttempts {
				// Quarantined: it will never be claimed again, so this is the last
				// notice anyone gets. A paid order is not being issued.
				d.log.ErrorContext(ctx, "owed completion event dead-lettered; ticket not issued",
					"event_id", m.EventID, "order_id", m.OrderID, "attempts", m.Attempts, "err", err)
			} else {
				d.log.WarnContext(ctx, "publish owed completion event",
					"event_id", m.EventID, "order_id", m.OrderID, "attempts", m.Attempts, "err", err)
			}
			continue
		}
		// The broker acked (PublishRaw is synchronous), so retiring the row is safe —
		// but only if this drainer still holds the claim. If the lease lapsed and
		// another drainer took over, retiring here would mask its outcome.
		retired, err := store.MarkPublished(ctx, d.db, m.EventID, m.ClaimID)
		if err != nil {
			d.log.ErrorContext(ctx, "mark completion event published", "event_id", m.EventID, "err", err)
			continue
		}
		if !retired {
			// Lost the claim mid-publish. The event did go out; whoever holds the row
			// now may publish it again. That is a duplicate, which access dedupes on
			// consumed_events — noisy, not incorrect.
			d.log.WarnContext(ctx, "published after losing the outbox claim; duplicate delivery possible",
				"event_id", m.EventID, "order_id", m.OrderID)
			continue
		}
		published++
	}
	return published
}
