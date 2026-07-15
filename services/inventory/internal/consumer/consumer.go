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
		// Schema numbers start at 1 and only climb, so <= 0 is not a variant from the future —
		// it is an envelope that omitted `schema` (ADR-009 §5 requires it) or a producer bug.
		// No binary will ever provision it, which makes it poison, not skew: terminate it rather
		// than park it forever waiting for a version that cannot exist.
		if e.Schema <= 0 {
			return provisionInput{}, fmt.Errorf("publication envelope has no usable schema (%d)", e.Schema)
		}
		return provisionInput{}, fmt.Errorf("%w %d", errUnsupportedPublicationSchema, e.Schema)
	}
}

// errUnsupportedPublicationSchema marks a variant this binary cannot judge — as opposed to a
// payload it can judge and has found bad. The disposition turns on that distinction, so the
// default arm wraps this rather than returning a bare message.
var errUnsupportedPublicationSchema = errors.New("unsupported publication schema")

type disposition string

const (
	dispositionTerminate disposition = "terminate"
	dispositionRetry     disposition = "retry"
	dispositionPark      disposition = "park"
)

// dispositionForProvisionError decides a failed publication's fate. It is a function, not an inline
// branch, because the disposition is the part worth testing: an unknown variant reaching Term() is
// the TKT-61 bug, and Term() is unobservable without a live JetStream message.
//
// Poison — malformed, or invalid at a schema we know — terminates: no binary can provision it.
// An unknown variant is not poison. It is well-formed and a newer binary can provision it, so it
// parks and waits for one; terminating would discard recoverable inventory on the authority of the
// one component that provably cannot read it (ADR-017 §5b).
func dispositionForProvisionError(schema int, err error) disposition {
	switch {
	case errors.Is(err, errUnsupportedPublicationSchema):
		return dispositionPark
	case schema == 1:
		// Schema 1 resolves against catalog; its failures are transient.
		return dispositionRetry
	default:
		return dispositionTerminate
	}
}

// handle is the message handler, split out of Run's Consume closure so the disposition it actually
// ships can be tested against a fake jetstream.Msg (the shape services/access already uses). The
// pure dispositionForProvisionError decides; this is the wiring that obeys it, and the wiring is
// what dropped inventory in TKT-61 — so it does not get to go untested.
func (c *Consumer) handle(ctx context.Context, msg jetstream.Msg) {
	var e publication
	if err := json.Unmarshal(msg.Data(), &e); err != nil {
		c.log.Error("invalid publication event", "err", err)
		_ = msg.Term()
		return
	}
	input, err := c.provisionInput(ctx, e)
	if err != nil {
		switch dispositionForProvisionError(e.Schema, err) {
		case dispositionPark:
			// Version skew: hold the event for a binary that understands it, and stop
			// reporting ready. Readiness is the only signal wired here, and a drop nobody
			// notices is the whole of TKT-61 — being loudly unready beats being quietly
			// wrong. It never self-heals: recovering on the next good message would hide
			// the event still pending behind this one.
			c.log.Error("unsupported publication schema", "event_id", e.ID, "schema", e.Schema, "err", err)
			c.ready.Store(false)
			_ = msg.NakWithDelay(5 * time.Second)
		case dispositionRetry:
			c.log.Error("resolve legacy publication", "event_id", e.ID, "err", err)
			_ = msg.NakWithDelay(5 * time.Second)
		default:
			c.log.Error("invalid publication event", "event_id", e.ID, "err", err)
			_ = msg.Term()
		}
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
