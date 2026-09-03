//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// createAdminObserver attaches a JetStream consumer and core subscription to observe events
// published to the given subject. It runs on the admin connection before publish attempts.
func createAdminObserver(t *testing.T, ctx context.Context, subject string) (chan string, func()) {
	t.Helper()
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("admin connect for observer: %v", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		t.Fatalf("admin jetstream for observer: %v", err)
	}
	stream, err := js.Stream(ctx, "PLATFORM")
	if err != nil {
		nc.Close()
		t.Fatalf("admin get stream: %v", err)
	}
	consumerName := "obs-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	cons, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		DeliverPolicy: jetstream.DeliverNewPolicy,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		nc.Close()
		t.Fatalf("create observer consumer: %v", err)
	}
	seenIDs := make(chan string, 20)
	stopCh := make(chan struct{})

	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		var ev struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(msg.Data, &ev)
		if ev.ID != "" {
			seenIDs <- ev.ID
		}
	})
	if err == nil {
		_ = nc.Flush()
	}

	go func() {
		for {
			select {
			case <-stopCh:
				return
			default:
				msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(200*time.Millisecond))
				if err != nil {
					continue
				}
				for msg := range msgs.Messages() {
					_ = msg.Ack()
					var ev struct {
						ID string `json:"id"`
					}
					_ = json.Unmarshal(msg.Data(), &ev)
					if ev.ID != "" {
						seenIDs <- ev.ID
					}
				}
			}
		}
	}()

	cleanup := func() {
		close(stopCh)
		if sub != nil {
			_ = sub.Unsubscribe()
		}
		_ = stream.DeleteConsumer(context.Background(), consumerName)
		nc.Close()
	}
	return seenIDs, cleanup
}

// TestNATSUnauthenticatedPublishIsRefused asserts that a connection without credentials
// cannot publish to platform.commerce.order.completed, and that an admin observer subscribed
// before the attempt never sees the message in the stream.
func TestNATSUnauthenticatedPublishIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	eventID := uuid.NewString()
	seenIDs, cleanup := createAdminObserver(t, ctx, "platform.commerce.order.completed")
	defer cleanup()

	u, err := url.Parse(natsURL)
	if err != nil {
		t.Fatalf("parse natsURL: %v", err)
	}
	unauthURL := fmt.Sprintf("nats://%s", u.Host)

	body, _ := json.Marshal(map[string]any{
		"id":          eventID,
		"type":        "platform.commerce.order.completed",
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
		"schema":      1,
		"data": map[string]any{
			"order_id":        uuid.NewString(),
			"guest_order_ref": uuid.NewString(),
			"organizer_id":    organizerID,
			"buyer_id":        uuid.NewString(),
			"slot_id":         uuid.NewString(),
			"ticket_type_id":  uuid.NewString(),
			"quantity":        1,
		},
	})

	nc, err := nats.Connect(unauthURL, nats.Timeout(2*time.Second), nats.RetryOnFailedConnect(false))
	if err == nil {
		defer nc.Close()
		js, err := jetstream.New(nc)
		if err == nil {
			pubCtx, pubCancel := context.WithTimeout(ctx, 2*time.Second)
			defer pubCancel()
			_, pubErr := js.Publish(pubCtx, "platform.commerce.order.completed", body, jetstream.WithMsgID(eventID))
			if pubErr == nil {
				t.Fatalf("unauthenticated js.Publish succeeded; want refusal")
			}
		}
		_ = nc.Publish("platform.commerce.order.completed", body)
		_ = nc.FlushTimeout(1 * time.Second)
	}

	select {
	case got := <-seenIDs:
		if got == eventID {
			t.Fatalf("admin observer saw unauthenticated event %s; want refusal", eventID)
		}
	case <-time.After(1 * time.Second):
		// Confirmed absent
	}
}

// TestNATSCommerceCannotPublishAccessSubject asserts that commerce's credentials cannot
// publish to platform.access.ticket-issuance.failed, verified by absence from the stream.
func TestNATSCommerceCannotPublishAccessSubject(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	eventID := uuid.NewString()
	seenIDs, cleanup := createAdminObserver(t, ctx, "platform.access.ticket-issuance.failed")
	defer cleanup()

	u, err := url.Parse(natsURL)
	if err != nil {
		t.Fatalf("parse natsURL: %v", err)
	}
	commURL := fmt.Sprintf("nats://commerce:%s@%s", natsPassword("commerce"), u.Host)

	commConn, err := nats.Connect(commURL, nats.Timeout(3*time.Second), nats.RetryOnFailedConnect(false))
	if err != nil {
		t.Fatalf("commerce connect: %v", err)
	}
	defer commConn.Close()

	commJS, err := jetstream.New(commConn)
	if err != nil {
		t.Fatalf("commerce jetstream: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"id":          eventID,
		"type":        "platform.access.ticket-issuance.failed",
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
		"schema":      1,
		"data": map[string]any{
			"source_event_id":     uuid.NewString(),
			"message_fingerprint": "fingerprint",
			"reason":              "test_reason",
			"stage":               "delivery",
			"attempts":            1,
		},
	})

	// Refused publish returns context deadline exceeded rather than typed permission error.
	// Refusal is asserted by absence from the stream.
	pubCtx, pubCancel := context.WithTimeout(ctx, 1*time.Second)
	defer pubCancel()
	_, pubErr := commJS.Publish(pubCtx, "platform.access.ticket-issuance.failed", body, jetstream.WithMsgID(eventID))
	if pubErr == nil {
		t.Fatalf("commerce publish to platform.access subject succeeded; want refusal")
	}

	select {
	case got := <-seenIDs:
		if got == eventID {
			t.Fatalf("stream contains event %s published by unauthorized commerce principal", eventID)
		}
	case <-time.After(1 * time.Second):
		// Confirmed absent
	}
}

// TestNATSInventoryServerCannotPublishCatalogSubject asserts that the long-running inventory
// credential cannot publish to catalog subjects, demonstrating separation from inventory-reprocess.
func TestNATSInventoryServerCannotPublishCatalogSubject(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	eventID := uuid.NewString()
	seenIDs, cleanup := createAdminObserver(t, ctx, "platform.catalog.performance.published")
	defer cleanup()

	u, err := url.Parse(natsURL)
	if err != nil {
		t.Fatalf("parse natsURL: %v", err)
	}
	invURL := fmt.Sprintf("nats://inventory:%s@%s", natsPassword("inventory"), u.Host)

	invConn, err := nats.Connect(invURL, nats.Timeout(3*time.Second), nats.RetryOnFailedConnect(false))
	if err != nil {
		t.Fatalf("inventory connect: %v", err)
	}
	defer invConn.Close()

	invJS, err := jetstream.New(invConn)
	if err != nil {
		t.Fatalf("inventory jetstream: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"id":          eventID,
		"type":        "platform.catalog.performance.published",
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
		"schema":      1,
		"data": map[string]any{
			"slot_id":      uuid.NewString(),
			"organizer_id": organizerID,
		},
	})

	pubCtx, pubCancel := context.WithTimeout(ctx, 1*time.Second)
	defer pubCancel()
	_, pubErr := invJS.Publish(pubCtx, "platform.catalog.performance.published", body, jetstream.WithMsgID(eventID))
	if pubErr == nil {
		t.Fatalf("inventory server publish to platform.catalog subject succeeded; want refusal")
	}

	select {
	case got := <-seenIDs:
		if got == eventID {
			t.Fatalf("stream contains event %s published by unauthorized inventory server", eventID)
		}
	case <-time.After(1 * time.Second):
		// Confirmed absent
	}
}

// TestNATSPaymentsConnectsWithZeroSubjectRights asserts that payments credentials connect
// successfully to support its healthcheck, but all publish and subscribe operations are refused.
func TestNATSPaymentsConnectsWithZeroSubjectRights(t *testing.T) {
	u, err := url.Parse(natsURL)
	if err != nil {
		t.Fatalf("parse natsURL: %v", err)
	}
	payURL := fmt.Sprintf("nats://payments:%s@%s", natsPassword("payments"), u.Host)

	var mu sync.Mutex
	errCh := make(chan error, 10)

	nc, err := nats.Connect(payURL,
		nats.Timeout(3*time.Second),
		nats.RetryOnFailedConnect(false),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			mu.Lock()
			defer mu.Unlock()
			errCh <- err
		}),
	)
	if err != nil {
		t.Fatalf("payments connect: %v", err)
	}
	defer nc.Close()

	if !nc.IsConnected() {
		t.Fatalf("payments nc.IsConnected() = false; want true")
	}

	t.Run("PublishRefused", func(t *testing.T) {
		err := nc.Publish("platform.payments.test", []byte("payload"))
		if err == nil {
			_ = nc.FlushTimeout(1 * time.Second)
		}
		select {
		case e := <-errCh:
			if !strings.Contains(e.Error(), "Permissions Violation") {
				t.Fatalf("async error = %v; want Permissions Violation", e)
			}
		case <-time.After(1 * time.Second):
			t.Fatalf("payments publish produced no permission violation")
		}
	})

	t.Run("SubscribeRefused", func(t *testing.T) {
		sub, _ := nc.Subscribe("platform.>", func(_ *nats.Msg) {})
		if sub != nil {
			defer func() { _ = sub.Unsubscribe() }()
		}
		_ = nc.FlushTimeout(1 * time.Second)
		select {
		case e := <-errCh:
			if !strings.Contains(e.Error(), "Permissions Violation") {
				t.Fatalf("async error = %v; want Permissions Violation", e)
			}
		case <-time.After(1 * time.Second):
			t.Fatalf("payments subscribe produced no permission violation")
		}
	})
}

// TestNATSResidualCredentialedForgeryStillMintsTickets pins the accepted security gap:
// using commerce's legitimate credentials, publishing a hand-built valid order.completed event
// mints an authentic signed ticket that admits at the gate scanner, while commerce has zero
// database records for the order and inventory confirmed_quantity is unchanged (ADR-021 pin-the-gap).
func TestNATSResidualCredentialedForgeryStillMintsTickets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	suffix := "forgery-" + admissionSuffix()
	slotID, ticketTypeID := setupCheckoutOffer(t, suffix)

	invConn, err := pgx.Connect(ctx, dsn("inventory", "inventory"))
	if err != nil {
		t.Fatalf("connect inventory db: %v", err)
	}
	defer func() { _ = invConn.Close(ctx) }()

	var initialConfirmed int
	if err := invConn.QueryRow(ctx, "SELECT confirmed_quantity FROM inventory_pools WHERE slot_id=$1", slotID).Scan(&initialConfirmed); err != nil {
		t.Fatalf("query inventory initial confirmed_quantity: %v", err)
	}

	u, err := url.Parse(natsURL)
	if err != nil {
		t.Fatalf("parse natsURL: %v", err)
	}
	commURL := fmt.Sprintf("nats://commerce:%s@%s", natsPassword("commerce"), u.Host)

	commConn, err := nats.Connect(commURL, nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatalf("commerce connect: %v", err)
	}
	defer commConn.Close()

	commJS, err := jetstream.New(commConn)
	if err != nil {
		t.Fatalf("commerce jetstream: %v", err)
	}

	orderID := uuid.New()
	guestRef := uuid.New()
	buyerID := uuid.New()
	eventID := uuid.New()

	event := map[string]any{
		"id":          eventID.String(),
		"type":        "platform.commerce.order.completed",
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
		"schema":      1,
		"data": map[string]any{
			"order_id":        orderID.String(),
			"guest_order_ref": guestRef.String(),
			"organizer_id":    organizerID,
			"buyer_id":        buyerID.String(),
			"slot_id":         slotID,
			"ticket_type_id":  ticketTypeID,
			"quantity":        1,
		},
	}
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	if _, err := commJS.Publish(ctx, "platform.commerce.order.completed", body, jetstream.WithMsgID(eventID.String())); err != nil {
		t.Fatalf("publish commerce order.completed: %v", err)
	}

	var qrPayload string
	retry(t, 15*time.Second, func() error {
		code, respBody, _ := getWithHeaders(t, gatewayURL+"/api/access/orders/"+guestRef.String()+"/tickets")
		if code != http.StatusOK {
			return fmt.Errorf("ticket bundle status %d: %s", code, respBody)
		}
		var bundle struct {
			Tickets []struct {
				QRPayload string `json:"qr_payload"`
			} `json:"tickets"`
		}
		if err := json.Unmarshal(respBody, &bundle); err != nil {
			return err
		}
		if len(bundle.Tickets) != 1 {
			return fmt.Errorf("expected 1 ticket in bundle, got %d", len(bundle.Tickets))
		}
		if bundle.Tickets[0].QRPayload == "" {
			return fmt.Errorf("ticket qr_payload is empty")
		}
		qrPayload = bundle.Tickets[0].QRPayload
		return nil
	})

	occ := uuid.NewString()
	occurredAt := time.Now().UTC().Add(-1 * time.Minute).Truncate(time.Microsecond)
	scanPayload := map[string]any{
		"qr_payload":    qrPayload,
		"occurrence_id": occ,
		"occurred_at":   occurredAt.Format(time.RFC3339Nano),
	}
	code, scanResp := postWithKey(t, gatewayURL+"/api/access/scans", "scan-"+suffix, scanPayload)
	if code != http.StatusOK {
		t.Fatalf("scan status %d: %s", code, scanResp)
	}
	var scanResult struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(scanResp, &scanResult); err != nil {
		t.Fatalf("unmarshal scan response: %v", err)
	}
	if scanResult.Decision != "accepted" {
		t.Fatalf("scan decision = %q, want %q", scanResult.Decision, "accepted")
	}

	commDB, err := pgx.Connect(ctx, dsn("commerce", "commerce"))
	if err != nil {
		t.Fatalf("connect commerce db: %v", err)
	}
	defer func() { _ = commDB.Close(ctx) }()

	var orderCount int
	if err := commDB.QueryRow(ctx, "SELECT count(*) FROM orders WHERE id=$1", orderID).Scan(&orderCount); err != nil {
		t.Fatalf("query commerce orders: %v", err)
	}
	if orderCount != 0 {
		t.Fatalf("commerce.orders count = %d for forged order %s, want 0", orderCount, orderID)
	}

	var finalConfirmed int
	if err := invConn.QueryRow(ctx, "SELECT confirmed_quantity FROM inventory_pools WHERE slot_id=$1", slotID).Scan(&finalConfirmed); err != nil {
		t.Fatalf("query inventory final confirmed_quantity: %v", err)
	}
	if finalConfirmed != initialConfirmed {
		t.Fatalf("inventory_pools confirmed_quantity changed: initial=%d, final=%d", initialConfirmed, finalConfirmed)
	}
}
