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
		PerformanceID uuid.UUID `json:"performance_id"`
		OrganizerID   uuid.UUID `json:"organizer_id"`
		Capacity      int32     `json:"capacity"`
	} `json:"data"`
}

func (c *Consumer) provisionInput(ctx context.Context, e publication) (uuid.UUID, int32, error) {
	if e.ID == uuid.Nil || e.Data.PerformanceID == uuid.Nil || e.Data.OrganizerID == uuid.Nil {
		return uuid.Nil, 0, fmt.Errorf("publication is missing required identifiers")
	}
	switch e.Schema {
	case 2:
		if e.Data.Capacity <= 0 {
			return uuid.Nil, 0, fmt.Errorf("schema-2 publication has invalid capacity")
		}
		return e.Data.OrganizerID, e.Data.Capacity, nil
	case 1:
		if c.resolver == nil {
			return uuid.Nil, 0, fmt.Errorf("schema-1 publication needs catalog resolver")
		}
		organizerID, capacity, err := c.resolver.PublishedPerformance(ctx, e.Data.PerformanceID)
		if err != nil {
			return uuid.Nil, 0, err
		}
		if organizerID != e.Data.OrganizerID || capacity <= 0 {
			return uuid.Nil, 0, fmt.Errorf("schema-1 publication conflicts with catalog")
		}
		return organizerID, capacity, nil
	default:
		return uuid.Nil, 0, fmt.Errorf("unsupported publication schema %d", e.Schema)
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
		organizerID, capacity, err := c.provisionInput(ctx, e)
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
		if err := c.st.Provision(ctx, e.ID, e.Data.PerformanceID, organizerID, capacity); err != nil {
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
