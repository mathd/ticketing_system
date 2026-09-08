//go:build smoke

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// seedPartnerReservationFixture inserts a reservation in 'held' status with partner attribution.
func seedPartnerReservationFixture(t *testing.T, db *sql.DB, ctx context.Context, orgID, resellerID uuid.UUID, channelCode *string, seatIdentities []string, feeSnapshot []byte) uuid.UUID {
	t.Helper()
	resID := uuid.New()
	const faceValue, totalAmount, quantity = 5000, 5600, 1

	var seatsJSON []byte
	if len(seatIdentities) > 0 {
		var err error
		seatsJSON, err = json.Marshal(seatIdentities)
		if err != nil {
			t.Fatal(err)
		}
	}

	var resellerParam any
	if resellerID != uuid.Nil {
		resellerParam = resellerID
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id, organizer_id, hold_id, slot_id, ticket_type_id, buyer_id, quantity,
		                         unit_amount, total_amount, face_value_amount, currency, status,
		                         channel_code, reseller_id, seat_identities, fee_resolution_snapshot)
		VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'EUR', 'held', $11, $12, $13, $14)`,
		resID, orgID, uuid.New(), uuid.New(), uuid.New(), uuid.New(),
		quantity, int64(totalAmount), int64(totalAmount), int64(faceValue), channelCode, resellerParam, seatsJSON, feeSnapshot); err != nil {
		t.Fatalf("seed partner reservation: %v", err)
	}

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM order_facts WHERE organizer_id=$1`, orgID)
		_, _ = db.Exec(`DELETE FROM orders WHERE reservation_id=$1`, resID)
		_, _ = db.Exec(`DELETE FROM reservations WHERE id=$1`, resID)
	})

	return resID
}

// COS 5: Scoped Lookup Query.
// A partner confirm MUST verify (id, organizer_id, channel_code, reseller_id) in ONE SQL query.
// Any mismatch (or an unchannelled public reservation) must return a uniform 404 with no side effects.
func TestPartnerConfirmScopedLookup5CasesUniform404(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	srv := newTestServer(db, http.DefaultClient, "", "", "", "tok")

	orgID := uuid.New()
	resellerID := uuid.New()
	channelCode := "reseller-alpha"
	feeSnap := sampleCommissionSnapshot(channelCode, resellerID, 10000, fmt.Sprintf("reseller:%s", resellerID))

	// Base reservation matching (orgID, resellerID, channelCode)
	resID := seedPartnerReservationFixture(t, db, ctx, orgID, resellerID, &channelCode, nil, feeSnap)

	// Public reservation (NULL reseller_id, NULL channel_code)
	publicResID := seedPartnerReservationFixture(t, db, ctx, orgID, uuid.Nil, nil, nil, feeSnap)

	cases := []struct {
		name      string
		resID     uuid.UUID
		scopeOrg  uuid.UUID
		scopeRes  uuid.UUID
		scopeChan string
	}{
		{
			name:      "wrong organizer",
			resID:     resID,
			scopeOrg:  uuid.New(),
			scopeRes:  resellerID,
			scopeChan: channelCode,
		},
		{
			name:      "wrong channel",
			resID:     resID,
			scopeOrg:  orgID,
			scopeRes:  resellerID,
			scopeChan: "reseller-beta",
		},
		{
			name:      "wrong reseller",
			resID:     resID,
			scopeOrg:  orgID,
			scopeRes:  uuid.New(),
			scopeChan: channelCode,
		},
		{
			name:      "unknown reservation id",
			resID:     uuid.New(),
			scopeOrg:  orgID,
			scopeRes:  resellerID,
			scopeChan: channelCode,
		},
		{
			name:      "public reservation with null reseller attribution",
			resID:     publicResID,
			scopeOrg:  orgID,
			scopeRes:  resellerID,
			scopeChan: channelCode,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"reservation_id":%q,"name":"Buyer","email":"buyer@example.test","payment_token":"tok"}`, tc.resID)
			req := httptest.NewRequest(http.MethodPost, "/partners/orders", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "idemp-"+uuid.NewString())

			scope := &partnerScope{
				CredentialID: uuid.New(),
				ResellerID:   tc.scopeRes,
				OrganizerID:  tc.scopeOrg,
				ChannelCode:  tc.scopeChan,
			}
			req = req.WithContext(context.WithValue(req.Context(), partnerScopeKey{}, scope))

			rec := httptest.NewRecorder()
			srv.partnerConfirm(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("case %q: expected status 404, got %d: %s", tc.name, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "reservation not found") {
				t.Fatalf("case %q: expected uniform 'reservation not found', got %s", tc.name, rec.Body.String())
			}

			// Verify no side effects: zero orders created
			var orderCount int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM orders WHERE reservation_id=$1`, tc.resID).Scan(&orderCount); err != nil {
				t.Fatalf("count orders: %v", err)
			}
			if orderCount != 0 {
				t.Fatalf("case %q: expected 0 orders created, got %d", tc.name, orderCount)
			}
		})
	}
}

// COS 6: Seated Reservation Unsupported.
// If a partner attempts to confirm a seated reservation, the handler refuses with
// 409 Conflict and machine-readable code seated_pool_unsupported.
func TestPartnerConfirmSeatedReservationRefusedWith409(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	srv := newTestServer(db, http.DefaultClient, "", "", "", "tok")

	orgID := uuid.New()
	resellerID := uuid.New()
	channelCode := "reseller-seated"
	feeSnap := sampleCommissionSnapshot(channelCode, resellerID, 10000, fmt.Sprintf("reseller:%s", resellerID))

	resID := seedPartnerReservationFixture(t, db, ctx, orgID, resellerID, &channelCode, []string{"SEC-A-1"}, feeSnap)

	body := fmt.Sprintf(`{"reservation_id":%q,"name":"Seated Buyer","email":"seated@example.test","payment_token":"tok"}`, resID)
	req := httptest.NewRequest(http.MethodPost, "/partners/orders", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "idemp-seated-"+uuid.NewString())

	scope := &partnerScope{
		CredentialID: uuid.New(),
		ResellerID:   resellerID,
		OrganizerID:  orgID,
		ChannelCode:  channelCode,
	}
	req = req.WithContext(context.WithValue(req.Context(), partnerScopeKey{}, scope))

	rec := httptest.NewRecorder()
	srv.partnerConfirm(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status 409, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}

	if errResp.Code != string(SeatedPoolUnsupported) {
		t.Fatalf("expected code %q, got %q", SeatedPoolUnsupported, errResp.Code)
	}
	if !strings.Contains(errResp.Error, "seated") {
		t.Fatalf("expected error message mentioning seated, got %q", errResp.Error)
	}
}

// Public Checkout Restriction:
// A partner reservation (reseller_id IS NOT NULL) cannot be completed through the public POST /orders.
func TestPublicCheckoutRefusesPartnerReservation(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	srv := newTestServer(db, http.DefaultClient, "", "", "", "tok")

	orgID := uuid.New()
	resellerID := uuid.New()
	channelCode := "reseller-public-attempt"
	feeSnap := sampleCommissionSnapshot(channelCode, resellerID, 10000, fmt.Sprintf("reseller:%s", resellerID))

	resID := seedPartnerReservationFixture(t, db, ctx, orgID, resellerID, &channelCode, nil, feeSnap)

	// Attempt checkout through public endpoint (s.checkout)
	body := fmt.Sprintf(`{"reservation_id":%q,"name":"Public Attempter","email":"attempter@example.test","payment_token":"tok"}`, resID)
	req := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "idemp-public-"+uuid.NewString())

	rec := httptest.NewRecorder()
	srv.checkout(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status 404 for public checkout of partner reservation, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reservation not found") {
		t.Fatalf("expected 'reservation not found', got %s", rec.Body.String())
	}
}
