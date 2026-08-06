package api

import (
	"net/http"

	"github.com/google/uuid"
)

// getOrderSettlement answers "who was owed what out of this order's capture"
// (TKT-217 / ADR-048).
//
// Internal, like every other route here: settlement names who gets paid what,
// which is the most sensitive thing this epic produces.
func (s *Server) getOrderSettlement(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		write(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	orderID, err := uuid.Parse(pathTail(r.URL.Path, "/internal/orders/", "/settlement"))
	if err != nil {
		write(w, 404, map[string]string{"error": "not found"})
		return
	}
	organizerID, err := uuid.Parse(r.URL.Query().Get("organizer_id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "organizer_id required"})
		return
	}
	lines, total, currency, err := s.journal.ReadOrderSettlement(r.Context(), organizerID, orderID)
	if err != nil {
		write(w, 500, map[string]string{"error": "read settlement"})
		return
	}
	if len(lines) == 0 {
		// No entries means no captured order under this organizer — not an empty
		// settlement. A captured order always has lines (the database refuses it
		// otherwise), so "none" can only mean "no such capture".
		write(w, 404, map[string]string{"error": "not found"})
		return
	}
	entries := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		entries = append(entries, map[string]any{
			"entry_kind": string(l.Kind), "amount": l.Amount, "currency": l.Currency,
			"payee_id": l.PayeeID, "payee_kind": l.PayeeKind,
			"payee_display_name": l.PayeeDisplayName,
			"fee_code":           l.FeeCode, "incidence": l.Incidence,
		})
	}
	write(w, 200, map[string]any{
		"order_id": orderID, "currency": currency, "total": total, "entries": entries,
	})
}

// pathTail extracts the id between a prefix and a suffix.
func pathTail(path, prefix, suffix string) string {
	if len(path) <= len(prefix)+len(suffix) {
		return ""
	}
	if path[:len(prefix)] != prefix || path[len(path)-len(suffix):] != suffix {
		return ""
	}
	return path[len(prefix) : len(path)-len(suffix)]
}
