package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

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
func (s *Server) Router(log *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=300")
		_, _ = w.Write(apispec.Spec)
	})
	r.Get("/orders/{ref}/tickets", s.tickets)
	r.Get("/orders/{ref}/tickets/{ticket}/qr.png", s.qr)
	r.Post("/scans", s.scan)
	r.Post("/scans/reconciliations", s.reconcile)
	validated, err := contract.RequestValidatorWithErrorHandler(apispec.Spec, r, log, func(w http.ResponseWriter, _ string, _ int) {
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

// parseOccurrence enforces the pairing the contract cannot express: occurred_at
// is required iff occurrence_id is present, and the id must be a UUIDv4
// (ADR-025 §D3). An empty occurrence_id returns Nil — old-scanner semantics.
func parseOccurrence(occurrenceID, occurredAt string) (uuid.UUID, time.Time, error) {
	if occurrenceID == "" {
		return uuid.Nil, time.Time{}, nil
	}
	occ, err := uuid.Parse(occurrenceID)
	if err != nil || occ == uuid.Nil || occ.Version() != 4 || occ.Variant() != uuid.RFC4122 {
		return uuid.Nil, time.Time{}, errors.New("occurrence_id must be a UUIDv4")
	}
	at, err := time.Parse(time.RFC3339, occurredAt)
	if err != nil {
		// RFC 3339 permits lowercase t/z; Go's layout is strict about them.
		// The contract says date-time, so accept what it advertises.
		at, err = time.Parse(time.RFC3339, strings.ToUpper(occurredAt))
	}
	if err != nil {
		return uuid.Nil, time.Time{}, errors.New("occurred_at (RFC 3339) is required with occurrence_id")
	}
	return occ, at, nil
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	if s.verifier == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "scanner unavailable"})
		return
	}
	var input struct {
		QRPayload    string `json:"qr_payload"`
		OccurrenceID string `json:"occurrence_id"`
		OccurredAt   string `json:"occurred_at"`
	}
	if err := httpx.DecodeJSON(w, r, &input, 8<<10); err != nil || input.QRPayload == "" {
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "invalid_credential"})
		return
	}
	occ, occurredAt, err := parseOccurrence(input.OccurrenceID, input.OccurredAt)
	if err != nil {
		// Its own reason: a scanner must be able to tell a broken occurrence
		// envelope (fix the request) from a bad credential (deny the holder).
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "invalid_occurrence"})
		return
	}
	claims, err := s.verifier.Verify(input.QRPayload)
	if err != nil {
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "invalid_credential"})
		return
	}
	result, err := s.st.Redeem(r.Context(), store.RedeemInput{
		TicketID: claims.TicketID, OrderID: claims.OrderID, OrganizerID: claims.OrganizerID, SlotID: claims.SlotID,
		OccurrenceID: occ, OccurredAt: occurredAt,
	})
	if errors.Is(err, store.ErrTicketCredential) {
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "invalid_credential"})
		return
	}
	if errors.Is(err, store.ErrOccurrenceCollision) {
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "occurrence_collision"})
		return
	}
	if err != nil {
		write(w, http.StatusInternalServerError, map[string]string{"error": "redeem ticket"})
		return
	}
	// A degraded admission (ADR-021 §D6) is indistinguishable from a clean one to
	// the scanner, deliberately: the door opened either way, and the person at
	// the turnstile is not who the alarm is for. The operator learns about it
	// through the alarm route, not through the gate's screen.
	if !result.Accepted {
		reason := "already_redeemed"
		switch result.Decision {
		case store.DecisionIntegrityQuarantined:
			reason = "integrity_quarantined"
		case store.DecisionIntegrityOperatorControlled:
			reason = "integrity_operator_controlled"
		}
		// No cryptographic detail leaves the gate: which field failed to verify
		// is exactly what an attacker probing the trail would want back.
		write(w, http.StatusConflict, map[string]any{"decision": "rejected", "reason": reason, "original_scan_at": result.OccurredAt})
		return
	}
	response := map[string]any{"decision": "accepted", "scanned_at": result.OccurredAt}
	if result.Replayed {
		// Present-and-true only on replays (ADR-025 §D3: never a bare accepted)
		// — absent on first-time results so old scanners parse them unchanged.
		response["replay"] = true
	}
	write(w, http.StatusOK, response)
}

// reconcile syncs offline occurrences (ADR-025 §D6). Recording, not deciding:
// each occurrence gets its own result in request order, and one bad occurrence
// never fails the batch — a gate syncing a night's queue must learn the fate
// of every entry.
func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	if s.verifier == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "scanner unavailable"})
		return
	}
	var input struct {
		Occurrences []struct {
			QRPayload    string `json:"qr_payload"`
			OccurrenceID string `json:"occurrence_id"`
			OccurredAt   string `json:"occurred_at"`
		} `json:"occurrences"`
	}
	if err := httpx.DecodeJSON(w, r, &input, 256<<10); err != nil || len(input.Occurrences) == 0 {
		write(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid reconciliation request"})
		return
	}
	results := make([]map[string]any, 0, len(input.Occurrences))
	// The id is echoed VERBATIM, even when malformed: the scanner correlates
	// results to its queue by this value, and a normalized or zeroed id would
	// strand the queue entry in retry-forever (ai-review R3).
	rejected := func(occurrenceID string) map[string]any {
		return map[string]any{"occurrence_id": occurrenceID, "result": "rejected"}
	}
	for _, entry := range input.Occurrences {
		occ, occurredAt, err := parseOccurrence(entry.OccurrenceID, entry.OccurredAt)
		if err != nil || occ == uuid.Nil {
			results = append(results, rejected(entry.OccurrenceID))
			continue
		}
		claims, err := s.verifier.Verify(entry.QRPayload)
		if err != nil {
			results = append(results, rejected(entry.OccurrenceID))
			continue
		}
		result, err := s.st.ReconcileAdmission(r.Context(), store.ReconcileOccurrence{
			TicketID: claims.TicketID, OrderID: claims.OrderID, OrganizerID: claims.OrganizerID, SlotID: claims.SlotID,
			OccurrenceID: occ, OccurredAt: occurredAt,
		})
		if errors.Is(err, store.ErrTicketCredential) || errors.Is(err, store.ErrOccurrenceCollision) {
			results = append(results, rejected(entry.OccurrenceID))
			continue
		}
		if err != nil {
			// Infrastructure failure, not a per-occurrence verdict: the whole
			// batch fails so the scanner keeps its queue and retries.
			write(w, http.StatusInternalServerError, map[string]string{"error": "reconcile admissions"})
			return
		}
		entryResult := map[string]any{"occurrence_id": result.OccurrenceID, "result": string(result.Outcome), "occurred_at": result.OccurredAt}
		if result.SkewFlagged {
			entryResult["skew_flagged"] = true
		}
		results = append(results, entryResult)
	}
	write(w, http.StatusOK, map[string]any{"results": results})
}
