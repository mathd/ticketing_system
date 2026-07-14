package consumer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"ticketing/services/access/internal/store"
	"ticketing/services/access/internal/ticket"
)

const (
	SubjectOrderCompleted = "platform.commerce.order.completed"
	SubjectFailure        = "platform.access.ticket-issuance.failed"
)

type FailureStage string

const (
	StageContract FailureStage = "contract"
	StageIssuance FailureStage = "issuance"
	StageDelivery FailureStage = "delivery"
)

const (
	ReasonInvalidJSON       = "invalid_json"
	ReasonInvalidContract   = "invalid_contract"
	ReasonIssuanceExhausted = "issuance_retries_exhausted"
	ReasonDeliveryExhausted = "delivery_retries_exhausted"
)

type FailureEvent struct {
	ID         uuid.UUID   `json:"id"`
	Type       string      `json:"type"`
	OccurredAt time.Time   `json:"occurred_at"`
	Schema     int         `json:"schema"`
	Data       FailureData `json:"data"`
}

type FailureData struct {
	SourceEventID      string       `json:"source_event_id,omitempty"`
	MessageFingerprint string       `json:"message_fingerprint,omitempty"`
	Reason             string       `json:"reason"`
	Stage              FailureStage `json:"stage"`
	Attempts           uint64       `json:"attempts"`
}

type Options struct {
	MaxProcessAttempts int
	MaxDeliver         int
	BackOff            []time.Duration
}

func DefaultOptions() Options {
	return Options{
		MaxProcessAttempts: 4,
		MaxDeliver:         6,
		BackOff:            []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute},
	}
}

func ParseOptions(backoff string) (Options, error) {
	options := DefaultOptions()
	if strings.TrimSpace(backoff) == "" {
		return options, nil
	}
	parts := strings.Split(backoff, ",")
	parsed := make([]time.Duration, 0, len(parts))
	for _, part := range parts {
		duration, err := time.ParseDuration(strings.TrimSpace(part))
		if err != nil || duration <= 0 {
			return Options{}, fmt.Errorf("invalid event retry backoff %q", part)
		}
		parsed = append(parsed, duration)
	}
	if len(parsed) > options.MaxDeliver {
		return Options{}, fmt.Errorf("event retry backoff has %d intervals, maximum is %d", len(parsed), options.MaxDeliver)
	}
	options.BackOff = parsed
	return options, nil
}

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
	maxProcessAttempts            int
	maxDeliver                    int
	backoff                       []time.Duration
	process                       func(context.Context, completed) (FailureStage, error)
	failurePublisher              func(context.Context, FailureEvent) error
	failureCounter                metric.Int64Counter
	retryCounter                  metric.Int64Counter
	failurePublishCounter         metric.Int64Counter
}

func New(js jetstream.JetStream, st *store.Postgres, signer *ticket.Signer, client *http.Client, commerceURL, token, publicURL string, mailer Mailer, log *slog.Logger, configured ...Options) *Consumer {
	options := DefaultOptions()
	if len(configured) > 0 {
		options = configured[0]
	}
	meter := otel.Meter("ticketing/access/consumer")
	failures, _ := meter.Int64Counter("access.event.failures")
	retries, _ := meter.Int64Counter("access.event.retries")
	publishFailures, _ := meter.Int64Counter("access.event.failure_publish_errors")
	c := &Consumer{
		js: js, st: st, signer: signer, client: client, commerceURL: strings.TrimSuffix(commerceURL, "/"), token: token,
		publicURL: strings.TrimSuffix(publicURL, "/"), mailer: mailer, log: log,
		maxProcessAttempts: options.MaxProcessAttempts, maxDeliver: options.MaxDeliver, backoff: append([]time.Duration(nil), options.BackOff...),
		failureCounter: failures, retryCounter: retries, failurePublishCounter: publishFailures,
	}
	c.failurePublisher = c.publishFailure
	return c
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

func validateCompleted(e completed) error {
	if e.ID == uuid.Nil || e.Type != SubjectOrderCompleted || e.Schema != 1 || e.Data.OrderID == uuid.Nil || e.Data.GuestOrderRef == uuid.Nil || e.Data.OrganizerID == uuid.Nil || e.Data.BuyerID == uuid.Nil || e.Data.SlotID == uuid.Nil || e.Data.TicketTypeID == uuid.Nil || e.Data.Quantity < 1 || e.Data.Quantity > 50 {
		return errors.New("invalid completed order event")
	}
	return nil
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
func (c *Consumer) deliver(ctx context.Context, orderID uuid.UUID) error {
	pending, err := c.st.PendingDeliveries(ctx, orderID)
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

func (c *Consumer) processCompleted(ctx context.Context, event completed) (FailureStage, error) {
	if err := c.issue(ctx, event); err != nil {
		return StageIssuance, err
	}
	if err := c.deliver(ctx, event.Data.OrderID); err != nil {
		return StageDelivery, err
	}
	return "", nil
}

func (c *Consumer) consumerConfig(durable string) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Durable: durable, FilterSubject: SubjectOrderCompleted, DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy: jetstream.AckExplicitPolicy, MaxDeliver: c.maxDeliver, BackOff: append([]time.Duration(nil), c.backoff...),
	}
}

func (c *Consumer) publishFailure(ctx context.Context, event FailureEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal failed-event record: %w", err)
	}
	if _, err := c.js.Publish(ctx, SubjectFailure, body, jetstream.WithMsgID(event.ID.String())); err != nil {
		return fmt.Errorf("publish failed-event record: %w", err)
	}
	return nil
}

func failureRecord(data []byte, eventID uuid.UUID, stage FailureStage, reason string, attempts uint64) FailureEvent {
	fingerprint := sha256.Sum256(data)
	source := eventID.String()
	if eventID == uuid.Nil {
		source = ""
	}
	identity := source
	if identity == "" {
		identity = hex.EncodeToString(fingerprint[:])
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(SubjectFailure+":"+identity+":"+reason))
	return FailureEvent{
		ID: id, Type: SubjectFailure, OccurredAt: time.Now().UTC(), Schema: 1,
		Data: FailureData{SourceEventID: source, MessageFingerprint: hex.EncodeToString(fingerprint[:]), Reason: reason, Stage: stage, Attempts: attempts},
	}
}

func (c *Consumer) deliveryCount(msg jetstream.Msg) uint64 {
	metadata, err := msg.Metadata()
	if err != nil || metadata.NumDelivered == 0 {
		return 1
	}
	return metadata.NumDelivered
}

func (c *Consumer) retryDelay(attempt uint64) time.Duration {
	if len(c.backoff) == 0 {
		return time.Second
	}
	index := int(attempt)
	if index >= len(c.backoff) {
		index = len(c.backoff) - 1
	}
	return c.backoff[index]
}

func addCounter(ctx context.Context, counter metric.Int64Counter, attributes ...attribute.KeyValue) {
	if counter != nil {
		counter.Add(ctx, 1, metric.WithAttributes(attributes...))
	}
}

func (c *Consumer) reject(ctx context.Context, msg jetstream.Msg, event FailureEvent) {
	if err := c.failurePublisher(ctx, event); err != nil {
		c.log.Error("publish failed-event record", "source_event_id", event.Data.SourceEventID, "reason", event.Data.Reason, "err", err)
		addCounter(ctx, c.failurePublishCounter, attribute.String("reason", event.Data.Reason))
		_ = msg.NakWithDelay(c.retryDelay(event.Data.Attempts))
		return
	}
	addCounter(ctx, c.failureCounter, attribute.String("reason", event.Data.Reason), attribute.String("stage", string(event.Data.Stage)))
	_ = msg.TermWithReason(event.Data.Reason)
}

func (c *Consumer) handle(ctx context.Context, msg jetstream.Msg) {
	attempts := c.deliveryCount(msg)
	var event completed
	if err := json.Unmarshal(msg.Data(), &event); err != nil {
		c.log.Error("invalid completed order event", "reason", ReasonInvalidJSON)
		c.reject(ctx, msg, failureRecord(msg.Data(), uuid.Nil, StageContract, ReasonInvalidJSON, attempts))
		return
	}
	if err := validateCompleted(event); err != nil {
		c.log.Error("invalid completed order event", "event_id", event.ID, "reason", ReasonInvalidContract)
		c.reject(ctx, msg, failureRecord(msg.Data(), event.ID, StageContract, ReasonInvalidContract, attempts))
		return
	}
	process := c.process
	if process == nil {
		process = c.processCompleted
	}
	stage, err := process(ctx, event)
	if err != nil {
		if attempts >= uint64(c.maxProcessAttempts) {
			reason := ReasonIssuanceExhausted
			if stage == StageDelivery {
				reason = ReasonDeliveryExhausted
			}
			c.log.Error("event retries exhausted", "event_id", event.ID, "stage", stage, "attempts", attempts, "err", err)
			c.reject(ctx, msg, failureRecord(msg.Data(), event.ID, stage, reason, attempts))
			return
		}
		c.log.Error("transient event failure", "event_id", event.ID, "stage", stage, "attempt", attempts, "err", err)
		addCounter(ctx, c.retryCounter, attribute.String("stage", string(stage)))
		// Intentionally leave the message unacknowledged: JetStream applies the
		// configured BackOff schedule. Explicit NAKs bypass that schedule.
		return
	}
	_ = msg.Ack()
}

func (c *Consumer) Run(ctx context.Context) error {
	stream, err := c.js.Stream(ctx, "PLATFORM")
	if err != nil {
		return err
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, c.consumerConfig("access-ticket-issuer"))
	if err != nil {
		return err
	}
	c.ready.Store(true)
	defer c.ready.Store(false)
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		c.handle(ctx, msg)
	})
	if err != nil {
		return err
	}
	defer cc.Stop()
	<-ctx.Done()
	return fmt.Errorf("consumer stopped: %w", ctx.Err())
}
