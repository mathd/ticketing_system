//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
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

// TestNATSResidualCredentialedForgeryStillMintsTickets pins both the narrowed and still-open
// parts of ADR-072 §6(b): an unknown order now fails the seat read, but commerce's NATS
// credentials can still replay the exact line of a real order under a new event ID.
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

// TestNATSOperatorCredentialIsConfinedToTheBroker asserts the ticket's CENTRAL claim: the
// inventory-reprocess operator password reaches the nats container and NOTHING else. In
// particular it must never enter the long-running inventory server's environment.
//
// Why this test exists (ai-review F3). Every other test here is a NEGATIVE about the
// `inventory` principal: they prove that credential cannot publish on catalog's prefix. None of
// them observes the operator credential at all, so adding NATS_INVENTORY_REPROCESS_PASSWORD to
// compose's shared &go-env anchor would leave this entire file green while voiding the
// separation the ADR claims. A mitigation whose failure no test can see is a claim, not a
// control.
//
// It asserts on the ENVIRONMENT rather than on a refusal because that is where the property
// lives. The broker cannot tell us who else holds a password; only the container's environment
// can. Per ADR-021: state inside a system cannot constrain what is handed to it from outside.
func TestNATSOperatorCredentialIsConfinedToTheBroker(t *testing.T) {
	ctx := context.Background()
	const operatorVar = "NATS_INVENTORY_REPROCESS_PASSWORD"

	envOf := func(service string) string {
		t.Helper()
		container := fmt.Sprintf("%s-%s-1", project, service)
		out, err := dockerRun(ctx, "inspect", "-f", "{{range .Config.Env}}{{println .}}{{end}}", container)
		if err != nil {
			t.Fatalf("docker inspect %s: %v: %s", container, err, out)
		}
		return out
	}

	// The operator password must be absent from every long-running service, by NAME and by
	// VALUE. The name check catches the obvious regression (someone adds the variable to the
	// shared anchor); the value check catches it arriving under a different name, e.g. folded
	// into a NATS_URL.
	// FAIL rather than skip when the value is unavailable. A `!= ""` guard here would let the
	// value check silently become a no-op in any environment that does not export the password,
	// leaving a test that still passes while performing half the job it documents — the
	// green-test-that-cannot-reach-the-failing-state shape. If this fires, the harness is
	// misconfigured (scripts/smoke.sh exports it), not the code under test.
	operatorPassword := strings.TrimSpace(os.Getenv(operatorVar))
	if operatorPassword == "" {
		t.Fatalf("%s is not set in the test environment, so the value check cannot run; "+
			"scripts/smoke.sh must export it (a name-only check would pass while a renamed "+
			"copy of the credential sat in a service's environment)", operatorVar)
	}
	// Sweep every container the project has INSTANTIATED, not a hand-listed subset. A fixed list
	// of the five Go services would pass while the credential sat in the gateway, a web app, or a
	// one-shot job (ai-review pass 2), and the property is "nothing but the broker holds it", so
	// the target set is discovered rather than enumerated.
	//
	// The bound is honest: `compose ps --all` reports containers that EXIST, so a service behind
	// an inactive profile, or one never started, is not covered (ai-review pass 3). Within the
	// gate that is the whole stack, and an inspect failure now fails the test rather than
	// silently reducing the sweep.
	listed, err := dockerRun(ctx, "compose", "-p", project, "ps", "--all", "--format", "{{.Name}}")
	if err != nil {
		t.Fatalf("docker compose ps: %v: %s", err, listed)
	}
	containers := strings.Fields(listed)
	if len(containers) < 5 {
		t.Fatalf("found only %d containers in project %s; the sweep below would prove almost nothing", len(containers), project)
	}
	broker := fmt.Sprintf("%s-nats-1", project)
	swept := 0
	for _, container := range containers {
		if container == broker {
			continue // the broker is the one component that MUST hold it
		}
		env, err := dockerRun(ctx, "inspect", "-f", "{{range .Config.Env}}{{println .}}{{end}}", container)
		if err != nil {
			// FAIL rather than skip. A one-shot job is exactly the kind of container that could
			// receive the credential and then exit, and a `continue` here would let the sweep
			// report success having never looked at it (ai-review pass 3).
			t.Errorf("cannot inspect %s, so its environment is unchecked: %v: %s", container, err, env)
			continue
		}
		swept++
		if strings.Contains(env, operatorVar+"=") {
			t.Errorf("%s holds %s; the operator credential must reach only %s", container, operatorVar, broker)
		}
		if strings.Contains(env, operatorPassword) {
			t.Errorf("%s holds the operator password VALUE; it must reach only %s", container, broker)
		}
	}
	if swept == 0 {
		t.Fatalf("swept no containers; the assertions above proved nothing")
	}
	t.Logf("swept %d containers (excluding %s)", swept, broker)

	// And the inventory server must hold its OWN credential, so the check above is not passing
	// merely because the service has no NATS credential at all.
	inventoryEnv := envOf("inventory")
	if !strings.Contains(inventoryEnv, "nats://inventory:") {
		t.Errorf("inventory does not carry its own `inventory` principal; the absence assertions above prove nothing")
	}

	// The broker DOES need it: it is the component that enforces the identity. If this fails,
	// the operator credential is not configured anywhere and the recovery path is dead.
	if natsEnv := envOf("nats"); !strings.Contains(natsEnv, operatorVar+"=") {
		t.Errorf("the nats container lacks %s; the operator identity cannot be enforced", operatorVar)
	}
}
