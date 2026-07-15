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
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		var e publication
		if err := json.Unmarshal(msg.Data(), &e); err != nil {
			c.log.Error("invalid publication event", "err", err)
			_ = msg.Term()
			return
		}
		input, err := c.provisionInput(ctx, e)
		if err != nil {
			if e.Schema == 1 {
				c.log.Error("resolve legacy publication", "event_id", e.ID, "err", err)
				_ = msg.NakWithDelay(5 * time.Second)
				return
			}
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
	})
	if err != nil {
		return err
	}
	defer cc.Stop()
	<-ctx.Done()
	return fmt.Errorf("consumer stopped: %w", ctx.Err())
}
