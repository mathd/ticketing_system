//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ticketing/smoke/internal/loadtest"
)

// TKT-82 (US-019): the on-sale load proof. An open-loop sustained profile drives
// the real claim path — hold through the gateway, finalize/confirm service-direct
// (the gateway deliberately blocks those routes) — then the database, not the
// client, is asked whether oversell happened. The gate profile is
// correctness-fatal and throughput-advisory: drops and latency are recorded, not
// asserted, because at 25 attempts/s a slow CI runner can sit below the pool's
// serialization ceiling without anything being wrong (plan-final, TKT-82).
// ONSALE_PROFILE=full runs the festival-NFR profile (make onsale-load-full).

// loadClient reuses connections aggressively: at thousands of attempts/min the
// default 2 idle conns per host turns the client into the bottleneck.
var loadClient = &http.Client{
	Transport: &http.Transport{MaxIdleConns: 1024, MaxIdleConnsPerHost: 1024},
	Timeout:   15 * time.Second,
}

func timedPost(url string, headers map[string]string, body any) (int, []byte, time.Duration, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, url, rd)
	if err != nil {
		return 0, nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	t0 := time.Now()
	resp, err := loadClient.Do(req)
	d := time.Since(t0)
	if err != nil {
		return 0, nil, d, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, d, nil
}

// checkoutAttempt runs one full hold→finalize→confirm lifecycle and classifies
// it. A 409 on the hold is KindRejected — expected once the pool is full,
// unexpected instability otherwise; the caller decides which via stage
// bookkeeping. Latency samples cover mutations only (ADR-004: availability is a
// cached read and never appears here).
func checkoutAttempt(runID, slot string, quantity int) func(loadtest.Stage, int) loadtest.Outcome {
	holds := gatewayURL + "/api/inventory/holds"
	return func(stage loadtest.Stage, seq int) loadtest.Outcome {
		key := fmt.Sprintf("onsale:%s:%s:%d", runID, stage.Name, seq)
		code, body, holdD, err := timedPost(holds, map[string]string{"Idempotency-Key": key},
			map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": quantity})
		switch {
		case err != nil:
			return loadtest.Outcome{Kind: loadtest.KindError, Note: "hold: " + err.Error()}
		case code == http.StatusConflict:
			return loadtest.Outcome{Kind: loadtest.KindRejected}
		case code != http.StatusCreated:
			return loadtest.Outcome{Kind: loadtest.KindError, Note: fmt.Sprintf("hold: %d %.200s", code, body)}
		}
		var claim struct {
			ID string `json:"hold_id"`
		}
		if json.Unmarshal(body, &claim) != nil || claim.ID == "" {
			return loadtest.Outcome{Kind: loadtest.KindError, Note: fmt.Sprintf("hold body unparseable: %.200s", body)}
		}
		out := loadtest.Outcome{Hold: holdD}
		hdr := map[string]string{"X-Internal-Token": internalToken}
		for _, step := range []struct {
			name string
			dst  *time.Duration
		}{{"finalize", &out.Finalize}, {"confirm", &out.Confirm}} {
			url := fmt.Sprintf("%s/holds/%s/%s?organizer_id=%s", inventoryURL, claim.ID, step.name, organizerID)
			code, rbody, d, err := timedPost(url, hdr, nil)
			if err != nil {
				return loadtest.Outcome{Kind: loadtest.KindError, Note: step.name + ": " + err.Error()}
			}
			if code != http.StatusOK {
				return loadtest.Outcome{Kind: loadtest.KindError, Note: fmt.Sprintf("%s: %d %.200s", step.name, code, rbody)}
			}
			*step.dst = d
		}
		out.Kind = loadtest.KindOK
		out.Lifecycle = out.Hold + out.Finalize + out.Confirm
		return out
	}
}

// --- database side: the authoritative verdict ---

func inventoryAdminConn(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), fmt.Sprintf("postgres://postgres:postgres@%s/inventory", pgHostPort))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func poolAccounting(t *testing.T, conn *pgx.Conn, slot string, grantedUnits, lifecycles int) loadtest.Accounting {
	t.Helper()
	a := loadtest.Accounting{GrantedQuantity: grantedUnits, SuccessfulLifecycles: lifecycles}
	// live-held uses ADR-010's real predicate (store.go liveClaims).
	err := conn.QueryRow(t.Context(), `
		SELECT p.capacity, p.confirmed_quantity,
		       COALESCE(SUM(c.quantity) FILTER (WHERE (c.status='held' AND (c.expires_at IS NULL OR c.expires_at > now())) OR c.status='finalizing'),0),
		       COALESCE(SUM(c.quantity) FILTER (WHERE c.status='confirmed'),0)
		FROM inventory_pools p LEFT JOIN claims c ON c.pool_id = p.slot_id
		WHERE p.slot_id = $1::uuid
		GROUP BY p.capacity, p.confirmed_quantity`, slot).
		Scan(&a.Capacity, &a.PoolConfirmed, &a.LiveHeld, &a.SumConfirmedClaims)
	if err != nil {
		t.Fatalf("pool accounting %s: %v", slot, err)
	}
	rows, err := conn.Query(t.Context(),
		`SELECT h.action, count(*) FROM claim_history h JOIN claims c ON c.id = h.claim_id
		 WHERE c.pool_id = $1::uuid GROUP BY h.action`, slot)
	if err != nil {
		t.Fatalf("history counts %s: %v", slot, err)
	}
	defer rows.Close()
	for rows.Next() {
		var action string
		var n int
		if err := rows.Scan(&action, &n); err != nil {
			t.Fatal(err)
		}
		switch action {
		case "create":
			a.HistoryCreate = n
		case "finalize":
			a.HistoryFinalize = n
		case "confirm":
			a.HistoryConfirm = n
		}
	}
	return a
}

func assertAccounting(t *testing.T, conn *pgx.Conn, slot string, grantedUnits, lifecycles int) loadtest.Accounting {
	t.Helper()
	a := poolAccounting(t, conn, slot, grantedUnits, lifecycles)
	for _, v := range a.Violations() {
		t.Error(v)
	}
	return a
}

// pg_stat_statements carries the claim_history INSERT overhead line (the
// TKT-77/ADR-023 amendment). The smoke stack always preloads it via
// compose.onsale-load.yaml; failing loudly here catches a dropped override.
func statStatementsSetup(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	if _, err := conn.Exec(t.Context(), `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err != nil {
		t.Fatalf("pg_stat_statements unavailable (is compose.onsale-load.yaml in the stack?): %v", err)
	}
	if _, err := conn.Exec(t.Context(), `SELECT pg_stat_statements_reset()`); err != nil {
		t.Fatalf("pg_stat_statements not preloaded (compose.onsale-load.yaml missing from the compose files?): %v", err)
	}
}

func statStatementsReset(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	if _, err := conn.Exec(t.Context(), `SELECT pg_stat_statements_reset()`); err != nil {
		t.Fatal(err)
	}
}

func historyInsertStats(t *testing.T, conn *pgx.Conn) loadtest.HistoryStats {
	t.Helper()
	var s loadtest.HistoryStats
	err := conn.QueryRow(t.Context(), `
		SELECT COALESCE(SUM(calls),0), COALESCE(SUM(total_exec_time),0),
		       COALESCE(SUM(total_exec_time)/NULLIF(SUM(calls),0),0),
		       COALESCE(MIN(min_exec_time),0), COALESCE(MAX(max_exec_time),0), COALESCE(MAX(stddev_exec_time),0)
		FROM pg_stat_statements WHERE query LIKE 'INSERT INTO claim_history%'`).
		Scan(&s.Calls, &s.TotalMs, &s.MeanMs, &s.MinMs, &s.MaxMs, &s.StddevMs)
	if err != nil {
		t.Fatalf("history insert stats: %v", err)
	}
	return s
}

func logStage(t *testing.T, r loadtest.StageResult) loadtest.StageReport {
	sr := r.Report()
	for _, n := range r.ErrorNotes {
		t.Logf("stage %s error sample: %s", sr.Name, n)
	}
	t.Logf("stage %-10s offered=%d started=%d dropped=%d ok=%d rejected=%d errors=%d hold p50/p95/p99=%.1f/%.1f/%.1fms lifecycle p50/p95/p99=%.1f/%.1f/%.1fms lag p99=%.1fms achieved=%.1f ok/s",
		sr.Name, sr.Offered, sr.Started, sr.Dropped, sr.OK, sr.Rejected, sr.Errors,
		sr.HoldP50Ms, sr.HoldP95Ms, sr.HoldP99Ms, sr.LifeP50Ms, sr.LifeP95Ms, sr.LifeP99Ms, sr.LagP99Ms, sr.AchievedRate)
	return sr
}

func writeReport(t *testing.T, r *loadtest.Report) {
	path := os.Getenv("ONSALE_REPORT")
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("report written to %s", path)
}

func gitSHA() string {
	b, err := os.ReadFile(filepath.Join("..", ".git", "HEAD"))
	if err != nil {
		return "unknown"
	}
	head := string(bytes.TrimSpace(b))
	if ref, ok := strings.CutPrefix(head, "ref: "); ok {
		if b, err := os.ReadFile(filepath.Join("..", ".git", filepath.FromSlash(ref))); err == nil {
			return string(bytes.TrimSpace(b))
		}
		return head
	}
	return head
}

func TestOnsaleLoadProof(t *testing.T) {
	if os.Getenv("ONSALE_PROFILE") == "full" {
		onsaleFull(t)
		return
	}
	onsaleGate(t)
}

// Gate profile: capacity-500 pool; sustained 25 attempts/s (1,500/min) for 10s,
// then an exact fill and a rejection tail. Correctness-fatal, throughput-advisory;
// the only timing gate is the 30s hard deadline on the load portion.
func onsaleGate(t *testing.T) {
	const capacity = 500
	runID := uuid.NewString()[:8]
	slot, _ := publishedSlot(t, "Onsale Load Hall "+runID, capacity)
	conn := inventoryAdminConn(t)
	statStatementsSetup(t, conn)
	attempt := checkoutAttempt(runID, slot, 1)

	report := loadtest.NewReport("TKT-82", "gate", gitSHA())
	loadStart := time.Now()

	warm := loadtest.RunStage(loadtest.Stage{Name: "warmup", Rate: 5, Duration: 2 * time.Second, Quantity: 1}, 64, attempt)
	report.Stages = append(report.Stages, logStage(t, warm))
	statStatementsReset(t, conn) // measured window starts after warm-up

	sustained := loadtest.RunStage(loadtest.Stage{Name: "sustained", Rate: 25, Duration: 10 * time.Second, Quantity: 1}, 64, attempt)
	report.Stages = append(report.Stages, logStage(t, sustained))

	// Fill exactly to capacity, sized from what actually landed so a dropped
	// arrival on a slow runner cannot desynchronize the rejection tail.
	grantedUnits := warm.OK + sustained.OK
	lifecycles := warm.OK + sustained.OK
	measuredLifecycles := sustained.OK
	for remaining := capacity - grantedUnits; remaining > 0; {
		q := min(remaining, 50)
		out := checkoutAttempt(runID+"-fill", slot, q)(loadtest.Stage{Name: fmt.Sprintf("fill-%d", remaining)}, remaining)
		if out.Kind != loadtest.KindOK {
			t.Fatalf("fill attempt (qty %d) failed: %s", q, out.Kind)
		}
		grantedUnits += q
		lifecycles++
		measuredLifecycles++
		remaining -= q
	}

	reject := loadtest.RunStage(loadtest.Stage{Name: "rejection", Rate: 25, Duration: time.Second, Quantity: 1}, 64, attempt)
	report.Stages = append(report.Stages, logStage(t, reject))
	loadElapsed := time.Since(loadStart)

	// The one timing gate: the load portion must stay inside the smoke budget.
	if loadElapsed > 30*time.Second {
		t.Fatalf("load portion took %v, budget is 30s", loadElapsed)
	}
	// Correctness-fatal: errors anywhere, a rejected hold while capacity was
	// ample (the sustained window must produce real lifecycles — an all-409
	// window would otherwise pass with empty latency sets), or a grant after
	// the pool filled.
	if n := warm.Errors + sustained.Errors + reject.Errors; n != 0 {
		t.Fatalf("%d attempts errored (timeout/transport/unexpected status)", n)
	}
	for _, r := range []loadtest.StageResult{warm, sustained} {
		if r.Rejected != 0 || r.OK != r.Started || r.OK == 0 {
			t.Fatalf("stage %s: %d/%d started attempts succeeded, %d rejected — capacity was ample, every started attempt must complete a lifecycle", r.Stage.Name, r.OK, r.Started, r.Rejected)
		}
	}
	if reject.OK != 0 {
		t.Fatalf("%d holds granted on a full pool", reject.OK)
	}
	if reject.Rejected != reject.Started {
		t.Fatalf("rejection tail: %d/%d answered 409", reject.Rejected, reject.Started)
	}
	// Advisory: drops mean this runner sat below the offered rate — report, don't fail.
	if n := warm.Dropped + sustained.Dropped + reject.Dropped; n != 0 {
		t.Logf("advisory: %d arrivals dropped at the in-flight cap (runner below offered rate)", n)
	}

	a := assertAccounting(t, conn, slot, grantedUnits, lifecycles)
	report.Accounting = append(report.Accounting, a)
	if a.PoolConfirmed != capacity || a.LiveHeld != 0 {
		t.Errorf("final state confirmed=%d live_held=%d, want %d/0", a.PoolConfirmed, a.LiveHeld, capacity)
	}

	stats := historyInsertStats(t, conn)
	report.History = &stats
	if want := int64(3 * measuredLifecycles); stats.Calls != want {
		t.Errorf("claim_history INSERT calls %d, want 3×%d=%d for the measured window", stats.Calls, measuredLifecycles, want)
	}
	t.Logf("claim_history INSERT: %.3fms/mutation mean (min %.3f max %.3f stddev %.3f), %d calls, %.1fms total — pg_stat_statements over the measured window",
		stats.MeanMs, stats.MinMs, stats.MaxMs, stats.StddevMs, stats.Calls, stats.TotalMs)
	writeReport(t, report)
}

// Full NFR profile (ONSALE_PROFILE=full, `make onsale-load-full`): the
// festival-scale window (3,000 attempts/min × 3 min), the per-pool ceiling
// sweep, and a quantity-50 oversell tail. SLO (Gate 2 decision): per-mutation
// p99 ≤ 1s, lifecycle p99 ≤ 3s at the NFR window.
func onsaleFull(t *testing.T) {
	runID := uuid.NewString()[:8]
	conn := inventoryAdminConn(t)
	statStatementsSetup(t, conn)
	report := loadtest.NewReport("TKT-82", "full", gitSHA())
	report.Notes = "local compose topology; DB_MAX_OPEN_CONNS=25; single-host client+server — see docs/verification/on-sale-load/README.md"

	slot, _ := publishedSlot(t, "Onsale NFR Hall "+runID, 100000)
	attempt := checkoutAttempt(runID, slot, 1)

	warm := loadtest.RunStage(loadtest.Stage{Name: "warmup", Rate: 10, Duration: 30 * time.Second, Quantity: 1}, 512, attempt)
	report.Stages = append(report.Stages, logStage(t, warm))
	statStatementsReset(t, conn)

	// A stage's claims are only meaningful if the generator held its schedule:
	// arrivals starting late (lag) without drops would publish a rate that was
	// never actually offered. Nominal lag p99 is ~2ms; 1s means the schedule
	// was not sustained — inconclusive, never publishable.
	generatorHeldSchedule := func(r loadtest.StageResult) {
		if lag := loadtest.Percentile(r.Lag, 99); lag > time.Second {
			t.Fatalf("stage %s: scheduler lag p99 %v — the generator did not sustain the offered rate; run inconclusive", r.Stage.Name, lag)
		}
	}

	nfr := loadtest.RunStage(loadtest.Stage{Name: "nfr-3000pm", Rate: 50, Duration: 180 * time.Second, Quantity: 1}, 512, attempt)
	generatorHeldSchedule(nfr)
	nfrReport := logStage(t, nfr)
	report.Stages = append(report.Stages, nfrReport)
	stats := historyInsertStats(t, conn)
	report.History = &stats
	t.Logf("claim_history INSERT: %.3fms/mutation mean (min %.3f max %.3f stddev %.3f), %d calls, %.1fms total — pg_stat_statements during the NFR window",
		stats.MeanMs, stats.MinMs, stats.MaxMs, stats.StddevMs, stats.Calls, stats.TotalMs)
	if want := int64(3 * nfr.OK); stats.Calls != want {
		t.Errorf("claim_history INSERT calls %d, want 3×%d=%d for the NFR window", stats.Calls, nfr.OK, want)
	}
	report.Accounting = append(report.Accounting, assertAccounting(t, conn, slot, warm.OK+nfr.OK, warm.OK+nfr.OK))

	if !nfr.Stable(0.99) {
		t.Errorf("NFR window unstable: %+v", nfr.Report())
	}
	// SLO percentiles are only meaningful over real lifecycles: a wholesale-409
	// window would satisfy them vacuously with empty sample sets.
	if nfr.Rejected != 0 || nfr.OK != nfr.Started || nfr.OK == 0 {
		t.Fatalf("NFR window: %d/%d started attempts succeeded, %d rejected — capacity was ample, every started attempt must complete a lifecycle", nfr.OK, nfr.Started, nfr.Rejected)
	}
	for name, p99 := range map[string]float64{"hold": nfrReport.HoldP99Ms, "finalize": nfrReport.FinalizeP99Ms, "confirm": nfrReport.ConfirmP99Ms} {
		if p99 > 1000 {
			t.Errorf("NFR %s p99 %.1fms exceeds 1s SLO", name, p99)
		}
	}
	if nfrReport.LifeP99Ms > 3000 {
		t.Errorf("NFR lifecycle p99 %.1fms exceeds 3s SLO", nfrReport.LifeP99Ms)
	}

	// Ceiling sweep: fresh pool per rate so earlier confirmed inventory can't
	// turn later stages into sold-out tests; stop at the first unstable stage.
	// Stability is delivery AND the lifecycle SLO: a stage that answers slowly
	// forever (or rejects with capacity ample) is past the knee even before the
	// client's in-flight cap starts dropping arrivals. The cap is sized by
	// Little's law above rate × SLO (rate × 4s of headroom for the 3s SLO), so
	// the generator cannot define the knee: at the cap, mean latency already
	// exceeds the SLO. If drops still occur while the SLO holds, the run is
	// INCONCLUSIVE — a generator limit, never publishable as a ceiling.
	sweepStable := func(r loadtest.StageResult) bool {
		return r.Stable(0.99) && r.Rejected == 0 && r.OK == r.Started && r.OK > 0 &&
			loadtest.Percentile(r.Lifecycle, 99) <= 3*time.Second
	}
	var sweep []loadtest.StageResult
	for _, rate := range []int{75, 150, 300, 600, 1200, 2400, 3000} {
		s, _ := publishedSlot(t, fmt.Sprintf("Onsale Sweep %s %d", runID, rate), 100000)
		r := loadtest.RunStage(loadtest.Stage{Name: fmt.Sprintf("sweep-%d", rate), Rate: rate, Duration: 30 * time.Second, Quantity: 1}, max(512, rate*4), checkoutAttempt(runID, s, 1))
		generatorHeldSchedule(r)
		report.Stages = append(report.Stages, logStage(t, r))
		report.Accounting = append(report.Accounting, assertAccounting(t, conn, s, r.OK, r.OK))
		if r.Dropped > 0 && r.Errors == 0 && r.Rejected == 0 && loadtest.Percentile(r.Lifecycle, 99) <= 3*time.Second {
			t.Fatalf("sweep-%d: %d arrivals dropped at the in-flight cap while the SLO held — generator-limited, ceiling inconclusive; raise the cap and rerun", rate, r.Dropped)
		}
		sweep = append(sweep, r)
		if !sweepStable(r) {
			break
		}
	}
	hi, first, lower := loadtest.CeilingBracket(sweep, sweepStable)
	report.CeilingHighestStable, report.CeilingFirstUnstable, report.CeilingLowerBoundOnly = hi, first, lower
	if lower {
		t.Logf("ceiling: every stage stable — publish as a lower bound ≥%.0f attempts/s (knee not observed)", hi)
	} else {
		t.Logf("ceiling bracket: highest stable %.0f attempts/s, first unstable %.0f attempts/s (×3 pool mutations per checkout)", hi, first)
	}

	// Oversell tail: adversarial quantity-50 burst against a 50k pool. Exactly
	// 1,000 can succeed; the rest must be clean 409s. Correctness evidence only —
	// excluded from the ceiling.
	tailSlot, _ := publishedSlot(t, "Onsale Tail "+runID, 50000)
	tail := loadtest.RunStage(loadtest.Stage{Name: "oversell-tail", Rate: 275, Duration: 4 * time.Second, Quantity: 50}, 512, checkoutAttempt(runID, tailSlot, 50))
	report.Stages = append(report.Stages, logStage(t, tail))
	ta := assertAccounting(t, conn, tailSlot, tail.OK*50, tail.OK)
	report.Accounting = append(report.Accounting, ta)
	if tail.Errors != 0 {
		t.Errorf("oversell tail: %d errors", tail.Errors)
	}
	// A dropped arrival means the generator never brought the pool to its
	// capacity boundary — that is an invalid proof, not a weaker one.
	if tail.Dropped != 0 {
		t.Fatalf("oversell tail dropped %d arrivals: the boundary was never contested — rerun on a generator that can offer the full tail", tail.Dropped)
	}
	if tail.OK != 1000 {
		t.Errorf("oversell tail granted %d lifecycles, want exactly 1000 (50,000 units)", tail.OK)
	}
	if tail.Rejected < 100 {
		t.Errorf("oversell tail rejected %d, want ≥100 post-capacity rejections", tail.Rejected)
	}
	if ta.PoolConfirmed != 50000 {
		t.Errorf("oversell tail pool confirmed %d, want exactly 50000 — the boundary must be reached", ta.PoolConfirmed)
	}
	writeReport(t, report)
}
