package availability

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
type budgetSource struct {
	waitForDeadline bool
	err             error // nil means errServerCancel
}

func (budgetSource) RegisterAvailabilityInvalidator(func(uuid.UUID)) {}

func (b budgetSource) Availability(ctx context.Context, _, _ uuid.UUID, _ string) (store.Availability, error) {
	if b.waitForDeadline {
		<-ctx.Done()
	}
	if b.err != nil {
		return store.Availability{}, b.err
	}
	return store.Availability{}, errServerCancel
}

// The budget, not the error, decides. The same error must be the sentinel when the
// load's deadline passed and must NOT be when the source failed fast: otherwise an
// ordinary store failure would advise a retry (503) and hide a broken query.
func TestLoadBudgetSentinelFollowsTheBudgetNotTheError(t *testing.T) {
	slow := New(budgetSource{waitForDeadline: true}, WithLoadTimeout(20*time.Millisecond))
	_, err := slow.Read(context.Background(), uuid.New(), uuid.New(), "")
	if !errors.Is(err, ErrLoadBudgetExceeded) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errServerCancel) {
		t.Fatalf("a load past its budget returned %v, want ErrLoadBudgetExceeded wrapping the source error", err)
	}

	fast := New(budgetSource{}, WithLoadTimeout(time.Second))
	_, err = fast.Read(context.Background(), uuid.New(), uuid.New(), "")
	if errors.Is(err, ErrLoadBudgetExceeded) || !errors.Is(err, errServerCancel) {
		t.Fatalf("a load that failed within its budget returned %v, want the plain source error", err)
	}
}

// A domain answer is never a slow dependency, however late it arrives (review F1): a slot
// that does not exist stays a 404 even when the query that found out used the whole
// budget. Without this, a slow-but-correct "not found" would advise a retry.
func TestADomainAnswerAfterTheBudgetIsNotTheSentinel(t *testing.T) {
	for _, domain := range []error{
		store.ErrNotFound,
	} {
		svc := New(budgetSource{waitForDeadline: true, err: domain}, WithLoadTimeout(20*time.Millisecond))
		_, err := svc.Read(context.Background(), uuid.New(), uuid.New(), "")
		if errors.Is(err, ErrLoadBudgetExceeded) || !errors.Is(err, domain) {
			t.Fatalf("a late %v became %v, want the domain error unchanged", domain, err)
		}
	}
}
