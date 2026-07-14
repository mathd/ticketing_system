package api

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	apispec "ticketing/services/payments/api"
	"ticketing/services/payments/internal/store"
	"ticketing/shared/contract"
	"ticketing/shared/fakepsp"
)

const (
	TokenSuccess = fakepsp.TokenSuccess
	TokenDecline = fakepsp.TokenDecline
	TokenTimeout = fakepsp.TokenTimeout
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
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=300")
		_, _ = w.Write(apispec.Spec)
	})
	r.Post("/internal/facts", s.fact)
	r.Post("/internal/charges", s.charge)
	validated, err := contract.RequestValidator(apispec.Spec, r)
	if err != nil {
		panic(err)
	}
	return validated
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
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d\n%s\n%s", in.OrderID, in.BuyerID, in.Amount, in.Currency, in.PaymentToken))))
	boundStatus, boundID, occurredAt, replay, err := s.journal.BindOperation(r.Context(), in.OrganizerID, key, fingerprint)
	if err != nil {
		write(w, 409, map[string]string{"error": err.Error()})
		return
	}
	if replay {
		code := 200
		if boundStatus == "declined" {
			code = 402
		}
		if boundStatus == "timeout" {
			code = 408
		}
		write(w, code, map[string]any{"status": boundStatus, "payment_id": boundID, "replay": true})
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
		authorizedID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("payment:"+in.OrganizerID.String()+":"+key+":payment.authorized"))
		if _, _, err := s.journal.Append(r.Context(), store.Fact{ID: authorizedID, OrganizerID: in.OrganizerID, Type: "payment.authorized", OccurredAt: occurredAt, BuyerID: in.BuyerID, Amount: in.Amount, Currency: in.Currency, Payload: map[string]string{"order_id": in.OrderID.String()}}); err != nil {
			write(w, 500, map[string]string{"error": "journal append failed"})
			return
		}
	default:
		write(w, 400, map[string]string{"error": "unknown fake payment token"})
		return
	}
	factID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("payment:"+in.OrganizerID.String()+":"+key+":"+factType))
	e, replay, err := s.journal.Append(r.Context(), store.Fact{ID: factID, OrganizerID: in.OrganizerID, Type: factType, OccurredAt: occurredAt, BuyerID: in.BuyerID, Amount: in.Amount, Currency: in.Currency, Payload: map[string]string{"order_id": in.OrderID.String()}})
	if err != nil {
		write(w, 500, map[string]string{"error": "journal append failed"})
		return
	}
	if err := s.journal.CompleteOperation(r.Context(), in.OrganizerID, key, status, factID); err != nil {
		write(w, 500, map[string]string{"error": "persist payment result"})
		return
	}
	write(w, code, map[string]any{"status": status, "payment_id": factID, "sequence": e.Sequence, "replay": replay})
}
