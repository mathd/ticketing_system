package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	qrcode "github.com/skip2/go-qrcode"

	apispec "ticketing/services/access/api"
	"ticketing/services/access/internal/store"
	"ticketing/services/access/internal/ticket"
	"ticketing/shared/contract"
	"ticketing/shared/httpx"
)

type Server struct {
	st       *store.Postgres
	verifier *ticket.Verifier
}

func New(st *store.Postgres, verifier *ticket.Verifier) *Server {
	return &Server{st: st, verifier: verifier}
}
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=300")
		_, _ = w.Write(apispec.Spec)
	})
	r.Get("/orders/{ref}/tickets", s.tickets)
	r.Get("/orders/{ref}/tickets/{ticket}/qr.png", s.qr)
	r.Post("/scans", s.scan)
	validated, err := contract.RequestValidatorWithErrorHandler(apispec.Spec, r, func(w http.ResponseWriter, _ string, _ int) {
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "invalid_credential"})
	})
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
func parseRef(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	ref, err := uuid.Parse(chi.URLParam(r, "ref"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid order reference"})
		return uuid.Nil, false
	}
	return ref, true
}
func (s *Server) tickets(w http.ResponseWriter, r *http.Request) {
	ref, ok := parseRef(w, r)
	if !ok {
		return
	}
	ts, err := s.st.Tickets(r.Context(), ref)
	if err != nil {
		write(w, 500, map[string]string{"error": "load tickets"})
		return
	}
	if len(ts) == 0 {
		write(w, 404, map[string]string{"error": "not found"})
		return
	}
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		history, err := s.st.History(r.Context(), t.ID)
		if err != nil {
			write(w, 500, map[string]string{"error": "load ticket history"})
			return
		}
		out = append(out, map[string]any{"ticket_id": t.ID, "qr_payload": t.Payload, "issued_at": t.IssuedAt, "history": history, "qr_url": "/api/access/orders/" + ref.String() + "/tickets/" + t.ID.String() + "/qr.png"})
	}
	write(w, 200, map[string]any{"order_ref": ref, "tickets": out})
}
func (s *Server) qr(w http.ResponseWriter, r *http.Request) {
	ref, ok := parseRef(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "ticket"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid ticket"})
		return
	}
	payload, err := s.st.TicketForQR(r.Context(), ref, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			write(w, 404, map[string]string{"error": "not found"})
			return
		}
		write(w, 500, map[string]string{"error": "load QR"})
		return
	}
	image, err := qrcode.Encode(payload, qrcode.Medium, 256)
	if err != nil {
		write(w, 500, map[string]string{"error": "encode QR"})
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(image)
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	if s.verifier == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "scanner unavailable"})
		return
	}
	var input struct {
		QRPayload string `json:"qr_payload"`
	}
	if err := httpx.DecodeJSON(w, r, &input, 8<<10); err != nil || input.QRPayload == "" {
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "invalid_credential"})
		return
	}
	claims, err := s.verifier.Verify(input.QRPayload)
	if err != nil {
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "invalid_credential"})
		return
	}
	result, err := s.st.Redeem(r.Context(), store.RedeemInput{
		TicketID: claims.TicketID, OrderID: claims.OrderID, OrganizerID: claims.OrganizerID, SlotID: claims.SlotID,
	})
	if errors.Is(err, store.ErrTicketCredential) {
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "invalid_credential"})
		return
	}
	if err != nil {
		write(w, http.StatusInternalServerError, map[string]string{"error": "redeem ticket"})
		return
	}
	if !result.Accepted {
		write(w, http.StatusConflict, map[string]any{"decision": "rejected", "reason": "already_redeemed", "original_scan_at": result.OccurredAt})
		return
	}
	write(w, http.StatusOK, map[string]any{"decision": "accepted", "scanned_at": result.OccurredAt})
}
