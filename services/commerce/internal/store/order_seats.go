package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

var ErrInvalidOrderSeats = errors.New("invalid stored order seats")

// OrderSeats is the order line and its persisted seat identities.
type OrderSeats struct {
	OrderID        uuid.UUID
	OrganizerID    uuid.UUID
	SlotID         uuid.UUID
	TicketTypeID   uuid.UUID
	Quantity       int
	Seated         bool
	SeatIdentities []string
}

// ReadOrderSeats scopes an order through its reservation, which owns the organizer.
// NULL seat_identities means the reservation is unseated. A non-NULL empty array is
// invalid stored data and remains distinguishable from NULL.
func ReadOrderSeats(ctx context.Context, db *sql.DB, org, order uuid.UUID) (OrderSeats, error) {
	var out OrderSeats
	var seats sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT o.id, r.organizer_id, r.slot_id, r.ticket_type_id, r.quantity,
		       r.seat_identities::text
		FROM orders o
	JOIN reservations r ON r.id=o.reservation_id
	WHERE o.id=$1 AND r.organizer_id=$2`, order, org).Scan(
		&out.OrderID, &out.OrganizerID, &out.SlotID, &out.TicketTypeID, &out.Quantity, &seats)
	if err != nil {
		return OrderSeats{}, err
	}
	if !seats.Valid {
		out.SeatIdentities = []string{}
		return out, nil
	}
	out.Seated = true
	if err := json.Unmarshal([]byte(seats.String), &out.SeatIdentities); err != nil || len(out.SeatIdentities) == 0 {
		return OrderSeats{}, ErrInvalidOrderSeats
	}
	return out, nil
}
