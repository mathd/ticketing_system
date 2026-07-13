package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Server struct {
	db                                           *sql.DB
	client                                       *http.Client
	catalogURL, inventoryURL, paymentsURL, token string
}

func New(db *sql.DB, client *http.Client, catalog, inventory, payments, token string) *Server {
	return &Server{db: db, client: client, catalogURL: strings.TrimSuffix(catalog, "/"), inventoryURL: strings.TrimSuffix(inventory, "/"), paymentsURL: strings.TrimSuffix(payments, "/"), token: token}
}
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Post("/reservations", s.reserve)
	r.Post("/orders", s.checkout)
	r.Get("/orders/{id}", s.getOrder)
	return r
}
func write(w http.ResponseWriter, c int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(c)
	_ = json.NewEncoder(w).Encode(v)
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		write(w, 400, map[string]string{"error": "invalid body"})
		return false
	}
	return true
}
func (s *Server) call(ctx context.Context, method, url, key string, in any, internal bool) (int, []byte, error) {
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return 0, nil, e
		}
		body = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, url, body)
	if e != nil {
		return 0, nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if internal {
		req.Header.Set("X-Internal-Token", s.token)
	}
	resp, e := s.client.Do(req)
	if e != nil {
		return 0, nil, e
	}
	defer func() { _ = resp.Body.Close() }()
	out, e := io.ReadAll(resp.Body)
	return resp.StatusCode, out, e
}

type price struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}
type offer struct {
	ID            uuid.UUID `json:"id"`
	OrganizerID   uuid.UUID `json:"organizer_id"`
	PerformanceID uuid.UUID `json:"performance_id"`
	Price         price     `json:"price"`
}
type reserveRequest struct {
	OrganizerID, TicketTypeID uuid.UUID `json:"-"`
	Quantity                  int32     `json:"quantity"`
	IdempotencyKey            string    `json:"-"`
}

func (r *reserveRequest) UnmarshalJSON(b []byte) error {
	var x struct {
		OrganizerID  uuid.UUID `json:"organizer_id"`
		TicketTypeID uuid.UUID `json:"ticket_type_id"`
		Quantity     int32     `json:"quantity"`
	}
	if e := json.Unmarshal(b, &x); e != nil {
		return e
	}
	r.OrganizerID = x.OrganizerID
	r.TicketTypeID = x.TicketTypeID
	r.Quantity = x.Quantity
	return nil
}
func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	var in reserveRequest
	if !decode(w, r, &in) {
		return
	}
	if in.OrganizerID == uuid.Nil || in.TicketTypeID == uuid.Nil || in.Quantity < 1 || in.Quantity > 50 {
		write(w, 400, map[string]string{"error": "invalid reservation"})
		return
	}
	code, body, err := s.call(r.Context(), http.MethodGet, s.catalogURL+"/internal/ticket-types/"+in.TicketTypeID.String(), "", nil, false)
	if err != nil || code != 200 {
		write(w, 502, map[string]string{"error": "catalog unavailable"})
		return
	}
	var o offer
	if json.Unmarshal(body, &o) != nil || o.OrganizerID != in.OrganizerID || o.Price.Currency != "EUR" || o.Price.Amount < 0 || o.Price.Amount > math.MaxInt64/int64(in.Quantity) {
		write(w, 409, map[string]string{"error": "offer not sellable in EUR"})
		return
	}
	total := o.Price.Amount * int64(in.Quantity)
	holdBody := map[string]any{"organizer_id": in.OrganizerID, "slot_id": o.PerformanceID, "ticket_type_id": in.TicketTypeID, "quantity": in.Quantity, "unit_amount": o.Price.Amount, "currency": o.Price.Currency}
	code, body, err = s.call(r.Context(), http.MethodPost, s.inventoryURL+"/holds", key, holdBody, false)
	if err != nil || (code != 200 && code != 201) {
		write(w, 409, map[string]string{"error": "inventory unavailable"})
		return
	}
	var hold struct {
		ID         uuid.UUID `json:"hold_id"`
		ExpiresAt  time.Time `json:"expires_at"`
		ServerTime time.Time `json:"server_time"`
	}
	if json.Unmarshal(body, &hold) != nil {
		write(w, 502, map[string]string{"error": "invalid inventory response"})
		return
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("reservation:"+in.OrganizerID.String()+":"+key))
	buyer := uuid.NewSHA1(uuid.NameSpaceOID, []byte("buyer:"+id.String()))
	_, err = s.db.ExecContext(r.Context(), `INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,currency,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'held') ON CONFLICT(id) DO NOTHING`, id, in.OrganizerID, hold.ID, o.PerformanceID, in.TicketTypeID, buyer, in.Quantity, o.Price.Amount, total, o.Price.Currency)
	if err != nil {
		write(w, 500, map[string]string{"error": "persist reservation"})
		return
	}
	write(w, 201, map[string]any{"reservation_id": id, "hold_id": hold.ID, "buyer_id": buyer, "amount": total, "currency": o.Price.Currency, "expires_at": hold.ExpiresAt, "server_time": hold.ServerTime})
}

type checkoutRequest struct {
	ReservationID uuid.UUID `json:"reservation_id"`
	Name          string    `json:"name"`
	Email         string    `json:"email"`
	PaymentToken  string    `json:"payment_token"`
}
type reservation struct {
	ID, OrganizerID, HoldID, BuyerID uuid.UUID
	Amount                           int64
	Currency, Status                 string
}

func (s *Server) load(ctx context.Context, id uuid.UUID) (reservation, error) {
	var x reservation
	err := s.db.QueryRowContext(ctx, `SELECT id,organizer_id,hold_id,buyer_id,total_amount,currency,status FROM reservations WHERE id=$1`, id).Scan(&x.ID, &x.OrganizerID, &x.HoldID, &x.BuyerID, &x.Amount, &x.Currency, &x.Status)
	return x, err
}
func (s *Server) fact(ctx context.Context, x reservation, order uuid.UUID, typ string) error {
	fid := uuid.NewSHA1(uuid.NameSpaceOID, []byte(order.String()+":"+typ))
	_, e := s.db.ExecContext(ctx, `INSERT INTO order_facts(fact_id,order_id,organizer_id,buyer_id,fact_type,amount,currency) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, fid, order, x.OrganizerID, x.BuyerID, typ, x.Amount, x.Currency)
	if e != nil {
		return e
	}
	code, _, e := s.call(ctx, http.MethodPost, s.paymentsURL+"/internal/facts", "", map[string]any{"fact_id": fid, "organizer_id": x.OrganizerID, "fact_type": typ, "buyer_id": x.BuyerID, "amount": x.Amount, "currency": x.Currency, "occurred_at": time.Now().UTC(), "payload": map[string]string{"order_id": order.String()}}, true)
	if e != nil || code != 200 {
		return errors.New("journal unavailable")
	}
	return nil
}
func (s *Server) checkout(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	var in checkoutRequest
	if !decode(w, r, &in) {
		return
	}
	if in.ReservationID == uuid.Nil || strings.TrimSpace(in.Name) == "" || !strings.Contains(in.Email, "@") || in.PaymentToken == "" {
		write(w, 400, map[string]string{"error": "invalid checkout"})
		return
	}
	x, err := s.load(r.Context(), in.ReservationID)
	if err != nil {
		write(w, 404, map[string]string{"error": "reservation not found"})
		return
	}
	order := uuid.NewSHA1(uuid.NameSpaceOID, []byte("order:"+x.OrganizerID.String()+":"+key))
	if x.Status == "completed" {
		write(w, 200, map[string]any{"order_id": order, "status": "completed"})
		return
	}
	_, _ = s.db.ExecContext(r.Context(), `INSERT INTO buyer_pii(buyer_id,name,email) VALUES($1,$2,$3) ON CONFLICT(buyer_id) DO UPDATE SET name=EXCLUDED.name,email=EXCLUDED.email`, x.BuyerID, in.Name, in.Email)
	_, _ = s.db.ExecContext(r.Context(), `INSERT INTO orders(id,reservation_id,status) VALUES($1,$2,'created') ON CONFLICT DO NOTHING`, order, x.ID)
	if err := s.fact(r.Context(), x, order, "order.created"); err != nil {
		write(w, 503, map[string]string{"error": "journal unavailable"})
		return
	}
	code, _, err := s.call(r.Context(), http.MethodPost, fmt.Sprintf("%s/holds/%s/finalize?organizer_id=%s", s.inventoryURL, x.HoldID, x.OrganizerID), "", nil, false)
	if err != nil || code != 200 {
		write(w, 409, map[string]string{"error": "hold expired"})
		return
	}
	_, _ = s.db.ExecContext(r.Context(), `UPDATE reservations SET status='finalizing' WHERE id=$1`, x.ID)
	charge := map[string]any{"order_id": order, "organizer_id": x.OrganizerID, "buyer_id": x.BuyerID, "amount": x.Amount, "currency": x.Currency, "payment_token": in.PaymentToken}
	code, body, err := s.call(r.Context(), http.MethodPost, s.paymentsURL+"/internal/charges", key, charge, true)
	if err != nil {
		_, _ = s.db.ExecContext(r.Context(), `UPDATE reservations SET status='unknown' WHERE id=$1`, x.ID)
		write(w, 202, map[string]any{"order_id": order, "status": "payment_unknown"})
		return
	}
	if code == 402 || code == 408 {
		_, _, _ = s.call(r.Context(), http.MethodPost, fmt.Sprintf("%s/holds/%s/release?organizer_id=%s", s.inventoryURL, x.HoldID, x.OrganizerID), "", nil, false)
		_, _ = s.db.ExecContext(r.Context(), `UPDATE reservations SET status='failed' WHERE id=$1`, x.ID)
		_ = s.fact(r.Context(), x, order, "order.failed")
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		out["order_id"] = order
		write(w, code, out)
		return
	}
	if code != 200 {
		write(w, 202, map[string]any{"order_id": order, "status": "payment_unknown"})
		return
	}
	code, _, err = s.call(r.Context(), http.MethodPost, fmt.Sprintf("%s/holds/%s/confirm?organizer_id=%s", s.inventoryURL, x.HoldID, x.OrganizerID), "", nil, false)
	if err != nil || code != 200 {
		write(w, 202, map[string]any{"order_id": order, "status": "confirmation_pending"})
		return
	}
	_, _ = s.db.ExecContext(r.Context(), `UPDATE reservations SET status='completed' WHERE id=$1`, x.ID)
	_, _ = s.db.ExecContext(r.Context(), `UPDATE orders SET status='completed',updated_at=now() WHERE id=$1`, order)
	_ = s.fact(r.Context(), x, order, "order.completed")
	write(w, 200, map[string]any{"order_id": order, "status": "completed"})
}
func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	id, e := uuid.Parse(chi.URLParam(r, "id"))
	if e != nil {
		write(w, 400, map[string]string{"error": "invalid order id"})
		return
	}
	var status string
	e = s.db.QueryRowContext(r.Context(), `SELECT status FROM orders WHERE id=$1`, id).Scan(&status)
	if e != nil {
		write(w, 404, map[string]string{"error": "not found"})
		return
	}
	write(w, 200, map[string]any{"order_id": id, "status": status})
}
