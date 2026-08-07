// Package mailer drains owed transactional messages to a mail.Sender (TKT-226,
// ADR-050).
//
// It is commerce's second background worker and it is deliberately shaped like the
// first (internal/outbox): claim under a lease, send, retire only after the sender
// returned, release with backoff on failure, dead-letter once attempts are exhausted.
// The store functions differ because the tables differ; the protocol does not.
//
// Why a queue at all, when the only sender in this repo is a fake that cannot fail —
// the answer is NOT "durability for its own sake", and ADR-050 §3 is where it lives:
// inline sending cannot satisfy the reset endpoint's enumeration-parity requirement,
// because an unknown address cannot fail to send and a known one can. Enqueueing is the
// only shape where both answers are written before any delivery is attempted.
package mailer

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"ticketing/shared/mail"

	"ticketing/services/commerce/internal/store"
)

type Drainer struct {
	db       store.OutboxDB
	sender   mail.Sender
	interval time.Duration
	batch    int
	lease    time.Duration
	log      *slog.Logger
}

// New builds the drainer. Defaults mirror the completion outbox's, including the lease
// derivation: the whole batch shares one lease but sends sequentially, so the lease must
// cover the slowest plausible batch rather than a single send, or a drainer's own later
// rows get stolen mid-pass.
func New(db store.OutboxDB, sender mail.Sender, interval time.Duration, batch int, log *slog.Logger) *Drainer {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if batch <= 0 {
		batch = 32
	}
	if log == nil {
		log = slog.Default()
	}
	lease := time.Duration(batch)*2*time.Second + 30*time.Second
	return &Drainer{db: db, sender: sender, interval: interval, batch: batch, lease: lease, log: log}
}

// Run drains until ctx is cancelled, once immediately. On restart the rows owed by the
// process that died are the point: a buyer waiting for a reset link should not wait an
// extra interval because commerce was redeployed.
func (d *Drainer) Run(ctx context.Context) {
	d.DrainOnce(ctx)
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Shutdown: stop claiming. In-flight rows keep their lease and are picked up
			// once it lapses — at-least-once holds across a restart because a row is only
			// retired after the sender returned.
			return
		case <-t.C:
			d.DrainOnce(ctx)
		}
	}
}

// DrainOnce claims a batch and sends it. Returns the number of messages successfully
// sent, for tests and for callers draining to quiescence.
func (d *Drainer) DrainOnce(ctx context.Context) int {
	msgs, err := store.ClaimMail(ctx, d.db, d.batch, d.lease)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			d.log.ErrorContext(ctx, "claim mail outbox", "err", err)
		}
		return 0
	}
	var sent int
	for _, m := range msgs {
		if ctx.Err() != nil {
			// Shutting down: leave the rest leased. They are re-claimed once the lease
			// lapses, which is why the lease exists.
			return sent
		}
		if err := d.sender.Send(ctx, mail.Message{To: m.Recipient, Subject: m.Subject, Body: m.Body}); err != nil {
			// Return the row with the cause recorded and a backoff applied. Marking it
			// sent here would turn a lost reset into a delivered one.
			if relErr := store.ReleaseMail(ctx, d.db, m.ID, m.ClaimID, err); relErr != nil {
				d.log.ErrorContext(ctx, "release mail outbox", "message_id", m.ID, "err", relErr)
			}
			// NOTHING from the message reaches these lines — no recipient, no subject,
			// no body. The row id is the operator's handle, and it is a uuid that
			// discloses nothing. For a password reset the body IS a live credential and
			// the recipient is the fact the endpoint refuses to disclose, so logging
			// either here would undo the whole enumeration argument in a WARN line.
			if m.Attempts >= store.MaxMailAttempts {
				// Quarantined: it will never be claimed again, so this is the last notice
				// anyone gets. Someone asked for a password reset and will not receive it.
				d.log.ErrorContext(ctx, "transactional message dead-lettered; it will never be sent",
					"message_id", m.ID, "attempts", m.Attempts, "err", err)
			} else {
				d.log.WarnContext(ctx, "send transactional message",
					"message_id", m.ID, "attempts", m.Attempts, "err", err)
			}
			continue
		}
		// The sender returned success, so retiring is safe — but only if this drainer
		// still holds the claim. If the lease lapsed and another drainer took over,
		// retiring here would mask its outcome.
		retired, err := store.MarkMailSent(ctx, d.db, m.ID, m.ClaimID)
		if err != nil {
			// The message WAS sent and the row still says it was not, so it will be
			// claimed and sent again once the lease lapses. That is at-least-once, which
			// the port accepts — but only if it is BOUNDED (ai-review [high]).
			//
			// Releasing is what bounds it: it applies the backoff and, after
			// MaxMailAttempts, dead-letters. Without it the row keeps its lease, expires,
			// is re-claimed, sends again, fails to retire again — a persistent write
			// failure mails the same reset link forever, to a person.
			//
			// The cost is that a dead-lettered row here may have been delivered every
			// time, so `last_error` on a quarantined message is not proof it never
			// arrived. That trade is deliberate: ten duplicates and a loud ERROR beats an
			// unbounded loop. The completion outbox takes the opposite branch and is
			// right to — a republished event is deduped by access on `consumed_events`,
			// while a re-sent email lands in a human's inbox.
			if relErr := store.ReleaseMail(ctx, d.db, m.ID, m.ClaimID, err); relErr != nil {
				d.log.ErrorContext(ctx, "release after failing to mark sent", "message_id", m.ID, "err", relErr)
			}
			d.log.ErrorContext(ctx, "message was sent but could not be marked sent; it may be delivered again",
				"message_id", m.ID, "attempts", m.Attempts, "err", err)
			continue
		}
		if !retired {
			// The lease lapsed mid-send and someone else owns the row. The message may
			// be sent twice, which the port accepts (a duplicate email is an annoyance).
			d.log.WarnContext(ctx, "mail claim lost mid-send; message may be delivered twice",
				"message_id", m.ID)
			continue
		}
		sent++
	}
	return sent
}
