//go:build smoke

package consumer

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/access/internal/lifecycle"
	"ticketing/services/access/internal/store"
	"ticketing/services/access/internal/ticket"
)

func issuanceDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("ACCESS_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ACCESS_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := "access_issuance_" + uuid.NewString()[:8]
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate access issuance database: %v", err)
	}
	return db, ctx
}

func issuanceConfig(t *testing.T) store.Config {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := lifecycle.NewSigner(base64.RawStdEncoding.EncodeToString(priv.Seed()), "access-lifecycle/issuance-test")
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := lifecycle.NewKeyring("access-lifecycle/issuance-test=" + base64.RawStdEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	return store.Config{Signer: signer, Keyring: keyring, Policy: store.DefaultPolicy()}
}

func TestCompletedSeatedOrderAssignsExactSeats(t *testing.T) {
	db, ctx := issuanceDB(t)
	event := issuanceEvent()
	identities := []string{"Stalls/A/2", "Stalls/A/1"}
	var requestCount int
	commerce := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Method != http.MethodGet || r.URL.Path != "/internal/orders/"+event.Data.OrderID.String()+"/seats" ||
			r.URL.Query().Get("organizer_id") != event.Data.OrganizerID.String() || r.Header.Get("X-Internal-Token") != "internal" {
			t.Errorf("commerce read request = %s %s token=%q", r.Method, r.URL, r.Header.Get("X-Internal-Token"))
		}
		_ = json.NewEncoder(w).Encode(orderSeats{
			OrderID: event.Data.OrderID, OrganizerID: event.Data.OrganizerID,
			SlotID: event.Data.SlotID, TicketTypeID: event.Data.TicketTypeID,
			Quantity: 2, Seated: true, SeatIdentities: identities,
		})
	}))
	defer commerce.Close()
	st := store.New(db, issuanceConfig(t))
	qr, err := ticket.New(base64.RawStdEncoding.EncodeToString(make([]byte, ed25519.SeedSize)), "ticket/test")
	if err != nil {
		t.Fatal(err)
	}
	c := &Consumer{st: st, signer: qr, client: commerce.Client(), commerceURL: commerce.URL, token: "internal"}
	if err := c.issue(ctx, event); err != nil {
		t.Fatal(err)
	}
	if requestCount != 1 {
		t.Fatalf("commerce read count = %d, want 1", requestCount)
	}
	for ordinal, want := range []string{"Stalls/A/1", "Stalls/A/2"} {
		ticketID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(event.ID.String()+fmt.Sprintf(":%d", ordinal)))
		var got *string
		if err := db.QueryRowContext(ctx, `SELECT seat_identity FROM tickets WHERE id=$1 AND order_id=$2`, ticketID, event.Data.OrderID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == nil || *got != want {
			t.Fatalf("ticket ordinal %d seat identity = %v, want %q", ordinal, got, want)
		}
	}
	if n := countIssuanceRows(t, ctx, db, `SELECT count(*) FROM consumed_events WHERE event_id=$1`, event.ID); n != 1 {
		t.Fatalf("consumed event rows = %d, want 1", n)
	}
	if n := countIssuanceRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events l JOIN tickets t ON t.id=l.ticket_id WHERE t.order_id=$1 AND l.event_type='issued'`, event.Data.OrderID); n != 2 {
		t.Fatalf("issued lifecycle rows = %d, want 2", n)
	}
}

func TestCompletedGAOrderIssuesWithNullSeatIdentity(t *testing.T) {
	db, ctx := issuanceDB(t)
	event := issuanceEvent()
	var requestCount int
	commerce := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Method != http.MethodGet || r.URL.Path != "/internal/orders/"+event.Data.OrderID.String()+"/seats" ||
			r.URL.Query().Get("organizer_id") != event.Data.OrganizerID.String() || r.Header.Get("X-Internal-Token") != "internal" {
			t.Errorf("commerce read request = %s %s token=%q", r.Method, r.URL, r.Header.Get("X-Internal-Token"))
		}
		_ = json.NewEncoder(w).Encode(orderSeats{
			OrderID: event.Data.OrderID, OrganizerID: event.Data.OrganizerID,
			SlotID: event.Data.SlotID, TicketTypeID: event.Data.TicketTypeID,
			Quantity: int(event.Data.Quantity), Seated: false, SeatIdentities: []string{},
		})
	}))
	defer commerce.Close()
	st := store.New(db, issuanceConfig(t))
	qr, err := ticket.New(base64.RawStdEncoding.EncodeToString(make([]byte, ed25519.SeedSize)), "ticket/test")
	if err != nil {
		t.Fatal(err)
	}
	c := &Consumer{st: st, signer: qr, client: commerce.Client(), commerceURL: commerce.URL, token: "internal"}
	if err := c.issue(ctx, event); err != nil {
		t.Fatalf("issue GA order: %v", err)
	}
	if requestCount != 1 {
		t.Fatalf("commerce read count = %d, want 1", requestCount)
	}
	rows, err := db.QueryContext(ctx, `SELECT seat_identity FROM tickets WHERE order_id=$1`, event.Data.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ticketCount int
	for rows.Next() {
		var seatIdentity sql.NullString
		if err := rows.Scan(&seatIdentity); err != nil {
			t.Fatal(err)
		}
		ticketCount++
		if seatIdentity.Valid {
			t.Errorf("GA ticket %d seat identity = %q, want NULL", ticketCount, seatIdentity.String)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if ticketCount != int(event.Data.Quantity) {
		t.Fatalf("ticket rows = %d, want %d", ticketCount, event.Data.Quantity)
	}
	if n := countIssuanceRows(t, ctx, db, `SELECT count(*) FROM consumed_events WHERE event_id=$1`, event.ID); n != 1 {
		t.Fatalf("consumed event rows = %d, want 1", n)
	}
	if n := countIssuanceRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events l JOIN tickets t ON t.id=l.ticket_id WHERE t.order_id=$1 AND l.event_type='issued'`, event.Data.OrderID); n != int(event.Data.Quantity) {
		t.Fatalf("issued lifecycle rows = %d, want %d", n, event.Data.Quantity)
	}
}

func TestCompletedSeatedOrderPersists200CharacterMultibyteSeat(t *testing.T) {
	db, ctx := issuanceDB(t)
	event := issuanceEvent()
	event.Data.Quantity = 1
	want := strings.Repeat("🎟", 200)
	commerce := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(orderSeats{
			OrderID: event.Data.OrderID, OrganizerID: event.Data.OrganizerID,
			SlotID: event.Data.SlotID, TicketTypeID: event.Data.TicketTypeID,
			Quantity: 1, Seated: true, SeatIdentities: []string{want},
		})
	}))
	defer commerce.Close()
	qr, err := ticket.New(base64.RawStdEncoding.EncodeToString(make([]byte, ed25519.SeedSize)), "ticket/test")
	if err != nil {
		t.Fatal(err)
	}
	c := &Consumer{st: store.New(db, issuanceConfig(t)), signer: qr, client: commerce.Client(), commerceURL: commerce.URL, token: "internal"}
	if err := c.issue(ctx, event); err != nil {
		t.Fatalf("issue seated order: %v", err)
	}
	ticketID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(event.ID.String()+":0"))
	var got *string
	if err := db.QueryRowContext(ctx, `SELECT seat_identity FROM tickets WHERE id=$1 AND order_id=$2`, ticketID, event.Data.OrderID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != want {
		t.Fatalf("ticket seat identity = %v, want exact 200-character identity", got)
	}
}

func TestConsumedCompletedOrderSkipsSeatReadOnRedelivery(t *testing.T) {
	db, ctx := issuanceDB(t)
	event := issuanceEvent()
	var seatReads atomic.Int32
	commerce := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if seatReads.Add(1) > 1 {
			http.Error(w, "seat service unavailable", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(orderSeats{
			OrderID: event.Data.OrderID, OrganizerID: event.Data.OrganizerID,
			SlotID: event.Data.SlotID, TicketTypeID: event.Data.TicketTypeID,
			Quantity: int(event.Data.Quantity), Seated: true, SeatIdentities: []string{"Stalls/A/1", "Stalls/A/2"},
		})
	}))
	defer commerce.Close()
	qr, err := ticket.New(base64.RawStdEncoding.EncodeToString(make([]byte, ed25519.SeedSize)), "ticket/test")
	if err != nil {
		t.Fatal(err)
	}
	c := &Consumer{st: store.New(db, issuanceConfig(t)), signer: qr, client: commerce.Client(), commerceURL: commerce.URL, token: "internal"}
	readTicketRows := func() []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT id::text, order_id::text, guest_order_ref::text,
			organizer_id::text, buyer_id::text, slot_id::text, ticket_type_id::text,
			seat_identity, qr_payload, issued_at::text FROM tickets WHERE order_id=$1 ORDER BY id`, event.Data.OrderID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var snapshot []string
		for rows.Next() {
			var id, orderID, guestRef, organizerID, buyerID, slotID, ticketTypeID, payload, issuedAt string
			var seatIdentity sql.NullString
			if err := rows.Scan(&id, &orderID, &guestRef, &organizerID, &buyerID, &slotID, &ticketTypeID, &seatIdentity, &payload, &issuedAt); err != nil {
				t.Fatal(err)
			}
			snapshot = append(snapshot, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%t:%s|%s|%s", id, orderID, guestRef,
				organizerID, buyerID, slotID, ticketTypeID, seatIdentity.Valid, seatIdentity.String, payload, issuedAt))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	if err := c.issue(ctx, event); err != nil {
		t.Fatalf("first issue: %v", err)
	}
	before := readTicketRows()
	if len(before) != int(event.Data.Quantity) {
		t.Fatalf("ticket rows after first issue = %d, want %d", len(before), event.Data.Quantity)
	}
	if err := c.issue(ctx, event); err != nil {
		t.Fatalf("redelivered issue = %v, want nil", err)
	}
	after := readTicketRows()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("ticket rows changed on redelivery:\nbefore: %v\nafter:  %v", before, after)
	}
	if got := seatReads.Load(); got != 1 {
		t.Fatalf("commerce seat reads = %d, want exactly 1", got)
	}
}

func TestSeatCountMismatchIssuesNothing(t *testing.T) {
	db, ctx := issuanceDB(t)
	event := issuanceEvent()
	commerce := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(orderSeats{
			OrderID: event.Data.OrderID, OrganizerID: event.Data.OrganizerID,
			SlotID: event.Data.SlotID, TicketTypeID: event.Data.TicketTypeID,
			Quantity: 2, Seated: true, SeatIdentities: []string{"Stalls/A/1"},
		})
	}))
	defer commerce.Close()
	qr, err := ticket.New(base64.RawStdEncoding.EncodeToString(make([]byte, ed25519.SeedSize)), "ticket/test")
	if err != nil {
		t.Fatal(err)
	}
	c := &Consumer{st: store.New(db, issuanceConfig(t)), signer: qr, client: commerce.Client(), commerceURL: commerce.URL, token: "internal"}
	if err := c.issue(ctx, event); err == nil {
		t.Fatal("seat count mismatch issued tickets")
	}
	if n := countIssuanceRows(t, ctx, db, `SELECT count(*) FROM tickets WHERE order_id=$1`, event.Data.OrderID); n != 0 {
		t.Fatalf("tickets after mismatch = %d, want 0", n)
	}
	if n := countIssuanceRows(t, ctx, db, `SELECT count(*) FROM consumed_events WHERE event_id=$1`, event.ID); n != 0 {
		t.Fatalf("consumed event rows after mismatch = %d, want 0", n)
	}
}

func issuanceEvent() completed {
	return completed{ID: uuid.New(), Data: completedData{
		OrderID: uuid.New(), GuestOrderRef: uuid.New(), OrganizerID: uuid.New(),
		BuyerID: uuid.New(), SlotID: uuid.New(), TicketTypeID: uuid.New(), Quantity: 2,
	}}
}

func countIssuanceRows(t *testing.T, ctx context.Context, db *sql.DB, query string, id uuid.UUID) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, query, id).Scan(&count); err != nil {
		t.Fatal(fmt.Errorf("count issuance rows: %w", err))
	}
	return count
}
