//go:build smoke

package store

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestReadOrderSeatsDistinguishesUnseatedAndSeated(t *testing.T) {
	db, ctx := outboxDB(t)
	org := uuid.New()
	other := uuid.New()
	seatedOrder, unseatedOrder := uuid.New(), uuid.New()
	seatedReservation, unseatedReservation := uuid.New(), uuid.New()
	identities := []string{"Stalls/A/2", "Stalls/A/1"}
	seatJSON := []byte(`["Stalls/A/2","Stalls/A/1"]`)

	for _, row := range []struct {
		order, reservation uuid.UUID
		seats              any
	}{{seatedOrder, seatedReservation, seatJSON}, {unseatedOrder, unseatedReservation, nil}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO reservations
			(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status,seat_identities)
			VALUES($1,$2,$3,$4,$5,$6,2,100,200,200,'EUR','completed',$7)`,
			row.reservation, org, uuid.New(), uuid.New(), uuid.New(), uuid.New(), row.seats); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
			VALUES($1,$2,'completed',$3,'fingerprint')`, row.order, row.reservation, "seat-read-"+uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ReadOrderSeats(ctx, db, org, seatedOrder)
	if err != nil {
		t.Fatal(err)
	}
	if got.OrderID != seatedOrder || got.OrganizerID != org || got.Quantity != 2 || !got.Seated ||
		got.SeatIdentities == nil || len(got.SeatIdentities) != 2 || got.SeatIdentities[0] != identities[0] || got.SeatIdentities[1] != identities[1] {
		t.Fatalf("seated order = %+v; want the exact stored line and identities", got)
	}

	got, err = ReadOrderSeats(ctx, db, org, unseatedOrder)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seated || got.SeatIdentities == nil || len(got.SeatIdentities) != 0 {
		t.Fatalf("unseated order = %+v; want seated=false and an empty array", got)
	}
	if _, err := ReadOrderSeats(ctx, db, other, seatedOrder); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign organizer read err = %v; want sql.ErrNoRows", err)
	}
	if _, err := ReadOrderSeats(ctx, db, org, uuid.New()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("absent order read err = %v; want sql.ErrNoRows", err)
	}
}
