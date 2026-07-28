package events

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// Wire-compatibility golden for order.completed (TKT-126).
//
// Captured from the PRE-refactor emitter — this package's own `Envelope` struct —
// and committed before the shared envelope package existed. ADR-017 §5b′: a fixture
// built from the type under test cannot fail. Regenerating this literal from the
// shared envelope would prove nothing. Reviewer's check: at the commit
// introducing this file, the shared package does not exist yet —
// `git show <that-commit>:shared/go/domainevent/envelope.go` must fail.
//
// order.completed is the one subject with a live consumer that issues tickets
// for a paid order (access), so a silent wire change here loses tickets. The
// comparison is over the complete byte slice — key order and the RFC3339Nano
// timestamp included.
func TestWireGoldenOrderCompleted(t *testing.T) {
	orderID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	data := OrderCompletedData{
		OrderID:       orderID,
		GuestOrderRef: uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		OrganizerID:   uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		BuyerID:       uuid.MustParse("44444444-4444-4444-8444-444444444444"),
		SlotID:        uuid.MustParse("55555555-5555-4555-8555-555555555555"),
		TicketTypeID:  uuid.MustParse("66666666-6666-4666-8666-666666666666"),
		Quantity:      2,
	}
	occurred := time.Date(2026, 7, 20, 12, 34, 56, 123456789, time.UTC)

	body, err := OrderCompletedEnvelope(EventID(orderID), data, occurred)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"9aab8110-bc54-5566-8f42-5d318f70b447","type":"platform.commerce.order.completed","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":1,"data":{"order_id":"11111111-1111-4111-8111-111111111111","guest_order_ref":"22222222-2222-4222-8222-222222222222","organizer_id":"33333333-3333-4333-8333-333333333333","buyer_id":"44444444-4444-4444-8444-444444444444","slot_id":"55555555-5555-4555-8555-555555555555","ticket_type_id":"66666666-6666-4666-8666-666666666666","quantity":2}}`
	if string(body) != want {
		t.Fatalf("wire bytes changed (TKT-126 forbids it)\n got: %s\nwant: %s", body, want)
	}
}
