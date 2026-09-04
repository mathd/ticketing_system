//go:build smoke

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/commerce/internal/exchangesweep"
	commercestore "ticketing/services/commerce/internal/store"
)

// COS 3 (TKT-259, ADR-063): an outstanding exchange obligation left behind by a
// dead-lettered access redelivery is driven to completion by the commerce sweep.
//
// THE TIER IS THE POINT, and it is a different point from exchange_resume_smoke_test.go's.
// The gap this ticket closes is a SEQUENCE across two mechanisms — the callback commits the
// switch and fails the capacity return, redelivery stops happening, and only then does the
// sweep discharge it. Neither half can observe that alone:
//
//   - the store tier can construct the row but has no handler, so it cannot produce the
//     obligation the way the real callback produces it, and cannot tell a discharge that
//     went through inventory from a timestamp somebody wrote;
//   - the runner's unit tests drive fakes, so they prove the runner's decisions and are
//     silent about whether it is connected to the real SQL projection, the real discharge
//     unit, or the real inventory call.
//
// THE OBLIGATION IS DRIVEN THROUGH THE HANDLER, never seeded. Writing
// `tickets_exchanged_at` directly would produce a green test whose precondition the code
// under test never created — and worse, it would not prove the callback leaves the row in
// the state the sweep expects to find. Here the real callback runs against an inventory
// that refuses, which is the real 502 branch.
//
// THE READ DOES NOT DRIVE THE THING UNDER TEST (COS 3's own requirement). Verification is a
// raw SQL read of `order_exchanges` after the runner returns — not a re-POST of the
// callback, and not a store helper that discharges on the way past. If the sweep did
// nothing, this test fails.

// settleAnExchange drives one exchange all the way to settled through the real handler, and
// returns its id.
func settleAnExchange(t *testing.T, s *exchangeStack, f exchangeFixture, key string) uuid.UUID {
	t.Helper()
	if code, body := s.exchange(t, f, key); code != http.StatusOK {
		t.Fatalf("exchange = %d (%v), want 200: the fixture must reach a SETTLED exchange or "+
			"there is no obligation to sweep", code, body)
	}
	id := commercestore.ExchangeID(f.organizer, key)
	var settled bool
	if err := s.db.QueryRow(`SELECT settled_at IS NOT NULL FROM order_exchanges WHERE organizer_id=$1 AND id=$2`,
		f.organizer, id).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	if !settled {
		t.Fatal("the exchange did not settle; this fixture cannot reach the state under test")
	}
	return id
}

// ticketsSwitched posts the real access callback and returns its status.
func (s *exchangeStack) ticketsSwitched(t *testing.T, org, id uuid.UUID) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"organizer_id": org})
	req := httptest.NewRequest(http.MethodPost, "/internal/exchanges/"+id.String()+"/tickets-switched",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", s.token)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec.Code
}

// obligations reads the two durable facts straight from the row. Deliberately raw SQL and
// deliberately not a store helper: the assertion must observe what the sweep LEFT BEHIND,
// never re-drive it.
func (s *exchangeStack) obligations(t *testing.T, org, id uuid.UUID) (switched, returned bool) {
	t.Helper()
	if err := s.db.QueryRow(`
		SELECT tickets_exchanged_at IS NOT NULL, capacity_returned_at IS NOT NULL
		FROM order_exchanges WHERE organizer_id=$1 AND id=$2`, org, id).Scan(&switched, &returned); err != nil {
		t.Fatal(err)
	}
	return switched, returned
}

// runSweep drives one real pass over real PostgreSQL through the real discharge unit.
func runSweep(t *testing.T, s *exchangeStack, srv *Server) int {
	t.Helper()
	r := exchangesweep.New(exchangesweep.DBStore{DB: s.db}, srv.Exchanges(),
		time.Minute, 16, time.Minute, nil)
	return r.RunOnce(context.Background())
}

// THE TICKET'S TEST. The callback commits the switch and fails the capacity return; access
// then never redelivers (its consumer dead-lettered the message); the sweep discharges the
// obligation with no human replaying anything.
func TestADeadLetteredSwitchIsSweptToCompletion(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "sweep-deadletter", 2, 2500)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(2500)
	s := exchangeStackFor(t, db, f, policy)
	srv := newTestServer(db, http.DefaultClient, s.catalog.server.URL, s.inventory.server.URL,
		s.payments.server.URL, s.token)

	id := settleAnExchange(t, s, f, "sweep-deadletter")

	// 1. Access delivers the switch; inventory refuses the source line's capacity return.
	policy.capacityReturnFails.Store(true)
	if code := s.ticketsSwitched(t, f.organizer, id); code != http.StatusBadGateway {
		t.Fatalf("callback = %d, want 502: an unresolved capacity return must leave the "+
			"message unacknowledged so access redelivers it", code)
	}
	switched, returned := s.obligations(t, f.organizer, id)
	if !switched || returned {
		t.Fatalf("after the failed callback: switched=%v returned=%v, want true/false — the "+
			"marker is written BEFORE the inventory call so the ordering is checkable at all",
			switched, returned)
	}

	// 2. Access's consumer dead-letters the message: the callback is never delivered again.
	//    That is expressed by simply not calling it. Before this ticket, nothing else ever
	//    revisited this row and the obligation stayed outstanding forever.

	// 3. Inventory recovers.
	policy.capacityReturnFails.Store(false)
	callsBefore := s.inventory.count("refund-capacity")

	// 4. The sweep runs.
	if got := runSweep(t, s, srv); got != 1 {
		t.Fatalf("the sweep resolved %d exchanges, want 1: this is the gap the ticket exists "+
			"to close — a settled, switched exchange whose capacity never came back and whose "+
			"only retry mechanism has stopped", got)
	}

	// 5. Verified by reading the row, not by re-driving anything.
	switched, returned = s.obligations(t, f.organizer, id)
	if !switched || !returned {
		t.Fatalf("after the sweep: switched=%v returned=%v, want both true", switched, returned)
	}
	if got := s.inventory.count("refund-capacity") - callsBefore; got != 1 {
		t.Fatalf("the sweep made %d capacity calls, want exactly 1: the discharge must go "+
			"through inventory's real operation once — zero means the timestamp was written "+
			"without returning anything, more than one means a double return", got)
	}
}

// The sweep is IDEMPOTENT against a completed row: a second pass claims nothing and calls
// nobody. Without this, the test above would pass for a sweep that re-drove every exchange
// forever, returning capacity again on each pass.
func TestASweptExchangeIsNotSweptAgain(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "sweep-idempotent", 2, 2500)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(2500)
	s := exchangeStackFor(t, db, f, policy)
	srv := newTestServer(db, http.DefaultClient, s.catalog.server.URL, s.inventory.server.URL,
		s.payments.server.URL, s.token)

	id := settleAnExchange(t, s, f, "sweep-idempotent")
	policy.capacityReturnFails.Store(true)
	_ = s.ticketsSwitched(t, f.organizer, id)
	policy.capacityReturnFails.Store(false)
	if got := runSweep(t, s, srv); got != 1 {
		t.Fatalf("first sweep resolved %d, want 1", got)
	}

	callsAfterFirst := s.inventory.count("refund-capacity")
	if got := runSweep(t, s, srv); got != 0 {
		t.Fatalf("second sweep resolved %d, want 0: a discharged exchange must not be "+
			"claimable again", got)
	}
	if got := s.inventory.count("refund-capacity"); got != callsAfterFirst {
		t.Fatalf("the second sweep made %d more capacity calls, want 0: returning capacity "+
			"twice for one exchange oversells the source line",
			got-callsAfterFirst)
	}
}

// THE SAFETY PROPERTY, end to end (ADR-063). A settled exchange whose switch access has
// NEVER confirmed is claimed and observed by the sweep, and is left exactly as it was: no
// marker invented, no capacity returned. Only access can establish that the old tickets
// stopped admitting, and freeing capacity behind a marker commerce wrote itself is the one
// ordering that can oversell (ADR-038 §1).
//
// This is COS 1's SECOND branch — such a row is monitored, never driven to completion — and
// it is the case a sweep copied carelessly from the refund side would get wrong, because on
// that side both obligations ARE commerce's to discharge.
func TestTheSweepNeverInventsTheSwitchMarker(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "sweep-no-marker", 2, 2500)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(2500)
	s := exchangeStackFor(t, db, f, policy)
	srv := newTestServer(db, http.DefaultClient, s.catalog.server.URL, s.inventory.server.URL,
		s.payments.server.URL, s.token)

	// Settled, and the callback NEVER arrives — the `order.exchanged` event was never
	// consumed at all.
	id := settleAnExchange(t, s, f, "sweep-no-marker")
	callsBefore := s.inventory.count("refund-capacity")

	if got := runSweep(t, s, srv); got != 0 {
		t.Fatalf("the sweep resolved %d, want 0: commerce cannot complete an exchange access "+
			"has not confirmed switched", got)
	}

	switched, returned := s.obligations(t, f.organizer, id)
	if switched {
		t.Fatal("the sweep wrote tickets_exchanged_at: that marker is ACCESS's fact — commerce " +
			"asserting it would unlock the capacity return behind a switch that may never have " +
			"happened, and the tickets would still admit while the seat went back on sale")
	}
	if returned {
		t.Fatal("the sweep returned capacity for an exchange whose tickets were never switched: " +
			"this is the oversell ADR-038 §1 exists to prevent")
	}
	if got := s.inventory.count("refund-capacity") - callsBefore; got != 0 {
		t.Fatalf("the sweep made %d capacity calls on an unswitched exchange, want 0", got)
	}

	// And it is COUNTED, which is what makes "monitored rather than driven" true rather
	// than a euphemism for "ignored".
	b, err := commercestore.ReadExchangeReversalBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if b.AwaitingSwitch < 1 {
		t.Fatalf("awaiting-switch = %d, want >= 1: a row the sweep can never complete must be "+
			"visible to an operator, or parking is silent", b.AwaitingSwitch)
	}
}
