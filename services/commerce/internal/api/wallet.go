package api

// The wallet read (TKT-222 / US-A3). See ADR-049 § TKT-222.
//
// One page of a customer's completed purchases, newest first. Two things carry
// the weight: the caller is identified by the assertion and nothing else, and the
// display names come from catalog in ONE call per page rather than one per order.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// walletCursorFn is the store seam, so the handler's authorization and assembly
// can be tested without a database.
var listCustomerOrdersFn = commercestore.CustomerOrders

// encodeCursor renders a keyset position as one opaque string.
//
// Opaque because a cursor is a position in a result set, not an API: a client
// that learns to construct `<rfc3339>|<uuid>` starts depending on the sort key,
// and changing it then breaks them. base64 is not secrecy — anyone can decode it
// — it is a fence that makes the dependency deliberate rather than accidental.
func encodeCursor(c commercestore.WalletCursor) string {
	if c.OrderID == uuid.Nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.OrderID.String()))
}

// decodeCursor is deliberately unforgiving. A malformed cursor is a client error,
// not a reason to silently serve page one: quietly restarting would make a paging
// client loop over the same rows for ever without any error to notice.
func decodeCursor(raw string) (commercestore.WalletCursor, error) {
	if raw == "" {
		return commercestore.WalletCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return commercestore.WalletCursor{}, errors.New("invalid cursor")
	}
	at, id, ok := strings.Cut(string(decoded), "|")
	if !ok {
		return commercestore.WalletCursor{}, errors.New("invalid cursor")
	}
	created, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return commercestore.WalletCursor{}, errors.New("invalid cursor")
	}
	order, err := uuid.Parse(id)
	if err != nil {
		return commercestore.WalletCursor{}, errors.New("invalid cursor")
	}
	return commercestore.WalletCursor{CreatedAt: created, OrderID: order}, nil
}

func (s *Server) listCustomerOrders(w http.ResponseWriter, r *http.Request) {
	requested, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid customer id"})
		return
	}

	// Identity comes from the assertion, and only from it. The path id is a
	// selector this then has to justify.
	caller, err := customerFromRequest(s.assertionKey, r.Header.Get(assertionHeader), time.Now())
	if err != nil || !caller.Valid {
		write(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	if caller.UUID != requested {
		// 404, NOT 403. A 403 confirms that the named customer exists, which is
		// an account-existence oracle anyone who can register once could walk.
		// The same answer as an id that does not exist discloses nothing, and
		// commerce already prefers 404 over 401 on its internal surface for this
		// reason (ADR-043).
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	locale := r.URL.Query().Get("locale")
	after, err := decodeCursor(r.URL.Query().Get("after"))
	if err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
		return
	}

	orders, next, err := listCustomerOrdersFn(r.Context(), s.db, caller.UUID, after, commercestore.WalletPageLimit)
	if err != nil {
		// The customer id is safe to log — it is not the credential, and an
		// operator needs it. The assertion and the order references are not.
		slog.Default().ErrorContext(r.Context(), "read customer orders", "customer_id", caller.UUID, "err", err)
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily unavailable"})
		return
	}

	names := s.performanceNames(r.Context(), orders, locale)

	rows := make([]map[string]any, 0, len(orders))
	for _, o := range orders {
		row := map[string]any{
			"order_id":        o.OrderID,
			"guest_order_ref": o.GuestOrderRef,
			"purchased_at":    o.CreatedAt.UTC(),
			"quantity":        o.Quantity,
			"total_amount":    o.TotalAmount,
			"currency":        o.Currency,
			"event_name":      nil,
			"starts_at":       nil,
		}
		if n, ok := names[o.SlotID]; ok {
			row["event_name"] = n.name
			row["starts_at"] = n.startsAt.UTC()
		}
		rows = append(rows, row)
	}

	out := map[string]any{"orders": rows, "next_cursor": nil}
	if cursor := encodeCursor(next); cursor != "" {
		out["next_cursor"] = cursor
	}
	write(w, http.StatusOK, out)
}

type performanceName struct {
	name     string
	startsAt time.Time
}

// performanceNames resolves this page's performances in ONE catalog call.
//
// A wallet with twenty rows must not become twenty cross-service reads — that is
// the N+1 ADR-004's one-call-per-page rule exists to prevent, and it is the
// obvious thing to write.
//
// A failure here is NOT an error for the caller. The wallet's job is to get a
// buyer to their tickets; a row that says "your purchase, 18 March, €45.50, view
// tickets" without the show's name still does that, while a 503 does not. Catalog
// being down must not take the wallet with it.
func (s *Server) performanceNames(ctx context.Context, orders []commercestore.WalletOrder, locale string) map[uuid.UUID]performanceName {
	out := map[uuid.UUID]performanceName{}
	if len(orders) == 0 || s.catalogURL == "" || locale == "" {
		return out
	}
	// Deduplicated: several orders for the same performance are one id.
	seen := map[uuid.UUID]bool{}
	ids := make([]string, 0, len(orders))
	for _, o := range orders {
		if seen[o.SlotID] {
			continue
		}
		seen[o.SlotID] = true
		ids = append(ids, o.SlotID.String())
	}

	url := s.catalogURL + "/internal/performances/display-names?locale=" + locale + "&ids=" + strings.Join(ids, ",")
	code, body, err := s.call(ctx, http.MethodGet, url, "", nil, true)
	if err != nil || code != http.StatusOK {
		slog.Default().WarnContext(ctx, "wallet display names unavailable; rows will render without a name",
			"status", code, "err", err)
		return out
	}
	var resolved struct {
		Performances []struct {
			PerformanceID uuid.UUID `json:"performance_id"`
			EventName     string    `json:"event_name"`
			StartsAt      time.Time `json:"starts_at"`
		} `json:"performances"`
	}
	if err := json.Unmarshal(body, &resolved); err != nil {
		slog.Default().WarnContext(ctx, "wallet display names undecodable", "err", err)
		return out
	}
	for _, p := range resolved.Performances {
		out[p.PerformanceID] = performanceName{name: p.EventName, startsAt: p.StartsAt}
	}
	return out
}
