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
}

func TestSchedulerClassifiesOutcomes(t *testing.T) {
	stage := Stage{Name: "s", Rate: 50, Duration: 100 * time.Millisecond, Quantity: 1}
	res := RunStage(stage, 16, func(_ Stage, seq int) Outcome {
		switch seq % 3 {
		case 0:
			return Outcome{Kind: KindOK, Hold: time.Millisecond, Finalize: time.Millisecond, Confirm: time.Millisecond, Lifecycle: 3 * time.Millisecond}
		case 1:
			return Outcome{Kind: KindRejected}
		default:
			return Outcome{Kind: KindError}
		}
	})
	if res.OK == 0 || res.Rejected == 0 || res.Errors == 0 {
		t.Fatalf("classification missing: %+v", res)
	}
	if res.OK+res.Rejected+res.Errors != res.Started {
		t.Fatalf("outcomes %d+%d+%d != started %d", res.OK, res.Rejected, res.Errors, res.Started)
	}
	if len(res.Lifecycle) != res.OK {
		t.Fatalf("lifecycle samples %d, want one per OK %d", len(res.Lifecycle), res.OK)
	}
}

func TestStableRequiresCleanDelivery(t *testing.T) {
	clean := StageResult{Stage: Stage{Rate: 10, Duration: time.Second}, Offered: 10, Started: 10, OK: 10}
	if !clean.Stable(0.99) {
		t.Fatal("clean full delivery must be stable")
	}
	for name, r := range map[string]StageResult{
		"drops":  {Offered: 10, Started: 9, Dropped: 1, OK: 9},
		"errors": {Offered: 10, Started: 10, OK: 9, Errors: 1},
		"short":  {Offered: 100, Started: 100, OK: 90, Rejected: 0},
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

func TestCeilingBracket(t *testing.T) {
	mk := func(rate int, stable bool) StageResult {
		r := StageResult{Stage: Stage{Rate: rate, Duration: time.Second}, Offered: rate, Started: rate, OK: rate}
		if !stable {
			r.Errors = 1
			r.OK--
		}
		return r
	}
	hi, first, lower := CeilingBracket([]StageResult{mk(75, true), mk(150, true), mk(300, false)}, 0.99)
	if hi != 150 || first != 300 || lower {
		t.Fatalf("bracket = (%v,%v,%v), want (150,300,false)", hi, first, lower)
	}
	hi, first, lower = CeilingBracket([]StageResult{mk(75, true), mk(150, true)}, 0.99)
	if hi != 150 || first != 0 || !lower {
		t.Fatalf("all-stable bracket = (%v,%v,%v), want (150,0,true) lower bound only", hi, first, lower)
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

func TestReportJSONCarriesRequiredMetadata(t *testing.T) {
	r := NewReport("TKT-82", "full", "abc123")
	r.Stages = []StageReport{{Name: "nfr", OfferedRate: 50, AchievedRate: 49.9}}
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
}
