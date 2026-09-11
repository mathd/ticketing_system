package worklease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Adapter connects the generic lease-lifecycle loop to a domain runner's claim,
// drive, and hand-back operations.
//
// Key is a type parameter because runners differ in deduplication identity: some
// deduplicate on a bare UUID, while others deduplicate on a composite {org, id} key.
type Adapter[T any, K comparable] struct {
	Claim             func(ctx context.Context, limit int, lease time.Duration) ([]T, error)
	Key               func(T) K
	Process           func(ctx context.Context, row T) bool  // returns: resolved
	Abandon           func(ctx context.Context, row T) error // hand back undriven, on the GIVEN ctx
	Batch             int
	Lease             time.Duration
	MaxBatchesPerPass int
	Log               *slog.Logger
	Name              string // for log messages, e.g. "reversal"
}

// Drain runs a bounded multi-batch lease-claim loop over outstanding work items.
//
// ONE DRIVE PER ITEM PER PASS. A released row becomes due again after its progress
// floor or its backoff — both of which can be shorter than the time the rest of a
// slow batch takes — so a drain loop that only re-claimed would happily pick the
// same row up again in a later batch of the same pass. At the extreme that lets one
// row spend its whole attempt budget and PARK inside a single pass, which is the
// opposite of what a bounded budget spread over passes is for.
//
// `driven` is per-pass, bounded by MaxBatchesPerPass × Batch, and discarded on return.
// HandBackUndriven returns claims a pass never got to, so the next pass (or the next
// boot) picks them up immediately rather than waiting out the lease.
//
// It uses a FRESH, BOUNDED context rather than the caller's. This is the whole point of
// the function and not a detail: it is called when the caller's context is already
// cancelled, so reusing that context would fail every one of these writes and leave the
// rows leased for the full lease — minutes of nothing happening, for work that is
// already overdue. The lease exists to survive a crash, not to be the cost of an orderly
// restart.
//
// `abandon` must be conditional on the claim token in SQL, so a lease that lapsed and
// was re-claimed by a successor mid-shutdown is left alone.
//
// Exported because it is shared by two shapes of runner: the draining loop below, and
// single-batch runners that do not drain at all. Those runners share this mechanism and
// the per-row cancellation check, and nothing else — giving them the whole loop would
// hand them a duplicate map, a freshness count and a pass bound that can never fire.
func HandBackUndriven[T any](claims []T, abandon func(context.Context, T) error, log *slog.Logger, name string) {
	if len(claims) == 0 {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var released int
	for _, c := range claims {
		if err := abandon(ctx, c); err != nil {
			log.WarnContext(ctx, fmt.Sprintf("abandon undriven %s claim", name), "err", err)
			continue
		}
		released++
	}
	log.InfoContext(ctx, fmt.Sprintf("released undriven %s claims on shutdown", name),
		"released", released, "of", len(claims))
}

func Drain[T any, K comparable](ctx context.Context, a Adapter[T, K]) int {
	log := a.Log
	if log == nil {
		log = slog.Default()
	}

	abandonUndriven := func(claims []T) {
		HandBackUndriven(claims, a.Abandon, log, a.Name)
	}

	var resolved int
	driven := make(map[K]struct{})
	for pass := 0; pass < a.MaxBatchesPerPass; pass++ {
		claimed, err := a.Claim(ctx, a.Batch, a.Lease)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.ErrorContext(ctx, fmt.Sprintf("claim outstanding %s", a.Name), "err", err)
			}
			return resolved
		}
		if len(claimed) == 0 {
			return resolved
		}
		var fresh int
		for i, c := range claimed {
			// Checked per ROW, not per batch: an interrupted pass must leave the rest of
			// its claim immediately reclaimable rather than parking it behind the full
			// lease — with a big batch that is minutes of nothing happening, for
			// obligations that are already overdue. The lease exists to survive a crash,
			// not to be the cost of an orderly restart.
			if ctx.Err() != nil {
				abandonUndriven(claimed[i:])
				return resolved
			}
			k := a.Key(c)
			if _, seen := driven[k]; seen {
				// Already driven this pass. Hand the claim straight back — undriven, so it
				// costs no attempt.
				//
				// It stays DUE, so the store can offer it again immediately: `Abandon`
				// clears the lease and the token and deliberately does not touch
				// next_attempt_at, since the row was never tried. That is why a duplicate
				// must not merely be skipped — a batch made entirely of duplicates would
				// otherwise spin. The `fresh == 0` exit below is what stops it, and the
				// bound above is what stops everything else.
				//
				// On the CALLER's context, not abandonUndriven's detached one: that exists
				// because a shutdown's context is already cancelled, and reusing it here
				// would both mislabel this as a shutdown and let a degraded database burn a
				// 5s timeout per duplicate that cancellation cannot interrupt.
				// The ctx.Err() check above is NOT atomic with this call: a shutdown landing
				// between them, or while the write is in flight, fails it on a cancelled
				// context. Logging and moving on would leave the row
				// leased for the full lease with its obligation outstanding the whole time.
				// So a cancellation here falls back to the detached path, which is exactly
				// the case that path exists for.
				if err := a.Abandon(ctx, c); err != nil {
					if ctx.Err() != nil {
						abandonUndriven(claimed[i : i+1])
					} else {
						log.WarnContext(ctx, fmt.Sprintf("hand back a %s claim already driven this pass", a.Name),
							"key", k, "err", err)
					}
				}
				continue
			}
			driven[k] = struct{}{}
			fresh++
			if a.Process(ctx, c) {
				resolved++
			}
		}
		// A batch that was full but contained nothing new means the queue is now just this
		// pass's own releases coming back round; stop rather than spin.
		//
		// This can end a drain while genuinely new work sorts behind those duplicates —
		// accepted, and bounded in cost: the claim is ordered by next_attempt_at, a
		// duplicate's is in the past and a fresh row's is at most a minute out, so the next
		// tick reaches them. Trading a bounded delay for a loop that cannot spin is the
		// right side of that: the obligations are under-selling while they wait, never
		// over-selling.
		if len(claimed) < a.Batch || fresh == 0 {
			return resolved
		}
	}
	log.InfoContext(ctx, fmt.Sprintf("%s drain hit its per-pass bound; the rest waits for the next tick", a.Name),
		"batches", a.MaxBatchesPerPass, "driven", len(driven))
	return resolved
}
