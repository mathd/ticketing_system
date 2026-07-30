package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/consumer"
	"ticketing/services/inventory/internal/store"
	"ticketing/shared/runtimecfg"
)

// reconcilePinPageSize bounds one page of catalog's pin table. The operator scan is a full
// drain, so the page — not the table — is what bounds memory and each HTTP payload.
const reconcilePinPageSize = 100

// pinReconciler is the injectable core of reconcile-pins (TKT-112); the outer command owns
// environment, Postgres and HTTP wiring. Same shape as quarantineReprocessor.
type pinReconciler struct {
	listPins func(ctx context.Context, after uuid.UUID, limit int) ([]consumer.SeatPin, error)
	liveness func(ctx context.Context, claimIDs []uuid.UUID) (map[uuid.UUID]store.SeatClaimState, error)
	unpin    func(ctx context.Context, org, seatMapID uuid.UUID, seats []string, pinnedBy string) error
}

// pinReconcileStats is the run's report. Every scanned pin lands in exactly one bucket.
type pinReconcileStats struct {
	Scanned   int // pin rows read from catalog
	Reclaimed int // hold pins whose claim is terminal — unpinned
	Live      int // hold pins whose claim still consumes its seats — kept
	Unknown   int // hold pins naming a claim this database has never seen — kept, reported
	Malformed int // pinned_by "hold:<not-a-uuid>" — kept, reported
	Other     int // every non-hold pin (sale:*, anything else) — never touched
}

// holdPinPrefix is the pinned_by namespace an inventory seat hold writes (ADR-029 leaves
// pinned_by free-form; ADR-031 §3 fixes the hold's form as "hold:<claim_id>").
const holdPinPrefix = "hold:"

// unpinGroup is one batch-unpin call: every dead seat of ONE claim under one seat map.
// Grouping by pinned_by is what makes the reclaim safe against a concurrent re-hold — a
// claim id is unique to its hold, so a delete keyed to a dead claim can never touch the pin
// a NEWER hold wrote for the same seat identity. That is also why paging without a snapshot
// is harmless: a pin inserted behind the cursor is simply reclaimed by a later run, never
// wrongly deleted by this one.
type unpinGroup struct {
	org      uuid.UUID
	seatMap  uuid.UUID
	pinnedBy string
	seats    []string
}

// run drains catalog's pin table page by page and reclaims the pins of terminal holds.
//
// Per page: classify, ask inventory for ONE verdict per well-formed hold reference, then
// unpin the dead groups. Three dispositions are deliberately conservative and leave the pin
// in place — live, unknown, and malformed — because a leaked pin fails safe (it blocks a
// map edit) while a wrongly-removed pin lets an edit orphan a seat that is still sold
// (ADR-029/ADR-031). Any list, liveness or unpin failure aborts: the run must never claim
// work it cannot prove, and every delete it did apply is idempotent, so an operator reruns.
func (r pinReconciler) run(ctx context.Context) (pinReconcileStats, error) {
	var stats pinReconcileStats
	after := uuid.Nil
	for {
		page, err := r.listPins(ctx, after, reconcilePinPageSize)
		if err != nil {
			return stats, fmt.Errorf("list catalog pins after %s: %w", after, err)
		}
		if len(page) == 0 {
			return stats, nil
		}
		stats.Scanned += len(page)
		after = page[len(page)-1].ID

		holds := make([]consumer.SeatPin, 0, len(page))
		claimIDs := []uuid.UUID{}
		requested := map[uuid.UUID]bool{}
		for _, p := range page {
			if !strings.HasPrefix(p.PinnedBy, holdPinPrefix) {
				stats.Other++ // sale:* and anything else is not this command's business
				continue
			}
			id, parseErr := uuid.Parse(strings.TrimPrefix(p.PinnedBy, holdPinPrefix))
			if parseErr != nil || id == uuid.Nil {
				// Free-form column, so this is reachable. It cannot be correlated to an
				// inventory claim, so it is reported for a human, never reclaimed.
				stats.Malformed++
				continue
			}
			holds = append(holds, p)
			if !requested[id] {
				requested[id] = true
				claimIDs = append(claimIDs, id)
			}
		}
		if len(holds) == 0 {
			continue
		}

		states, err := r.liveness(ctx, claimIDs)
		if err != nil {
			return stats, fmt.Errorf("claim liveness: %w", err)
		}
		groups := map[string]*unpinGroup{}
		order := []string{}
		for _, p := range holds {
			id, _ := uuid.Parse(strings.TrimPrefix(p.PinnedBy, holdPinPrefix))
			switch states[id] {
			case store.SeatClaimLive:
				stats.Live++
				continue
			case store.SeatClaimUnknown:
				stats.Unknown++
				continue
			case store.SeatClaimDead:
			default:
				// Fail closed. A missing map entry lands here as the empty state, so an
				// incomplete answer from the liveness authority aborts the run instead of
				// being read as "dead" — this is the ONE place that decides an absent
				// verdict is not permission to delete.
				return stats, fmt.Errorf("claim liveness: no usable verdict (%q) for claim %s", states[id], id)
			}
			key := p.OrganizerID.String() + "|" + p.SeatMapID.String() + "|" + p.PinnedBy
			g, ok := groups[key]
			if !ok {
				g = &unpinGroup{org: p.OrganizerID, seatMap: p.SeatMapID, pinnedBy: p.PinnedBy}
				groups[key] = g
				order = append(order, key)
			}
			g.seats = append(g.seats, p.SeatIdentity)
		}
		for _, key := range order {
			g := groups[key]
			if err := r.unpin(ctx, g.org, g.seatMap, g.seats, g.pinnedBy); err != nil {
				return stats, fmt.Errorf("unpin %s on seat map %s: %w", g.pinnedBy, g.seatMap, err)
			}
			stats.Reclaimed += len(g.seats)
		}
	}
}

// reconcilePins is the operator subcommand (TKT-112), completing the remedy ADR-031
// §Consequences deferred: a leaked pin from a hold that expired on a pool nobody touched
// again blocks a now-safe map edit, and until now the only cleanup was calling the internal
// batch-unpin endpoint by hand.
//
// One-shot, never a worker or a scheduled job — ADR-031 §4 refuses one, and this stays a
// thing an operator runs (`docker compose exec inventory /app reconcile-pins`), like migrate
// and reprocess-quarantine (ADR-022).
//
// Exit code: ZERO when pins are left behind on purpose. Unknown and malformed references are
// the expected fail-safe residue, not a failure — the same contract reprocess-quarantine
// states for rows awaiting a newer binary. Non-zero is reserved for a store, transport, or
// unpin failure, so an operator (or a wrapper) can tell "nothing to do" from "it broke".
//
// It reconciles honest writers only (ADR-021): a writer with catalog database access can
// insert or delete pins at will, and nothing here detects that. It guards our own bugs and
// races, not an adversary.
func reconcilePins(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: %s reconcile-pins (no arguments)", serviceName)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	credential, err := runtimecfg.InternalTokenFromEnv()
	if err != nil {
		return err
	}
	catalogURL := strings.TrimRight(os.Getenv("CATALOG_URL"), "/")
	if catalogURL == "" {
		return errors.New("CATALOG_URL required")
	}
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()
	// The TTL is irrelevant here: this command never creates a hold, and the expiry it
	// settles is decided by each claim's own stored expires_at.
	st := store.New(db, time.Minute)
	catalog := consumer.NewCatalogResolver(catalogURL, credential, &http.Client{Timeout: 30 * time.Second})

	stats, runErr := pinReconciler{
		listPins: catalog.ListSeatPins,
		liveness: st.ReconcileSeatClaimStates,
		unpin:    catalog.UnpinSeats,
	}.run(ctx)
	fmt.Printf("%s reconcile-pins: scanned=%d reclaimed=%d live=%d unknown=%d malformed=%d other=%d\n",
		serviceName, stats.Scanned, stats.Reclaimed, stats.Live, stats.Unknown, stats.Malformed, stats.Other)
	if runErr != nil {
		return runErr
	}
	if stats.Unknown > 0 || stats.Malformed > 0 {
		fmt.Printf("%s reconcile-pins: %d pin(s) name a claim this database does not have and %d have a malformed reference; "+
			"both were LEFT IN PLACE on purpose — a stale pin only blocks a seat-map edit, while removing a live one would let an "+
			"edit orphan a sold seat. Investigate before unpinning them by hand.\n", serviceName, stats.Unknown, stats.Malformed)
	}
	return nil
}
