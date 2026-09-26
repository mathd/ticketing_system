package seatoccupancy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
)

// errServerCancel stands in for what pgx returns when the budget interrupts a query: a
// server-side cancellation that is NOT context.DeadlineExceeded (TKT-211 verified this
// against a real query; a handler matching context.DeadlineExceeded alone answered 500).
var errServerCancel = errors.New("ERROR: canceling statement due to user request (SQLSTATE 57014)")

// budgetSource fails with errServerCancel, either at once or only once the load's own
// deadline has passed.
type budgetSource struct{ waitForDeadline bool }

func (budgetSource) RegisterAvailabilityInvalidator(func(uuid.UUID)) {}

func (b budgetSource) SeatOccupancy(ctx context.Context, _, _ uuid.UUID) (store.SeatOccupancy, error) {
	if b.waitForDeadline {
		<-ctx.Done()
	}
	return store.SeatOccupancy{}, errServerCancel
}

// The budget, not the error, decides. The same error must be the sentinel when the
// load's deadline passed and must NOT be when the source failed fast: otherwise an
// ordinary store failure would advise a retry (503) and hide a broken query.
func TestLoadBudgetSentinelFollowsTheBudgetNotTheError(t *testing.T) {
	slow := New(budgetSource{waitForDeadline: true}, WithLoadTimeout(20*time.Millisecond))
	_, err := slow.Read(context.Background(), uuid.New(), uuid.New())
	if !errors.Is(err, ErrLoadBudgetExceeded) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errServerCancel) {
		t.Fatalf("a load past its budget returned %v, want ErrLoadBudgetExceeded wrapping the source error", err)
	}

	fast := New(budgetSource{}, WithLoadTimeout(time.Second))
	_, err = fast.Read(context.Background(), uuid.New(), uuid.New())
	if errors.Is(err, ErrLoadBudgetExceeded) || !errors.Is(err, errServerCancel) {
		t.Fatalf("a load that failed within its budget returned %v, want the plain source error", err)
	}
}
