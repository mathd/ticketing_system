package loadtest

import (
	"encoding/json"
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
}
