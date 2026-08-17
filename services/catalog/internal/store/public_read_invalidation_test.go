package store

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"testing"
)

// publicReadEffect classifies every mutating method on the Store interface by
// what it does to the four cached public reads.
//
// This map is the ticket's real risk control. The cache is correct only if every
// write that changes a public answer announces it, and nothing in Go's type
// system makes forgetting impossible. So the classification is pinned: adding a
// method to Store without a decision here fails the build's tests, and the
// decision has to be written down rather than assumed.
//
// "none" is a claim, not a default, and each one below was checked against the
// projection rather than guessed:
//   - CloseSlot/ReopenSlot: publicPerformancesSelect projects p.status, never
//     closure_status. Closure is not in the aggregate at all.
//   - UpdateVenueGACapacity: the query selects v.ga_capacity, but PublicVenue in
//     the contract is {id, name} — capacity never reaches the payload.
//   - CreatePriceRule: these reads carry ticket_types.price_amount; price rules
//     live in another table reached only by ResolveTicketTypePrice, a different
//     endpoint on the never tier.
//   - CreatePayee / CreateSplitSchedule: payout configuration. It reaches the
//     sale only through the fee resolution, an /internal/ read, and never
//     appears in a cached public payload at all.
//   - CreateChannel / UpdateChannel: the sales-channel registry (TKT-235). Same
//     shape as the two above — organizer configuration that no cached public
//     payload carries. The four cached reads project events, seasons, festivals
//     and their ticket types; none of them names a channel. /public/channels
//     serves the registry, but it is deliberately NOT one of the cached reads:
//     it goes straight to the store, so there is no cached entry to invalidate.
//     If it is ever added to the cache, this entry becomes PublicReadAll and the
//     read joins publicReadSource — one change, both halves, or the cache serves
//     a retired channel for a full tier.
//   - CreateFeeRule: same shape, one step further out. fee_rules is reached only
//     by ResolveTicketTypeFees, which is an /internal/ operation and not a public
//     read at all, so no cached public payload can carry a fee.
//   - Draft-creating writes: a draft is not publicly listable, so nothing cached
//     can change until the later lifecycle transition, which IS classified.
var publicReadEffect = map[string]PublicReadScope{
	// Membership and detail both change.
	"CreateTicketType":   PublicReadAll, // can make an already-published slot listable
	"PublishPerformance": PublicReadAll,
	"ArchivePerformance": PublicReadAll,
	"PublishSeries":      PublicReadAll,
	"ArchiveSeries":      PublicReadAll,
	"PublishFestival":    PublicReadAll,
	"ArchiveFestival":    PublicReadAll,

	// Detail only: season membership changes what a season detail returns
	// without changing which events are listable.
	"AttachSeriesToSeason": PublicReadDetail,
	"AttachEventToSeason":  PublicReadDetail,

	// No public effect. Each justified in the doc comment above.
	"CreateVenue":                   0,
	"CreateEvent":                   0,
	"CreatePerformance":             0,
	"CreatePriceRule":               0,
	"CreateFeeRule":                 0,
	"CreatePayee":                   0,
	"CreateSplitSchedule":           0,
	"CreateChannel":                 0,
	"UpdateChannel":                 0,
	"CloseSlot":                     0,
	"ReopenSlot":                    0,
	"CreateSeries":                  0,
	"AttachPerformanceToSeries":     0,
	"CreateSeason":                  0,
	"CreateFestival":                0,
	"AttachDayToFestival":           0,
	"UpdateVenueGACapacity":         0,
	"CreateSeatMap":                 0,
	"AddSeatMapSection":             0,
	"AddSeatMapRow":                 0,
	"AddSeatMapSeat":                0,
	"PublishSeatMap":                0,
	"EditSeatMap":                   0,
	"PinSeat":                       0,
	"PinSeats":                      0,
	"UnpinSeat":                     0,
	"UnpinSeats":                    0,
	"MarkSeatMapEventEmitted":       0,
	"MarkClosureEmitted":            0,
	"MarkPerformanceEventEmitted":   0,
	"MarkPerformanceArchiveEmitted": 0,
}

// readOnlyStoreMethods are the Store methods that only read. Listed so the
// completeness check below can tell "this is a read" from "nobody classified
// this write yet" — the distinction the whole guard exists to force.
var readOnlyStoreMethods = map[string]bool{
	"ListVenues": true, "ListVenueSeatMaps": true, "ListSeatMapVersions": true,
	"ListSeatMapPins": true, "GetSeatMapGeometry": true, "GetTicketType": true,
	"ResolveTicketTypePrice": true, "ResolveTicketTypeFees": true,
	"AuthenticateStaff": true,
	"GetPublishedPerformance": true, "GetPoolOfferState": true,
	"ListPublishedEvents": true, "GetPublishedEvent": true,
	"GetPublishedSeason": true, "GetPublishedFestival": true,
	"RegisterPublicReadInvalidator": true,
	// TKT-222. A pure read, and one that deliberately IGNORES publication state —
	// so it neither invalidates a public read nor participates in one.
	"PerformanceDisplayNames": true,
	// TKT-235. The registry's three reads. ListEnabledChannels backs
	// /public/channels, which is public but NOT cached — it reads through to
	// Postgres on every request, so it participates in no cached payload.
	"GetChannel": true, "ListChannels": true, "ListEnabledChannels": true,
	// TKT-243. An operator sweep over price and fee rule currencies. A pure
	// read, and one that touches no published payload — it reports
	// misconfiguration to a CLI, so it neither invalidates a public read nor
	// participates in one.
	"ListRuleCurrencyMismatches": true,
}

// TestEveryStoreMethodIsClassifiedForPublicReads is the anti-rot guard.
//
// What it does NOT do (ADR-021 — name the adversary): it stops an honest
// omission. It does not stop someone editing this map in the same commit, and it
// says nothing about direct SQL, which bypasses every callback in this package.
//
// It deliberately checks CLASSIFICATION COMPLETENESS only, not that each
// classified write actually reaches the notify helper. Proving reachability
// would mean call-graph analysis through unexported workers (toggleClosure,
// transitionSeries, attachSeasonMember), and a guard hard enough to get wrong is
// a poor guard. The behavioural tests cover that half.
func TestEveryStoreMethodIsClassifiedForPublicReads(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "store.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var methods []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Store" {
			return true
		}
		iface, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			return false
		}
		for _, m := range iface.Methods.List {
			for _, name := range m.Names {
				methods = append(methods, name.Name)
			}
		}
		return false
	})

	// A scan that finds no interface is a test that cannot fail.
	if len(methods) < 20 {
		t.Fatalf("found %d methods on store.Store — the AST walk is not reaching the interface", len(methods))
	}

	var unclassified, stale []string
	for _, m := range methods {
		if readOnlyStoreMethods[m] {
			continue
		}
		if _, ok := publicReadEffect[m]; !ok {
			unclassified = append(unclassified, m)
		}
	}
	for m := range publicReadEffect {
		if !slices.Contains(methods, m) {
			stale = append(stale, m)
		}
	}

	sort.Strings(unclassified)
	sort.Strings(stale)
	if len(unclassified) > 0 {
		t.Errorf("store.Store methods with no public-read classification: %v\n"+
			"Every write must declare what it does to the four cached public reads — "+
			"PublicReadAll, PublicReadDetail, or 0 with a reason in the doc comment. "+
			"If it only reads, add it to readOnlyStoreMethods.", unclassified)
	}
	if len(stale) > 0 {
		t.Errorf("classified methods that no longer exist on store.Store: %v — "+
			"a stale entry hides the fact that nothing is enforcing it", stale)
	}
}

// TestCommitPublicReadOrdersInvalidationAfterCommit is the ordering rule.
//
// Invalidating BEFORE the commit lets a concurrent read repopulate the entry
// from the pre-write row, so the cache serves the old answer for a full
// five-minute tier — the defect this seam removes, reintroduced by its own
// mechanism. ADR-018 sets the same rule for catalog's event emission.
func TestCommitPublicReadOrdersInvalidationAfterCommit(t *testing.T) {
	t.Run("success invalidates after the commit", func(t *testing.T) {
		var order []string
		p := &Postgres{}
		p.RegisterPublicReadInvalidator(func(got PublicReadScope) {
			if got != PublicReadAll {
				t.Errorf("scope = %v, want PublicReadAll", got)
			}
			order = append(order, "invalidate")
		})
		err := p.commitPublicRead(commitFunc(func() error {
			order = append(order, "commit")
			return nil
		}), PublicReadAll)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"commit", "invalidate"}; !slices.Equal(order, want) {
			t.Fatalf("order = %v, want %v", order, want)
		}
	})

	t.Run("a failed commit invalidates nothing", func(t *testing.T) {
		called := 0
		p := &Postgres{}
		p.RegisterPublicReadInvalidator(func(PublicReadScope) { called++ })
		boom := &fakeCommitError{}
		if err := p.commitPublicRead(commitFunc(func() error { return boom }), PublicReadAll); !errors.Is(err, boom) {
			t.Fatalf("got %v, want the commit error unwrapped", err)
		}
		if called != 0 {
			t.Fatalf("invalidator called %d times after a failed commit, want 0", called)
		}
	})

	t.Run("no invalidator registered is not a panic", func(t *testing.T) {
		p := &Postgres{}
		if err := p.commitPublicRead(commitFunc(func() error { return nil }), PublicReadAll); err != nil {
			t.Fatal(err)
		}
		p.notifyPublicRead(PublicReadList) // the autocommit path, same expectation
	})
}

// TestPublicReadScopeHas pins the bitmask, which decides whether a write dumps
// the list, the details, or both.
func TestPublicReadScopeHas(t *testing.T) {
	if !PublicReadAll.Has(PublicReadList) || !PublicReadAll.Has(PublicReadDetail) {
		t.Fatal("PublicReadAll must cover both scopes")
	}
	if PublicReadDetail.Has(PublicReadList) {
		t.Fatal("a detail-only write must not report the list scope — it would dump the list on every season attach")
	}
	if PublicReadScope(0).Has(PublicReadList) || PublicReadScope(0).Has(PublicReadDetail) {
		t.Fatal("the zero scope must cover nothing")
	}
}

type commitFunc func() error

func (f commitFunc) Commit() error { return f() }

type fakeCommitError struct{}

func (*fakeCommitError) Error() string { return "commit failed" }
