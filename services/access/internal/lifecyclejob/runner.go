// Package lifecyclejob runs Access's two lifecycle-trail background workers: the
// per-organizer checkpoint builder and the integrity alarm drainer.
//
// Neither worker gates the door. ADR-021 §D6 chose fail-open on the judgement
// that our own bugs are likelier than an attacker and that denying a real
// customer at a live turnstile is the worse failure — "the trail's job is to make
// tampering evident, not to make the door brittle". A stale checkpoint or a
// backlogged broker is worth an alarm and a metric; it is not worth closing a
// gate. Access's /readyz is what its container healthcheck probes, so anything
// wired into readiness takes the scan path down with it.
package lifecyclejob

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/metric"

	"ticketing/services/access/internal/store"
)

// CheckpointStore is the checkpoint worker's view of the store.
type CheckpointStore interface {
	PendingCheckpointOrganizers(ctx context.Context) ([]uuid.UUID, error)
	CheckpointOrganizer(ctx context.Context, organizerID uuid.UUID, observed store.LastRoot) (store.CheckpointResult, error)
	OldestPendingChange(ctx context.Context) (time.Time, bool, error)
}

// Checkpointer periodically commits one delta checkpoint per active organizer.
//
// What it is for, per ADR-021 §D2: this buys NO rollback detection today. It is
// the scaffolding TKT-11 anchors — one root per interval instead of one
// attestation per ticket head. Do not describe it, in metrics, dashboards or
// comments, as rollback protection.
type Checkpointer struct {
	st       CheckpointStore
	interval time.Duration
	log      *slog.Logger
	now      func() time.Time

	// observed is this process's memory of each organizer's chain position. It
	// is deliberately volatile and buys no tripwire — see
	// store.ErrCheckpointRegression. It stops the worker laundering a regression
	// it can see; it cannot see one that happened before this process started.
	observed map[uuid.UUID]store.LastRoot

	// lastSuccess is Unix seconds, atomic because the OTel callback observes it
	// from the collector's goroutine while Run writes it from the worker's. A
	// plain time.Time here was a real data race — it is multiword, so a reader
	// can see a torn value — and the tests missed it only because none of them
	// ran a callback concurrently with Once (PR #51 review, R5).
	lastSuccess atomic.Int64
}

// DefaultInterval is ADR-021 §D3's 60 seconds.
//
// The clause is explicit that this is not today's fraud window — targeted
// rollback is undetected at any interval until an external witness exists. What
// the interval bounds is permanent: a head rolled back before any checkpoint
// committed it was never committed to anything, so no future anchor can ever
// speak to it, and shortening the interval later does not retroactively cover
// it. 60s keeps that hole to about a minute at 1,440 checkpoint opportunities
// per active organizer per day; 5s would buy a marginally smaller hole for 12x
// the write volume on a delta that is already off the gate path.
const DefaultInterval = 60 * time.Second

func NewCheckpointer(st CheckpointStore, interval time.Duration, log *slog.Logger, now func() time.Time) *Checkpointer {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &Checkpointer{st: st, interval: interval, log: log, now: now, observed: map[uuid.UUID]store.LastRoot{}}
}

// Run checkpoints until ctx is cancelled, starting immediately: on restart the
// backlog left by the process that died is the whole point, and waiting a full
// interval to notice it would widen the permanent hole in §D3 for no reason.
func (c *Checkpointer) Run(ctx context.Context) {
	c.Once(ctx)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Once(ctx)
		}
	}
}

// Once runs one pass and returns how many checkpoints it committed. A pass that
// finds nothing pending still counts as a success: the freshness signal has to
// distinguish "idle" from "dead", and only a heartbeat on the no-change path
// does that.
//
// A pass in which any organizer failed does NOT count. "Last success" that
// refreshes on failure reads healthy while checkpointing is broken, which is the
// one thing this metric exists to rule out (PR #51 review, R4).
func (c *Checkpointer) Once(ctx context.Context) int {
	organizers, err := c.st.PendingCheckpointOrganizers(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			c.log.ErrorContext(ctx, "list organizers pending a lifecycle checkpoint", "err", err)
		}
		return 0
	}
	var committed int
	var failed bool
	for _, organizerID := range organizers {
		if ctx.Err() != nil {
			return committed
		}
		result, err := c.st.CheckpointOrganizer(ctx, organizerID, c.observed[organizerID])
		if err != nil {
			failed = true
			if errors.Is(err, store.ErrCheckpointRegression) {
				// Refuse to extend a chain that went backwards where we can see
				// it. This is not detection — the memory is this process's own
				// and an adversary need only wait for a restart (ADR-021 §D2).
				// It is a refusal to re-bless a regression under a fresh
				// signature while we are watching.
				c.log.ErrorContext(ctx, "refusing to extend a regressed lifecycle checkpoint chain",
					"organizer_id", organizerID, "err", err)
				continue
			}
			if !errors.Is(err, context.Canceled) {
				c.log.ErrorContext(ctx, "commit lifecycle checkpoint", "organizer_id", organizerID, "err", err)
			}
			continue
		}
		if result.Sequence == 0 {
			continue // Nothing pending by the time we held the lock.
		}
		c.observed[organizerID] = store.LastRoot{Sequence: result.Sequence, Root: result.Root}
		committed++
	}
	if !failed {
		c.lastSuccess.Store(c.now().Unix())
	}
	return committed
}

// LastSuccess is when the last pass completed, no-change passes included. The
// zero time means no pass has succeeded yet.
func (c *Checkpointer) LastSuccess() time.Time {
	sec := c.lastSuccess.Load()
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// ObserveMetrics registers the freshness gauges. These are the concrete
// staleness signal ADR-021 §Consequences asks for ("checkpoint freshness must be
// monitored"). They are observability, not a gate: nothing here can make Access
// unready, because the checkpoint is scaffolding and a turnstile must not close
// when scaffolding stalls.
func (c *Checkpointer) ObserveMetrics(meter metric.Meter) error {
	lastSuccess, err := meter.Int64ObservableGauge("access.lifecycle.checkpoint.last_success",
		metric.WithDescription("Unix seconds of the last completed checkpoint pass, no-change passes included. Staleness means the worker stopped; it does not mean tampering."))
	if err != nil {
		return err
	}
	pendingAge, err := meter.Int64ObservableGauge("access.lifecycle.checkpoint.pending_oldest_age_seconds",
		metric.WithDescription("Age of the oldest head change still waiting for a checkpoint."))
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		if last := c.LastSuccess(); !last.IsZero() {
			o.ObserveInt64(lastSuccess, last.Unix())
		}
		oldest, ok, err := c.st.OldestPendingChange(ctx)
		if err != nil {
			return err
		}
		if ok {
			o.ObserveInt64(pendingAge, int64(c.now().Sub(oldest).Seconds()))
		} else {
			o.ObserveInt64(pendingAge, 0)
		}
		return nil
	}, lastSuccess, pendingAge)
	return err
}

// AlarmStore is the drainer's view of the store.
type AlarmStore interface {
	ClaimAlarms(ctx context.Context, batch int, lease time.Duration) ([]store.AlarmMessage, error)
	ReleaseAlarm(ctx context.Context, eventID, claimID uuid.UUID, cause error) error
	MarkAlarmPublished(ctx context.Context, eventID, claimID uuid.UUID) (bool, error)
	OldestUnpublishedAlarm(ctx context.Context) (time.Time, bool, error)
	DeadLetteredAlarms(ctx context.Context) (int64, error)
}

// Publisher transmits an already-frozen envelope.
type Publisher interface {
	PublishRaw(ctx context.Context, subject string, eventID uuid.UUID, envelope []byte) error
}

// AlarmDrainer publishes owed integrity alarms.
//
// ADR-021 §D6 makes fail-open defensible only while the alarm reaches someone —
// "unrouted, this clause is a silent bypass with extra steps". The outbox is what
// makes that survive a crash: the alarm is committed with the admission, so an
// admission cannot happen without an owed alarm.
//
// What this does NOT prove is that a human acknowledged anything. It delivers to
// a durable; attaching that durable to a pager is deployment configuration.
type AlarmDrainer struct {
	st        AlarmStore
	publisher Publisher
	interval  time.Duration
	batch     int
	lease     time.Duration
	log       *slog.Logger
}

func NewAlarmDrainer(st AlarmStore, publisher Publisher, interval time.Duration, batch int, log *slog.Logger) *AlarmDrainer {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if batch <= 0 {
		batch = 32
	}
	if log == nil {
		log = slog.Default()
	}
	// One lease covers a whole batch published sequentially, so it is sized from
	// the batch rather than from a single publish; too short and the drainer's
	// own later rows get stolen mid-pass.
	lease := time.Duration(batch)*2*time.Second + 30*time.Second
	return &AlarmDrainer{st: st, publisher: publisher, interval: interval, batch: batch, lease: lease, log: log}
}

// Run drains until ctx is cancelled, immediately first: an owed alarm is
// evidence that someone was admitted through a broken chain, and it is already
// late.
func (d *AlarmDrainer) Run(ctx context.Context) {
	d.Once(ctx)
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.Once(ctx)
		}
	}
}

// Once claims a batch and publishes it, returning the number published.
func (d *AlarmDrainer) Once(ctx context.Context) int {
	msgs, err := d.st.ClaimAlarms(ctx, d.batch, d.lease)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			d.log.ErrorContext(ctx, "claim lifecycle integrity alarms", "err", err)
		}
		return 0
	}
	var published int
	for _, m := range msgs {
		if ctx.Err() != nil {
			return published
		}
		if err := d.publisher.PublishRaw(ctx, m.Subject, m.EventID, m.Envelope); err != nil {
			if relErr := d.st.ReleaseAlarm(ctx, m.EventID, m.ClaimID, err); relErr != nil {
				d.log.ErrorContext(ctx, "release lifecycle integrity alarm", "event_id", m.EventID, "err", relErr)
			}
			if m.Attempts >= store.MaxAlarmAttempts {
				// Dead-lettered: this is the last notice anyone gets that a
				// ticket was admitted through a chain that did not verify.
				d.log.ErrorContext(ctx, "lifecycle integrity alarm dead-lettered; a degraded admission may be unreported",
					"event_id", m.EventID, "attempts", m.Attempts, "err", err)
			} else {
				d.log.WarnContext(ctx, "publish lifecycle integrity alarm", "event_id", m.EventID, "attempts", m.Attempts, "err", err)
			}
			continue
		}
		retired, err := d.st.MarkAlarmPublished(ctx, m.EventID, m.ClaimID)
		if err != nil {
			d.log.ErrorContext(ctx, "mark lifecycle integrity alarm published", "event_id", m.EventID, "err", err)
			continue
		}
		if !retired {
			d.log.WarnContext(ctx, "published a lifecycle integrity alarm after losing its claim; duplicate delivery possible", "event_id", m.EventID)
			continue
		}
		published++
	}
	return published
}

// ObserveMetrics registers the alarm backlog gauge. Monitored, not gated, for
// the same reason as checkpoint freshness: a broker blip must not close a door.
func (d *AlarmDrainer) ObserveMetrics(meter metric.Meter, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	backlog, err := meter.Int64ObservableGauge("access.lifecycle.alarm.oldest_unpublished_age_seconds",
		metric.WithDescription("Age of the oldest unpublished integrity alarm. Fail-open is only defensible while alarms are reaching someone."))
	if err != nil {
		return err
	}
	// Dead letters leave the backlog gauge (they are unclaimable, and their age
	// would grow forever and drown the live signal), so they need their own.
	// Without it the backlog falls to zero exactly when an alarm has permanently
	// failed to reach anyone — the queue looks empty at the moment a degraded
	// admission became unreportable. Alert on this being non-zero, ever.
	deadLettered, err := meter.Int64ObservableGauge("access.lifecycle.alarm.dead_lettered",
		metric.WithDescription("Integrity alarms that exhausted their retries and will never reach the operator durable. Each one is a degraded admission nobody will hear about; non-zero means this deployment is running fail-open unmonitored (ADR-021 §D6)."))
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		oldest, ok, err := d.st.OldestUnpublishedAlarm(ctx)
		if err != nil {
			return err
		}
		if ok {
			o.ObserveInt64(backlog, int64(now().Sub(oldest).Seconds()))
		} else {
			o.ObserveInt64(backlog, 0)
		}
		dead, err := d.st.DeadLetteredAlarms(ctx)
		if err != nil {
			return err
		}
		o.ObserveInt64(deadLettered, dead)
		return nil
	}, backlog, deadLettered)
	return err
}
