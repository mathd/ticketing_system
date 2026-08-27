package refunds

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// TKT-171, COS-3: tickets are voided BEFORE capacity is returned.
//
// The oversell-critical ordering (ADR-038 §1). Freeing the seat while the original
// ticket still admits is the one sequence that can sell a seat twice; voiding
// first can only under-sell. So this needs its OWN assertion — "both obligations
// were discharged" is satisfied by either order, and the wrong one is the defect.
//
// The seam is the injected Caller. What is deliberately NOT asserted is a
// recorded call order in a slice: that is a fact about the test's own
// instrumentation, and a refactor that moved the calls could keep the slice happy
// (AGENTS.md — a test that pins the harness, not the contract). Instead each fake
// reports WHAT THE CODE OBSERVED at the moment it ran: the inventory leg refuses
// unless the marker it depends on is already set, so the assertion is about the
// state the second call could actually see.
//
// Recorded by defer, so the probe fires on the runs where the contract is broken
// too — a probe that only records on success cannot observe a failure.

type legRecorder struct {
	// ticketsVoided is the durable marker's stand-in: set by the mark callback,
	// read by the capacity leg. Using the callback rather than a bare bool is what
	// makes this a test of "capacity waits for the RECORDED void" rather than
	// "capacity waits for the void CALL" — the two differ exactly when the marker
	// write fails, which is the case that must not free the seat.
	ticketsVoided     bool
	order             []string
	capacitySawVoided bool
	fail              map[string]int // url substring -> status to answer with
}

func (l *legRecorder) call(_ context.Context, _, url, _ string, _ any, _ bool) (int, []byte, error) {
	leg := "tickets"
	if strings.Contains(url, "refund-capacity") {
		leg = "capacity"
		l.capacitySawVoided = l.ticketsVoided
	}
	defer func() { l.order = append(l.order, leg) }()
	if status, ok := l.fail[leg]; ok {
		return status, nil, nil
	}
	return http.StatusOK, []byte(`{}`), nil
}

func newVoidUnderTest(t *testing.T, rec *legRecorder) (*Service, commercestore.OrderVoid) {
	t.Helper()
	s := New(nil, rec.call, "http://payments", "http://access", "http://inventory")
	v := commercestore.OrderVoid{
		ID: uuid.New(), OrderID: uuid.New(), OrganizerID: uuid.New(),
		HoldID: uuid.New(), Quantity: 2,
	}
	return s, v
}

// driveVoidWithMarkers runs DriveVoid's ordering through the shared driver with
// in-memory markers, so the test needs no database while still exercising the real
// driveOrderedReversal.
func driveVoidWithMarkers(s *Service, rec *legRecorder, v commercestore.OrderVoid) commercestore.OrderVoid {
	rev := s.driveOrderedReversal(context.Background(), reversal{
		OperationID: v.ID, OrderID: v.OrderID, OrganizerID: v.OrganizerID,
		HoldID: v.HoldID, Quantity: v.Quantity,
		markVoided:   func(context.Context) error { rec.ticketsVoided = true; return nil },
		markReturned: func(context.Context) error { return nil },
	})
	v.TicketsVoided, v.CapacityReturned = rev.TicketsVoided, rev.CapacityReturned
	return v
}

func TestVoidReturnsCapacityOnlyAfterTicketsAreVoided(t *testing.T) {
	rec := &legRecorder{}
	s, v := newVoidUnderTest(t, rec)

	out := driveVoidWithMarkers(s, rec, v)

	if !out.TicketsVoided || !out.CapacityReturned {
		t.Fatalf("both obligations must discharge on a clean run: %+v", out)
	}
	// The load-bearing assertion: what the CAPACITY leg observed when it ran. If
	// the driver returned the seat first, this is false — and no reordering of the
	// recorder can make it true, because the value is read inside the call.
	if !rec.capacitySawVoided {
		t.Fatal("capacity was returned while the tickets were not yet recorded as void — " +
			"this is the ADR-038 §1 sequence that oversells")
	}
}

// An access failure must leave BOTH obligations outstanding. This is the case the
// ordering exists for: if the capacity leg ran anyway, the seat would be resold
// while the original ticket still admits.
func TestVoidDoesNotReturnCapacityWhenVoidingFails(t *testing.T) {
	for name, status := range map[string]int{
		// 503 is access saying "not yet" — issuance is asynchronous and a prompt
		// reversal can outrun it. It must not read as "nothing to void".
		"access not ready (503)": http.StatusServiceUnavailable,
		"access error (500)":     http.StatusInternalServerError,
	} {
		t.Run(name, func(t *testing.T) {
			rec := &legRecorder{fail: map[string]int{"tickets": status}}
			s, v := newVoidUnderTest(t, rec)

			out := driveVoidWithMarkers(s, rec, v)

			if out.TicketsVoided {
				t.Error("a failed voiding must not be recorded as done")
			}
			if out.CapacityReturned {
				t.Fatal("capacity must not be returned when voiding failed")
			}
			for _, leg := range rec.order {
				if leg == "capacity" {
					t.Fatal("the capacity leg must not even be CALLED when voiding failed — " +
						"inventory would have returned the seat")
				}
			}
		})
	}
}

// The marker write is what the capacity leg waits on, not the call's success. If
// access voided the tickets but recording it failed, the capacity leg must NOT
// run: a later replay re-drives access (which answers as a replay) and re-marks.
//
// Without this, `discharge` could set the flag optimistically on a 200 and the
// suite would stay green — while a crash between the call and the marker would
// leave a seat freed against a voiding nothing recorded.
func TestVoidWaitsForTheRecordedMarkerNotTheCall(t *testing.T) {
	rec := &legRecorder{}
	s, v := newVoidUnderTest(t, rec)

	rev := s.driveOrderedReversal(context.Background(), reversal{
		OperationID: v.ID, OrderID: v.OrderID, OrganizerID: v.OrganizerID,
		HoldID: v.HoldID, Quantity: v.Quantity,
		markVoided:   func(context.Context) error { return errors.New("marker write failed") },
		markReturned: func(context.Context) error { return nil },
	})

	if rev.TicketsVoided {
		t.Error("an unrecorded voiding must not report as recorded")
	}
	if rev.CapacityReturned {
		t.Fatal("capacity must not be returned on the strength of a voiding that was never recorded")
	}
}

// Both legs must carry the SAME operation id, and it must be the void's own —
// this is what makes a replay converge on one downstream operation instead of
// creating a second.
func TestBothLegsCarryTheVoidsOwnIdentity(t *testing.T) {
	var seen []uuid.UUID
	rec := &legRecorder{}
	capture := func(ctx context.Context, method, url, key string, in any, internal bool) (int, []byte, error) {
		if body, ok := in.(map[string]any); ok {
			if id, ok := body["refund_id"].(uuid.UUID); ok {
				seen = append(seen, id)
			}
		}
		return rec.call(ctx, method, url, key, in, internal)
	}
	s := New(nil, capture, "http://payments", "http://access", "http://inventory")
	v := commercestore.OrderVoid{
		ID: uuid.New(), OrderID: uuid.New(), OrganizerID: uuid.New(),
		HoldID: uuid.New(), Quantity: 1,
	}
	s.driveOrderedReversal(context.Background(), reversal{
		OperationID: v.ID, OrderID: v.OrderID, OrganizerID: v.OrganizerID,
		HoldID: v.HoldID, Quantity: v.Quantity,
		markVoided:   func(context.Context) error { rec.ticketsVoided = true; return nil },
		markReturned: func(context.Context) error { return nil },
	})

	if len(seen) != 2 {
		t.Fatalf("both legs must send an operation id, saw %d", len(seen))
	}
	if seen[0] != v.ID || seen[1] != v.ID {
		t.Fatalf("both legs must carry the void's own id %v, saw %v", v.ID, seen)
	}
}

// The identity is derived from the ORDER, not the request key — the decision that
// makes a staff retry, a cancellation-run retry and a restart converge on one
// downstream operation. And it must never collide with the refund id for the same
// order, or one operation would replay as the other.
func TestVoidIdentityIsStableAndDistinctFromARefund(t *testing.T) {
	org, order := uuid.New(), uuid.New()

	if a, b := commercestore.VoidID(org, order), commercestore.VoidID(org, order); a != b {
		t.Fatalf("VoidID must be deterministic: %v vs %v", a, b)
	}
	// Different order, different id — otherwise one void would discharge another
	// order's obligations.
	if commercestore.VoidID(org, order) == commercestore.VoidID(org, uuid.New()) {
		t.Fatal("two orders must not share a void id")
	}
	if commercestore.VoidID(org, order) == commercestore.VoidID(uuid.New(), order) {
		t.Fatal("two organizers must not share a void id")
	}
	// The collision that would actually hurt: access and inventory key their
	// idempotency on this value under the name `refund_id`, so a void id equal to a
	// refund id for the same order would let one replay as the other.
	if commercestore.VoidID(org, order) == commercestore.RefundID(org, order.String()) {
		t.Fatal("a void id must never equal a refund id — they share a downstream namespace")
	}
}
