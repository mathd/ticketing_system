package consumer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
)

// Startup offering-state reconciliation (TKT-90).
//
// The stream is the primary convergence path; this pass exists for pools whose
// archive/closure events fell outside stream retention. Semantics (plan-final,
// deliberately narrow):
//   - Positive assertions only: a pool is touched only when catalog answers for it
//     concretely. A festival-kind answer, an unknown id, a non-published non-archived
//     lifecycle, or any error leaves the pool exactly as it was — absence of an
//     answer is never treated as death (a live festival pool 404s the per-performance
//     lookup, which is why the dedicated offer-state endpoint exists).
//   - One write path: drift converges through the same ApplyArchive/ApplyClosure the
//     event path uses (ADR-017: a second, un-dispatched apply path is the trap), with
//     fresh uuid.New() event ids — a deterministic id would dedupe-block the next
//     reconciliation of the same slot forever.
//   - Fail-open: after reconcileAttempts failed passes, readiness latches anyway and
//     the failure is loud. Blocking readiness on catalog would turn a catalog outage
//     into an inventory outage; the pass narrows the fail-open window, it does not
//     close it. Group pools and periodic re-runs are deliberately out of scope.
const reconcileAttempts = 3

// reconcile runs one pass over the live pools. A lookup or apply failure is
// logged, skips only that pool, and fails the pass (so the caller retries);
// non-positive answers skip without failing anything.
func (c *Consumer) reconcile(ctx context.Context) error {
	pools, err := c.st.ListPublishedPoolOfferings(ctx)
	if err != nil {
		return fmt.Errorf("list reconciliation candidates: %w", err)
	}
	var firstErr error
	for _, p := range pools {
		state, err := c.resolver.PoolOfferState(ctx, p.SlotID)
		switch {
		case errors.Is(err, ErrPoolStateNotFound):
			c.log.Error("reconcile: pool unknown to catalog; leaving untouched", "pool", p.SlotID)
			continue
		case err != nil:
			c.log.Error("reconcile: offer-state lookup failed", "pool", p.SlotID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := c.reconcilePool(ctx, p, state); err != nil {
			c.log.Error("reconcile: apply failed", "pool", p.SlotID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (c *Consumer) reconcilePool(ctx context.Context, p store.PoolOffering, state PoolOfferState) error {
	switch {
	case state.Kind == "festival":
		c.log.Info("reconcile: festival pool skipped; group offer state converges via events", "pool", p.SlotID)
		return nil
	case state.Kind != "performance":
		c.log.Error("reconcile: unrecognized kind; leaving untouched", "pool", p.SlotID, "kind", state.Kind)
		return nil
	case state.Lifecycle == "archived":
		c.log.Info("reconcile: archiving pool whose slot died beyond retention", "pool", p.SlotID)
		return c.st.ApplyArchive(ctx, uuid.New(), p.SlotID)
	case state.Lifecycle != "published":
		c.log.Error("reconcile: non-published lifecycle; leaving untouched", "pool", p.SlotID, "lifecycle", state.Lifecycle)
		return nil
	case state.ClosureVersion >= 1 && state.ClosureStatus != p.ClosureStatus:
		// Both directions: a missed closed OR a missed reopened. ApplyClosure's
		// per-slot version guard makes a racing in-flight event with a newer
		// version win regardless of the order they land in.
		c.log.Info("reconcile: converging closure drift", "pool", p.SlotID, "to", state.ClosureStatus, "version", state.ClosureVersion)
		return c.st.ApplyClosure(ctx, uuid.New(), p.SlotID, p.SlotID, state.ClosureStatus == "closed", state.ClosureVersion)
	}
	return nil
}

// startupConverge is Run's pre-readiness step: reconcile (with bounded retries,
// fail-open), then decide readiness exactly as before — the quarantine latch
// still wins over a clean pass. It returns an error only for ctx cancellation
// or a readiness-check DB failure; a catalog outage never exits Run.
func (c *Consumer) startupConverge(ctx context.Context) error {
	for attempt := 1; ; attempt++ {
		err := c.reconcile(ctx)
		if err == nil {
			break
		}
		if attempt >= reconcileAttempts {
			c.log.Error("offering reconciliation failed; starting fail-open (event stream remains the convergence path)",
				"attempts", attempt, "err", err)
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.retryBackoff):
		}
	}
	return c.refreshStartupReadiness(ctx)
}
