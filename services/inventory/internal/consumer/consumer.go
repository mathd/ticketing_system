package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"ticketing/services/inventory/internal/store"
)

const subject = "platform.catalog.performance.published"

type Consumer struct {
	js       jetstream.JetStream
	st       *store.Postgres
	resolver PerformanceResolver
	ready    atomic.Bool
	log      *slog.Logger
}

func New(js jetstream.JetStream, st *store.Postgres, resolver PerformanceResolver, log *slog.Logger) *Consumer {
	return &Consumer{js: js, st: st, resolver: resolver, log: log}
}
func (c *Consumer) Ready() bool { return c.ready.Load() }

type publication struct {
	ID     uuid.UUID `json:"id"`
	Schema int       `json:"schema"`
	Data   struct {
		PerformanceID   uuid.UUID  `json:"performance_id"`
		OrganizerID     uuid.UUID  `json:"organizer_id"`
		Capacity        int32      `json:"capacity"`
		CapacityGroupID *uuid.UUID `json:"capacity_group_id,omitempty"`
		SharedCapacity  *int32     `json:"shared_capacity,omitempty"`
	} `json:"data"`
}

type provisionInput struct {
	organizerID uuid.UUID
	poolID      uuid.UUID
	capacity    int32
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
// tripwire if you add an arm and forget, and TestMaxKnownSchemaIsNotAheadOfTheArms if you bump it
// without adding one.
const maxKnownPublicationSchema = 3

// envelope is the part of an event that does not change across schema versions (ADR-009 §5).
// `data` stays raw on purpose: dispatch has to happen on `schema` alone, before anything reads
// `data`, because a variant this binary does not know may reshape `data` arbitrarily — that is
// what a bump *means* (ADR-017 §3). Decoding a future variant against today's struct would reject
// it as malformed and terminate it, dropping precisely what TKT-61 exists to preserve.
type envelope struct {
	ID     uuid.UUID       `json:"id"`
	Schema int             `json:"schema"`
	Data   json.RawMessage `json:"data"`
}

// knownPublicationSchema reports whether this binary can read `data` at this schema — i.e. whether
// the payload is ours to judge at all. Schema numbers start at 1 and only climb (ADR-009 §5), so
// <= 0 is not a variant from the future; it is a broken envelope.
func knownPublicationSchema(schema int) bool {
	return schema >= 1 && schema <= maxKnownPublicationSchema
}

// handle is the message handler, split out of Run's Consume closure so the disposition it actually
// ships can be tested against a fake jetstream.Msg (the shape services/access already uses). The
// disposition is what dropped inventory in TKT-61, so it does not get to go untested.
//
// The order of the three questions below is load-bearing, and they are asked from the outside in:
// is the envelope readable, is the variant ours to judge, and only then is the payload valid. Ask
// them in any other order and a future variant gets judged by rules that were never written for it.
func (c *Consumer) handle(ctx context.Context, msg jetstream.Msg) {
	// Read the envelope only. Dispatch on `schema` before `data` is decoded — see envelope's
	// doc comment for why the ordering is the whole fix and not a style choice.
	var env envelope
	if err := json.Unmarshal(msg.Data(), &env); err != nil {
		c.log.Error("invalid publication event", "err", err)
		_ = msg.Term()
		return
	}
	if !knownPublicationSchema(env.Schema) {
		if env.Schema <= 0 {
			// Not a variant from the future: an envelope with no usable schema, which ADR-009 §5
			// requires. No binary will ever provision it — poison, so terminate. Readiness is
			// deliberately untouched: a broken producer must not be able to take inventory down.
			c.log.Error("invalid publication event", "event_id", env.ID, "schema", env.Schema,
				"err", "publication envelope has no usable schema")
			_ = msg.Term()
			return
		}
		// Version skew: hold the event for a binary that understands it, and stop reporting
		// ready. Readiness is the only signal wired here, and a drop nobody notices is the whole
		// of TKT-61 — being loudly unready beats being quietly wrong. It never self-heals:
		// recovering on the next good message would hide the event still pending behind this one.
		c.log.Error("unsupported publication schema", "event_id", env.ID, "schema", env.Schema)
		c.ready.Store(false)
		_ = msg.NakWithDelay(5 * time.Second)
		return
	}
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
	// Grouped festival days deliberately converge on the festival id. Slot to
	// group resolution for claims is owned by TKT-14 and remains out of scope.
	if err := c.st.Provision(ctx, e.ID, input.poolID, input.organizerID, input.capacity); err != nil {
		c.log.Error("provision inventory", "err", err)
		_ = msg.Nak()
		return
	}
	_ = msg.Ack()
}

func (c *Consumer) Run(ctx context.Context) error {
	stream, err := c.js.Stream(ctx, "PLATFORM")
	if err != nil {
		return err
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{Durable: "inventory-performance-provisioner", FilterSubject: subject, DeliverPolicy: jetstream.DeliverAllPolicy, AckPolicy: jetstream.AckExplicitPolicy, MaxDeliver: -1})
	if err != nil {
		return err
	}
	c.ready.Store(true)
	defer c.ready.Store(false)
	cc, err := cons.Consume(func(msg jetstream.Msg) { c.handle(ctx, msg) })
	if err != nil {
		return err
	}
	defer cc.Stop()
	<-ctx.Done()
	return fmt.Errorf("consumer stopped: %w", ctx.Err())
}
