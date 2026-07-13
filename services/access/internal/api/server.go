package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	qrcode "github.com/skip2/go-qrcode"

	"ticketing/services/access/internal/store"
)

type Server struct{ st *store.Postgres }

func New(st *store.Postgres) *Server { return &Server{st: st} }
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Get("/orders/{ref}/tickets", s.tickets)
	r.Get("/orders/{ref}/tickets/{ticket}/qr.png", s.qr)
	return r
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
