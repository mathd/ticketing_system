package consumer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"ticketing/services/access/internal/store"
	"ticketing/services/access/internal/ticket"
)

const subject = "platform.commerce.order.completed"

type Mailer interface {
	Send(context.Context, uuid.UUID, string, string) error
}
type LogMailer struct{ log *slog.Logger }

func NewLogMailer(log *slog.Logger) LogMailer { return LogMailer{log: log} }
func (m LogMailer) Send(_ context.Context, id uuid.UUID, email, link string) error {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	linkSum := sha256.Sum256([]byte(link))
	m.log.Info("ticket delivery accepted", "delivery_id", id, "recipient_hash", hex.EncodeToString(sum[:]), "ticket_link_hash", hex.EncodeToString(linkSum[:]))
	return nil
}

type Consumer struct {
	js                            jetstream.JetStream
	st                            *store.Postgres
	signer                        *ticket.Signer
	client                        *http.Client
	commerceURL, token, publicURL string
	mailer                        Mailer
	log                           *slog.Logger
	ready                         atomic.Bool
}

func New(js jetstream.JetStream, st *store.Postgres, signer *ticket.Signer, client *http.Client, commerceURL, token, publicURL string, mailer Mailer, log *slog.Logger) *Consumer {
	return &Consumer{js: js, st: st, signer: signer, client: client, commerceURL: strings.TrimSuffix(commerceURL, "/"), token: token, publicURL: strings.TrimSuffix(publicURL, "/"), mailer: mailer, log: log}
}
func (c *Consumer) Ready() bool { return c.ready.Load() }

type completed struct {
	ID     uuid.UUID `json:"id"`
	Type   string    `json:"type"`
	Schema int       `json:"schema"`
	Data   struct {
		OrderID       uuid.UUID `json:"order_id"`
		GuestOrderRef uuid.UUID `json:"guest_order_ref"`
		OrganizerID   uuid.UUID `json:"organizer_id"`
		BuyerID       uuid.UUID `json:"buyer_id"`
		SlotID        uuid.UUID `json:"slot_id"`
		TicketTypeID  uuid.UUID `json:"ticket_type_id"`
		Quantity      int32     `json:"quantity"`
	} `json:"data"`
}

func (c *Consumer) issue(ctx context.Context, e completed) error {
	if e.ID == uuid.Nil || e.Type != subject || e.Schema != 1 || e.Data.OrderID == uuid.Nil || e.Data.GuestOrderRef == uuid.Nil || e.Data.OrganizerID == uuid.Nil || e.Data.BuyerID == uuid.Nil || e.Data.SlotID == uuid.Nil || e.Data.TicketTypeID == uuid.Nil || e.Data.Quantity < 1 || e.Data.Quantity > 50 {
		return fmt.Errorf("invalid completed order event")
	}
	now := time.Now().UTC()
	in := store.IssueInput{EventID: e.ID}
	for i := int32(0); i < e.Data.Quantity; i++ {
		id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(e.ID.String()+fmt.Sprintf(":%d", i)))
		payload, err := c.signer.Payload(id, e.Data.OrderID, e.Data.OrganizerID, e.Data.SlotID, now)
		if err != nil {
			return err
		}
		in.Tickets = append(in.Tickets, store.Ticket{ID: id, OrderID: e.Data.OrderID, GuestOrderRef: e.Data.GuestOrderRef, OrganizerID: e.Data.OrganizerID, BuyerID: e.Data.BuyerID, SlotID: e.Data.SlotID, TicketTypeID: e.Data.TicketTypeID, Payload: payload, IssuedAt: now})
	}
	return c.st.Issue(ctx, in)
}

func (c *Consumer) email(ctx context.Context, buyer uuid.UUID) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.commerceURL+"/internal/buyers/"+buyer.String()+"/delivery-email", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Internal-Token", c.token)
	res, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("commerce delivery address: %d", res.StatusCode)
	}
	var v struct {
		Email string `json:"email"`
	}
	if err = json.NewDecoder(res.Body).Decode(&v); err != nil || v.Email == "" {
		return "", fmt.Errorf("invalid commerce delivery address")
	}
	return v.Email, nil
}
func (c *Consumer) deliver(ctx context.Context) error {
	pending, err := c.st.PendingDeliveries(ctx)
	if err != nil {
		return err
	}
	for _, t := range pending {
		id, err := c.st.DeliveryID(ctx, t.ID)
		if err != nil {
			return err
		}
		email, err := c.email(ctx, t.BuyerID)
		if err != nil {
			return err
		}
		link := c.publicURL + "/en/tickets/" + t.GuestOrderRef.String()
		if err = c.mailer.Send(ctx, id, email, link); err != nil {
			return err
		}
		if err = c.st.MarkDelivered(ctx, t.ID, id); err != nil {
			return err
		}
	}
	return nil
}

func (c *Consumer) Run(ctx context.Context) error {
	stream, err := c.js.Stream(ctx, "PLATFORM")
	if err != nil {
		return err
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{Durable: "access-ticket-issuer", FilterSubject: subject, DeliverPolicy: jetstream.DeliverAllPolicy, AckPolicy: jetstream.AckExplicitPolicy, MaxDeliver: -1})
	if err != nil {
		return err
	}
	c.ready.Store(true)
	defer c.ready.Store(false)
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		var e completed
		if err := json.Unmarshal(msg.Data(), &e); err != nil {
			c.log.Error("invalid completed order event", "err", err)
			_ = msg.Term()
			return
		}
		if err := c.issue(ctx, e); err != nil {
			c.log.Error("issue tickets", "event_id", e.ID, "err", err)
			_ = msg.NakWithDelay(time.Second)
			return
		}
		if err := c.deliver(ctx); err != nil {
			c.log.Error("deliver tickets", "event_id", e.ID, "err", err)
			_ = msg.NakWithDelay(time.Second)
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
