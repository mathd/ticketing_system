package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"ticketing/services/payments/internal/store"
)

const (
	TokenSuccess = "fake-ok"
	TokenDecline = "fake-decline"
	TokenTimeout = "fake-timeout"
)

type Server struct {
	journal    *store.Journal
	credential string
}

func New(j *store.Journal, credential string) *Server {
	return &Server{journal: j, credential: credential}
}
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Post("/internal/facts", s.fact)
	r.Post("/internal/charges", s.charge)
	return r
}
func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) authorized(r *http.Request) bool {
	return s.credential != "" && r.Header.Get("X-Internal-Token") == s.credential
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
func (s *Server) fact(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		write(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var f store.Fact
	if !decode(w, r, &f) {
		return
	}
	e, replay, err := s.journal.Append(r.Context(), f)
	if err != nil {
		write(w, 400, map[string]string{"error": err.Error()})
		return
	}
	write(w, 200, map[string]any{"sequence": e.Sequence, "hash": store.Hex(e.EntryHash), "replay": replay})
}

type chargeRequest struct {
	OrderID      uuid.UUID `json:"order_id"`
	OrganizerID  uuid.UUID `json:"organizer_id"`
	BuyerID      uuid.UUID `json:"buyer_id"`
	Amount       int64     `json:"amount"`
	Currency     string    `json:"currency"`
	PaymentToken string    `json:"payment_token"`
}

func (s *Server) charge(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		write(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	var in chargeRequest
	if !decode(w, r, &in) {
		return
	}
	if in.OrderID == uuid.Nil || in.OrganizerID == uuid.Nil || in.BuyerID == uuid.Nil || in.Amount < 0 || in.Currency != "EUR" {
		write(w, 400, map[string]string{"error": "invalid charge"})
		return
	}
	status := "captured"
	factType := "payment.captured"
	code := 200
	switch in.PaymentToken {
	case TokenDecline:
		status = "declined"
		factType = "payment.declined"
		code = 402
	case TokenTimeout:
		status = "timeout"
		factType = "payment.timeout"
		code = 408
	case TokenSuccess:
		authorizedID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("payment:"+key+":payment.authorized"))
		if _, _, err := s.journal.Append(r.Context(), store.Fact{ID: authorizedID, OrganizerID: in.OrganizerID, Type: "payment.authorized", OccurredAt: time.Now().UTC(), BuyerID: in.BuyerID, Amount: in.Amount, Currency: in.Currency, Payload: map[string]string{"order_id": in.OrderID.String()}}); err != nil {
			write(w, 500, map[string]string{"error": "journal append failed"})
			return
		}
	default:
		write(w, 400, map[string]string{"error": "unknown fake payment token"})
		return
	}
	factID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("payment:"+key+":"+factType))
	e, replay, err := s.journal.Append(r.Context(), store.Fact{ID: factID, OrganizerID: in.OrganizerID, Type: factType, OccurredAt: time.Now().UTC(), BuyerID: in.BuyerID, Amount: in.Amount, Currency: in.Currency, Payload: map[string]string{"order_id": in.OrderID.String()}})
	if err != nil {
		write(w, 500, map[string]string{"error": "journal append failed"})
		return
	}
	write(w, code, map[string]any{"status": status, "payment_id": factID, "sequence": e.Sequence, "replay": replay})
}
