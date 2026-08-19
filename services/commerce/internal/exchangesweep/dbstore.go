package exchangesweep

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

func (d DBStore) Claim(ctx context.Context, limit int, lease time.Duration) ([]store.ClaimedExchangeReversal, error) {
	return store.ClaimOutstandingExchangeReversals(ctx, d.DB, limit, lease)
}

func (d DBStore) Release(ctx context.Context, org, exchangeID, claimID uuid.UUID, switchedAtClaim, capacityAtClaim bool, cause string) error {
	return store.ReleaseExchangeReversalClaim(ctx, d.DB, org, exchangeID, claimID, switchedAtClaim, capacityAtClaim, cause)
}

func (d DBStore) Finish(ctx context.Context, org, exchangeID, claimID uuid.UUID) error {
	return store.FinishExchangeReversalClaim(ctx, d.DB, org, exchangeID, claimID)
}

func (d DBStore) Abandon(ctx context.Context, org, exchangeID, claimID uuid.UUID) error {
	return store.AbandonExchangeReversalClaim(ctx, d.DB, org, exchangeID, claimID)
}

func (d DBStore) Backlog(ctx context.Context) (store.ExchangeReversalBacklog, error) {
	return store.ReadExchangeReversalBacklog(ctx, d.DB)
}
