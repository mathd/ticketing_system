package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

type orderSeats struct {
	OrderID        uuid.UUID `json:"order_id"`
	OrganizerID    uuid.UUID `json:"organizer_id"`
	SlotID         uuid.UUID `json:"slot_id"`
	TicketTypeID   uuid.UUID `json:"ticket_type_id"`
	Quantity       int       `json:"quantity"`
	Seated         bool      `json:"seated"`
	SeatIdentities []string  `json:"seat_identities"`
}

func resolveSeatIdentities(got orderSeats, event completed) ([]string, error) {
	data := event.Data
	if got.OrderID != data.OrderID || got.OrganizerID != data.OrganizerID ||
		got.SlotID != data.SlotID || got.TicketTypeID != data.TicketTypeID ||
		got.Quantity != int(data.Quantity) || got.SeatIdentities == nil {
		return nil, errors.New("commerce order seats do not match completed order")
	}
	if !got.Seated {
		if len(got.SeatIdentities) != 0 {
			return nil, errors.New("unseated commerce order has seat identities")
		}
		return nil, nil
	}
	if len(got.SeatIdentities) != int(data.Quantity) {
		return nil, errors.New("commerce seat count does not match completed order")
	}
	identities := append([]string(nil), got.SeatIdentities...)
	sort.Strings(identities)
	for i, identity := range identities {
		if strings.TrimSpace(identity) == "" || utf8.RuneCountInString(identity) > 200 || (i > 0 && identities[i-1] == identity) {
			return nil, errors.New("commerce seat identities are invalid")
		}
	}
	return identities, nil
}

func (c *Consumer) seats(ctx context.Context, event completed) ([]string, error) {
	url := fmt.Sprintf("%s/internal/orders/%s/seats?organizer_id=%s", c.commerceURL,
		event.Data.OrderID, event.Data.OrganizerID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Internal-Token", c.token)
	res, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("commerce order seats: %d", res.StatusCode)
	}
	var got orderSeats
	decoder := json.NewDecoder(res.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&got); err != nil {
		return nil, fmt.Errorf("decode commerce order seats: %w", err)
	}
	return resolveSeatIdentities(got, event)
}
