//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// assertEventAbsent drains the observer until the deadline and fails only if the TARGET id
// appears. It deliberately does not stop at the first id it sees.
//
// The single-read version this replaces (ai-review F5) accepted an unrelated event as proof of
// absence: the observer listens on live production subjects, so any background event satisfied
// one read and ended the check before the forged id could arrive. The test then passed while
// the mechanism it names was never exercised — green, and about something else.
func assertEventAbsent(t *testing.T, seenIDs <-chan string, targetID, whenSeen string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-seenIDs:
			if got == targetID {
				t.Fatalf(whenSeen, targetID)
			}
			// An unrelated event. Keep waiting for the target or the deadline.
		case <-deadline:
			return
		}
	}
}

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

	assertEventAbsent(t, seenIDs, eventID,
		"admin observer saw unauthenticated event %s; want refusal")
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

	assertEventAbsent(t, seenIDs, eventID,
		"stream contains event %s published by unauthorized commerce principal")
}

// TestNATSInventoryServerCannotPublishCatalogSubject asserts that the inventory credential
// cannot publish to catalog subjects. Since TKT-326 no inventory principal can: reprocess-quarantine
// applies quarantined events in-process, so catalog is the only service that publishes on its prefix.
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

	assertEventAbsent(t, seenIDs, eventID,
		"stream contains event %s published by unauthorized inventory server")
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

// TestNATSResidualCredentialedForgeryStillMintsTickets pins the unknown-order refusal and one
// still-open case from ADR-072 §6(b). TKT-327 closed the JetStream read of order.completed through
// a widened access or inventory durable (TestNATSServiceCannotWidenItsOwnPluralDurable); the
// forgery does not need that read, because the publisher holds the real line in its own database.
// This test reads the line from the database and pins only the completed-order case. The read does not check status, buyer ID,
// or guest order reference. It is not a security control; signed envelopes (TKT-296) are the fix.
func TestNATSResidualCredentialedForgeryStillMintsTickets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	suffix := "forgery-" + admissionSuffix()
	unknownSlotID, unknownTicketTypeID := setupCheckoutOffer(t, suffix)

	invConn, err := pgx.Connect(ctx, dsn("inventory", "inventory"))
	if err != nil {
		t.Fatalf("connect inventory db: %v", err)
	}
	defer func() { _ = invConn.Close(ctx) }()

	var initialConfirmed int
	if err := invConn.QueryRow(ctx, "SELECT confirmed_quantity FROM inventory_pools WHERE slot_id=$1", unknownSlotID).Scan(&initialConfirmed); err != nil {
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

	// (a) Commerce's NATS credentials cannot make an unknown order pass the seat
	// read. Wait for the failure record so a momentary absence of tickets is not
	// mistaken for a refusal.
	adminConn, err := nats.Connect(natsURL, nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatalf("admin connect for failure observer: %v", err)
	}
	defer adminConn.Close()
	adminJS, err := jetstream.New(adminConn)
	if err != nil {
		t.Fatalf("admin jetstream for failure observer: %v", err)
	}
	stream, err := adminJS.Stream(ctx, "PLATFORM")
	if err != nil {
		t.Fatalf("admin get PLATFORM stream: %v", err)
	}
	failureName := "forgery-failures-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	failures, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{
		Durable: failureName, FilterSubject: accessFailureSubject,
		DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("create access failure consumer: %v", err)
	}
	t.Cleanup(func() { _ = stream.DeleteConsumer(context.Background(), failureName) })

	unknownOrderID, unknownGuestRef, unknownEventID := uuid.New(), uuid.New(), uuid.New()
	unknownEvent := map[string]any{
		"id": unknownEventID.String(), "type": "platform.commerce.order.completed",
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano), "schema": 1,
		"data": map[string]any{
			"order_id": unknownOrderID.String(), "guest_order_ref": unknownGuestRef.String(),
			"organizer_id": organizerID, "buyer_id": uuid.NewString(),
			"slot_id": unknownSlotID, "ticket_type_id": unknownTicketTypeID, "quantity": 1,
		},
	}
	unknownBody, err := json.Marshal(unknownEvent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commJS.Publish(ctx, "platform.commerce.order.completed", unknownBody, jetstream.WithMsgID(unknownEventID.String())); err != nil {
		t.Fatalf("publish unknown-order event: %v", err)
	}
	unknownFailure := nextAccessFailure(t, failures, 15*time.Second)
	if unknownFailure.Data.SourceEventID != unknownEventID.String() || unknownFailure.Data.Reason != "issuance_retries_exhausted" || unknownFailure.Data.Stage != "issuance" || unknownFailure.Data.Attempts != 4 {
		t.Fatalf("unknown-order failure = %+v", unknownFailure)
	}
	accessDB := accessConn(t, ctx)
	defer func() { _ = accessDB.Close(ctx) }()
	var unknownTicketCount int
	if err := accessDB.QueryRow(ctx, "SELECT count(*) FROM tickets WHERE order_id=$1", unknownOrderID).Scan(&unknownTicketCount); err != nil {
		t.Fatalf("query tickets for unknown order: %v", err)
	}
	if unknownTicketCount != 0 {
		t.Fatalf("unknown order %s has %d tickets, want 0", unknownOrderID, unknownTicketCount)
	}

	var finalConfirmed int
	if err := invConn.QueryRow(ctx, "SELECT confirmed_quantity FROM inventory_pools WHERE slot_id=$1", unknownSlotID).Scan(&finalConfirmed); err != nil {
		t.Fatalf("query inventory final confirmed_quantity: %v", err)
	}
	if finalConfirmed != initialConfirmed {
		t.Fatalf("inventory_pools confirmed_quantity changed: initial=%d, final=%d", initialConfirmed, finalConfirmed)
	}

	// (b) The read does not close the gap. Reuse the real-checkout fixture, then
	// read its exact persisted line and publish that line under a fresh event ID.
	realOrderID, realGuestRef, _, _ := consoleFixture(t, suffix+"-real")
	commDB, err := pgx.Connect(ctx, dsn("commerce", "commerce"))
	if err != nil {
		t.Fatalf("connect commerce db: %v", err)
	}
	defer func() { _ = commDB.Close(ctx) }()
	var realOrganizerID, realBuyerID, realSlotID, realTicketTypeID string
	var quantity int
	if err := commDB.QueryRow(ctx, `SELECT r.organizer_id::text, r.buyer_id::text, r.slot_id::text,
		r.ticket_type_id::text, r.quantity
		FROM orders o JOIN reservations r ON r.id=o.reservation_id WHERE o.id=$1`, realOrderID).
		Scan(&realOrganizerID, &realBuyerID, &realSlotID, &realTicketTypeID, &quantity); err != nil {
		t.Fatalf("read real order line: %v", err)
	}
	var originalTicketIDs []string
	retry(t, 15*time.Second, func() error {
		var count int
		if err := accessDB.QueryRow(ctx, "SELECT count(*) FROM tickets WHERE order_id=$1", realOrderID).Scan(&count); err != nil {
			return err
		}
		if count != quantity {
			return fmt.Errorf("real order ticket count = %d, want quantity %d before forged replay", count, quantity)
		}
		rows, err := accessDB.Query(ctx, "SELECT id::text FROM tickets WHERE order_id=$1", realOrderID)
		if err != nil {
			return err
		}
		defer rows.Close()
		originalTicketIDs = originalTicketIDs[:0]
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			originalTicketIDs = append(originalTicketIDs, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(originalTicketIDs) != quantity {
			return fmt.Errorf("read %d original ticket IDs, want %d", len(originalTicketIDs), quantity)
		}
		return nil
	})
	replayEventID := uuid.New()
	replayEvent := map[string]any{
		"id": replayEventID.String(), "type": "platform.commerce.order.completed",
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano), "schema": 1,
		"data": map[string]any{
			"order_id": realOrderID, "guest_order_ref": realGuestRef,
			"organizer_id": realOrganizerID, "buyer_id": realBuyerID,
			"slot_id": realSlotID, "ticket_type_id": realTicketTypeID, "quantity": quantity,
		},
	}
	replayBody, err := json.Marshal(replayEvent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commJS.Publish(ctx, "platform.commerce.order.completed", replayBody, jetstream.WithMsgID(replayEventID.String())); err != nil {
		t.Fatalf("publish real-order forged event: %v", err)
	}
	retry(t, 15*time.Second, func() error {
		var count int
		if err := accessDB.QueryRow(ctx, "SELECT count(*) FROM tickets WHERE order_id=$1", realOrderID).Scan(&count); err != nil {
			return err
		}
		if count != 2*quantity {
			return fmt.Errorf("real order ticket count = %d, want %d after forged replay", count, 2*quantity)
		}
		return nil
	})

	original := make(map[string]struct{}, len(originalTicketIDs))
	for _, id := range originalTicketIDs {
		original[id] = struct{}{}
	}
	var forgedQR string
	rows, err := accessDB.Query(ctx, "SELECT id::text, qr_payload FROM tickets WHERE order_id=$1", realOrderID)
	if err != nil {
		t.Fatalf("query forged ticket set: %v", err)
	}
	for rows.Next() {
		var id, qr string
		if err := rows.Scan(&id, &qr); err != nil {
			rows.Close()
			t.Fatalf("scan ticket row: %v", err)
		}
		if _, exists := original[id]; !exists {
			forgedQR = qr
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate forged ticket rows: %v", err)
	}
	if forgedQR == "" {
		t.Fatal("forged replay produced no additional ticket credential")
	}

	scanPayload := map[string]any{
		"qr_payload": forgedQR, "occurrence_id": uuid.NewString(),
		"occurred_at": time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond).Format(time.RFC3339Nano),
	}
	code, scanResp := postWithKey(t, gatewayURL+"/api/access/scans", "scan-forged-"+suffix, scanPayload)
	if code != http.StatusOK {
		t.Fatalf("scan forged ticket status %d: %s", code, scanResp)
	}
	var scanResult struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(scanResp, &scanResult); err != nil {
		t.Fatalf("unmarshal forged ticket scan response: %v", err)
	}
	if scanResult.Decision != "accepted" {
		t.Fatalf("forged ticket scan decision = %q, want accepted", scanResult.Decision)
	}
}

// TestNATSInventoryContainerConnectsAsTheInventoryPrincipal asserts that the RUNNING inventory
// container connects to NATS as the `inventory` principal. TestNATSInventoryServerCannotPublishCatalogSubject
// builds its own connection with the inventory credential, so it proves what that principal can
// do, not that the container uses it: a Compose change that pointed inventory at platform-admin
// would leave it green. This reads the container's environment, where the property lives.
// (Kept from TestNATSOperatorCredentialIsConfinedToTheBroker, which TKT-326 deleted with the
// operator identity; this assertion was still valid.)
func TestNATSInventoryContainerConnectsAsTheInventoryPrincipal(t *testing.T) {
	ctx := context.Background()
	container := fmt.Sprintf("%s-inventory-1", project)
	env, err := dockerRun(ctx, "inspect", "-f", "{{range .Config.Env}}{{println .}}{{end}}", container)
	if err != nil {
		t.Fatalf("docker inspect %s: %v: %s", container, err, env)
	}
	var natsURLs []string
	for _, line := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "NATS_URL="); ok {
			natsURLs = append(natsURLs, v)
		}
	}
	if len(natsURLs) != 1 {
		t.Fatalf("%s has %d NATS_URL entries, want exactly one", container, len(natsURLs))
	}
	u, err := url.Parse(natsURLs[0])
	if err != nil {
		// Do not echo the value or the parse error: both can carry the password.
		t.Fatalf("%s NATS_URL does not parse as a URL", container)
	}
	if u.User == nil || u.User.Username() != "inventory" {
		got := "<none>"
		if u.User != nil {
			got = u.User.Username()
		}
		t.Fatalf("%s connects to NATS as %q, want exactly %q", container, got, "inventory")
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		t.Fatalf("%s NATS_URL names the inventory principal with no password", container)
	}
}

// TestNATSServiceCannotWidenItsOwnPluralDurable is the executed attack from TKT-170, asserted as
// refused (TKT-327, ADR-072 §5). The adversary is a holder of the access or inventory NATS
// credential (ADR-021: name the adversary). Before TKT-327 each service held CREATE on its own
// plural-filter durable, and a plural-filter create request carries its filters in the BODY, where
// no subject permission can constrain them: inventory's credential re-created
// inventory-catalog-offering filtered on platform.commerce.order.completed and read commerce's
// payload back, which carries guest_order_ref.
//
// Each case sends a raw request, the way an attacker would, not through a client helper that
// might refuse first. A case passes only on all three: the broker reports a publish Permissions
// Violation for THAT exact subject, no reply arrives, and an admin INFO shows the filters
// unchanged. With a CREATE grant restored, the broker answers the request, so the case goes red
// on any reply, whether or not a pull later wins a race for the message.
//
// The body keeps every other field of the current config, so a refusal cannot come from an
// invalid config. The legacy DURABLE.CREATE subject was never granted to either service, so its
// case is a pin, not a mutation-checked guard.
func TestNATSServiceCannotWidenItsOwnPluralDurable(t *testing.T) {
	u, err := url.Parse(natsURL)
	if err != nil {
		t.Fatalf("parse natsURL: %v", err)
	}
	for _, target := range []struct {
		principal string
		durable   string
		foreign   string // a subject this principal must not read
	}{
		{principal: "inventory", durable: "inventory-catalog-offering", foreign: "platform.commerce.order.completed"},
		{principal: "access", durable: "access-ticket-issuer", foreign: "platform.catalog.performance.archived"},
	} {
		t.Run(target.principal, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			adminConn, err := nats.Connect(natsURL, nats.Timeout(3*time.Second))
			if err != nil {
				t.Fatalf("admin connect: %v", err)
			}
			t.Cleanup(adminConn.Close)
			adminJS, err := jetstream.New(adminConn)
			if err != nil {
				t.Fatalf("admin jetstream: %v", err)
			}
			stream, err := adminJS.Stream(ctx, "PLATFORM")
			if err != nil {
				t.Fatalf("admin get PLATFORM stream: %v", err)
			}
			currentConfig := func() jetstream.ConsumerConfig {
				t.Helper()
				cons, err := stream.Consumer(ctx, target.durable)
				if err != nil {
					t.Fatalf("admin INFO %s: %v", target.durable, err)
				}
				return cons.CachedInfo().Config
			}
			original := currentConfig()
			if len(original.FilterSubjects) < 2 || original.FilterSubject != "" {
				t.Fatalf("%s filters = %v / %q, want the plural form this attack targets", target.durable, original.FilterSubjects, original.FilterSubject)
			}
			// If an attempt got through (a restored grant), put the durable back as admin. It runs
			// after EACH attempt, not once at the end: a widened durable left in place would make
			// the next attempt fail on "filters changed" for a request that was in fact refused,
			// and it would leave the running service consuming the foreign subject.
			restore := func(t *testing.T) {
				restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer restoreCancel()
				cons, err := stream.Consumer(restoreCtx, target.durable)
				if err != nil {
					t.Errorf("cleanup: admin INFO %s: %v", target.durable, err)
					return
				}
				got := cons.CachedInfo().Config
				if slices.Equal(got.FilterSubjects, original.FilterSubjects) && got.FilterSubject == original.FilterSubject {
					return
				}
				if _, err := stream.CreateOrUpdateConsumer(restoreCtx, original); err != nil {
					t.Errorf("cleanup: restore %s filters as admin: %v", target.durable, err)
				}
			}

			widened := original
			widened.FilterSubjects = append(slices.Clone(original.FilterSubjects), target.foreign)
			single := original
			single.FilterSubjects = nil
			single.FilterSubject = target.foreign

			for _, attempt := range []struct {
				name    string
				subject string
				config  jetstream.ConsumerConfig
			}{
				// The TKT-170 attack: the plural filter rides in the body of the unsuffixed subject.
				{name: "plural body on the unsuffixed create subject",
					subject: "$JS.API.CONSUMER.CREATE.PLATFORM." + target.durable, config: widened},
				// The filter as a subject token: the second removed grant (CREATE.<durable>.>).
				{name: "single filter on the suffixed create subject",
					subject: "$JS.API.CONSUMER.CREATE.PLATFORM." + target.durable + "." + target.foreign, config: single},
				// Pin: the pre-2.9 durable create API, never granted to either service.
				{name: "legacy durable create subject",
					subject: "$JS.API.CONSUMER.DURABLE.CREATE.PLATFORM." + target.durable, config: widened},
			} {
				t.Run(attempt.name, func(t *testing.T) {
					t.Cleanup(func() { restore(t) })
					body, err := json.Marshal(struct {
						Stream string                   `json:"stream_name"`
						Config jetstream.ConsumerConfig `json:"config"`
					}{Stream: "PLATFORM", Config: attempt.config})
					if err != nil {
						t.Fatal(err)
					}
					asyncErrs := make(chan error, 8)
					serviceURL := fmt.Sprintf("nats://%s:%s@%s", target.principal, natsPassword(target.principal), u.Host)
					nc, err := nats.Connect(serviceURL,
						nats.Timeout(3*time.Second),
						nats.RetryOnFailedConnect(false),
						nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
							select {
							case asyncErrs <- err:
							default:
							}
						}),
					)
					if err != nil {
						t.Fatalf("%s connect: %v", target.principal, err)
					}
					defer nc.Close()

					reply, reqErr := nc.Request(attempt.subject, body, 2*time.Second)
					if reqErr == nil {
						t.Fatalf("%s got a reply on %s, want a refused publish: %s", target.principal, attempt.subject, reply.Data)
					}
					want := fmt.Sprintf("Permissions Violation for Publish to %q", attempt.subject)
					deadline := time.After(2 * time.Second)
				wait:
					for {
						select {
						case e := <-asyncErrs:
							if strings.Contains(e.Error(), want) {
								break wait
							}
						case <-deadline:
							t.Fatalf("%s request on %s failed (%v) with no %s; the refusal is not proven to be the ACL", target.principal, attempt.subject, reqErr, want)
						}
					}

					got := currentConfig()
					if !slices.Equal(got.FilterSubjects, original.FilterSubjects) || got.FilterSubject != original.FilterSubject {
						t.Fatalf("%s filters changed to %v / %q after a refused request, want %v", target.durable, got.FilterSubjects, got.FilterSubject, original.FilterSubjects)
					}
				})
			}
		})
	}
}

// TestNATSInboxWildcardStillExposesOtherPrincipalsDeliveries PINS A GAP; it asserts the gap is
// PRESENT (ADR-072 §6(c)). It is not a security control, and it must not be read as one.
//
// Every service principal subscribes to _INBOX.>, and no service sets a custom inbox prefix, so
// they all share nats.go's default `_INBOX.` namespace. A JetStream pull delivers each message to
// the puller's reply inbox. So any principal can subscribe to _INBOX.> and receive copies of
// another principal's deliveries. TKT-327 closed the body-controlled filter path
// (TestNATSServiceCannotWidenItsOwnPluralDurable); it did not close this one. Executed during
// TKT-327 on an isolated broker with the production ACL: the inventory credential, watching
// _INBOX.>, read access-ticket-issuer's delivery of an order.completed carrying guest_order_ref.
//
// TKT-519 tracks the fix (a per-principal inbox prefix in every service's connection, with each
// principal's subscribe grant narrowed to it). When TKT-519 lands, this test goes red because
// inventory sees nothing; TKT-519 then flips it to assert a REFUSAL (inventory's subscribe on
// another principal's inbox namespace is a Permissions Violation and no delivery arrives) and
// updates ADR-072 §6(c). Until then, do not weaken or delete it.
//
// This test shows the mechanism without touching a service durable: the admin pulls from a
// throwaway durable on a subject no service consumes, and the inventory credential reads the
// delivery from the shared inbox namespace.
func TestNATSInboxWildcardStillExposesOtherPrincipalsDeliveries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	u, err := url.Parse(natsURL)
	if err != nil {
		t.Fatalf("parse natsURL: %v", err)
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	subject := "platform.smoke.inbox-pin." + suffix
	marker := "inbox-pin-marker-" + suffix

	invConn, err := nats.Connect(fmt.Sprintf("nats://inventory:%s@%s", natsPassword("inventory"), u.Host),
		nats.Timeout(3*time.Second), nats.RetryOnFailedConnect(false))
	if err != nil {
		t.Fatalf("inventory connect: %v", err)
	}
	defer invConn.Close()
	seen := make(chan string, 4)
	sub, err := invConn.Subscribe("_INBOX.>", func(m *nats.Msg) {
		if strings.Contains(string(m.Data), marker) {
			select {
			case seen <- m.Subject:
			default:
			}
		}
	})
	if err != nil {
		t.Fatalf("inventory subscribe _INBOX.>: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := invConn.Flush(); err != nil {
		t.Fatalf("inventory flush: %v", err)
	}

	adminConn, err := nats.Connect(natsURL, nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer adminConn.Close()
	adminJS, err := jetstream.New(adminConn)
	if err != nil {
		t.Fatalf("admin jetstream: %v", err)
	}
	stream, err := adminJS.Stream(ctx, "PLATFORM")
	if err != nil {
		t.Fatalf("admin get PLATFORM stream: %v", err)
	}
	durable := "inbox-pin-" + suffix
	cons, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{
		Durable: durable, FilterSubject: subject,
		DeliverPolicy: jetstream.DeliverAllPolicy, AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("create throwaway durable: %v", err)
	}
	t.Cleanup(func() { _ = stream.DeleteConsumer(context.Background(), durable) })
	if _, err := adminJS.Publish(ctx, subject, []byte(`{"marker":"`+marker+`"}`)); err != nil {
		t.Fatalf("publish marker: %v", err)
	}
	batch, err := cons.Fetch(1, jetstream.FetchMaxWait(3*time.Second))
	if err != nil {
		t.Fatalf("admin fetch: %v", err)
	}
	pulled := false
	for m := range batch.Messages() {
		pulled = strings.Contains(string(m.Data()), marker)
		_ = m.Ack()
	}
	if !pulled {
		t.Fatal("the admin pull did not receive the marker; the fixture did not create a delivery to observe")
	}
	select {
	case <-seen:
		// The gap is present, as ADR-072 §6(c) records.
	case <-time.After(3 * time.Second):
		t.Fatal("the inventory credential did not see another connection's JetStream delivery on _INBOX.>; " +
			"if TKT-519 has landed, flip this test to assert a refusal and update ADR-072 §6(c)")
	}
}
