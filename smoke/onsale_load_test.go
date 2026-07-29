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

// Transport bounds (TKT-92) and the fail-closed redirect policy (TKT-93) live
// with the harness in loadtest.NewClient; a 3xx surfaces as its raw status for
// classification, never as a followed hop that could re-send X-Internal-Token.
var loadClient = loadtest.NewClient()

func timedPost(t *testing.T, url string, headers map[string]string, body any) (int, []byte, time.Duration, error) {
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
	out, rerr := io.ReadAll(resp.Body)
	// TKT-95: contract-validate the delivered response, AFTER d is measured so no
	// local schema-walking time enters the latency evidence (TKT-82/TKT-125), and
	// only when the body arrived whole — validating a truncated body would
	// manufacture a contract violation out of a client-side symptom, inverting
	// TKT-92's conservative direction. Async (t.Error): a stage must still join its
	// in-flight attempts.
	if rerr == nil {
		if service := directService(url); service != "" {
			validateDirectServiceResponseAsync(t, service, resp.Request, resp.StatusCode, resp.Header, out)
		} else {
			validateServiceResponseAsync(t, resp.Request, resp.StatusCode, resp.Header, out)
		}
	}
	if rerr != nil {
		// Surface a truncated/reset body as err with the delivered status:
		// callers classify — a forbidden status is server evidence on its own;
		// a cut-off success body is client-side/inconclusive (TKT-92).
		return resp.StatusCode, out, d, fmt.Errorf("read body: %w", rerr)
	}
	return resp.StatusCode, out, d, nil
}

// checkoutAttempt runs one full hold→finalize→confirm lifecycle and classifies
// it. A 409 on the hold is KindRejected — expected once the pool is full,
// unexpected instability otherwise; the caller decides which via stage
// bookkeeping. Latency samples cover mutations only (ADR-004: availability is a
// cached read and never appears here).
func checkoutAttempt(t *testing.T, runID, slot string, quantity int) func(loadtest.Stage, int) loadtest.Outcome {
	holds := gatewayURL + "/api/inventory/holds"
	return func(stage loadtest.Stage, seq int) loadtest.Outcome {
		key := fmt.Sprintf("onsale:%s:%s:%d", runID, stage.Name, seq)
		code, body, holdD, err := timedPost(t, holds, map[string]string{"Idempotency-Key": key},
			map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": quantity})
		// Classification precedence (TKT-92): a delivered status decides on its
		// own wherever it can — a forbidden status is server evidence even if
		// the body was then truncated/reset. err is client-side only when no
		// status arrived, or when a success body the harness needs was cut off
		// (the conservative, inconclusive direction).
		switch {
		case err != nil && code == 0:
			return loadtest.Outcome{Kind: loadtest.KindClientError, Note: "hold: " + err.Error()}
		// A 409 is only sellout if the body says so (TKT-93): coded 409s
		// (slot_archived/slot_closed) and unrecognized shapes fail closed as
		// server evidence. A read error truncating the body lands there too —
		// an unprovable rejection must not count as capacity evidence.
		case code == http.StatusConflict:
			kind := loadtest.ClassifyHold409(body)
			out := loadtest.Outcome{Kind: kind}
			if kind != loadtest.KindRejected {
				out.Note = fmt.Sprintf("hold 409 not a capacity rejection: %.200s (%v)", body, err)
			}
			return out
		// 200 is an idempotent replay. Keys are unique per attempt, so it only
		// arises when Go transparently retried after losing the first response
		// on a reused connection — the hold is real, not instability.
		case code != http.StatusCreated && code != http.StatusOK:
			return loadtest.Outcome{Kind: loadtest.KindServerError, Note: fmt.Sprintf("hold: %d %.200s (%v)", code, body, err)}
		case err != nil: // truncated success body — the hold may exist, but the proof is gone
			return loadtest.Outcome{Kind: loadtest.KindClientError, Note: "hold: " + err.Error()}
		}
		var claim struct {
			ID string `json:"hold_id"`
		}
		if json.Unmarshal(body, &claim) != nil || claim.ID == "" {
			return loadtest.Outcome{Kind: loadtest.KindServerError, Note: fmt.Sprintf("hold body unparseable: %.200s", body)}
		}
		out := loadtest.Outcome{Hold: holdD}
		hdr := map[string]string{"X-Internal-Token": internalToken}
		for _, step := range []struct {
			name, target string
			dst          *time.Duration
		}{{"finalize", "finalizing", &out.Finalize}, {"confirm", "confirmed", &out.Confirm}} {
			url := fmt.Sprintf("%s/internal/holds/%s/%s?organizer_id=%s", inventoryURL, claim.ID, step.name, organizerID)
			code, rbody, d, err := timedPost(t, url, hdr, nil)
			switch {
			case err != nil && code == 0:
				return loadtest.Outcome{Kind: loadtest.KindClientError, Note: step.name + ": " + err.Error()}
			case code != http.StatusOK: // delivered forbidden status decides alone, truncated body or not
				return loadtest.Outcome{Kind: loadtest.KindServerError, Note: fmt.Sprintf("%s: %d %.200s (%v)", step.name, code, rbody, err)}
			case err != nil: // 200 delivered, body then cut off — transport health, inconclusive
				return loadtest.Outcome{Kind: loadtest.KindClientError, Note: step.name + ": " + err.Error()}
			// A fully delivered 200 whose body is not the claim in the target
			// state is a malformed success body — server-side per the taxonomy
			// (TKT-93; ADR-028 posture).
			case !loadtest.ValidTransitionBody(rbody, step.target):
				return loadtest.Outcome{Kind: loadtest.KindServerError, Note: fmt.Sprintf("%s: 200 with malformed body %.200s", step.name, rbody)}
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
	t.Logf("stage %-10s offered=%d started=%d dropped=%d ok=%d rejected=%d client-err=%d server-err=%d inflight max/peak=%d/%d hold p50/p95/p99=%.1f/%.1f/%.1fms lifecycle p50/p95/p99=%.1f/%.1f/%.1fms lag p99=%.1fms achieved=%.1f ok/s",
		sr.Name, sr.Offered, sr.Started, sr.Dropped, sr.OK, sr.Rejected, sr.ClientErrors, sr.ServerErrors,
		sr.MaxInFlight, sr.PeakInFlight,
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

// beginReport arms the report writer up front and returns the func that marks
// the run complete. Call it once, right after NewReport; call the returned func
// as the profile's last statement.
//
// TKT-130: writeReport used to be the profile's own last statement, so a single
// aborted stage discarded every stage that had completed before it — every
// generatorHealthy check is a t.Fatalf, and TKT-125 lost its finalize/confirm
// p99 to exactly that (the sweep-600 stage trips the generator guard every time
// on a single-host setup, minutes after the stage the run actually needed had
// completed 9,000/9,000 clean).
//
// The report is marked partial up front and cleared only on the normal path, so
// any exit that is not the end of the profile leaves it partial without anyone
// having to enumerate the exits. t.Cleanup rather than defer because the writer
// has to outlive the profile function, and it runs after a t.Fatalf for the
// same reason a defer does: FailNow is runtime.Goexit on the test goroutine.
func beginReport(t *testing.T, r *loadtest.Report) (complete func()) {
	r.Partial = true
	t.Cleanup(func() { writeReport(t, r) })
	return func() { r.Partial = false }
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
	// Armed before any fallible setup: publishedSlot, the admin connection and
	// statStatementsSetup can all t.Fatalf, and a run that dies there must still
	// overwrite the configured path. Otherwise a previous run's file survives
	// and reads as this run's output.
	report := loadtest.NewReport("TKT-82", "gate", gitSHA())
	complete := beginReport(t, report)
	report.ClientMaxConnsPerHost = loadtest.MaxConnsPerHost

	slot, _ := publishedSlot(t, "Onsale Load Hall "+runID, capacity)
	conn := inventoryAdminConn(t)
	statStatementsSetup(t, conn)
	attempt := checkoutAttempt(t, runID, slot, 1)
	loadStart := time.Now()

	warm := loadtest.RunStage(loadtest.Stage{Name: "warmup", Rate: 5, Duration: 2 * time.Second, Quantity: 1}, 64, attempt)
	report.Stages = append(report.Stages, logStage(t, warm))
	statStatementsReset(t, conn) // measured window starts after warm-up

	sustained := loadtest.RunStage(loadtest.Stage{Name: "sustained", Rate: 25, Duration: 10 * time.Second, Quantity: 1}, 64, attempt)
	report.Stages = append(report.Stages, logStage(t, sustained))

	// Any error invalidates the fill arithmetic below (a hold that committed
	// but whose response was lost or malformed undercounts OK, so the fill
	// over-requests and dies on a confusing 409) — diagnose the real class
	// before filling.
	if n := warm.ClientErrors + sustained.ClientErrors; n != 0 {
		t.Fatalf("%d attempts hit client-side transport errors before the fill — run inconclusive, rerun on a healthy generator", n)
	}
	if n := warm.ServerErrors + sustained.ServerErrors; n != 0 {
		t.Fatalf("%d attempts hit server-side errors before the fill (5xx/unexpected status/malformed body)", n)
	}

	// Fill exactly to capacity, sized from what actually landed so a dropped
	// arrival on a slow runner cannot desynchronize the rejection tail.
	grantedUnits := warm.OK + sustained.OK
	lifecycles := warm.OK + sustained.OK
	measuredLifecycles := sustained.OK
	for remaining := capacity - grantedUnits; remaining > 0; {
		q := min(remaining, 50)
		out := checkoutAttempt(t, runID+"-fill", slot, q)(loadtest.Stage{Name: fmt.Sprintf("fill-%d", remaining)}, remaining)
		if out.Kind == loadtest.KindClientError {
			t.Fatalf("fill attempt (qty %d) hit a client-side transport error — run inconclusive, not server instability: %s", q, out.Note)
		}
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
	// Both error classes still fail the gate — the correctness proof did not
	// complete either way — but a client-side class means the run is
	// inconclusive (generator/transport health), not evidence of instability.
	if n := warm.ClientErrors + sustained.ClientErrors + reject.ClientErrors; n != 0 {
		t.Fatalf("%d attempts hit client-side transport errors — run inconclusive, rerun on a healthy generator", n)
	}
	// Correctness-fatal: server errors anywhere, a rejected hold while capacity
	// was ample (the sustained window must produce real lifecycles — an all-409
	// window would otherwise pass with empty latency sets), or a grant after
	// the pool filled.
	if n := warm.ServerErrors + sustained.ServerErrors + reject.ServerErrors; n != 0 {
		t.Fatalf("%d attempts hit server-side errors (5xx/unexpected status/malformed body)", n)
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
	complete()
}

// Full NFR profile (ONSALE_PROFILE=full, `make onsale-load-full`): the
// festival-scale window (3,000 attempts/min × 3 min), the per-pool ceiling
// sweep, and a quantity-50 oversell tail. SLO (Gate 2 decision): per-mutation
// p99 ≤ 1s, lifecycle p99 ≤ 3s at the NFR window.
func onsaleFull(t *testing.T) {
	runID := uuid.NewString()[:8]
	// Armed before any fallible setup — see onsaleGate.
	report := loadtest.NewReport("TKT-82", "full", gitSHA())
	complete := beginReport(t, report)
	report.ClientMaxConnsPerHost = loadtest.MaxConnsPerHost
	report.Notes = "local compose topology; DB_MAX_OPEN_CONNS=25; single-host client+server — see docs/verification/on-sale-load/README.md"

	conn := inventoryAdminConn(t)
	statStatementsSetup(t, conn)
	slot, _ := publishedSlot(t, "Onsale NFR Hall "+runID, 100000)
	attempt := checkoutAttempt(t, runID, slot, 1)

	// A stage's claims are only meaningful if the generator itself was healthy:
	// a sustained schedule (nominal lag p99 is ~2ms; 1s means the offered rate
	// was never real), no client-side transport errors, and no drops while the
	// SLO held. Any of those is inconclusive — a generator verdict, never
	// publishable as server evidence (loadtest.CeilingInconclusive).
	generatorHealthy := func(r loadtest.StageResult) {
		if reason, inconclusive := loadtest.CeilingInconclusive(r, time.Second, 3*time.Second); inconclusive {
			t.Fatalf("stage %s inconclusive: %s; rerun", r.Stage.Name, reason)
		}
	}

	warm := loadtest.RunStage(loadtest.Stage{Name: "warmup", Rate: 10, Duration: 30 * time.Second, Quantity: 1}, 512, attempt)
	generatorHealthy(warm)
	// Warm-up is not published evidence, but a delivered server failure (or a
	// rejection with a 100k pool ample) already invalidates the run.
	if warm.ServerErrors != 0 || warm.Rejected != 0 {
		t.Fatalf("warm-up: %d server errors, %d rejections with capacity ample", warm.ServerErrors, warm.Rejected)
	}
	report.Stages = append(report.Stages, logStage(t, warm))
	statStatementsReset(t, conn)

	nfr := loadtest.RunStage(loadtest.Stage{Name: "nfr-3000pm", Rate: 50, Duration: 180 * time.Second, Quantity: 1}, 512, attempt)
	generatorHealthy(nfr)
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
	// Little's law above rate × SLO (rate × 4s of headroom for the 3s SLO),
	// bounded by the shared transport's connection limit — above ~1024/s a
	// saturated generator resolves as inconclusive (client errors or
	// drops-with-SLO-met via generatorHealthy), never as a published ceiling.
	sweepStable := func(r loadtest.StageResult) bool {
		return r.Stable(0.99) && r.Rejected == 0 && r.OK == r.Started && r.OK > 0 &&
			loadtest.Percentile(r.Lifecycle, 99) <= 3*time.Second
	}
	var sweep []loadtest.StageResult
	for _, rate := range []int{75, 150, 300, 600, 1200, 2400, 3000} {
		s, _ := publishedSlot(t, fmt.Sprintf("Onsale Sweep %s %d", runID, rate), 100000)
		r := loadtest.RunStage(loadtest.Stage{Name: fmt.Sprintf("sweep-%d", rate), Rate: rate, Duration: 30 * time.Second, Quantity: 1}, min(max(512, rate*4), loadtest.MaxConnsPerHost), checkoutAttempt(t, runID, s, 1))
		generatorHealthy(r)
		report.Stages = append(report.Stages, logStage(t, r))
		report.Accounting = append(report.Accounting, assertAccounting(t, conn, s, r.OK, r.OK))
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
	tail := loadtest.RunStage(loadtest.Stage{Name: "oversell-tail", Rate: 275, Duration: 4 * time.Second, Quantity: 50}, 512, checkoutAttempt(t, runID, tailSlot, 50))
	generatorHealthy(tail)
	report.Stages = append(report.Stages, logStage(t, tail))
	ta := assertAccounting(t, conn, tailSlot, tail.OK*50, tail.OK)
	report.Accounting = append(report.Accounting, ta)
	if tail.ServerErrors != 0 {
		t.Errorf("oversell tail: %d server errors", tail.ServerErrors)
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
	complete()
}
