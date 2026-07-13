// Package events publishes Commerce domain events using the ADR-009 envelope.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const SubjectOrderCompleted = "platform.commerce.order.completed"

type Envelope struct {
	ID         uuid.UUID `json:"id"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurred_at"`
	Schema     int       `json:"schema"`
	Data       any       `json:"data"`
}

type OrderCompletedData struct {
	OrderID       uuid.UUID `json:"order_id"`
	GuestOrderRef uuid.UUID `json:"guest_order_ref"`
	OrganizerID   uuid.UUID `json:"organizer_id"`
	BuyerID       uuid.UUID `json:"buyer_id"`
	SlotID        uuid.UUID `json:"slot_id"`
	TicketTypeID  uuid.UUID `json:"ticket_type_id"`
	Quantity      int32     `json:"quantity"`
}

type Publisher interface {
	OrderCompleted(context.Context, OrderCompletedData) error
}
type JetStream struct{ js jetstream.JetStream }

func NewJetStream(nc *nats.Conn) (*JetStream, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return &JetStream{js: js}, nil
}

func EventID(orderID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(SubjectOrderCompleted+":"+orderID.String()))
}

func (p *JetStream) OrderCompleted(ctx context.Context, data OrderCompletedData) error {
	id := EventID(data.OrderID)
	body, err := json.Marshal(Envelope{ID: id, Type: SubjectOrderCompleted, OccurredAt: time.Now().UTC(), Schema: 1, Data: data})
	if err != nil {
		return fmt.Errorf("marshal order completed: %w", err)
	}
	msg := &nats.Msg{Subject: SubjectOrderCompleted, Data: body, Header: nats.Header{"Nats-Msg-Id": []string{id.String()}}}
	if _, err := p.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", SubjectOrderCompleted, err)
	}
	return nil
}
