//go:build smoke

package smoke_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"ticketing/smoke/internal/loadtest"
)

// TKT-207: the on-sale READ-load proof — epic TKT-31's first COS, and the only
// one that is evidence rather than behaviour.
//
// ADR-004's Consequences promised "read load during on-sales scales with
// cache/memory, not database". TKT-205 (inventory availability, seconds tier)
// and TKT-206 (catalog public reads, minutes tier) made that true. This proves
// it, and — the part that matters — proves it can FAIL: the control arm disables
// both caches through TKT-210's switch and requires the same predicate to trip.
//
// The evidence is server-side: pg_stat_statements call counts for the exact
// statements those reads execute. Latency is recorded but never asserted; it
// would say nothing about where an answer came from.
//
// WHAT THIS DOES NOT PROVE, and the test says so because a green gate is easy to
// over-read:
//   - Not the 300-second expiry cadence. A two-second stage cannot observe it;
//     that stays with the caches' clock-driven unit tests.
//   - Not replica behaviour, HTTP throughput, or bounded memory. One process,
//     one Compose stack.
//   - Not that the answers are semantically fresh — only that serving them did
//     not touch the database once per request.

// readEndpoint is one buyer-facing read under measurement.
//
// Two endpoints, chosen to span both services and both cached tiers rather than
// to be exhaustive: inventory's availability is the seconds-tier read under real
// on-sale contention, and catalog's public event list is the minutes-tier read
// every storefront page view lands on.
type readEndpoint struct {
	name string
	url  string
	// db is which database's pg_stat_statements rows to count.
	db string
	// fragment matches the normalized statement text. Distinctive enough that
	// exactly one queryid matches — asserted, because a fragment matching zero
	// statements would make the endpoint look permanently flat.
	fragment string
	// statementsPerLoad: one logical read is not one statement. Inventory's
	// default-channel availability executes two — the pool/claims query and then
	// reservedForChannelsSQL.
	statementsPerLoad int
	tier              time.Duration
}

func readEndpoints(slot, organizer string) []readEndpoint {
	return []readEndpoint{
		{
			name:              "inventory availability",
			url:               fmt.Sprintf("%s/api/inventory/slots/%s/availability?organizer_id=%s", gatewayURL, slot, organizer),
			db:                "inventory",
			fragment:          "%FROM inventory_pools WHERE slot_id=%",
			statementsPerLoad: 1,
			tier:              5 * time.Second,
		},
		{
			name:              "catalog public event list",
			url:               gatewayURL + "/api/catalog/public/events?locale=en",
			db:                "catalog",
			fragment:          "%FROM performances p%JOIN events e%",
			statementsPerLoad: 1,
			tier:              5 * time.Minute,
		},
	}
}

func catalogAdminConn(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), fmt.Sprintf("postgres://postgres:postgres@%s/catalog", pgHostPort))
	if err != nil {
		t.Fatalf("catalog admin connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(t.Context()) })
	return conn
}

// statementCalls reads the cumulative call count for one statement family.
//
// Scoped to the current database, because pg_stat_statements is cluster-wide and
// two services' statements live in one view. Requires EXACTLY ONE matching
// queryid: zero would make the endpoint look permanently flat (the failure this
// whole proof exists to avoid), and more than one means the fragment is
// ambiguous and the number means nothing.
func statementCalls(t *testing.T, conn *pgx.Conn, fragment string) int64 {
	t.Helper()
	var ids, calls int64
	err := conn.QueryRow(t.Context(), `
		SELECT COUNT(DISTINCT queryid), COALESCE(SUM(calls), 0)
		FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND query LIKE $1`, fragment).Scan(&ids, &calls)
	if err != nil {
		t.Fatalf("statement calls for %q: %v", fragment, err)
	}
	if ids != 1 {
		t.Fatalf("fragment %q matched %d distinct statements, want exactly 1 — "+
			"zero would make this endpoint look permanently flat, and more than one makes the count meaningless",
			fragment, ids)
	}
	return calls
}

// buyerRead is one GET, recorded in the Hold slot so RunStage needs no
// read-specific branch; ReadReport renames it at the report layer.
func buyerRead(t *testing.T, url string) loadtest.AttemptFunc {
	return func(_ loadtest.Stage, _ int) loadtest.Outcome {
		t0 := time.Now()
		resp, err := loadClient.Get(url)
		d := time.Since(t0)
		if err != nil {
			return loadtest.Outcome{Kind: loadtest.KindClientError, Note: err.Error(), Hold: d}
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return loadtest.Outcome{Kind: loadtest.KindServerError,
				Note: fmt.Sprintf("%d %s", resp.StatusCode, snippet(body)), Hold: d}
		}
		return loadtest.Outcome{Kind: loadtest.KindOK, Hold: d, Lifecycle: d}
	}
}

func snippet(b []byte) string {
	if len(b) > 120 {
		return string(b[:120])
	}
	return string(b)
}

// runReadStage drives one stage and returns its result plus per-endpoint query
// deltas, measured across exactly that stage.
func runReadStage(t *testing.T, name string, rate int, dur time.Duration,
	eps []readEndpoint, conns map[string]*pgx.Conn) (loadtest.StageResult, []loadtest.ReadQueryEvidence) {
	t.Helper()

	before := make([]int64, len(eps))
	for i, ep := range eps {
		before[i] = statementCalls(t, conns[ep.db], ep.fragment)
	}

	// Round-robin across endpoints so each gets a deterministic share.
	var results []loadtest.StageResult
	for _, ep := range eps {
		results = append(results, loadtest.RunStage(
			loadtest.Stage{Name: name + "/" + ep.name, Rate: rate, Duration: dur, Quantity: 1},
			32, buyerRead(t, ep.url)))
	}

	merged := loadtest.StageResult{Stage: loadtest.Stage{Name: name, Rate: rate, Duration: dur}}
	var evidence []loadtest.ReadQueryEvidence
	for i, r := range results {
		merged.Offered += r.Offered
		merged.Started += r.Started
		merged.Dropped += r.Dropped
		merged.OK += r.OK
		merged.ClientErrors += r.ClientErrors
		merged.ServerErrors += r.ServerErrors
		merged.Hold = append(merged.Hold, r.Hold...)
		merged.Elapsed += r.Elapsed
		if r.Elapsed > 0 {
			ep := eps[i]
			after := statementCalls(t, conns[ep.db], ep.fragment)
			evidence = append(evidence, loadtest.ReadQueryEvidence{
				Endpoint:     ep.name,
				TierSeconds:  int(ep.tier.Seconds()),
				StoreQueries: int(after - before[i]),
				MaxAllowed:   loadtest.MaxStoreQueries(r.Elapsed, ep.tier, ep.statementsPerLoad),
				Requests:     r.OK,
			})
		}
	}
	return merged, evidence
}

// readProof is called by the gate profile after the claim work, so no write is
// in flight against the slot being read. That ordering is load-bearing rather
// than tidy: every claim invalidates the availability cache by design (TKT-205),
// so reads racing claims would count writes, not requests, and the flat
// assertion would fail while the cache worked perfectly.
func readProof(t *testing.T, report *loadtest.Report, slot, organizer string, invConn *pgx.Conn) {
	t.Helper()
	eps := readEndpoints(slot, organizer)
	conns := map[string]*pgx.Conn{"inventory": invConn, "catalog": catalogAdminConn(t)}
	statStatementsSetup(t, conns["catalog"])

	// Pre-warm: every endpoint has an entry before measurement starts, so the
	// bound's +1 covers only the pre-warm/stage boundary.
	for _, ep := range eps {
		if out := buyerRead(t, ep.url)(loadtest.Stage{}, 0); out.Kind != loadtest.KindOK {
			t.Fatalf("pre-warm %s: %s %s", ep.name, out.Kind, out.Note)
		}
	}

	low, lowEv := runReadStage(t, "cached-low", 6, 2*time.Second, eps, conns)
	high, highEv := runReadStage(t, "cached-high", 30, 2*time.Second, eps, conns)

	for _, stage := range []struct {
		r  loadtest.StageResult
		ev []loadtest.ReadQueryEvidence
	}{{low, lowEv}, {high, highEv}} {
		rr := stage.r.ReadReport()
		rr.Queries = stage.ev
		report.ReadStages = append(report.ReadStages, rr)
		t.Logf("read stage %-11s offered=%d ok=%d errors=%d p99=%.1fms", rr.Name, rr.Offered, rr.OK, rr.Errors, rr.RequestP99Ms)
		if stage.r.ClientErrors+stage.r.ServerErrors != 0 {
			t.Fatalf("read stage %s: %d client / %d server errors", rr.Name, stage.r.ClientErrors, stage.r.ServerErrors)
		}
		for _, e := range stage.ev {
			t.Logf("  %-26s store_queries=%d max_allowed=%d requests=%d", e.Endpoint, e.StoreQueries, e.MaxAllowed, e.Requests)
			if !e.Flat() {
				t.Errorf("%s in %s: %d store queries for %d requests, ceiling %d — reads are NOT being served from memory",
					e.Endpoint, rr.Name, e.StoreQueries, e.Requests, e.MaxAllowed)
			}
		}
	}

	// The scaling half of the COS: five times the offered rate must not move the
	// database. Asserted as a ratio on what was actually achieved, because a slow
	// runner can sit below the offered rate without anything being wrong.
	if low.OK > 0 && high.OK > 0 {
		lowRate, highRate := low.ReadReport().AchievedRate, high.ReadReport().AchievedRate
		t.Logf("achieved read rate: low=%.1f/s high=%.1f/s (%.1fx)", lowRate, highRate, highRate/lowRate)
		if highRate < 2*lowRate {
			t.Logf("advisory: high stage only reached %.1fx the low stage — the generator, not the server, bounded this run", highRate/lowRate)
		}
	}

	readControl(t, report, eps, conns)
}

// readControl is COS 3, and the reason this ticket is evidence rather than
// decoration: with both caches disabled the same predicate must FAIL. A proof
// that cannot fail proves nothing.
func readControl(t *testing.T, report *loadtest.Report, eps []readEndpoint, conns map[string]*pgx.Conn) {
	t.Helper()
	disableBothCaches(t) // registers its own restore before mutating anything

	bypass, ev := runReadStage(t, "control-bypassed", 30, time.Second, eps, conns)
	rr := bypass.ReadReport()
	rr.CacheBypass = true
	rr.Queries = ev
	report.ReadStages = append(report.ReadStages, rr)
	t.Logf("read stage %-11s offered=%d ok=%d errors=%d (caches bypassed)", rr.Name, rr.Offered, rr.OK, rr.Errors)

	if bypass.ClientErrors+bypass.ServerErrors != 0 {
		t.Fatalf("control stage: %d client / %d server errors — a bypassed read must still succeed", bypass.ClientErrors, bypass.ServerErrors)
	}
	for _, e := range ev {
		t.Logf("  %-26s store_queries=%d max_allowed=%d requests=%d", e.Endpoint, e.StoreQueries, e.MaxAllowed, e.Requests)
		if e.Requests == 0 {
			t.Fatalf("%s served no requests in the control stage — it cannot distinguish the negative", e.Endpoint)
		}
		// Every request must reach the database. Exact, not approximate: a
		// bypassed cache has no other behaviour.
		if want := e.Requests * epStatementsPerLoad(eps, e.Endpoint); e.StoreQueries != want {
			t.Errorf("%s bypassed: %d store queries for %d requests, want exactly %d — "+
				"the control is not actually bypassing the cache, so the flat assertion above proves nothing",
				e.Endpoint, e.StoreQueries, e.Requests, want)
		}
		// And the flat predicate must now trip. This is the assertion that makes
		// the positive arms meaningful.
		if e.Flat() {
			t.Errorf("%s bypassed: %d store queries still within the ceiling %d — "+
				"the flat predicate cannot detect an uncached read, so it proves nothing when it passes",
				e.Endpoint, e.StoreQueries, e.MaxAllowed)
		}
	}

	restoreBothCaches(t)
}

func epStatementsPerLoad(eps []readEndpoint, name string) int {
	for _, e := range eps {
		if e.name == name {
			return e.statementsPerLoad
		}
	}
	return 1
}
