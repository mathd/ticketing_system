//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestAdmittedExchangeRefusesBeforeAnyTargetOrMoneyCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	suffix := admissionSuffix()
	slot, sourceType := publishedSlot(t, "TKT-169 Admission Hall "+suffix, 10)
	target := created(t, gatewayURL+"/api/catalog/ticket-types", map[string]any{
		"performance_id": slot,
		"name":           map[string]string{"fr": "Nouveau", "en": "New"},
		"price":          map[string]any{"amount": 3500, "currency": "EUR"},
	})

	code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", "tkt169-reserve-"+suffix, map[string]any{
		"organizer_id": organizerID, "ticket_type_id": sourceType, "quantity": 1,
	})
	if code != http.StatusCreated {
		t.Fatalf("source reservation %d %s", code, body)
	}
	var reservation map[string]any
	if err := json.Unmarshal(body, &reservation); err != nil {
		t.Fatal(err)
	}
	code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", "tkt169-order-"+suffix, map[string]any{
		"reservation_id": reservation["reservation_id"], "name": "TKT-169 Buyer",
		"email": "tkt169-" + suffix + "@example.test", "payment_token": "fake-ok",
	})
	if code != http.StatusOK {
		t.Fatalf("source checkout %d %s", code, body)
	}
	var source struct {
		OrderID       string `json:"order_id"`
		GuestOrderRef string `json:"guest_order_ref"`
	}
	if err := json.Unmarshal(body, &source); err != nil {
		t.Fatal(err)
	}
	waitForExchangeTickets(t, source.OrderID, 1)
	code, body, _ = getWithHeaders(t, gatewayURL+"/api/access/orders/"+source.GuestOrderRef+"/tickets")
	if code != http.StatusOK {
		t.Fatalf("source tickets %d %s", code, body)
	}
	var bundle struct {
		Tickets []issuedTicket `json:"tickets"`
	}
	if err := json.Unmarshal(body, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Tickets) != 1 {
		t.Fatalf("source tickets = %d, want 1", len(bundle.Tickets))
	}
	if code, body := postWithKey(t, gatewayURL+"/api/access/scans", "tkt169-scan-"+suffix,
		scanBody(bundle.Tickets[0].QRPayload, uuid.NewString(), time.Now().UTC(), "")); code != http.StatusOK {
		t.Fatalf("source scan %d %s", code, body)
	}

	key := "tkt169-admitted-" + suffix
	if code, body := internalJSON(t, http.MethodPost,
		fmt.Sprintf("%s/internal/orders/%s/exchanges", commerceURL, source.OrderID), key,
		map[string]any{"organizer_id": organizerID, "target_ticket_type_id": target["id"],
			"actor": "coverage@example.test", "reason": "source already admitted",
			"payment_token": "fake-ok"}); code != http.StatusConflict {
		t.Fatalf("admitted exchange = %d %s, want 409", code, body)
	}

	exchangeID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("exchange:"+organizerID+":"+key))
	inventory := inventoryAdminConn(t)
	var targetClaims int
	if err := inventory.QueryRow(ctx, `SELECT count(*) FROM claims WHERE organizer_id=$1 AND idempotency_key=$2`, organizerID,
		"exchange-target:"+exchangeID.String()).Scan(&targetClaims); err != nil {
		t.Fatal(err)
	}
	if targetClaims != 0 {
		t.Fatalf("target claims = %d, want none", targetClaims)
	}
	commerce, err := pgx.Connect(ctx, dsn("postgres", "commerce"))
	if err != nil {
		t.Fatalf("connect commerce db: %v", err)
	}
	t.Cleanup(func() { _ = commerce.Close(context.Background()) })
	var exchanges, exchangeFacts int
	if err := commerce.QueryRow(ctx, `SELECT count(*) FROM order_exchanges WHERE organizer_id=$1 AND id=$2`, organizerID, exchangeID).Scan(&exchanges); err != nil {
		t.Fatal(err)
	}
	if err := commerce.QueryRow(ctx, `SELECT count(*) FROM order_facts WHERE organizer_id=$1 AND fact_type IN ('order.exchange.reversed','order.exchange.sold')`, organizerID).Scan(&exchangeFacts); err != nil {
		t.Fatal(err)
	}
	if exchanges != 0 || exchangeFacts != 0 {
		t.Fatalf("exchange rows=%d facts=%d, want none", exchanges, exchangeFacts)
	}
}
