package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"ticketing/services/access/internal/store"
)

// The voided-ticket feed handler (TKT-162, ADR-066). Distribution only: it hands
// a scanner the ids its organizer has voided, and has no opinion about what the
// scanner does with them — local storage, freshness and the admission decision
// are TKT-271's.

// feedCursorVersion is carried in the encoded cursor so a future change to the
// keyset can refuse an old cursor outright rather than misread it. A cursor is a
// position in a specific ordering; reinterpreting one under a new ordering is how
// a page silently skips rows.
const feedCursorVersion = 1

// wireCursor is the encoded form. It is opaque to the client on purpose — the
// fields are this service's business, and a client that parses them acquires a
// dependency on the keyset it must not have.
type wireCursor struct {
	Version     int       `json:"v"`
	OccurredAt  time.Time `json:"occurred_at"`
	EventID     uuid.UUID `json:"event_id"`
	OrganizerID uuid.UUID `json:"organizer_id"`
}

var errBadCursor = errors.New("invalid cursor")

func encodeCursor(c store.VoidedCursor) (string, error) {
	raw, err := json.Marshal(wireCursor{
		Version: feedCursorVersion, OccurredAt: c.OccurredAt.UTC(),
		EventID: c.EventID, OrganizerID: c.OrganizerID,
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// decodeCursor parses a cursor and refuses one that does not belong to the
// authenticated organizer.
//
// The binding is the point. A cursor is only a position, so it cannot read
// another organizer's rows — the query filters on the authenticated organizer
// regardless. What an unbound cursor CAN do is suppress: one copied from another
// organizer's page, or forged with a future timestamp, makes the holder's own
// next page skip rows or come back empty, with no error anywhere. For a
// revocation feed that is the dangerous direction, because silently missing a
// revocation is exactly the state this ticket exists to prevent. Refusing is
// loud; a short page is not.
func decodeCursor(encoded string, organizer uuid.UUID) (store.VoidedCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return store.VoidedCursor{}, errBadCursor
	}
	var c wireCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return store.VoidedCursor{}, errBadCursor
	}
	if c.Version != feedCursorVersion {
		return store.VoidedCursor{}, errBadCursor
	}
	if c.EventID == uuid.Nil || c.OrganizerID == uuid.Nil || c.OccurredAt.IsZero() {
		return store.VoidedCursor{}, errBadCursor
	}
	if c.OrganizerID != organizer {
		return store.VoidedCursor{}, errBadCursor
	}
	return store.VoidedCursor{OccurredAt: c.OccurredAt, EventID: c.EventID, OrganizerID: c.OrganizerID}, nil
}

func (s *Server) voidedTickets(w http.ResponseWriter, r *http.Request) {
	// The organizer comes from the enrolled device token and nowhere else. There
	// is no organizer parameter on this operation, which is what stops a caller
	// choosing whose revocations to read — a check on a submitted value would
	// leave the trust boundary in the client.
	organizer, ok := scannerOrganizer(r.Context())
	if !ok {
		// Fails closed. An empty feed would be the wrong answer twice over here:
		// it reads as "nothing is revoked", which is precisely the belief that
		// lets a voided holder through a gate.
		write(w, http.StatusUnauthorized, map[string]string{"error": "scanner device is not enrolled"})
		return
	}

	limit := store.VoidedFeedPageLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > store.VoidedFeedPageLimit {
			write(w, http.StatusBadRequest, map[string]string{"error": "invalid limit"})
			return
		}
		limit = n
	}

	var after store.VoidedCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeCursor(raw, organizer)
		if err != nil {
			write(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
			return
		}
		after = c
	}

	page, next, err := s.st.VoidedTickets(r.Context(), organizer, after, limit)
	if err != nil {
		write(w, http.StatusInternalServerError, map[string]string{"error": "load voided tickets"})
		return
	}

	ids := make([]uuid.UUID, 0, len(page))
	for _, v := range page {
		ids = append(ids, v.TicketID)
	}
	// next_cursor is present and null on the last page rather than absent: an
	// omitted field and an explicit null read the same to a careless client, and
	// the difference between "there is more" and "you have it all" is the whole
	// point of the field for a scanner deciding whether its view is complete.
	body := map[string]any{"ticket_ids": ids, "next_cursor": nil}
	if !next.IsZero() {
		encoded, err := encodeCursor(next)
		if err != nil {
			write(w, http.StatusInternalServerError, map[string]string{"error": "encode cursor"})
			return
		}
		body["next_cursor"] = encoded
	}
	write(w, http.StatusOK, body)
}
