package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"ticketing/services/inventory/internal/store"
	"ticketing/shared/domainevent"
)

const (
	subjectPublished = "platform.catalog.performance.published"
	subjectArchived  = "platform.catalog.performance.archived"
	subjectClosed    = "platform.catalog.performance.closed"
	subjectReopened  = "platform.catalog.performance.reopened"
)

// catalogStore is what the consumer needs from the store; *store.Postgres implements it,
// and the disposition tests substitute a recorder.
type catalogStore interface {
	Provision(ctx context.Context, eventID, slotID, organizerID uuid.UUID, capacity int32) error
	ProvisionSeated(ctx context.Context, eventID, slotID, organizerID, seatMapID uuid.UUID, capacity int32) error
	ApplyArchive(ctx context.Context, eventID, pool uuid.UUID) error
	ApplyClosure(ctx context.Context, eventID, pool, performance uuid.UUID, closed bool, version int32) error
	QuarantineCatalogEvent(ctx context.Context, subject string, eventID uuid.UUID, schema int, envelope []byte) error
	HasPendingCatalogQuarantine(ctx context.Context) (bool, error)
	ListPublishedPoolOfferings(ctx context.Context) ([]store.PoolOffering, error)
}

type Consumer struct {
	js       jetstream.JetStream
	st       catalogStore
	resolver PerformanceResolver
	ready    atomic.Bool
	// readinessMu serializes the two writers that can decide readiness once
	// consumption is running (TKT-90 reorder): handle's skew latch and
	// refreshStartupReadiness. Without it, the startup check can read the
	// quarantine table before a concurrent skew insert commits and then
	// Store(true) AFTER that event latched false — silently ready with a
	// pending quarantine, which is the exact state the latch exists to shout
	// about. Serialized, either order converges on unready — but only because
	// the quarantine insert COMMITS before the latch is taken; latch first and
	// the commit escapes the critical section, reopening the race.
	readinessMu sync.Mutex
	log         *slog.Logger
	// retryBackoff paces startupConverge's reconciliation retries; the zero
	// value (used by tests) retries immediately.
	retryBackoff time.Duration
}

func New(js jetstream.JetStream, st catalogStore, resolver PerformanceResolver, log *slog.Logger) *Consumer {
	return &Consumer{js: js, st: st, resolver: resolver, log: log, retryBackoff: 5 * time.Second}
}
func (c *Consumer) Ready() bool { return c.ready.Load() }

// publicationData is the per-subject payload — inventory's own, and it stays
// here (ADR-033 puts only the envelope in the kernel). publication is the
// envelope carrying it, decoded once the schema arm is known.
type publicationData struct {
	PerformanceID   uuid.UUID  `json:"performance_id"`
	OrganizerID     uuid.UUID  `json:"organizer_id"`
	Capacity        int32      `json:"capacity"`
	CapacityGroupID *uuid.UUID `json:"capacity_group_id,omitempty"`
	SharedCapacity  *int32     `json:"shared_capacity,omitempty"`
	SeatMapID       *uuid.UUID `json:"seat_map_id,omitempty"`
}

type publication = domainevent.Decoded[publicationData]

type provisionInput struct {
	organizerID uuid.UUID
	poolID      uuid.UUID
	capacity    int32
	// seatMapID is set (non-nil) only for a seated pool (schema 4); it routes the
	// apply to ProvisionSeated. Zero for a GA/festival pool.
	seatMapID uuid.UUID
}

func (c *Consumer) provisionInput(ctx context.Context, e publication) (provisionInput, error) {
	if e.ID == uuid.Nil || e.Data.PerformanceID == uuid.Nil || e.Data.OrganizerID == uuid.Nil {
		return provisionInput{}, fmt.Errorf("publication is missing required identifiers")
	}
	switch e.Schema {
	case 2:
		if e.Data.Capacity <= 0 {
			return provisionInput{}, fmt.Errorf("schema-2 publication has invalid capacity")
		}
		if e.Data.CapacityGroupID != nil || e.Data.SharedCapacity != nil {
			return provisionInput{}, fmt.Errorf("schema-2 publication must not carry festival capacity")
		}
		return provisionInput{organizerID: e.Data.OrganizerID, poolID: e.Data.PerformanceID, capacity: e.Data.Capacity}, nil
	case 4:
		// Seated fork (TKT-103): a slot claimed seat-by-seat (TKT-80). This provisions
		// a SEATED pool — distinct from a GA quantity pool — carrying the catalog seat
		// map so seat holds can pin, and a coarse capacity ceiling. The tight per-seat
		// oversell boundary is the claim_seats unique index + PinSeat existence
		// validation, not this capacity (ADR-031).
		if e.Data.PerformanceID == uuid.Nil || e.Data.OrganizerID == uuid.Nil {
			return provisionInput{}, fmt.Errorf("schema-4 seated publication is missing required identifiers")
		}
		if e.Data.SeatMapID == nil || *e.Data.SeatMapID == uuid.Nil {
			return provisionInput{}, fmt.Errorf("schema-4 seated publication has no seat map reference")
		}
		if e.Data.CapacityGroupID != nil || e.Data.SharedCapacity != nil {
			return provisionInput{}, fmt.Errorf("schema-4 seated publication must not carry festival capacity")
		}
		// `capacity` is the venue GA snapshot, used only as a coarse ceiling — but it
		// MUST be vetted here (the schema-2 arm's validation does not cover this arm):
		// a non-positive value would provision a stillborn pool where every hold fails.
		if e.Data.Capacity <= 0 {
			return provisionInput{}, fmt.Errorf("schema-4 seated publication has invalid capacity")
		}
		return provisionInput{organizerID: e.Data.OrganizerID, poolID: e.Data.PerformanceID, capacity: e.Data.Capacity, seatMapID: *e.Data.SeatMapID}, nil
	case 3:
		// Deploy this consumer before catalog starts emitting Schema 3 so grouped
		// festival publications remain safe during a rolling rollout.
		if e.Data.CapacityGroupID == nil || *e.Data.CapacityGroupID == uuid.Nil || e.Data.SharedCapacity == nil || *e.Data.SharedCapacity <= 0 {
			return provisionInput{}, fmt.Errorf("schema-3 publication has invalid festival capacity")
		}
		return provisionInput{organizerID: e.Data.OrganizerID, poolID: *e.Data.CapacityGroupID, capacity: *e.Data.SharedCapacity}, nil
	case 1:
		if c.resolver == nil {
			return provisionInput{}, fmt.Errorf("schema-1 publication needs catalog resolver")
		}
		resolved, err := c.resolver.PublishedPerformance(ctx, e.Data.PerformanceID)
		if err != nil {
			return provisionInput{}, err
		}
		if resolved.OrganizerID != e.Data.OrganizerID {
			return provisionInput{}, fmt.Errorf("schema-1 publication conflicts with catalog")
		}
		if resolved.CapacityGroupID == nil {
			if resolved.SharedCapacity != nil || resolved.Capacity <= 0 {
				return provisionInput{}, fmt.Errorf("schema-1 publication conflicts with catalog")
			}
			return provisionInput{organizerID: resolved.OrganizerID, poolID: e.Data.PerformanceID, capacity: resolved.Capacity}, nil
		}
		if *resolved.CapacityGroupID == uuid.Nil || resolved.SharedCapacity == nil || *resolved.SharedCapacity <= 0 {
			return provisionInput{}, fmt.Errorf("schema-1 publication conflicts with catalog")
		}
		return provisionInput{organizerID: resolved.OrganizerID, poolID: *resolved.CapacityGroupID, capacity: *resolved.SharedCapacity}, nil
	default:
		return provisionInput{}, fmt.Errorf("unsupported publication schema %d", e.Schema)
	}
}

// maxKnownPublicationSchema is the highest `performance.published` variant this binary can read.
// Keep it in step with provisionInput's case arms above — TestEveryKnownSchemaHasAnArm is the
// tripwire if you add an arm and forget, and TestMaxKnownSchemaIsNotBehindTheArms if you bump it
// without adding one. Schema 4 is the seated fork (TKT-103): a KNOWN variant that provisions a
// SEATED pool for seat-level claims (TKT-80) — see provisionInput's case 4.
const maxKnownPublicationSchema = 4

// knownSchemas is the per-subject registry of variants this binary can read (ADR-017 §5b′).
// Above max is the future (park + latch unready); at or below zero is a broken envelope; a gap
// below min was never emitted by any catalog (archived started at 2) — both are poison.
var knownSchemas = map[string]struct{ min, max int }{
	subjectPublished: {1, maxKnownPublicationSchema},
	subjectArchived:  {2, 3},
	subjectClosed:    {1, 1},
	subjectReopened:  {1, 1},
}

// envelope is the shared platform decode view (ADR-033). `data` stays raw on purpose: dispatch has
// to happen on `schema` alone, before anything reads `data`, because a variant this binary does not
// know may reshape `data` arbitrarily — that is what a bump *means* (ADR-017 §3). Decoding a future
// variant against today's struct would reject it as malformed and terminate it, dropping precisely
// what TKT-61 exists to preserve. The shared type makes that structural: there is nothing typed to
// decode into until this consumer has chosen an arm.
type envelope = domainevent.Raw

// handle is the message handler, split out of Run's Consume closure so the disposition it actually
// ships can be tested against a fake jetstream.Msg (the shape services/access already uses). The
// disposition is what dropped inventory in TKT-61, so it does not get to go untested.
//
// The order of the three questions below is load-bearing, and they are asked from the outside in:
// is the envelope readable, is the variant ours to judge, and only then is the payload valid. Ask
// them in any other order and a future variant gets judged by rules that were never written for it.
func (c *Consumer) handle(ctx context.Context, msg jetstream.Msg) {
	// Read the envelope only. Dispatch on (subject, `schema`) before `data` is decoded — see
	// envelope's doc comment for why the ordering is the whole fix and not a style choice.
	// The bottom-end rule (`schema <= 0`) is the shared one; everything below it here — the
	// subject registry, this subject's minimum, the disposition — is inventory's own (ADR-033).
	// decodeErr is held rather than acted on immediately so the question order is unchanged:
	// an unknown subject is still answered before a broken schema.
	env, decodeErr := domainevent.DecodeEnvelope(msg.Data())
	if decodeErr != nil && !errors.Is(decodeErr, domainevent.ErrInvalidSchema) {
		c.log.Error("invalid catalog event", "subject", msg.Subject(), "err", decodeErr)
		_ = msg.Term()
		return
	}
	spec, ok := knownSchemas[msg.Subject()]
	if !ok {
		// The durable filter should make this unreachable; if it happens it is a wiring bug,
		// not the future — parking it would stall the stream behind a subject nobody owns.
		c.log.Error("unexpected subject", "subject", msg.Subject(), "event_id", env.ID)
		_ = msg.Term()
		return
	}
	if decodeErr != nil {
		// Not a variant from the future: an envelope with no usable schema, which ADR-009 §5
		// requires. No binary will ever apply it — poison, so terminate. Readiness is
		// deliberately untouched: a broken producer must not be able to take inventory down.
		c.log.Error("invalid catalog event", "subject", msg.Subject(), "event_id", env.ID,
			"schema", env.Schema, "err", "envelope has no usable schema")
		_ = msg.Term()
		return
	}
	if env.ID == uuid.Nil {
		// `id` is stable across every schema variant (ADR-009 §5), so its absence is a broken
		// envelope even when the schema claims to be from the future — parking it would NAK
		// forever and latch readiness for an event no binary will ever apply (ai-review finding 3).
		c.log.Error("invalid catalog event", "subject", msg.Subject(), "schema", env.Schema,
			"err", "envelope has no usable id")
		_ = msg.Term()
		return
	}
	if env.Schema > spec.max {
		// Version skew: persist the raw envelope to the bounded quarantine and ack the
		// original — a parked event occupies the durable's ack window, and ~1000 of them
		// stall every variant behind them, including ones this binary understands (TKT-68).
		// Readiness still latches false and never self-heals: recovery is a supporting
		// binary + reprocess-quarantine + restart, because a drop nobody notices is the
		// whole of TKT-61 — being loudly unready beats being quietly wrong.
		c.log.Error("unsupported catalog event schema; quarantining", "subject", msg.Subject(), "event_id", env.ID, "schema", env.Schema)
		raw := append([]byte(nil), msg.Data()...)
		err := c.st.QuarantineCatalogEvent(ctx, msg.Subject(), env.ID, env.Schema, raw)
		if errors.Is(err, store.ErrCatalogQuarantineCollision) {
			// Two payloads under one id is a producer invariant break (ADR-009 §5), not
			// skew: the row will never be overwritten, so a NAK would re-park it forever.
			// Poison — terminate, readiness untouched; the first copy stays quarantined.
			c.log.Error("catalog event id collision; terminating", "subject", msg.Subject(), "event_id", env.ID, "schema", env.Schema)
			_ = msg.Term()
			return
		}
		c.readinessMu.Lock()
		c.ready.Store(false)
		c.readinessMu.Unlock()
		if err != nil {
			// Includes ErrCatalogQuarantineFull: without a committed copy the event must
			// stay outstanding — at the quarantine cap the stall is deliberate, explicit,
			// inventory-owned backpressure, never a drop.
			c.log.Error("quarantine catalog event", "subject", msg.Subject(), "event_id", env.ID, "err", err)
			_ = msg.NakWithDelay(5 * time.Second)
			return
		}
		_ = msg.Ack()
		return
	}
	if env.Schema < spec.min {
		// Below the subject's first variant: no catalog ever emitted it, so no binary — present
		// or future — will apply it. Poison, same as the broken envelope above.
		c.log.Error("invalid catalog event", "subject", msg.Subject(), "event_id", env.ID,
			"schema", env.Schema, "err", "schema below the subject's first variant")
		_ = msg.Term()
		return
	}
	switch msg.Subject() {
	case subjectArchived:
		c.handleArchived(ctx, msg, env)
	case subjectClosed, subjectReopened:
		c.handleClosure(ctx, msg, env, msg.Subject() == subjectClosed)
	default:
		c.handlePublication(ctx, msg, env)
	}
}

func (c *Consumer) handlePublication(ctx context.Context, msg jetstream.Msg, env envelope) {
	// Known variant: now it is ours to judge, so read and validate `data` as this schema defines it.
	var e publication
	if err := json.Unmarshal(msg.Data(), &e); err != nil {
		c.log.Error("invalid publication event", "event_id", env.ID, "err", err)
		_ = msg.Term()
		return
	}
	input, err := c.provisionInput(ctx, e)
	if err != nil {
		if e.Schema == 1 {
			// Schema 1 resolves against catalog; its failures are transient.
			c.log.Error("resolve legacy publication", "event_id", e.ID, "err", err)
			_ = msg.NakWithDelay(5 * time.Second)
			return
		}
		// Bad at a schema we know: poison. No binary can provision it, so it terminates —
		// this is what stops parking from becoming an infinite retry loop for corrupt data.
		c.log.Error("invalid publication event", "event_id", e.ID, "err", err)
		_ = msg.Term()
		return
	}
	// A seated publication (schema 4) provisions a seated pool carrying its seat map;
	// everything else is a GA/festival quantity pool. Grouped festival days deliberately
	// converge on the festival id (slot→group resolution is TKT-14, out of scope).
	if input.seatMapID != uuid.Nil {
		if err := c.st.ProvisionSeated(ctx, e.ID, input.poolID, input.organizerID, input.seatMapID, input.capacity); err != nil {
			c.log.Error("provision seated inventory", "err", err)
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
		return
	}
	if err := c.st.Provision(ctx, e.ID, input.poolID, input.organizerID, input.capacity); err != nil {
		c.log.Error("provision inventory", "err", err)
		_ = msg.Nak()
		return
	}
	_ = msg.Ack()
}

type archivedData struct {
	PerformanceID   uuid.UUID  `json:"performance_id"`
	OrganizerID     uuid.UUID  `json:"organizer_id"`
	CapacityGroupID *uuid.UUID `json:"capacity_group_id,omitempty"`
}

// handleArchived marks the pool terminally archived (TKT-75). Schema 2 is a solo slot,
// schema 3 a grouped one whose pool is the festival's capacity group — the same pool
// convergence the publication path uses.
func (c *Consumer) handleArchived(ctx context.Context, msg jetstream.Msg, env envelope) {
	var d archivedData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		c.log.Error("invalid archived event", "event_id", env.ID, "err", err)
		_ = msg.Term()
		return
	}
	pool := d.PerformanceID
	switch {
	case env.ID == uuid.Nil || d.PerformanceID == uuid.Nil || d.OrganizerID == uuid.Nil:
		c.log.Error("invalid archived event", "event_id", env.ID, "err", "missing required identifiers")
		_ = msg.Term()
		return
	case env.Schema == 2 && d.CapacityGroupID != nil:
		c.log.Error("invalid archived event", "event_id", env.ID, "err", "schema-2 archive must not carry a capacity group")
		_ = msg.Term()
		return
	case env.Schema == 3:
		if d.CapacityGroupID == nil || *d.CapacityGroupID == uuid.Nil {
			c.log.Error("invalid archived event", "event_id", env.ID, "err", "schema-3 archive has no capacity group")
			_ = msg.Term()
			return
		}
		pool = *d.CapacityGroupID
	}
	c.applyOffering(msg, env.ID, func() error { return c.st.ApplyArchive(ctx, env.ID, pool) })
}

type closureData struct {
	PerformanceID uuid.UUID `json:"performance_id"`
	OrganizerID   uuid.UUID `json:"organizer_id"`
	Version       int32     `json:"closure_version"`
}

// handleClosure applies a closed/reopened toggle at the catalog's monotonic closure version
// (TKT-75). The payload carries no capacity group, so the pool is resolved against catalog —
// a grouped day converges on the shared festival pool, closing it whole (owner decision at
// Gate 2; day-level offer state is TKT-14's).
func (c *Consumer) handleClosure(ctx context.Context, msg jetstream.Msg, env envelope, closed bool) {
	var d closureData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		c.log.Error("invalid closure event", "event_id", env.ID, "err", err)
		_ = msg.Term()
		return
	}
	if env.ID == uuid.Nil || d.PerformanceID == uuid.Nil || d.OrganizerID == uuid.Nil || d.Version < 1 {
		c.log.Error("invalid closure event", "event_id", env.ID, "err", "missing identifiers or closure version")
		_ = msg.Term()
		return
	}
	if c.resolver == nil {
		c.log.Error("closure event needs catalog resolver", "event_id", env.ID)
		_ = msg.NakWithDelay(5 * time.Second)
		return
	}
	resolved, err := c.resolver.PublishedPerformance(ctx, d.PerformanceID)
	if errors.Is(err, ErrPerformanceNotFound) {
		// Not transient and not skew: closure transitions only exist while published (spike
		// TKT-50 §Case 3), so a 404 means the slot has been archived since. The archived event
		// later in the stream owns the pool's terminal state — this toggle is moot. Parking it
		// instead would be poison: the slot never comes back.
		c.log.Info("closure event for a no-longer-published slot; skipping as moot", "event_id", env.ID, "performance_id", d.PerformanceID)
		_ = msg.Ack()
		return
	}
	if err != nil {
		c.log.Error("resolve closure event", "event_id", env.ID, "err", err)
		_ = msg.NakWithDelay(5 * time.Second)
		return
	}
	if resolved.OrganizerID != d.OrganizerID {
		c.log.Error("invalid closure event", "event_id", env.ID, "err", "closure event conflicts with catalog")
		_ = msg.Term()
		return
	}
	pool := d.PerformanceID
	if resolved.CapacityGroupID != nil && *resolved.CapacityGroupID != uuid.Nil {
		pool = *resolved.CapacityGroupID
	}
	c.applyOffering(msg, env.ID, func() error { return c.st.ApplyClosure(ctx, env.ID, pool, d.PerformanceID, closed, d.Version) })
}

// applyOffering shares the store-outcome disposition for archive/closure mutations:
// a missing pool parks the event until publication provisions it (the publication is
// earlier in the stream; only a NAK-induced reorder gets us here), other store errors
// retry, success acks. Readiness is never touched — none of these are version skew.
func (c *Consumer) applyOffering(msg jetstream.Msg, eventID uuid.UUID, apply func() error) {
	switch err := apply(); {
	case errors.Is(err, store.ErrNotFound):
		c.log.Warn("offering event precedes its pool; parking for redelivery", "event_id", eventID)
		_ = msg.NakWithDelay(5 * time.Second)
	case err != nil:
		c.log.Error("apply offering event", "event_id", eventID, "err", err)
		_ = msg.Nak()
	default:
		_ = msg.Ack()
	}
}

// maxAckPending bounds the durable's outstanding deliveries. With future variants quarantined
// and acked (TKT-68), outstanding messages are just processing concurrency plus the rare
// quarantine-full NAKs — 64 is generous for both while keeping the stall bound explicit.
const maxAckPending = 64

// SupportsCatalogSchema reports whether this binary can read the given (subject, schema)
// variant. It is the reprocess-quarantine gate and derives from the same knownSchemas registry
// as live dispatch — two sources would let a binary re-inject variants it then cannot apply.
func SupportsCatalogSchema(subject string, schema int) bool {
	spec, ok := knownSchemas[subject]
	return ok && schema >= spec.min && schema <= spec.max
}

// consumerConfig is the durable's full configuration, extracted so a test can pin every field —
// most importantly the explicit MaxAckPending (TKT-68): the consumer's backpressure bound must
// be visible in code, not inherited from the nats-server default.
func (c *Consumer) consumerConfig() jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Durable:        "inventory-catalog-offering",
		FilterSubjects: []string{subjectPublished, subjectArchived, subjectClosed, subjectReopened},
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		AckPolicy:      jetstream.AckExplicitPolicy,
		MaxDeliver:     -1,
		MaxAckPending:  maxAckPending,
	}
}

// refreshStartupReadiness decides initial readiness from quarantine state: quarantined originals
// were acked, so a restart can no longer rediscover unresolved skew from JetStream — Postgres is
// the source of truth. Pending rows keep readiness latched false (known variants still flow);
// recovery is reprocess-quarantine + restart, never silent.
func (c *Consumer) refreshStartupReadiness(ctx context.Context) error {
	// The check-then-latch must be atomic against handle's skew latch (see
	// readinessMu): a skew event quarantined between the read and the Store
	// would otherwise be overwritten into silent readiness.
	c.readinessMu.Lock()
	defer c.readinessMu.Unlock()
	pending, err := c.st.HasPendingCatalogQuarantine(ctx)
	if err != nil {
		return fmt.Errorf("check pending catalog quarantine: %w", err)
	}
	if pending {
		c.log.Error("unresolved quarantined catalog events; staying unready until reprocess-quarantine + restart")
		return nil
	}
	c.ready.Store(true)
	return nil
}

func (c *Consumer) Run(ctx context.Context) error {
	stream, err := c.js.Stream(ctx, "PLATFORM")
	if err != nil {
		return err
	}
	// TKT-75 replaced the publish-only durable with the multi-subject one below; a new
	// durable replays retained archive/closure events the old filter skipped (safe through
	// consumed_events). Delete the orphan so it does not linger in stream reports.
	_ = stream.DeleteConsumer(ctx, "inventory-performance-provisioner")
	cons, err := stream.CreateOrUpdateConsumer(ctx, c.consumerConfig())
	if err != nil {
		return err
	}
	defer c.ready.Store(false)
	cc, err := cons.Consume(func(msg jetstream.Msg) { c.handle(ctx, msg) })
	if err != nil {
		return err
	}
	defer cc.Stop()
	// TKT-90: reconcile pool offering state against catalog before readiness —
	// dead-beyond-retention pools converge here; the quarantine latch still wins.
	// Consumption is already running: the durable's backlog drains in parallel
	// with the pass (ai-review finding 4 — the pass gates readiness, never
	// delivery). Interleaved writes are safe per pair: closure vs closure by the
	// per-slot version guard, and anything vs archive because archival is
	// terminal and offeringStatus collapses archived over closed — a post-archive
	// closure row changes nothing the read path can see.
	if err := c.startupConverge(ctx); err != nil {
		return err
	}
	// TKT-127: observe async termination, not just cancellation. Blocking on
	// <-ctx.Done() alone left a deleted durable invisible — the process stayed up
	// and READY with nothing consuming (ADR-017 §236-241 forbids exactly that
	// silent stall). waitConsume returns nil on clean shutdown, so the tail below
	// is unchanged for an ordinary stop; on termination it latches unready and
	// returns, and main exits. Arbitrating that error against a cancellation race
	// in main is TKT-121, still open and untouched here.
	//
	// The observation starts HERE, not at Consume above, so a durable deleted
	// while startupConverge is still running is not noticed until the pass ends —
	// and that pass retries up to reconcileAttempts times with retryBackoff
	// between them. Readiness is false throughout it (refreshStartupReadiness
	// stores true as its last act, immediately below), so the service is honestly
	// unready rather than falsely ready; the cost is a late process exit, not a
	// wrong answer. Closing that window means racing termination against the pass,
	// which entangles the pass's error with shutdown cancellation and so with
	// TKT-121 — deliberately deferred to TKT-135 rather than done here
	// (TKT-127 ai-review, triaged incidental).
	if err := waitConsume(ctx, cc.Closed(), &c.ready, "inventory-catalog-offering"); err != nil {
		return err
	}
	return fmt.Errorf("consumer stopped: %w", ctx.Err())
}
