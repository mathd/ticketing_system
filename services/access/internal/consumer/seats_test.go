package consumer

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestResolveSeatIdentities(t *testing.T) {
	event := completed{Data: completedData{
		OrderID: uuid.New(), OrganizerID: uuid.New(), SlotID: uuid.New(),
		TicketTypeID: uuid.New(), Quantity: 2,
	}}
	base := orderSeats{
		OrderID: event.Data.OrderID, OrganizerID: event.Data.OrganizerID,
		SlotID: event.Data.SlotID, TicketTypeID: event.Data.TicketTypeID,
		Quantity: 2, Seated: true, SeatIdentities: []string{"Stalls/A/2", "Stalls/A/1"},
	}
	got, err := resolveSeatIdentities(base, event)
	if err != nil || len(got) != 2 || got[0] != "Stalls/A/1" || got[1] != "Stalls/A/2" {
		t.Fatalf("resolve seated identities = %v, %v; want sorted exact set", got, err)
	}

	unseated := base
	unseated.Seated = false
	unseated.SeatIdentities = []string{}
	got, err = resolveSeatIdentities(unseated, event)
	if err != nil || got != nil {
		t.Fatalf("resolve unseated order = %v, %v; want nil association", got, err)
	}

	tests := []struct {
		name string
		edit func(*orderSeats)
	}{
		{"wrong order", func(s *orderSeats) { s.OrderID = uuid.New() }},
		{"wrong organizer", func(s *orderSeats) { s.OrganizerID = uuid.New() }},
		{"wrong slot", func(s *orderSeats) { s.SlotID = uuid.New() }},
		{"wrong ticket type", func(s *orderSeats) { s.TicketTypeID = uuid.New() }},
		{"wrong quantity", func(s *orderSeats) { s.Quantity++ }},
		{"missing array", func(s *orderSeats) { s.SeatIdentities = nil }},
		{"wrong count", func(s *orderSeats) { s.SeatIdentities = []string{"Stalls/A/1"} }},
		{"duplicate", func(s *orderSeats) { s.SeatIdentities = []string{"Stalls/A/1", "Stalls/A/1"} }},
		{"blank", func(s *orderSeats) { s.SeatIdentities = []string{"Stalls/A/1", " "} }},
		{"too long", func(s *orderSeats) { s.SeatIdentities = []string{"Stalls/A/1", string(make([]byte, 201))} }},
		{"unseated with identities", func(s *orderSeats) { s.Seated = false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seats := base
			seats.SeatIdentities = append([]string(nil), base.SeatIdentities...)
			tt.edit(&seats)
			if got, err := resolveSeatIdentities(seats, event); err == nil {
				t.Fatalf("resolve = %v; want refusal", got)
			}
		})
	}

	t.Run("200 multibyte characters accepted", func(t *testing.T) {
		seats := base
		seats.Quantity = 1
		seats.SeatIdentities = []string{strings.Repeat("🎟", 200)}
		event.Data.Quantity = 1
		got, err := resolveSeatIdentities(seats, event)
		if err != nil || len(got) != 1 || got[0] != seats.SeatIdentities[0] {
			t.Fatalf("resolve 200-character identity = %v, %v; want exact identity", got, err)
		}
	})
	t.Run("201 multibyte characters refused", func(t *testing.T) {
		seats := base
		seats.Quantity = 1
		seats.SeatIdentities = []string{strings.Repeat("🎟", 201)}
		event.Data.Quantity = 1
		if got, err := resolveSeatIdentities(seats, event); err == nil {
			t.Fatalf("resolve 201-character identity = %v; want refusal", got)
		}
	})
}
