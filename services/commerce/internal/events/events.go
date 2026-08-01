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

	"ticketing/shared/domainevent"
)

const SubjectOrderCompleted = "platform.commerce.order.completed"

type OrderCompletedData struct {
	OrderID       uuid.UUID `json:"order_id"`
	GuestOrderRef uuid.UUID `json:"guest_order_ref"`
	OrganizerID   uuid.UUID `json:"organizer_id"`
	BuyerID       uuid.UUID `json:"buyer_id"`
	SlotID        uuid.UUID `json:"slot_id"`
	TicketTypeID  uuid.UUID `json:"ticket_type_id"`
	Quantity      int32     `json:"quantity"`
}

// SubjectOrderExchanged carries an exchange whose money has settled and whose
// entitlement has not yet switched (TKT-166, ADR-039 §5).
//
// It is a NEW SUBJECT rather than `order.completed` schema 2. Bumping the completed
// schema would drag ADR-017's skew rules across the issuance path every consumer already
// depends on, to describe an event most of them will never see — and TKT-61 shipped that
// exact ordering bug twice. A new subject cannot break `order.completed`'s decode at all.
//
// It is also not "an event followed by ordinary issuance": the replacement order
// deliberately owes no `order.completed`, so this event is the SOLE trigger that issues
// its tickets, and the consumer voids the old ones in the same transaction. Anything
// looser reopens the both-admit window (ADR-039 §3).
const SubjectOrderExchanged = "platform.commerce.order.exchanged"

// OrderExchangedData names both sides of the switch: the source order whose tickets are
// voided, and the replacement line whose tickets are issued.
//
// GuestOrderRef is the SOURCE order's reference, not the replacement's. The buyer's link
// is a capability they already hold and have possibly already been emailed; reusing it
// means the old and new tickets appear together under one link. The replacement commerce
// order keeps its own (nullable, unset) reference — `orders.guest_order_ref` is unique,
// so it could not have carried the source's even if that were wanted.
type OrderExchangedData struct {
	ExchangeID         uuid.UUID `json:"exchange_id"`
	SourceOrderID      uuid.UUID `json:"source_order_id"`
	ReplacementOrderID uuid.UUID `json:"replacement_order_id"`
	GuestOrderRef      uuid.UUID `json:"guest_order_ref"`
	OrganizerID        uuid.UUID `json:"organizer_id"`
	BuyerID            uuid.UUID `json:"buyer_id"`
	SlotID             uuid.UUID `json:"slot_id"`
	TicketTypeID       uuid.UUID `json:"ticket_type_id"`
	Quantity           int32     `json:"quantity"`
}

// ExchangedEventID derives the deterministic id from the exchange, so a republish of the
// frozen bytes dedupes at the broker on Nats-Msg-Id.
func ExchangedEventID(exchangeID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(SubjectOrderExchanged+":"+exchangeID.String()))
}

// OrderExchangedEnvelope is the ONE definition of these bytes, as
// OrderCompletedEnvelope is for its subject: the settlement transaction freezes what it
// returns and the drainer publishes that verbatim.
func OrderExchangedEnvelope(id uuid.UUID, data OrderExchangedData, occurred time.Time) ([]byte, error) {
	body, err := json.Marshal(domainevent.Envelope[OrderExchangedData]{ID: id, Type: SubjectOrderExchanged, OccurredAt: occurred, Schema: 1, Data: data})
	if err != nil {
		return nil, fmt.Errorf("marshal order exchanged: %w", err)
	}
	return body, nil
}

type Publisher interface {
	OrderCompleted(context.Context, OrderCompletedData) error
	// PublishRaw transmits an already-frozen envelope. Callers holding committed
	// bytes must use this: OrderCompleted re-marshals with a fresh timestamp, which
	// would put a different payload on the wire under the same deterministic id.
	PublishRaw(ctx context.Context, subject string, eventID uuid.UUID, envelope []byte) error
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

// OrderCompletedEnvelope marshals the order.completed envelope at a
// caller-supplied instant. It is the ONE definition of these bytes: the outbox
// path (store.completionEnvelope) freezes what it returns, and OrderCompleted
// publishes it directly. Exported so the outbox shares it rather than
// re-declaring the envelope a seventh time (TKT-126); the caller-supplied
// instant is what lets the golden pin it without racing time.Now.
func OrderCompletedEnvelope(id uuid.UUID, data OrderCompletedData, occurred time.Time) ([]byte, error) {
	body, err := json.Marshal(domainevent.Envelope[OrderCompletedData]{ID: id, Type: SubjectOrderCompleted, OccurredAt: occurred, Schema: 1, Data: data})
	if err != nil {
		return nil, fmt.Errorf("marshal order completed: %w", err)
	}
	return body, nil
}

func (p *JetStream) OrderCompleted(ctx context.Context, data OrderCompletedData) error {
	id := EventID(data.OrderID)
	body, err := OrderCompletedEnvelope(id, data, time.Now().UTC())
	if err != nil {
		return err
	}
	return p.PublishRaw(ctx, SubjectOrderCompleted, id, body)
}

// PublishRaw transmits an already-serialized envelope verbatim. The outbox drainer
// uses this so the frozen bytes committed with the order are what reach the broker;
// re-marshalling here would reintroduce the retry-dependent payload the outbox exists
// to eliminate. Nats-Msg-Id carries the deterministic event id, so JetStream dedupes
// the duplicates that at-least-once delivery implies.
func (p *JetStream) PublishRaw(ctx context.Context, subject string, eventID uuid.UUID, envelope []byte) error {
	msg := &nats.Msg{Subject: subject, Data: envelope, Header: nats.Header{"Nats-Msg-Id": []string{eventID.String()}}}
	if _, err := p.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}
