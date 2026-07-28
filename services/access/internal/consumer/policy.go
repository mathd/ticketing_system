package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"ticketing/services/access/internal/store"
	"ticketing/shared/domainevent"
)

// SubjectPerformancePublished is catalog's publication subject. Access
// consumes it for one field only — the slot's re_entry policy (TKT-87,
// ADR-005: catalog owns the policy, access enforces it at the gate).
const SubjectPerformancePublished = "platform.catalog.performance.published"

// PolicyStore is what the projector needs from the store.
type PolicyStore interface {
	UpsertSlotPolicy(ctx context.Context, eventID uuid.UUID, sp store.SlotPolicy) error
}

// maxKnownPublicationSchema is the highest performance.published variant this
// consumer can read. The re_entry field rides ADDITIVELY at schemas 2 and 3
// (ADR-017 §2 — no bump; absence means single), so "known" here tracks
// catalog's published range, not a policy-specific number. Above it is the
// future: park + latch unready (§5b′).
//
// Schema 4 is the seated fork (TKT-103). Access reads only re_entry, which a
// seated slot carries exactly like any other slot; the seat-map fields are
// ignored by construction (see the publication struct). So this consumer needs
// nothing but to STOP treating schema 4 as the future — no new arm, no decode
// change. Without this bump, every seated publication would park access and
// latch it unready (ADR-017 §5b′ binds both consumers; TKT-74).
const maxKnownPublicationSchema = 4

// publicationSchemaMin is the bottom of the known range: performance.published
// started at 1 (the capacity-resolver era). At or below zero is a broken
// envelope — poison, not the future.
const publicationSchemaMin = 1

// PolicyConsumer projects performance.published re_entry policies into
// slot_re_entry_policies. Its durable replays the stream from the start
// (DeliverAll), so history — including slots published before this consumer
// existed — backfills on first boot for free.
type PolicyConsumer struct {
	js    jetstream.JetStream
	st    PolicyStore
	log   *slog.Logger
	ready atomic.Bool
}

func NewPolicyConsumer(js jetstream.JetStream, st PolicyStore, log *slog.Logger) *PolicyConsumer {
	return &PolicyConsumer{js: js, st: st, log: log}
}

func (c *PolicyConsumer) Ready() bool { return c.ready.Load() }

// publication is the known-schema shape of the fields this projector reads.
// Everything else in the payload is deliberately ignored — this consumer must
// not grow opinions about capacity fields it does not use.
// publicationData is the slice of performance.published this projector reads —
// re_entry and the identifiers it keys on, nothing else. The seat-map fields of
// schema 4 are ignored by construction, which is why the schema-4 bump needed no
// new arm (TKT-103).
type publicationData struct {
	PerformanceID uuid.UUID `json:"performance_id"`
	OrganizerID   uuid.UUID `json:"organizer_id"`
	ReEntry       *struct {
		Mode         string `json:"mode"`
		MaxEntries   *int32 `json:"max_entries"`
		RequiresExit bool   `json:"requires_exit"`
	} `json:"re_entry"`
}

type publication = domainevent.Envelope[publicationData]

// handle asks the three questions strictly from the outside in — envelope
// readable, variant ours to judge, payload valid (ADR-017 §5b′). Decoding a
// future variant against today's struct would reject it as malformed and
// terminate it, silently disenforcing every pass slot it described.
func (c *PolicyConsumer) handle(ctx context.Context, msg jetstream.Msg) {
	// `schema <= 0` is the shared bottom-end rule (ADR-033); this subject's own
	// minimum stays here, because only catalog's owner knows where the subject
	// started. Today they coincide at 1 — the check is kept separate so that
	// stays a fact about this subject and not an accident.
	env, decodeErr := domainevent.DecodeEnvelope(msg.Data())
	if decodeErr != nil && !errors.Is(decodeErr, domainevent.ErrInvalidSchema) {
		c.log.Error("invalid publication event", "err", decodeErr)
		_ = msg.TermWithReason("invalid_json")
		return
	}
	if decodeErr != nil || env.Type != SubjectPerformancePublished || env.ID == uuid.Nil || env.Schema < publicationSchemaMin {
		// Broken envelope: poison, not the future. Readiness is deliberately
		// untouched — a broken producer must not take access down.
		c.log.Error("invalid publication event", "event_id", env.ID, "schema", env.Schema)
		_ = msg.TermWithReason("invalid_contract")
		return
	}
	if env.Schema > maxKnownPublicationSchema {
		// Version skew, not a failure: a newer binary can read it. Park it and
		// go loudly unready — terminating would drop the policy for a slot
		// that may be selling passes.
		c.log.Error("unsupported publication schema; parking", "event_id", env.ID, "schema", env.Schema)
		c.ready.Store(false)
		_ = msg.NakWithDelay(30 * time.Second)
		return
	}

	var event publication
	if err := json.Unmarshal(msg.Data(), &event); err != nil {
		c.log.Error("invalid publication event", "event_id", env.ID, "err", err)
		_ = msg.TermWithReason("invalid_json")
		return
	}
	sp, err := slotPolicyFrom(event)
	if errors.Is(err, errUnknownMode) {
		// Future vocabulary inside a known schema (a mode this binary does not
		// know) is version skew arriving without a bump, not poison
		// (ai-review K4): terminating it would silently enforce single on a
		// slot that may be selling passes, with no readiness signal. Park it
		// and go loudly unready, exactly like a future schema.
		c.log.Error("unknown re_entry mode; parking", "event_id", env.ID, "err", err)
		c.ready.Store(false)
		_ = msg.NakWithDelay(30 * time.Second)
		return
	}
	if err != nil {
		c.log.Error("invalid publication policy", "event_id", env.ID, "err", err)
		_ = msg.TermWithReason("invalid_contract")
		return
	}
	if err := c.st.UpsertSlotPolicy(ctx, env.ID, sp); err != nil {
		// Transient: the projection must converge, never drop. JetStream
		// redelivers until it lands.
		c.log.Error("project slot policy", "event_id", env.ID, "err", err)
		_ = msg.NakWithDelay(5 * time.Second)
		return
	}
	_ = msg.Ack()
}

// errUnknownMode is future re_entry vocabulary — parked, never terminated.
var errUnknownMode = errors.New("unknown re_entry mode")

// slotPolicyFrom validates the data-level contract. An absent re_entry field
// is a pre-TKT-87 emission and means explicit single (COS 7) — the same
// answer the scan path gives a slot it knows nothing about.
func slotPolicyFrom(event publication) (store.SlotPolicy, error) {
	if event.Data.PerformanceID == uuid.Nil || event.Data.OrganizerID == uuid.Nil {
		return store.SlotPolicy{}, fmt.Errorf("publication lacks slot or organizer identity")
	}
	policy := store.ReEntryPolicy{Mode: "single"}
	if re := event.Data.ReEntry; re != nil {
		policy = store.ReEntryPolicy{Mode: re.Mode, MaxEntries: re.MaxEntries, RequiresExit: re.RequiresExit}
		switch policy.Mode {
		case "single", "multi", "count_limited":
		default:
			return store.SlotPolicy{}, fmt.Errorf("%w: %q", errUnknownMode, policy.Mode)
		}
		if policy.Mode == "count_limited" && (policy.MaxEntries == nil || *policy.MaxEntries <= 0) {
			return store.SlotPolicy{}, fmt.Errorf("count_limited without a positive max_entries")
		}
		if policy.Mode != "count_limited" && policy.MaxEntries != nil {
			return store.SlotPolicy{}, fmt.Errorf("max_entries outside count_limited")
		}
	}
	return store.SlotPolicy{SlotID: event.Data.PerformanceID, OrganizerID: event.Data.OrganizerID, Policy: policy}, nil
}

// Run consumes the publication subject on its own durable. DeliverAll is
// load-bearing: a fresh durable replays every historical publication, which
// is the projection's backfill.
func (c *PolicyConsumer) Run(ctx context.Context) error {
	stream, err := c.js.Stream(ctx, "PLATFORM")
	if err != nil {
		return err
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable: "access-slot-policy", FilterSubject: SubjectPerformancePublished,
		DeliverPolicy: jetstream.DeliverAllPolicy, AckPolicy: jetstream.AckExplicitPolicy, MaxDeliver: -1,
	})
	if err != nil {
		return err
	}
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		c.handle(ctx, msg)
	})
	if err != nil {
		return err
	}
	defer cc.Stop()
	defer c.ready.Store(false)
	// Readiness waits for the initial backlog to drain (ai-review G2): on a
	// first boot the DeliverAll replay IS the projection's backfill, and
	// declaring ready mid-replay would let a pass ticket scan as single and
	// take an irreversible `redeemed`. A parked message (future schema or
	// vocabulary) keeps NumPending non-zero, so this also refuses readiness
	// until skew is resolved — the same signal the latch carries later.
	for {
		info, infoErr := cons.Info(ctx)
		if infoErr != nil {
			return infoErr
		}
		if info.NumPending == 0 && info.NumAckPending == 0 {
			c.ready.Store(true)
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("policy consumer stopped: %w", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	if err := waitConsume(ctx, cc.Closed(), &c.ready, "access-slot-policy"); err != nil {
		return err
	}
	return fmt.Errorf("policy consumer stopped: %w", ctx.Err())
}
