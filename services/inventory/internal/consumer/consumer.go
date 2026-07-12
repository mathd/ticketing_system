package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"ticketing/services/inventory/internal/store"
)

const subject = "platform.catalog.performance.published"

type Consumer struct {
	js    jetstream.JetStream
	st    *store.Postgres
	ready atomic.Bool
	log   *slog.Logger
}

func New(js jetstream.JetStream, st *store.Postgres, log *slog.Logger) *Consumer {
	return &Consumer{js: js, st: st, log: log}
}
func (c *Consumer) Ready() bool { return c.ready.Load() }
func (c *Consumer) Run(ctx context.Context) error {
	stream, err := c.js.Stream(ctx, "PLATFORM")
	if err != nil {
		return err
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{Durable: "inventory-performance-provisioner", FilterSubject: subject, DeliverPolicy: jetstream.DeliverAllPolicy, AckPolicy: jetstream.AckExplicitPolicy, MaxDeliver: 5})
	if err != nil {
		return err
	}
	c.ready.Store(true)
	defer c.ready.Store(false)
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		var e struct {
			ID     uuid.UUID `json:"id"`
			Schema int       `json:"schema"`
			Data   struct {
				PerformanceID uuid.UUID `json:"performance_id"`
				OrganizerID   uuid.UUID `json:"organizer_id"`
				Capacity      int32     `json:"capacity"`
			} `json:"data"`
		}
		if err := json.Unmarshal(msg.Data(), &e); err != nil || e.ID == uuid.Nil || e.Data.Capacity <= 0 {
			c.log.Error("invalid publication event", "err", err)
			_ = msg.Term()
			return
		}
		if err := c.st.Provision(ctx, e.ID, e.Data.PerformanceID, e.Data.OrganizerID, e.Data.Capacity); err != nil {
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
