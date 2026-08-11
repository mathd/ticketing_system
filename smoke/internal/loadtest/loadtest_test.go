package loadtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPercentileNearestRank(t *testing.T) {
	var s []time.Duration
	for i := 1; i <= 100; i++ {
		s = append(s, time.Duration(i)*time.Millisecond)
	}
	for _, tc := range []struct {
		p    float64
		want time.Duration
	}{{50, 50 * time.Millisecond}, {95, 95 * time.Millisecond}, {99, 99 * time.Millisecond}, {100, 100 * time.Millisecond}} {
		if got := Percentile(s, tc.p); got != tc.want {
			t.Errorf("p%v = %v, want %v", tc.p, got, tc.want)
		}
	}
	if got := Percentile([]time.Duration{7 * time.Millisecond}, 99); got != 7*time.Millisecond {
		t.Errorf("single sample p99 = %v", got)
	}
	if got := Percentile(nil, 99); got != 0 {
		t.Errorf("empty p99 = %v, want 0", got)
	}
	// Input order must not matter.
	if got := Percentile([]time.Duration{30, 10, 20}, 50); got != 20 {
		t.Errorf("unsorted p50 = %v, want 20", got)
	}
}

// The scheduler is open-loop: arrivals are derived from the stage clock, not from
// completions. A slow attempt with a tiny in-flight cap must surface as recorded
// drops, never as a stretched schedule.
func TestOpenLoopSchedulerDerivesArrivalsFromTheClock(t *testing.T) {
	stage := Stage{Name: "s", Rate: 100, Duration: 200 * time.Millisecond, Quantity: 1}
	res := RunStage(stage, 1, func(Stage, int) Outcome {
		time.Sleep(150 * time.Millisecond)
		return Outcome{Kind: KindOK}
	})
	if res.Offered != 20 {
		t.Fatalf("offered %d, want 20 (rate*duration)", res.Offered)
	}
	if res.Dropped == 0 {
		t.Fatalf("in-flight cap 1 with 150ms attempts at 100/s must drop arrivals")
	}
	if res.Started+res.Dropped != res.Offered {
		t.Fatalf("started %d + dropped %d != offered %d", res.Started, res.Dropped, res.Offered)
	}
	if res.OK != res.Started {
		t.Fatalf("ok %d, want %d", res.OK, res.Started)
	}
	if res.MaxInFlight != 1 || res.PeakInFlight != 1 {
		t.Fatalf("in-flight bounds max=%d peak=%d, want 1/1", res.MaxInFlight, res.PeakInFlight)
	}
}

func TestSchedulerClassifiesOutcomes(t *testing.T) {
	stage := Stage{Name: "s", Rate: 50, Duration: 100 * time.Millisecond, Quantity: 1}
	res := RunStage(stage, 16, func(_ Stage, seq int) Outcome {
		switch seq % 4 {
		case 0:
			return Outcome{Kind: KindOK, Hold: time.Millisecond, Finalize: time.Millisecond, Confirm: time.Millisecond, Lifecycle: 3 * time.Millisecond}
		case 1:
			return Outcome{Kind: KindRejected}
		case 2:
			return Outcome{Kind: KindClientError}
		default:
			return Outcome{Kind: KindServerError}
		}
	})
	if res.OK == 0 || res.Rejected == 0 || res.ClientErrors == 0 || res.ServerErrors == 0 {
		t.Fatalf("classification missing: %+v", res)
	}
	if res.OK+res.Rejected+res.ClientErrors+res.ServerErrors != res.Started {
		t.Fatalf("outcomes %d+%d+%d+%d != started %d", res.OK, res.Rejected, res.ClientErrors, res.ServerErrors, res.Started)
	}
	if len(res.Lifecycle) != res.OK {
		t.Fatalf("lifecycle samples %d, want one per OK %d", len(res.Lifecycle), res.OK)
	}
}

// An unrecognized outcome kind must fail closed into the server-side count —
// never silently vanish from accounting.
func TestSchedulerFailsClosedOnUnknownKind(t *testing.T) {
	stage := Stage{Name: "s", Rate: 20, Duration: 100 * time.Millisecond, Quantity: 1}
	res := RunStage(stage, 16, func(Stage, int) Outcome { return Outcome{Kind: "bogus"} })
	if res.ServerErrors != res.Started || res.ClientErrors != 0 {
		t.Fatalf("unknown kind must count as server error: %+v", res)
	}
}

func TestStableRequiresCleanDelivery(t *testing.T) {
	clean := StageResult{Stage: Stage{Rate: 10, Duration: time.Second}, Offered: 10, Started: 10, OK: 10}
	if !clean.Stable(0.99) {
		t.Fatal("clean full delivery must be stable")
	}
	for name, r := range map[string]StageResult{
		"drops":         {Offered: 10, Started: 9, Dropped: 1, OK: 9},
		"client errors": {Offered: 10, Started: 10, OK: 9, ClientErrors: 1},
		"server errors": {Offered: 10, Started: 10, OK: 9, ServerErrors: 1},
		"short":         {Offered: 100, Started: 100, OK: 90, Rejected: 0},
	} {
		if r.Stable(0.99) {
			t.Errorf("%s must be unstable: %+v", name, r)
		}
	}
	// Expected rejections (a full pool) still count as delivered responses.
	sellout := StageResult{Offered: 10, Started: 10, OK: 4, Rejected: 6}
	if !sellout.Stable(0.99) {
		t.Fatal("expected 409s are delivered outcomes, not instability")
	}
}

// COS 3(a): a stage that started every arrival (zero drops) but scheduled them
// late must be inconclusive — lag without drops is a generator that silently
// stretched the offered rate.
func TestCeilingInconclusiveOnLateScheduleWithoutDrops(t *testing.T) {
	r := StageResult{Stage: Stage{Name: "s", Rate: 10, Duration: time.Second}, Offered: 10, Started: 10, OK: 10}
	for i := 0; i < 10; i++ {
		r.Lag = append(r.Lag, 2*time.Second)
		r.Lifecycle = append(r.Lifecycle, 10*time.Millisecond)
	}
	reason, inconclusive := CeilingInconclusive(r, time.Second, 3*time.Second)
	if !inconclusive || reason == "" {
		t.Fatalf("late schedule without drops must be inconclusive, got (%q, %v)", reason, inconclusive)
	}
}

// COS 3(b): drops accompanied by client-side errors are the generator failing,
// not the server — inconclusive even when the lifecycle SLO is already blown
// (the over-SLO sample pins that the old drops-with-SLO-met branch is not what
// makes this pass).
func TestCeilingInconclusiveOnDropsWithClientErrors(t *testing.T) {
	r := StageResult{Stage: Stage{Name: "s", Rate: 10, Duration: time.Second},
		Offered: 10, Started: 8, Dropped: 2, OK: 7, ClientErrors: 1,
		Lag:       []time.Duration{time.Millisecond},
		Lifecycle: []time.Duration{5 * time.Second}}
	reason, inconclusive := CeilingInconclusive(r, time.Second, 3*time.Second)
	if !inconclusive || reason == "" {
		t.Fatalf("drops with client errors must be inconclusive, got (%q, %v)", reason, inconclusive)
	}

	// The pre-existing rule still holds: drops with the SLO met and no errors
	// of either class are generator-limited.
	gen := StageResult{Stage: Stage{Name: "s", Rate: 10, Duration: time.Second},
		Offered: 10, Started: 9, Dropped: 1, OK: 9,
		Lag: []time.Duration{time.Millisecond}, Lifecycle: []time.Duration{10 * time.Millisecond}}
	if reason, inc := CeilingInconclusive(gen, time.Second, 3*time.Second); !inc || reason == "" {
		t.Fatalf("drops with SLO met must stay inconclusive, got (%q, %v)", reason, inc)
	}

	// A server-side failure is a verdict, not a generator problem: conclusive.
	srv := StageResult{Stage: Stage{Name: "s", Rate: 10, Duration: time.Second},
		Offered: 10, Started: 10, OK: 9, ServerErrors: 1,
		Lag: []time.Duration{time.Millisecond}, Lifecycle: []time.Duration{10 * time.Millisecond}}
	if reason, inc := CeilingInconclusive(srv, time.Second, 3*time.Second); inc {
		t.Fatalf("server errors must stay conclusive (unstable), got (%q, %v)", reason, inc)
	}
}

func TestCeilingBracket(t *testing.T) {
	mk := func(rate int, stable bool) StageResult {
		r := StageResult{Stage: Stage{Rate: rate, Duration: time.Second}, Offered: rate, Started: rate, OK: rate}
		if !stable {
			r.ServerErrors = 1
			r.OK--
		}
		return r
	}
	pred := func(r StageResult) bool { return r.Stable(0.99) }
	hi, first, lower := CeilingBracket([]StageResult{mk(75, true), mk(150, true), mk(300, false)}, pred)
	if hi != 150 || first != 300 || lower {
		t.Fatalf("bracket = (%v,%v,%v), want (150,300,false)", hi, first, lower)
	}
	hi, first, lower = CeilingBracket([]StageResult{mk(75, true), mk(150, true)}, pred)
	if hi != 150 || first != 0 || !lower {
		t.Fatalf("all-stable bracket = (%v,%v,%v), want (150,0,true) lower bound only", hi, first, lower)
	}
	// A latency-aware predicate must be able to reject a stage that delivered
	// everything but too slowly — the knee is not only about drops.
	slow := mk(300, true)
	slow.Lifecycle = []time.Duration{5 * time.Second}
	slowAware := func(r StageResult) bool { return r.Stable(0.99) && Percentile(r.Lifecycle, 99) <= 3*time.Second }
	hi, first, lower = CeilingBracket([]StageResult{mk(150, true), slow}, slowAware)
	if hi != 150 || first != 300 || lower {
		t.Fatalf("latency-aware bracket = (%v,%v,%v), want (150,300,false)", hi, first, lower)
	}
}

// TKT-93 finding 1: lag must be measured at attempt start (inside the worker
// goroutine), not at spawn. Goroutine-start delay cannot be forced
// deterministically under the preemptive scheduler, so the ordering is pinned
// via the testBeforeAttempt hook: a delay injected after the goroutine starts
// must show up in the lag sample — it cannot if the capture happened at spawn.
func TestLagMeasuredAtAttemptStart(t *testing.T) {
	const delay = 50 * time.Millisecond
	testBeforeAttempt = func() { time.Sleep(delay) }
	defer func() { testBeforeAttempt = nil }()
	stage := Stage{Name: "s", Rate: 20, Duration: 100 * time.Millisecond, Quantity: 1}
	res := RunStage(stage, 16, func(Stage, int) Outcome { return Outcome{Kind: KindOK} })
	if res.Started == 0 {
		t.Fatal("no attempts started")
	}
	if got := Percentile(res.Lag, 1); got < delay {
		t.Fatalf("min lag %v < injected post-spawn delay %v — lag is captured at spawn, not at attempt start", got, delay)
	}
}

// TKT-93 finding 2: the load client must never follow a redirect — the 3xx is
// delivered for classification and the target is never contacted, so a custom
// header (the internal token) cannot be re-sent by the transport. 307 is the
// variant that would re-send body and headers on a follow; 302 covers the
// common shape.
func TestNewClientNeverFollowsRedirects(t *testing.T) {
	var targetHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) })
	mux.HandleFunc("/redirect302", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/target", http.StatusFound) })
	mux.HandleFunc("/redirect307", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient()
	for path, want := range map[string]int{"/redirect302": http.StatusFound, "/redirect307": http.StatusTemporaryRedirect} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(`{}`))
		req.Header.Set("X-Internal-Token", "secret")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s: redirect must be delivered, not errored: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s: status %d, want the raw %d", path, resp.StatusCode, want)
		}
	}
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("redirect target was contacted %d times — the transport followed a redirect", n)
	}
}

// TKT-93 finding 3: only the bare {"error":…} 409 shape is a plausible capacity
// rejection; coded and unrecognized bodies fail closed as server evidence.
func TestClassifyHold409(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"sellout bare error": {`{"error":"insufficient availability"}`, KindRejected},
		"idempotency bare":   {`{"error":"idempotency conflict"}`, KindRejected},
		"slot archived":      {`{"error":"slot archived","code":"slot_archived"}`, KindServerError},
		"slot closed":        {`{"error":"slot closed","code":"slot_closed"}`, KindServerError},
		// TKT-238. A closed sales window is not capacity evidence: a load run that
		// hit one measured nothing about contention, so it MUST fail closed rather
		// than be counted as a rejection. Pinned by name so a future exemption is
		// a deliberate edit here rather than a quiet reclassification.
		"channel window closed": {`{"error":"channel sales window closed","code":"channel_window_closed"}`, KindServerError},
		// TKT-239: a gated channel refusing for a missing/invalid code is NOT
		// capacity evidence. Every request fails identically regardless of load, so
		// blessing it would let a misconfigured on-sale proof report a clean sellout
		// curve while selling nothing.
		"presale code invalid": {`{"error":"invalid presale code","code":"presale_code_invalid"}`, KindServerError},
		"unknown code":          {`{"error":"x","code":"something_new"}`, KindServerError},
		"not json":              {`not json`, KindServerError},
		"empty object":          {`{}`, KindServerError},
		"empty body":            {``, KindServerError},
		"missing error field":   {`{"code":""}`, KindServerError},
	} {
		if got := ClassifyHold409([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: ClassifyHold409(%q) = %q, want %q", name, tc.body, got, tc.want)
		}
	}
}

// TKT-93 finding 4: a finalize/confirm 200 body must be the claim in the target
// state — empty/malformed success bodies are server errors per the taxonomy.
func TestValidTransitionBody(t *testing.T) {
	for name, tc := range map[string]struct {
		body, want string
		ok         bool
	}{
		"finalizing ok":  {`{"hold_id":"h1","status":"finalizing"}`, "finalizing", true},
		"confirmed ok":   {`{"hold_id":"h1","status":"confirmed"}`, "confirmed", true},
		"wrong status":   {`{"hold_id":"h1","status":"held"}`, "finalizing", false},
		"missing status": {`{"hold_id":"h1"}`, "finalizing", false},
		"empty object":   {`{}`, "confirmed", false},
		"empty body":     {``, "confirmed", false},
		"unparseable":    {`not json`, "confirmed", false},
	} {
		if got := ValidTransitionBody([]byte(tc.body), tc.want); got != tc.ok {
			t.Errorf("%s: ValidTransitionBody(%q, %q) = %v, want %v", name, tc.body, tc.want, got, tc.ok)
		}
	}
}

func TestAccountingViolations(t *testing.T) {
	ok := Accounting{Capacity: 500, PoolConfirmed: 500, LiveHeld: 0, SumConfirmedClaims: 500, GrantedQuantity: 500,
		HistoryCreate: 275, HistoryFinalize: 275, HistoryConfirm: 275, SuccessfulLifecycles: 275}
	if v := ok.Violations(); len(v) != 0 {
		t.Fatalf("equality at capacity must pass, got %v", v)
	}
	for name, a := range map[string]Accounting{
		"oversell":        {Capacity: 10, PoolConfirmed: 8, LiveHeld: 3},
		"grants":          {Capacity: 10, GrantedQuantity: 11},
		"ledger mismatch": {Capacity: 10, PoolConfirmed: 5, SumConfirmedClaims: 4},
		"missing history": {Capacity: 10, HistoryCreate: 4, HistoryFinalize: 5, HistoryConfirm: 5, SuccessfulLifecycles: 5},
	} {
		if len(a.Violations()) == 0 {
			t.Errorf("%s must be a violation: %+v", name, a)
		}
	}
}

// The report must expose the split error counts, the aggregate for report
// compatibility, and the client concurrency bounds.
func TestReportCarriesSplitErrorsAndBounds(t *testing.T) {
	r := StageResult{Stage: Stage{Name: "s", Rate: 10, Duration: time.Second},
		Offered: 10, Started: 10, OK: 7, ClientErrors: 1, ServerErrors: 2,
		MaxInFlight: 16, PeakInFlight: 5}
	sr := r.Report()
	if sr.ClientErrors != 1 || sr.ServerErrors != 2 || sr.Errors != 3 {
		t.Fatalf("split errors client=%d server=%d aggregate=%d, want 1/2/3", sr.ClientErrors, sr.ServerErrors, sr.Errors)
	}
	if sr.MaxInFlight != 16 || sr.PeakInFlight != 5 {
		t.Fatalf("in-flight bounds max=%d peak=%d, want 16/5", sr.MaxInFlight, sr.PeakInFlight)
	}
}

func TestReportJSONCarriesRequiredMetadata(t *testing.T) {
	r := NewReport("TKT-82", "full", "abc123")
	r.ClientMaxConnsPerHost = 4096
	r.Stages = []StageReport{{Name: "nfr", OfferedRate: 50, AchievedRate: 49.9, ClientErrors: 1, ServerErrors: 2, Errors: 3, MaxInFlight: 512, PeakInFlight: 40}}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Ticket != "TKT-82" || back.Profile != "full" || back.GitSHA != "abc123" {
		t.Fatalf("metadata lost: %+v", back)
	}
	if back.Host.CPUs == 0 || back.Host.OS == "" || back.Host.Arch == "" {
		t.Fatalf("host metadata missing: %+v", back.Host)
	}
	if back.ClientMaxConnsPerHost != 4096 {
		t.Fatalf("client conn bound lost: %+v", back)
	}
	s := back.Stages[0]
	if s.ClientErrors != 1 || s.ServerErrors != 2 || s.Errors != 3 || s.MaxInFlight != 512 || s.PeakInFlight != 40 {
		t.Fatalf("stage bounds/split lost in JSON round-trip: %+v", s)
	}
	if back.Partial {
		t.Fatalf("a fresh report must not be marked partial: %+v", back)
	}
	// Explicitly, not via omitempty (TKT-130): a complete run and a report
	// written before the field existed must not both present as an absent key.
	if !strings.Contains(string(b), `"partial": false`) {
		t.Fatalf(`complete report must serialize "partial": false explicitly: %s`, b)
	}
}

// --- TKT-207: the read-load proof's report and its bound ---

func TestReadReportRenamesWritePhaseFieldsForGets(t *testing.T) {
	r := StageResult{
		Stage:   Stage{Name: "cached-high", Rate: 30, Duration: 2 * time.Second},
		Offered: 60, Started: 60, OK: 60, Elapsed: 2 * time.Second,
		Hold: []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond},
	}
	got := r.ReadReport()
	if got.Name != "cached-high" || got.OK != 60 || got.Offered != 60 {
		t.Fatalf("read report lost the stage's shape: %+v", got)
	}
	if got.AchievedRate != 30 {
		t.Fatalf("achieved rate = %v, want 30", got.AchievedRate)
	}
	// The point of a separate type: a GET's latency must not surface as
	// hold_p99_ms or lifecycle_p99_ms, which mean write phases to every existing
	// consumer of `stages`.
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"hold_p", "lifecycle_p", "finalize_", "confirm_"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("read stage JSON contains %q — a GET has no write phases: %s", forbidden, b)
		}
	}
	if !strings.Contains(string(b), "request_p99_ms") {
		t.Errorf("read stage JSON has no request latency: %s", b)
	}
}

// TestFlatReadBoundUsesTheTierNotTheRequestCount is the assertion the whole
// ticket rests on: the ceiling scales with ELAPSED TIME over the tier, and
// nothing else. Boundaries are pinned explicitly, because an off-by-one here is
// the difference between a proof and a formality.
func TestFlatReadBoundUsesTheTierNotTheRequestCount(t *testing.T) {
	for _, tc := range []struct {
		name              string
		elapsed, tier     time.Duration
		statementsPerLoad int
		want              int
	}{
		// The measured window opens AFTER pre-warm, so a warm entry is present at
		// t=0 and only expiries cost a load.
		{"nothing can expire in no time", 0, 5 * time.Second, 1, 0},
		{"just inside the tier", 4999 * time.Millisecond, 5 * time.Second, 1, 1},
		{"exactly the tier", 5 * time.Second, 5 * time.Second, 1, 1},
		{"just past the tier", 5001 * time.Millisecond, 5 * time.Second, 1, 2},
		{"two statements per load doubles it", 5 * time.Second, 5 * time.Second, 2, 2},
		{"a 2s stage against a 300s tier", 2 * time.Second, 5 * time.Minute, 1, 1},
		{"a tier of zero is not a bound", time.Second, 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaxStoreQueries(tc.elapsed, tc.tier, tc.statementsPerLoad); got != tc.want {
				t.Fatalf("MaxStoreQueries(%v, %v, %d) = %d, want %d",
					tc.elapsed, tc.tier, tc.statementsPerLoad, got, tc.want)
			}
		})
	}
}

// TestFlatReadBoundIgnoresTraffic pins request-independence through the
// EVIDENCE type rather than by comparing two identical calls to the same
// function, which the first version did and which proved nothing. Same elapsed,
// wildly different traffic: same verdict.
func TestFlatReadBoundIgnoresTraffic(t *testing.T) {
	const elapsed, tier = 2 * time.Second, 5 * time.Second
	bound := MaxStoreQueries(elapsed, tier, 1)

	light := ReadQueryEvidence{Endpoint: "availability", StoreQueries: 1, MaxAllowed: bound, Requests: 12}
	heavy := ReadQueryEvidence{Endpoint: "availability", StoreQueries: 1, MaxAllowed: bound, Requests: 12000}
	if !light.Flat() || !heavy.Flat() {
		t.Fatal("a cached read is flat regardless of how much traffic it served")
	}
	// And the ceiling itself did not move with the traffic.
	if light.MaxAllowed != heavy.MaxAllowed {
		t.Fatalf("ceiling moved with request count: %d vs %d", light.MaxAllowed, heavy.MaxAllowed)
	}
	// An uncached read at the same traffic is NOT flat — the discriminator.
	uncached := ReadQueryEvidence{Endpoint: "availability", StoreQueries: 12000, MaxAllowed: bound, Requests: 12000}
	if uncached.Flat() {
		t.Fatal("one query per request must not be reported as flat")
	}
}

func TestReadQueryEvidenceFlagsAViolation(t *testing.T) {
	flat := ReadQueryEvidence{Endpoint: "availability", StoreQueries: 4, MaxAllowed: 4}
	if !flat.Flat() {
		t.Fatal("equal to the bound must be flat — the boundary is allowed")
	}
	over := ReadQueryEvidence{Endpoint: "availability", StoreQueries: 5, MaxAllowed: 4}
	if over.Flat() {
		t.Fatal("one query over the bound must not be flat")
	}
}

func TestReportCarriesReadStagesSeparately(t *testing.T) {
	r := NewReport("TKT-207", "gate", "sha")
	r.Stages = append(r.Stages, StageReport{Name: "sustained"})
	r.ReadStages = append(r.ReadStages, ReadStageReport{
		Name:    "cached-high",
		Queries: []ReadQueryEvidence{{Endpoint: "availability", StoreQueries: 2, MaxAllowed: 4}},
	})
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Stages) != 1 || back.Stages[0].Name != "sustained" {
		t.Fatalf("claim stages did not survive: %s", b)
	}
	if len(back.ReadStages) != 1 || len(back.ReadStages[0].Queries) != 1 {
		t.Fatalf("read stages did not survive: %s", b)
	}
	if back.ReadStages[0].Queries[0].StoreQueries != 2 {
		t.Fatalf("query evidence did not survive: %s", b)
	}
}
