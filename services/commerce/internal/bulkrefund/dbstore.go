package bulkrefund

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
)

// DBStore binds Store to the commerce store package. It is a pass-through on purpose —
// every decision lives either in the store's SQL (under a lock) or in the runner (against
// this port), and a layer that did anything of its own here would be a third place to look.
type DBStore struct{ DB *sql.DB }

func (d DBStore) Runs(ctx context.Context, limit int) ([]store.CancellationRun, error) {
	return store.ClaimCancellationRuns(ctx, d.DB, limit)
}

func (d DBStore) Enumerate(ctx context.Context, org, runID uuid.UUID, batch int) (bool, error) {
	return store.EnumerateCancellationBook(ctx, d.DB, org, runID, batch)
}

func (d DBStore) Claim(ctx context.Context, limit int, lease time.Duration) ([]store.CancellationWork, error) {
	return store.ClaimCancellationOrders(ctx, d.DB, limit, lease)
}

func (d DBStore) OrderState(ctx context.Context, org, order uuid.UUID) (store.OrderCancellationState, error) {
	return store.ReadOrderCancellationState(ctx, d.DB, org, order)
}

func (d DBStore) LookupRefund(ctx context.Context, org, refundID uuid.UUID) (store.Refund, bool, error) {
	return store.LookupRefundByID(ctx, d.DB, org, refundID)
}

func (d DBStore) FixQuantity(ctx context.Context, w store.CancellationWork, quantity int32) error {
	return store.FixCancellationRequestedQuantity(ctx, d.DB, w, quantity)
}

func (d DBStore) ClearQuantity(ctx context.Context, w store.CancellationWork) error {
	return store.ClearCancellationRequestedQuantity(ctx, d.DB, w)
}

func (d DBStore) Finalize(ctx context.Context, w store.CancellationWork, out store.CancellationOutcome) error {
	return store.FinalizeCancellationOrder(ctx, d.DB, w, out)
}

func (d DBStore) Abandon(ctx context.Context, w store.CancellationWork) error {
	return store.AbandonCancellationClaim(ctx, d.DB, w)
}

func (d DBStore) CompleteRuns(ctx context.Context) (int, error) {
	return store.CompleteFinishedCancellationRuns(ctx, d.DB)
}
