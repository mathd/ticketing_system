package reversal

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
)

// DBStore binds Store to the commerce store package. A pass-through on purpose — every
// decision lives either in the store's SQL (under the claim's row lock) or in the runner
// (against this port), and a layer doing anything of its own here would be a third place to
// look for behaviour.
type DBStore struct{ DB *sql.DB }

func (d DBStore) Claim(ctx context.Context, limit int, lease time.Duration) ([]store.ClaimedReversal, error) {
	return store.ClaimOutstandingReversals(ctx, d.DB, limit, lease)
}

func (d DBStore) Release(ctx context.Context, refundID, claimID uuid.UUID, progressed bool, cause string) error {
	return store.ReleaseReversalClaim(ctx, d.DB, refundID, claimID, progressed, cause)
}

func (d DBStore) Finish(ctx context.Context, refundID, claimID uuid.UUID) error {
	return store.FinishReversalClaim(ctx, d.DB, refundID, claimID)
}

func (d DBStore) Abandon(ctx context.Context, refundID, claimID uuid.UUID) error {
	return store.AbandonReversalClaim(ctx, d.DB, refundID, claimID)
}

func (d DBStore) Backlog(ctx context.Context) (store.ReversalBacklog, error) {
	return store.ReadReversalBacklog(ctx, d.DB)
}
