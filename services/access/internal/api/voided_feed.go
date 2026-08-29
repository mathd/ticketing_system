package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
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

// feedCursorSigner authenticates a cursor.
//
// Base64 is an encoding, not a protection: without a MAC an enrolled device can
// hand-craft a cursor for its OWN organizer — an old position, or one past the
// end — and receive an empty page with next_cursor: null (ai-review [high]). It
// cannot reach another organizer's rows, since the query filters on the token's
// organizer regardless, so the forger and the victim are the same party. That
// still matters here and nowhere else: for a revocation feed the damaging state
// is a device that believes its view is complete when it is not, and this is a
// way for a device to put ITSELF in that state and never learn.
//
// Its own key, like every other signing key in this service. This proves "this
// position was issued by us", which is not the claim the QR credential, the
// image link or the lifecycle trail makes, and one key making four claims is how
// a leak of the cheapest costs the most expensive (see qrlink.go).
type feedCursorSigner struct{ key []byte }

func (s feedCursorSigner) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

func (s *Server) encodeCursor(c store.VoidedCursor) (string, error) {
	raw, err := json.Marshal(wireCursor{
		Version: feedCursorVersion, OccurredAt: c.OccurredAt.UTC(),
		EventID: c.EventID, OrganizerID: c.OrganizerID,
	})
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + base64.RawURLEncoding.EncodeToString(s.cursors.sign([]byte(body))), nil
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
func (s *Server) decodeCursor(encoded string, organizer uuid.UUID) (store.VoidedCursor, error) {
	body, signature, ok := strings.Cut(encoded, ".")
	if !ok {
		return store.VoidedCursor{}, errBadCursor
	}
	presented, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return store.VoidedCursor{}, errBadCursor
	}
	// Constant-time, and checked BEFORE the payload is parsed: comparing with ==
	// returns on the first wrong byte, which answers how much of a guess was
	// right, and parsing an unauthenticated payload first would let a forger
	// distinguish "bad signature" from "bad contents" (same ordering as
	// qrlink.go's verify).
	if !hmac.Equal(presented, s.cursors.sign([]byte(body))) {
		return store.VoidedCursor{}, errBadCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
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
	return store.VoidedCursor{
		OccurredAt: c.OccurredAt, EventID: c.EventID, OrganizerID: c.OrganizerID,
	}, nil
}

func (s *Server) voidedTickets(w http.ResponseWriter, r *http.Request) {
	// The organizer comes from the enrolled device token and nowhere else. There
	// is no organizer parameter on this operation, which is what stops a caller
	// choosing whose revocations to read — a check on a submitted value would
	// leave the trust boundary in the client.
	identity, ok := scannerIdentityFrom(r.Context())
	if !ok {
		// Fails closed. An empty feed would be the wrong answer twice over here:
		// it reads as "nothing is revoked", which is precisely the belief that
		// lets a voided holder through a gate.
		write(w, http.StatusUnauthorized, map[string]string{"error": "scanner device is not enrolled"})
		return
	}
	organizer := identity.OrganizerID

	// The abuse record for this poll was already emitted during authentication
	// (see authenticateScannerDevice, TKT-272 / ai-review F1). Deliberately NOT
	// emitted again here: authentication runs before parameter validation, so
	// emitting in the handler would both miss the schema-refused polls and
	// double-count the ones that get this far.

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
		c, err := s.decodeCursor(raw, organizer)
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
		encoded, err := s.encodeCursor(next)
		if err != nil {
			write(w, http.StatusInternalServerError, map[string]string{"error": "encode cursor"})
			return
		}
		body["next_cursor"] = encoded
	}
	write(w, http.StatusOK, body)
}
