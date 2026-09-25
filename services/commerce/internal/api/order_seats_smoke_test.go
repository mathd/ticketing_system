//go:build smoke

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestInternalOrderSeatsScopedAndDistinguishesUnseated(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	org, other := uuid.New(), uuid.New()
	seatedOrder, unseatedOrder := uuid.New(), uuid.New()
	seatedReservation, unseatedReservation := uuid.New(), uuid.New()
	for _, row := range []struct {
		order, reservation uuid.UUID
		seats              any
	}{{seatedOrder, seatedReservation, []byte(`["Stalls/A/1","Stalls/A/2"]`)}, {unseatedOrder, unseatedReservation, nil}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO reservations
			(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status,seat_identities)
			VALUES($1,$2,$3,$4,$5,$6,2,100,200,200,'EUR','completed',$7)`,
			row.reservation, org, uuid.New(), uuid.New(), uuid.New(), uuid.New(), row.seats); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
			VALUES($1,$2,'completed',$3,'fingerprint')`, row.order, row.reservation, "seats-api-"+uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}

	const token = "internal-seat-read-token"
	handler := New(ServerConfig{DB: db, InternalToken: token}).Router(nil, true)
	request := func(order, organizer uuid.UUID, credential string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/internal/orders/%s/seats?organizer_id=%s", order, organizer), nil)
		if credential != "" {
			req.Header.Set("X-Internal-Token", credential)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}

	if res := request(seatedOrder, org, "wrong"); res.Code != http.StatusNotFound {
		t.Fatalf("wrong token status = %d body=%s; want 404", res.Code, res.Body.String())
	}
	if res := request(seatedOrder, other, token); res.Code != http.StatusNotFound {
		t.Fatalf("foreign organizer status = %d body=%s; want 404", res.Code, res.Body.String())
	}
	if res := request(uuid.New(), org, token); res.Code != http.StatusNotFound {
		t.Fatalf("absent order status = %d body=%s; want 404", res.Code, res.Body.String())
	}
	res := request(seatedOrder, org, token)
	if res.Code != http.StatusOK {
		t.Fatalf("seated order status = %d body=%s; want 200", res.Code, res.Body.String())
	}
	var seated struct {
		OrderID        string   `json:"order_id"`
		OrganizerID    string   `json:"organizer_id"`
		SlotID         string   `json:"slot_id"`
		TicketTypeID   string   `json:"ticket_type_id"`
		Quantity       int      `json:"quantity"`
		Seated         bool     `json:"seated"`
		SeatIdentities []string `json:"seat_identities"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &seated); err != nil {
		t.Fatal(err)
	}
	if seated.OrderID != seatedOrder.String() || seated.OrganizerID != org.String() || seated.SlotID == "" || seated.TicketTypeID == "" ||
		seated.Quantity != 2 || !seated.Seated || len(seated.SeatIdentities) != 2 || seated.SeatIdentities[0] != "Stalls/A/1" || seated.SeatIdentities[1] != "Stalls/A/2" {
		t.Fatalf("seated response = %+v", seated)
	}
	res = request(unseatedOrder, org, token)
	if res.Code != http.StatusOK {
		t.Fatalf("unseated order status = %d body=%s; want 200", res.Code, res.Body.String())
	}
	if err := json.Unmarshal(res.Body.Bytes(), &seated); err != nil {
		t.Fatal(err)
	}
	if seated.Seated || seated.SeatIdentities == nil || len(seated.SeatIdentities) != 0 {
		t.Fatalf("unseated response = %+v; want false and []", seated)
	}
}
