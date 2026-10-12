//go:build smoke

package smoke_test

// TKT-337 COS2: the opt-in induced-failure example for the checkout-path dashboard.
//
// It does NOT run in `make check`. Run it once, beside the full on-sale profile, and keep
// what it writes as evidence (docs/verification/checkout-dashboard/README.md):
//
//	SMOKE_CHECKOUT_DASHBOARD_INDUCE=1 \
//	SMOKE_CHECKOUT_DASHBOARD_EVIDENCE=<directory> \
//	ONSALE_PROFILE=full ONSALE_REPORT=<scratch>/full-profile.json \
//	SMOKE_TEST_TIMEOUT=60m ./scripts/smoke.sh
//
// Why it is opt-in (decision D2): the dashboard's rate window is at least 4 minutes,
// because the lgtm image's Prometheus datasource sets a 60 s scrape interval. A few slow
// requests inside 4 minutes of other gate traffic would move p95 on some runs and not on
// others. This test instead keeps each phase as long as the window and separates the
// phases by more than the window, which costs about 16 minutes.
//
// Phases, in order:
//
//  1. Slow inventory (checkoutInducedSlowPhase). Each round holds the real pool row lock
//     (`inventory_pools … FOR UPDATE`, the lock CreateHold takes) on a fresh pool,
//     sends checkoutInducedWaiters reservations through the gateway and commerce,
//     confirms that they are queued on that lock, keeps the lock for
//     checkoutInducedHold, then rolls back. Each reservation then succeeds, slowly.
//     The hold stays below 10 s, the largest finite histogram bucket, and far below the
//     15 s client timeout and the 30 s service timeouts, so it makes latency, not errors.
//  2. A quiet gap (checkoutInducedGap), longer than the rate window.
//  3. Payment failure: checkoutInducedFailures fake-auth-hold checkouts,
//     checkoutInducedFailureEvery apart. Payments answers 500; commerce answers 202.
//  4. A tail (checkoutInducedGap) so the window covers the last failure.
//
// It then reads every Prometheus panel target through Grafana over the whole run, and the
// two trace panels, and writes the raw results, the phase boundaries, one real trace per
// phase with its spans, and a computed signature. It asserts the signature the README
// names: inventory latency rises in phase 1 while payments latency does not; the payments
// 5xx rate rises in phase 3 while commerce and gateway 5xx stay at 0.
//
// No t.Parallel(), for the reason TestCheckoutDashboardProvisionedAndLive gives. The file
// sorts before onsale_load_test.go, so in a full-profile run this test finishes before
// TestOnsaleLoadProof starts and stays outside its measured windows.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	checkoutInducedSlowPhase    = 4 * time.Minute
	checkoutInducedGap          = 5 * time.Minute
	checkoutInducedHold         = 3 * time.Second
	checkoutInducedWaiters      = 4
	checkoutInducedFailures     = 8
	checkoutInducedFailureEvery = 15 * time.Second
)

type inducedPhase struct {
	Name  string    `json:"name"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Note  string    `json:"note"`
}

func TestCheckoutDashboardInducedSignatures(t *testing.T) {
	if os.Getenv("SMOKE_CHECKOUT_DASHBOARD_INDUCE") != "1" {
		t.Skip("opt-in TKT-337 evidence run: set SMOKE_CHECKOUT_DASHBOARD_INDUCE=1 (about 16 minutes)")
	}
	dir := os.Getenv("SMOKE_CHECKOUT_DASHBOARD_EVIDENCE")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	panels := checkoutDashboardPanels(t, fetchCheckoutDashboard(t))
	runStart := time.Now().UTC()

	// Phase 1: slow inventory.
	slow := inducedPhase{Name: "slow-inventory", Start: time.Now().UTC(),
		Note: fmt.Sprintf("rounds of %d reservations queued on a held inventory_pools row lock, released after %s", checkoutInducedWaiters, checkoutInducedHold)}
	var slowTrace string
	var slowDurations []time.Duration
	rounds := 0
	for time.Since(slow.Start) < checkoutInducedSlowPhase {
		trace, ds := slowInventoryRound(t, rounds == 0)
		if trace != "" {
			slowTrace = trace
		}
		slowDurations = append(slowDurations, ds...)
		rounds++
	}
	slow.End = time.Now().UTC()
	slow.Note += fmt.Sprintf("; %d rounds, %d slow reservations", rounds, len(slowDurations))
	t.Logf("slow inventory phase: %s to %s, %d rounds, %d reservations, trace %s", slow.Start.Format(time.RFC3339), slow.End.Format(time.RFC3339), rounds, len(slowDurations), slowTrace)

	time.Sleep(checkoutInducedGap)

	// Phase 3: payment failure. Each checkout needs its own seats: two per offer.
	pay := inducedPhase{Name: "payment-failure", Start: time.Now().UTC(),
		Note: fmt.Sprintf("%d fake-auth-hold checkouts, %s apart: payments 500, commerce 202 payment_unknown", checkoutInducedFailures, checkoutInducedFailureEvery)}
	var payTrace string
	for i := 0; i < checkoutInducedFailures; i++ {
		next := time.Now().Add(checkoutInducedFailureEvery)
		_, offer := setupCheckoutOffer(t, fmt.Sprintf("induced-pay-%d-%s", i, uuid.NewString()[:8]))
		trace, order := tracedCheckout(t, offer, "fake-auth-hold", http.StatusAccepted, "payment_unknown")
		drainPaymentUnknownOnCleanup(t, order)
		if payTrace == "" {
			payTrace = trace
		}
		if i < checkoutInducedFailures-1 {
			time.Sleep(time.Until(next))
		}
	}
	pay.End = time.Now().UTC()
	t.Logf("payment failure phase: %s to %s, trace %s", pay.Start.Format(time.RFC3339), pay.End.Format(time.RFC3339), payTrace)

	time.Sleep(checkoutInducedGap)
	runEnd := time.Now().UTC()

	// Evidence: the panels exactly as Grafana answers them, over the whole run.
	from := runStart.Add(-5 * time.Minute)
	series := map[string]map[string][]inducedPoint{} // "<panel> <refId>" -> service -> points
	for _, title := range []string{panelRequestRate, panelErrorRate, panelLatency} {
		for _, target := range panels[title].Targets {
			res, err := grafanaQuery(t, panels[title], target, from, runEnd)
			if err != nil {
				t.Fatalf("%q %v: %v", title, target["refId"], err)
			}
			key := fmt.Sprintf("%s %v", title, target["refId"])
			writeEvidenceJSON(t, dir, "query-range-"+evidenceSlug(key)+".json", map[string]any{
				"panel":          title,
				"refId":          target["refId"],
				"expr":           target["expr"],
				"variables":      map[string]string{"service": "All", "method": "POST"},
				"from":           from,
				"to":             runEnd,
				"intervalMs":     15000,
				"grafana_result": res,
			})
			series[key] = inducedSeries(t, res)
		}
	}
	for _, title := range []string{panelCheckoutTraces, panelPaymentErrors} {
		for _, target := range panels[title].Targets {
			res, err := grafanaQuery(t, panels[title], target, runStart.Add(-time.Minute), runEnd)
			if err != nil {
				t.Fatalf("%q: %v", title, err)
			}
			writeEvidenceJSON(t, dir, "traces-panel-"+evidenceSlug(title)+".json", map[string]any{
				"panel": title, "query": target["query"], "from": runStart.Add(-time.Minute), "to": runEnd, "grafana_result": res,
			})
		}
	}
	for name, trace := range map[string]string{"slow-inventory": slowTrace, "payment-failure": payTrace} {
		code, body := grafanaRequest(t, http.MethodGet, "/api/datasources/proxy/uid/tempo/api/v2/traces/"+trace, nil)
		if code != http.StatusOK {
			t.Fatalf("GET trace %s (%s): %d %.300s", trace, name, code, body)
		}
		var raw any
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatal(err)
		}
		writeEvidenceJSON(t, dir, "trace-"+name+".json", map[string]any{"trace_id": trace, "tempo": raw, "spans": inducedSpanSummary(t, body)})
	}

	// The signature, computed from the panel series.
	p95 := series[panelLatency+" B"]
	errs := series[panelErrorRate+" A"]
	slowWin := [2]time.Time{slow.Start.Add(30 * time.Second), slow.End.Add(30 * time.Second)}
	payWin := [2]time.Time{pay.Start, pay.End.Add(4 * time.Minute)}
	whole := [2]time.Time{runStart, runEnd}
	sig := map[string]any{}
	for _, svc := range checkoutPathServices {
		sig[svc] = map[string]any{
			"p95_seconds_before_slow_phase": jsonNumber(lastBefore(p95[svc], slow.Start)),
			"p95_seconds_max_slow_phase":    jsonNumber(maxIn(p95[svc], slowWin)),
			"p95_seconds_max_payment_phase": jsonNumber(maxIn(p95[svc], payWin)),
			"5xx_per_second_max_slow_phase": jsonNumber(maxIn(errs[svc], slowWin)),
			"5xx_per_second_max_pay_phase":  jsonNumber(maxIn(errs[svc], payWin)),
			"5xx_per_second_max_whole_run":  jsonNumber(maxIn(errs[svc], whole)),
		}
	}
	sort.Slice(slowDurations, func(i, j int) bool { return slowDurations[i] < slowDurations[j] })
	writeEvidenceJSON(t, dir, "phases.json", map[string]any{
		"run_start": runStart, "run_end": runEnd, "phases": []inducedPhase{slow, pay},
		"rate_window": "Grafana $__rate_interval (4m0s with the lgtm datasource's 60 s scrape interval)",
		"slow_reservation_client_seconds": map[string]float64{
			"min":    slowDurations[0].Seconds(),
			"median": slowDurations[len(slowDurations)/2].Seconds(),
			"max":    slowDurations[len(slowDurations)-1].Seconds(),
		},
		"traces": map[string]string{"slow-inventory": slowTrace, "payment-failure": payTrace},
	})
	writeEvidenceJSON(t, dir, "signature.json", sig)

	// Assert the signature the README states.
	if v := maxIn(p95["inventory"], slowWin); !(v >= 1.0) {
		t.Errorf("inventory p95 max in the slow phase = %v s, want at least 1 s", v)
	}
	if v := maxIn(p95["payments"], slowWin); !(v < 1.0) {
		t.Errorf("payments p95 max in the slow phase = %v s, want below 1 s (payments is not on the slow path)", v)
	}
	if v := maxIn(errs["payments"], payWin); !(v > 0) {
		t.Errorf("payments 5xx max in the payment phase = %v, want above 0", v)
	}
	for _, svc := range []string{"commerce", "gateway"} {
		if v := maxIn(errs[svc], whole); v != 0 {
			t.Errorf("%s 5xx max over the run = %v, want 0", svc, v)
		}
	}
	t.Logf("evidence written to %s", dir)
}

// slowInventoryRound runs one round of phase 1 on a fresh pool and returns the traced
// reservation's trace id (when traced) and every reservation's client-side duration.
func slowInventoryRound(t *testing.T, traced bool) (string, []time.Duration) {
	t.Helper()
	slot, ticketType := setupCheckoutOffer(t, "induced-slow-"+uuid.NewString()[:8])
	ctx := t.Context()
	blocker, err := pgx.Connect(ctx, dsn("inventory", "inventory"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Close(context.Background()) }()
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1::uuid FOR UPDATE`, slot); err != nil {
		t.Fatal(err)
	}

	n := checkoutInducedWaiters
	var traceparent, traceID string
	if traced {
		n++ // the venue has 5 seats: 4 plain waiters and 1 traced one, one seat each
		traceparent, traceID = newTraceparent()
	}
	type result struct {
		code int
		body []byte
		d    time.Duration
	}
	results := make(chan result, n)
	var workers sync.WaitGroup
	// Roll back FIRST, then join: a t.Fatal below must not return while the workers are
	// still queued on this transaction's lock (see inventory_contention_test.go).
	defer func() {
		_ = tx.Rollback(context.Background())
		workers.Wait()
	}()
	for i := 0; i < n; i++ {
		hdr := map[string]string{}
		if traced && i == 0 {
			hdr["traceparent"] = traceparent
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			start := time.Now()
			code, body := postWithKeyAndHeadersAsync(t, gatewayURL+"/api/commerce/reservations", "induced-slow-"+uuid.NewString(), hdr,
				map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 1})
			results <- result{code, body, time.Since(start)}
		}()
	}
	// The handshake: every reservation is observed BLOCKED on a pool lock before the hold
	// counts, so the slowness comes from the lock and not from setup timing.
	monitor, err := pgx.Connect(ctx, dsn("inventory", "inventory"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = monitor.Close(context.Background()) }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := monitor.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity a
				JOIN pg_locks l ON l.pid = a.pid AND NOT l.granted
				WHERE a.wait_event_type='Lock' AND a.state='active'
				  AND a.backend_type='client backend'
				  AND a.query LIKE '%FROM inventory_pools%FOR UPDATE%'
				  AND a.pid <> pg_backend_pid()`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d reservations queued on the pool lock", waiting, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(checkoutInducedHold)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	close(results)
	var ds []time.Duration
	for r := range results {
		if r.code != http.StatusCreated {
			t.Fatalf("reservation behind the held lock = %d %.300s, want 201 (slow, not failed)", r.code, r.body)
		}
		ds = append(ds, r.d)
	}
	return traceID, ds
}

// postWithKeyAndHeadersAsync is postWithKeyAndHeaders for a worker goroutine: it reports
// a transport failure or a contract violation with t.Error, never t.Fatal.
func postWithKeyAndHeadersAsync(t *testing.T, url, key string, hdr map[string]string, body any) (int, []byte) {
	code, out, req, header, err := doPostWithHeaders(url, key, hdr, body)
	if err != nil {
		t.Error(err)
		return 0, []byte(err.Error())
	}
	validateServiceResponseAsync(t, req, code, header, out)
	return code, out
}

type inducedPoint struct {
	At    time.Time
	Value float64
}

// inducedSeries turns a Grafana time-series result into points per service_name.
func inducedSeries(t *testing.T, res map[string]any) map[string][]inducedPoint {
	t.Helper()
	frames, err := resultFrames(res)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]inducedPoint{}
	for _, f := range frames {
		if len(f.Schema.Fields) < 2 || len(f.Data.Values) < 2 {
			continue
		}
		svc := f.Schema.Fields[1].Labels["service_name"]
		for i, ts := range f.Data.Values[0] {
			ms, ok := ts.(float64)
			if !ok || i >= len(f.Data.Values[1]) {
				continue
			}
			if v, ok := numeric(f.Data.Values[1][i]); ok {
				out[svc] = append(out[svc], inducedPoint{time.UnixMilli(int64(ms)).UTC(), v})
			}
		}
	}
	return out
}

// maxIn returns the largest value in the window, or NaN when the window has no point.
func maxIn(points []inducedPoint, win [2]time.Time) float64 {
	m := math.NaN()
	for _, p := range points {
		if p.At.Before(win[0]) || p.At.After(win[1]) {
			continue
		}
		if math.IsNaN(m) || p.Value > m {
			m = p.Value
		}
	}
	return m
}

// lastBefore returns the newest value at or before at, or NaN.
func lastBefore(points []inducedPoint, at time.Time) float64 {
	v := math.NaN()
	for _, p := range points {
		if !p.At.After(at) {
			v = p.Value
		}
	}
	return v
}

// inducedSpanSummary lists every span of a Tempo v2 trace as one line.
func inducedSpanSummary(t *testing.T, body []byte) []string {
	t.Helper()
	type attr struct {
		Key   string         `json:"key"`
		Value map[string]any `json:"value"`
	}
	var doc struct {
		Trace struct {
			ResourceSpans []struct {
				Resource struct {
					Attributes []attr `json:"attributes"`
				} `json:"resource"`
				ScopeSpans []struct {
					Spans []struct {
						Name       string `json:"name"`
						Kind       string `json:"kind"`
						Start      string `json:"startTimeUnixNano"`
						End        string `json:"endTimeUnixNano"`
						Attributes []attr `json:"attributes"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		} `json:"trace"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	value := func(attrs []attr, key string) string {
		for _, a := range attrs {
			if a.Key == key {
				for _, v := range a.Value {
					return fmt.Sprint(v)
				}
			}
		}
		return ""
	}
	var lines []string
	for _, rs := range doc.Trace.ResourceSpans {
		svc := value(rs.Resource.Attributes, "service.name")
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				var start, end int64
				_, _ = fmt.Sscan(s.Start, &start)
				_, _ = fmt.Sscan(s.End, &end)
				path := value(s.Attributes, "url.path")
				if path == "" {
					path = value(s.Attributes, "url.full")
				}
				lines = append(lines, fmt.Sprintf("%s %s %q %s status=%s duration=%.3fs",
					svc, strings.TrimPrefix(s.Kind, "SPAN_KIND_"), s.Name, path,
					value(s.Attributes, "http.response.status_code"), float64(end-start)/1e9))
			}
		}
	}
	sort.Strings(lines)
	return lines
}

// jsonNumber maps NaN (no point in the window) to null: encoding/json refuses NaN.
func jsonNumber(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return v
}

func evidenceSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

func writeEvidenceJSON(t *testing.T, dir, name string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
