// Package exchangesweep drives outstanding exchange capacity obligations to completion
// (TKT-259, ADR-063).
//
// ADR-062 closed this gap for REFUNDS and named exchanges as its deliberate omission:
// `order_exchanges` carries two obligations of identical shape and nothing in commerce ever
// swept them. Their only retry was access's JetStream redelivery, driven by the
// tickets-switched callback answering 502 when the capacity return was unresolved. That
// works while the consumer is healthy; if it dead-letters, or the `order.exchanged` event is
// never consumed, nothing revisits the row and the source capacity stays out of sale
// forever. This is the backstop.
//
// The 502-and-redelivery path REMAINS the first line: it discharges in milliseconds on the
// happy path, against a sweep interval of a minute. This runner exists for the rows that
// path gave up on.
//
// It copies internal/reversal's lifecycle — claim under a lease, work outside the
// transaction, release what it did not finish — rather than extending it. reversal.Store
// and reversal.Reverser are typed on store.Refund and refunds.Service.DriveReversal, so
// there is nothing to hand them for an exchange; and ADR-062 §1 already took this exact
// decision for this exact reason when it declined to extend internal/recovery or
// internal/bulkrefund. The SHAPE is reused; the state machine is not shared.
//
// # The one thing this runner must never do
//
// It never writes `tickets_exchanged_at`. Only access can establish that the old tickets
// stopped admitting, and migration 0011's CHECK gates the capacity return on that marker. A
// sweep that set the marker itself would assert a fact about another service's state in
// order to unlock the one write that can OVERSELL (ADR-038 §1). So a settled exchange
// awaiting its switch is claimed, observed, counted and released — visible and monitored,
// never completed. That is COS 1's second branch; a switched exchange owing only capacity is
// its first. The guarantee is structural: the Discharger port cannot express the marker.
package exchangesweep

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
	"ticketing/services/commerce/internal/worklease"
)

// Store is the durable state the runner decides against. A port rather than a *sql.DB so
// the decision table can be exercised against fakes; the SQL predicates it stands for —
// the five claim conjuncts, the lease, the claim fence, the backoff, parking — are covered
// against real PostgreSQL by the store's smoke tests, because that is the tier those
// mechanisms live at. A fake enforcing them in Go would prove only that the fake and the
// runner agree.
type Store interface {
	Claim(ctx context.Context, limit int, lease time.Duration) ([]store.ClaimedExchangeReversal, error)
	// Release takes the obligations as OBSERVED AT CLAIM TIME and decides progress in SQL
	// against the row as it stands. It does not take a `progressed bool`: this runner does
	// not hold a monopoly on discharging an exchange — the callback drives the same
	// obligation whenever access redelivers — so a verdict computed from its own
	// before/after would be wrong exactly when a concurrent callback helped.
	Release(ctx context.Context, org, exchangeID, claimID uuid.UUID, switchedAtClaim, capacityAtClaim bool, cause string) error
	Finish(ctx context.Context, org, exchangeID, claimID uuid.UUID) error
	Abandon(ctx context.Context, org, exchangeID, claimID uuid.UUID) error
	Backlog(ctx context.Context) (store.ExchangeReversalBacklog, error)
}

// Discharger is the shared exchange discharge unit (internal/exchanges). Only the one
// method: this runner must never be able to move money or mark a switch, and a port that
// cannot express either is a stronger guarantee than a comment saying it does not.
type Discharger interface {
	DriveExchange(ctx context.Context, ex store.ExchangeSwitch) store.ExchangeSwitch
}

// MaxCallsPerExchange is the longest external-call chain one exchange can drive: the
// inventory capacity return, and nothing else. LeaseFor derives from it, so the lease and
// the chain grow together rather than drifting apart.
//
// It is ONE, where the refund side's is two — a refund also calls access to void tickets,
// which for an exchange is access's own job and never commerce's. That difference is the
// reason this constant exists rather than being borrowed.
const MaxCallsPerExchange = 1

// LeaseFor sizes a sequential batch from the exchange service client's timeout.
func LeaseFor(batch int, callTimeout time.Duration) (time.Duration, error) {
	if batch <= 0 {
		batch = 1
	}
	if callTimeout <= 0 {
		callTimeout = 30 * time.Second
	}
	return worklease.ForBatch(batch, MaxCallsPerExchange, callTimeout, 60*time.Second)
}

// Runner reconciles outstanding exchange obligations.
type Runner struct {
	store      Store
	discharger Discharger
	interval   time.Duration
	batch      int
	lease      time.Duration
	log        *slog.Logger
}

func New(st Store, d Discharger, interval time.Duration, batch int, lease time.Duration, log *slog.Logger) *Runner {
	if interval <= 0 {
		interval = time.Minute
	}
	if batch <= 0 {
		batch = 16
	}
	if log == nil {
		log = slog.Default()
	}
	return &Runner{store: st, discharger: d, interval: interval, batch: batch, lease: lease, log: log}
}

// Run drives until ctx is cancelled, starting with one pass immediately: on restart, the
// obligations stranded by the process that died are the whole point, and waiting an interval
// to notice them leaves source capacity out of sale for no reason.
func (r *Runner) Run(ctx context.Context) {
	r.RunOnce(ctx)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.RunOnce(ctx)
		}
	}
}

// MaxBatchesPerPass bounds one drain. A pass drains rather than doing one batch per tick,
// because after an outage the backlog is deepest exactly when waiting an interval per batch
// is least affordable. But an UNBOUNDED drain is two defects at once: the per-pass `driven`
// set grows for as long as the loop runs, and a workload arriving at or above processing
// rate means the loop never returns at all — so the set grows for the life of the process
// and the ticker never fires again.
//
// What is left undrained is not lost: it is claimable, and the next tick is a minute away at
// most. The number is deliberately generous — 64 batches of 16 is 1024 exchanges per pass —
// because the common case is a backlog far smaller than one batch.
const MaxBatchesPerPass = 64

// key identifies a claimed row for the per-pass duplicate check.
//
// It is the FULL COMPOSITE (organizer, exchange), not the exchange id alone: `id` is not
// unique by schema — `order_exchanges`' primary key is (organizer_id, id) — so keying on it
// alone would let one tenant's row suppress another tenant's genuinely distinct work for the
// rest of the pass. Same reasoning the SQL uses for matching on the full composite.
type key struct{ org, id uuid.UUID }

// RunOnce drains the claimable backlog in bounded batches. Returns how many exchanges it
// drove to completion, for tests and for callers draining to quiescence.
//
// ONE DRIVE PER EXCHANGE PER PASS. A released row becomes due again after its progress floor
// or its backoff — both of which can be shorter than the time the rest of a slow batch takes
// — so a drain loop that only re-claimed would happily pick the same row up again in a later
// batch of the same pass. At the extreme that lets one row spend its whole attempt budget and
// PARK inside a single RunOnce, which is the opposite of what a bounded budget spread over
// passes is for.
func (r *Runner) RunOnce(ctx context.Context) int {
	var resolved int
	driven := make(map[key]struct{})
	for pass := 0; pass < MaxBatchesPerPass; pass++ {
		claimed, err := r.store.Claim(ctx, r.batch, r.lease)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				r.log.ErrorContext(ctx, "claim outstanding exchange reversals", "err", err)
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
			// lease. The lease exists to survive a crash, not to be the cost of an orderly
			// restart.
			if ctx.Err() != nil {
				r.abandonUndriven(claimed[i:])
				return resolved
			}
			k := key{c.Exchange.OrganizerID, c.Exchange.ID}
			if _, seen := driven[k]; seen {
				// Already driven this pass. Hand the claim straight back — undriven, so it
				// costs no attempt. It stays DUE, so the store can offer it again
				// immediately: Abandon clears the lease and the token and deliberately does
				// not touch next_attempt_at. That is why a duplicate must not merely be
				// skipped — a batch made entirely of duplicates would otherwise spin. The
				// `fresh == 0` exit below is what stops it.
				//
				// On the CALLER's context, not abandonUndriven's detached one: that exists
				// because a shutdown's context is already cancelled, and reusing it here
				// would both mislabel this as a shutdown and let a degraded database burn a
				// 5s timeout per duplicate that cancellation cannot interrupt. The ctx.Err()
				// check above is NOT atomic with this call: a shutdown landing between them,
				// or while the write is in flight, fails it on a cancelled context — and
				// logging and moving on would leave the row leased for the full lease with
				// its obligation outstanding the whole time. So a cancellation here falls
				// back to the detached path, which is what that path exists for.
				if err := r.store.Abandon(ctx, c.Exchange.OrganizerID, c.Exchange.ID, c.ClaimID); err != nil {
					if ctx.Err() != nil {
						r.abandonUndriven(claimed[i : i+1])
					} else {
						r.log.WarnContext(ctx, "hand back an exchange claim already driven this pass",
							"exchange_id", c.Exchange.ID, "err", err)
					}
				}
				continue
			}
			driven[k] = struct{}{}
			fresh++
			if r.drive(ctx, c) {
				resolved++
			}
		}
		// A batch that was full but contained nothing new means the queue is now just this
		// pass's own releases coming back round; stop rather than spin. This can end a drain
		// while genuinely new work sorts behind those duplicates — accepted, and bounded:
		// the claim is ordered by next_attempt_at, a duplicate's is in the past and a fresh
		// row's is at most a minute out, so the next tick reaches them. The obligations are
		// under-selling while they wait, never over-selling.
		if len(claimed) < r.batch || fresh == 0 {
			return resolved
		}
	}
	r.log.InfoContext(ctx, "exchange sweep drain hit its per-pass bound; the rest waits for the next tick",
		"batches", MaxBatchesPerPass, "driven", len(driven))
	return resolved
}

// drive discharges what it can of one exchange's obligation and records the outcome. It
// reports whether the exchange is now COMPLETE.
func (r *Runner) drive(ctx context.Context, c store.ClaimedExchangeReversal) bool {
	after := r.discharger.DriveExchange(ctx, c.Exchange)

	if after.TicketsExchanged && after.CapacityReturned {
		if err := r.store.Finish(ctx, after.OrganizerID, after.ID, c.ClaimID); err != nil {
			r.log.ErrorContext(ctx, "finish exchange claim", "exchange_id", after.ID, "err", err)
		}
		return true
	}

	// Still outstanding as far as THIS claimant can see. Whether the row actually made
	// progress is decided by the store, against the row as it stands, from the obligations
	// observed at claim time — because a concurrent tickets-switched callback can discharge
	// an obligation without this runner's knowledge, and calling that "no progress" would
	// park a recovering exchange.
	//
	// Commerce cannot see WHY inventory refused, so a permanently undischargeable obligation
	// is recognised by making no progress, never by predicting the refusal (ADR-062 §2's
	// general argument; its seated-partial case does not apply to exchanges, which are
	// whole-line and never seated).
	cause := "capacity return outstanding"
	if !after.TicketsExchanged {
		// The sweep cannot discharge this one: the switch is access's fact. The row is
		// counted (Backlog.AwaitingSwitch) and left visible rather than driven.
		//
		// The store decides what that COSTS, and the answer is nothing: an awaiting-switch
		// row keeps its budget and never parks, because parking a row for a condition this
		// service cannot influence would exclude it from the claim predicate forever — and
		// the capacity would be stranded by the mechanism added to prevent that (ai-review
		// F1). The decision lives in SQL, keyed on the row's own state rather than on this
		// cause string, so a mis-typed cause cannot change what a row is charged.
		cause = "awaiting access switch confirmation"
	}
	if err := r.store.Release(ctx, c.Exchange.OrganizerID, c.Exchange.ID, c.ClaimID,
		c.Exchange.TicketsExchanged, c.Exchange.CapacityReturned, cause); err != nil {
		r.log.ErrorContext(ctx, "release exchange claim", "exchange_id", c.Exchange.ID, "err", err)
	}
	return false
}

// abandonUndriven hands back claims the pass never got to, so the next pass (or the next
// boot) picks them up immediately rather than waiting out the lease.
//
// It uses a fresh, bounded context: the caller's is already cancelled, so reusing it would
// fail every one of these writes and defeat the point.
func (r *Runner) abandonUndriven(claims []store.ClaimedExchangeReversal) {
	if len(claims) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var released int
	for _, c := range claims {
		// Conditional on the claim token in SQL, so a lease that lapsed and was re-claimed
		// by a successor mid-shutdown is left alone.
		if err := r.store.Abandon(ctx, c.Exchange.OrganizerID, c.Exchange.ID, c.ClaimID); err != nil {
			r.log.WarnContext(ctx, "abandon undriven exchange claim", "exchange_id", c.Exchange.ID, "err", err)
			continue
		}
		released++
	}
	r.log.InfoContext(ctx, "released undriven exchange claims on shutdown",
		"released", released, "of", len(claims))
}
