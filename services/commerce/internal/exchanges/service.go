// Package exchanges is the single unit of work for discharging one settled exchange's
// capacity obligation.
//
// It exists because there are now TWO callers (TKT-259): the tickets-switched callback
// (POST /internal/exchanges/{id}/tickets-switched) and the exchange sweep
// (internal/exchangesweep). Before this ticket the logic lived on the HTTP handler as
// `(*Server).returnExchangedCapacity(r *http.Request, …)` — it took a *http.Request, so no
// background runner could call it at all. Lifting it here mirrors ADR-062's "one reversal
// path, three callers": one discharge path, two callers, so a sweep cannot drift from the
// callback's behaviour.
//
// WHAT THIS UNIT DELIBERATELY CANNOT DO. It never writes `tickets_exchanged_at`. That
// marker is access's fact — only access can establish that the old tickets stopped
// admitting — and migration 0011's CHECK gates the capacity return on it. Marking the
// switch stays on the callback, which is the only caller that has heard from access. A port
// that cannot express the marker is a stronger guarantee of that than a comment saying it
// does not (ADR-062 §1's reasoning about `Reverser`).
package exchanges

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// Caller performs a service-to-service HTTP call. A function rather than an interface so
// the API server can pass its existing `call` method unchanged — the transport, its
// timeouts and its internal-token handling stay in one place. Identical to refunds.Caller
// by construction: the sweep's lease is sized from that transport's timeout, so the two
// must not drift.
type Caller func(ctx context.Context, method, url, key string, in any, internal bool) (int, []byte, error)

// Service discharges exchange capacity obligations.
type Service struct {
	db           *sql.DB
	call         Caller
	inventoryURL string
}

func New(db *sql.DB, call Caller, inventory string) *Service {
	return &Service{db: db, call: call, inventoryURL: inventory}
}

// DriveExchange gives the OLD line's capacity back, reusing inventory's refund-capacity
// operation and its receipt. The exchange id is the deterministic `refund_id`, so a repeat
// answers as a replay rather than returning capacity twice.
//
// Reusing a refund-named contract for an exchange is a real cost — the receipt in
// `claim_history` says `refund_return` for something nobody refunded — and it buys the
// idempotent, seated-aware return that already exists. The source claim is GA by
// construction (TKT-158 refuses a seated source), and the return is FULL, which is the case
// ADR-038 §9 says is the only one seated claims accept anyway.
//
// It never fails the caller: the switch is already committed and durable, so the honest
// answer to a failure here is an exchange reporting `capacity_returned:false` — visible,
// and retried by the callback's 502 first and the sweep second.
//
// **The switch marker is a PRECONDITION, never something this unit sets.** An exchange whose
// tickets access has not confirmed switched is returned untouched: freeing capacity behind a
// marker commerce invented is the one ordering that can OVERSELL (ADR-038 §1), and 0011's
// CHECK would refuse the write anyway. This guard is what makes the sweep safe to point at
// every outstanding row rather than only switched ones.
func (s *Service) DriveExchange(ctx context.Context, ex commercestore.ExchangeSwitch) commercestore.ExchangeSwitch {
	if !ex.TicketsExchanged || ex.CapacityReturned || s.inventoryURL == "" || ex.SourceHoldID == uuid.Nil {
		return ex
	}
	code, _, err := s.call(ctx, http.MethodPost,
		fmt.Sprintf("%s/internal/holds/%s/refund-capacity", s.inventoryURL, ex.SourceHoldID), "",
		map[string]any{"organizer_id": ex.OrganizerID, "refund_id": ex.ID, "quantity": ex.Quantity}, true)
	if err != nil || code != http.StatusOK {
		slog.Default().WarnContext(ctx, "exchange capacity not returned; left outstanding",
			"exchange_id", ex.ID, "hold_id", ex.SourceHoldID, "status", code, "err", err)
		return ex
	}
	if err := commercestore.MarkExchangeCapacityReturned(ctx, s.db, ex.OrganizerID, ex.ID); err != nil {
		// Inventory returned it; only our record of it failed. A replay re-drives
		// inventory, which answers as a replay, and re-marks.
		slog.Default().ErrorContext(ctx, "record exchange capacity return", "exchange_id", ex.ID, "err", err)
		return ex
	}
	ex.CapacityReturned = true
	return ex
}
