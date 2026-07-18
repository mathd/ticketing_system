// Package loadtest is the TKT-82 on-sale load harness: an open-loop request
// scheduler, nearest-rank percentiles, stage stability classification and the
// post-drain accounting invariants. It is deliberately client-only — the
// authoritative no-oversell verdict comes from the database, not from here.
package loadtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	KindOK       = "ok"       // full hold→finalize→confirm lifecycle succeeded
	KindRejected = "rejected" // expected 409 (pool or tail sellout)
	// KindClientError is a transport-level failure with no delivered status
	// (timeout, dial, connection loss), or a truncated success body. It may be
	// client resource exhaustion or a server-caused reset — indistinguishable
	// from here, so it is never publishable as a server verdict: it makes a
	// run inconclusive (TKT-92). A delivered forbidden status classifies
	// server-side even when its body was then cut off.
	KindClientError = "client-error"
	// KindServerError is a delivered response the protocol forbids: unexpected
	// HTTP status or a malformed body on a success status. Server-side by
	// construction — evidence of instability.
	KindServerError = "server-error"
)

type Stage struct {
	Name     string
	Rate     int // attempts per second
	Duration time.Duration
	Quantity int // units per attempt
}

// Outcome is one attempt's classification plus its mutation latencies
// (successful attempts only; ADR-004: no cached read appears in any sample).
type Outcome struct {
	Kind                               string
	Note                               string // error diagnostic (status + body snippet)
	Hold, Finalize, Confirm, Lifecycle time.Duration
}

type AttemptFunc func(stage Stage, seq int) Outcome

type StageResult struct {
	Stage                              Stage
	Offered                            int // arrivals derived from the stage clock
	Started                            int
	Dropped                            int // arrivals refused because the in-flight cap was saturated
	OK                                 int
	Rejected                           int
	ClientErrors                       int // transport-level (err != nil) — generator health, not a server verdict
	ServerErrors                       int // delivered but protocol-forbidden responses
	MaxInFlight                        int // configured concurrency bound
	PeakInFlight                       int // highest observed semaphore occupancy
	Elapsed                            time.Duration
	ErrorNotes                         []string        // first few error diagnostics
	Lag                                []time.Duration // scheduled-start lag of started attempts
	Hold, Finalize, Confirm, Lifecycle []time.Duration
}

// testBeforeAttempt runs as the worker goroutine's first statement, before the
// lag capture. Test-only seam (TestLagMeasuredAtAttemptStart): goroutine-start
// delay cannot be forced deterministically under the preemptive scheduler, so
// the ordering "lag is captured after the goroutine is running" is pinned by
// delaying here and asserting the delay shows up in the sample. Nil in prod.
var testBeforeAttempt func()

// RunStage drives one stage open-loop: every arrival time is computed from the
// stage start (never from the previous completion), so a saturated server shows
// up as drops and lag instead of a silently stretched schedule (coordinated
// omission). maxInFlight bounds concurrency; an arrival that cannot acquire a
// slot at its scheduled instant is recorded as dropped, not delayed.
func RunStage(stage Stage, maxInFlight int, attempt AttemptFunc) StageResult {
	res := StageResult{Stage: stage, MaxInFlight: maxInFlight}
	res.Offered = int(int64(stage.Rate) * int64(stage.Duration) / int64(time.Second))
	interval := time.Second / time.Duration(stage.Rate)

	// The counter IS the semaphore: admission and the occupancy sample are one
	// atomic Add, so PeakInFlight is exact — the peak can only occur at an
	// admission instant, and that instant's occupancy is the Add's return value.
	var inFlight atomic.Int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < res.Offered; i++ {
		scheduled := start.Add(time.Duration(i) * interval)
		if d := time.Until(scheduled); d > 0 {
			time.Sleep(d)
		}
		n := int(inFlight.Add(1))
		if n > maxInFlight {
			inFlight.Add(-1)
			res.Dropped++
			continue
		}
		res.Started++
		if n > res.PeakInFlight { // only this goroutine writes PeakInFlight
			res.PeakInFlight = n
		}
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			defer inFlight.Add(-1)
			if testBeforeAttempt != nil {
				testBeforeAttempt()
			}
			// Measured at attempt start, not spawn (TKT-93): post-spawn
			// goroutine-scheduling delay is exactly the backpressure the
			// CeilingInconclusive lag-p99 guard exists to catch.
			lag := time.Since(scheduled)
			out := attempt(stage, seq)
			mu.Lock()
			defer mu.Unlock()
			res.Lag = append(res.Lag, lag)
			switch out.Kind {
			case KindOK:
				res.OK++
				res.Hold = append(res.Hold, out.Hold)
				res.Finalize = append(res.Finalize, out.Finalize)
				res.Confirm = append(res.Confirm, out.Confirm)
				res.Lifecycle = append(res.Lifecycle, out.Lifecycle)
			case KindRejected:
				res.Rejected++
			case KindClientError:
				res.ClientErrors++
				if len(res.ErrorNotes) < 3 {
					res.ErrorNotes = append(res.ErrorNotes, out.Note)
				}
			default: // KindServerError and anything unrecognized fail closed as server-side
				res.ServerErrors++
				if len(res.ErrorNotes) < 3 {
					res.ErrorNotes = append(res.ErrorNotes, out.Note)
				}
			}
		}(i)
	}
	wg.Wait()
	res.Elapsed = time.Since(start)
	return res
}

// Percentile is nearest-rank over raw samples. p in (0,100].
func Percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), samples...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := int(float64(len(s))*p/100 + 0.9999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(s) {
		rank = len(s)
	}
	return s[rank-1]
}

// Stable: every offered arrival was started (zero drops), every started attempt
// came back as a delivered outcome with no errors of either class, and delivery
// met minRatio. Expected rejections are delivered outcomes — a sold-out pool
// answering 409 is the system working. Requiring ClientErrors == 0 here is
// fail-closed belt-and-suspenders: CeilingInconclusive normally aborts the run
// first, but a caller that skips that guard must still never see a client-error
// stage reported stable.
func (r StageResult) Stable(minRatio float64) bool {
	delivered := r.OK + r.Rejected
	return r.Dropped == 0 && r.ClientErrors == 0 && r.ServerErrors == 0 && r.Offered > 0 &&
		float64(delivered) >= minRatio*float64(r.Offered)
}

// CeilingInconclusive reports whether a stage's evidence is unusable for any
// server verdict — a generator-health problem, not instability. Three rules:
// the schedule was not sustained (lag p99 over lagLimit), any client-side
// transport failure, or drops at the in-flight cap while the lifecycle SLO
// still held (a generator limit, not a knee). Returns a diagnostic reason for
// the caller's fatal message.
func CeilingInconclusive(r StageResult, lagLimit, lifecycleSLO time.Duration) (reason string, inconclusive bool) {
	if lag := Percentile(r.Lag, 99); lag > lagLimit {
		return fmt.Sprintf("scheduler lag p99 %v exceeds %v — the generator did not sustain the offered rate", lag, lagLimit), true
	}
	if r.ClientErrors > 0 {
		return fmt.Sprintf("%d client-side transport errors — client exhaustion or connection loss, not a server verdict", r.ClientErrors), true
	}
	if r.Dropped > 0 && r.ServerErrors == 0 && r.Rejected == 0 && Percentile(r.Lifecycle, 99) <= lifecycleSLO {
		return fmt.Sprintf("%d arrivals dropped at the in-flight cap while the SLO held — generator-limited", r.Dropped), true
	}
	return "", false
}

// CeilingBracket returns the highest stable offered rate and the first
// unstable one, judged by the caller's stability predicate (delivery + any
// latency SLO — a stage that answers slowly forever is not a ceiling). When
// every stage was stable the true knee was not observed: the result is a lower
// bound, and firstUnstable is 0.
func CeilingBracket(stages []StageResult, stable func(StageResult) bool) (highestStable, firstUnstable float64, lowerBoundOnly bool) {
	for _, s := range stages {
		if stable(s) {
			highestStable = float64(s.Stage.Rate)
			continue
		}
		return highestStable, float64(s.Stage.Rate), false
	}
	return highestStable, 0, true
}

// Accounting is the post-drain database snapshot the no-oversell verdict is
// judged on. LiveHeld uses ADR-010's real predicate (held-and-unexpired, plus
// finalizing) as queried; GrantedQuantity is the client-side sum of accepted
// units.
type Accounting struct {
	Capacity             int `json:"capacity"`
	PoolConfirmed        int `json:"pool_confirmed"`
	LiveHeld             int `json:"live_held"`
	SumConfirmedClaims   int `json:"sum_confirmed_claims"`
	GrantedQuantity      int `json:"granted_quantity"`
	HistoryCreate        int `json:"history_create"`
	HistoryFinalize      int `json:"history_finalize"`
	HistoryConfirm       int `json:"history_confirm"`
	SuccessfulLifecycles int `json:"successful_lifecycles"`
}

func (a Accounting) Violations() []string {
	var v []string
	if a.PoolConfirmed+a.LiveHeld > a.Capacity {
		v = append(v, fmt.Sprintf("oversell: confirmed %d + live held %d > capacity %d", a.PoolConfirmed, a.LiveHeld, a.Capacity))
	}
	if a.GrantedQuantity > a.Capacity {
		v = append(v, fmt.Sprintf("grants %d exceed capacity %d", a.GrantedQuantity, a.Capacity))
	}
	if a.SumConfirmedClaims != a.PoolConfirmed {
		v = append(v, fmt.Sprintf("claim ledger %d != pool confirmed counter %d", a.SumConfirmedClaims, a.PoolConfirmed))
	}
	for action, n := range map[string]int{"create": a.HistoryCreate, "finalize": a.HistoryFinalize, "confirm": a.HistoryConfirm} {
		if n != a.SuccessfulLifecycles {
			v = append(v, fmt.Sprintf("history %s count %d != successful lifecycles %d", action, n, a.SuccessfulLifecycles))
		}
	}
	return v
}

// HistoryStats is the claim_history INSERT overhead line (ADR-023 amendment),
// as reported by pg_stat_statements: aggregate DB execution time of the
// appendHistory statement, not a causal with/without delta.
type HistoryStats struct {
	Calls    int64   `json:"calls"`
	TotalMs  float64 `json:"total_exec_ms"`
	MeanMs   float64 `json:"mean_exec_ms"`
	MinMs    float64 `json:"min_exec_ms"`
	MaxMs    float64 `json:"max_exec_ms"`
	StddevMs float64 `json:"stddev_exec_ms"`
}

type StageReport struct {
	Name          string  `json:"name"`
	OfferedRate   float64 `json:"offered_rate_per_s"`
	AchievedRate  float64 `json:"achieved_ok_per_s"`
	Offered       int     `json:"offered"`
	Started       int     `json:"started"`
	Dropped       int     `json:"dropped"`
	OK            int     `json:"ok"`
	Rejected      int     `json:"rejected"`
	Errors        int     `json:"errors"` // derived: client_errors + server_errors (schema compatibility)
	ClientErrors  int     `json:"client_errors"`
	ServerErrors  int     `json:"server_errors"`
	MaxInFlight   int     `json:"max_in_flight"`
	PeakInFlight  int     `json:"peak_in_flight"`
	HoldP50Ms     float64 `json:"hold_p50_ms"`
	HoldP95Ms     float64 `json:"hold_p95_ms"`
	HoldP99Ms     float64 `json:"hold_p99_ms"`
	FinalizeP99Ms float64 `json:"finalize_p99_ms"`
	ConfirmP99Ms  float64 `json:"confirm_p99_ms"`
	LifeP50Ms     float64 `json:"lifecycle_p50_ms"`
	LifeP95Ms     float64 `json:"lifecycle_p95_ms"`
	LifeP99Ms     float64 `json:"lifecycle_p99_ms"`
	LagP99Ms      float64 `json:"scheduled_lag_p99_ms"`
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func (r StageResult) Report() StageReport {
	sr := StageReport{
		Name: r.Stage.Name, Offered: r.Offered, Started: r.Started, Dropped: r.Dropped,
		OK: r.OK, Rejected: r.Rejected, Errors: r.ClientErrors + r.ServerErrors,
		ClientErrors: r.ClientErrors, ServerErrors: r.ServerErrors,
		MaxInFlight: r.MaxInFlight, PeakInFlight: r.PeakInFlight,
		OfferedRate:   float64(r.Stage.Rate),
		HoldP50Ms:     ms(Percentile(r.Hold, 50)),
		HoldP95Ms:     ms(Percentile(r.Hold, 95)),
		HoldP99Ms:     ms(Percentile(r.Hold, 99)),
		FinalizeP99Ms: ms(Percentile(r.Finalize, 99)),
		ConfirmP99Ms:  ms(Percentile(r.Confirm, 99)),
		LifeP50Ms:     ms(Percentile(r.Lifecycle, 50)),
		LifeP95Ms:     ms(Percentile(r.Lifecycle, 95)),
		LifeP99Ms:     ms(Percentile(r.Lifecycle, 99)),
		LagP99Ms:      ms(Percentile(r.Lag, 99)),
	}
	if r.Elapsed > 0 {
		sr.AchievedRate = float64(r.OK) / r.Elapsed.Seconds()
	}
	return sr
}

type Report struct {
	Ticket  string `json:"ticket"`
	Profile string `json:"profile"`
	GitSHA  string `json:"git_sha"`
	Host    struct {
		OS   string `json:"os"`
		Arch string `json:"arch"`
		CPUs int    `json:"cpus"`
	} `json:"host"`
	Notes                 string        `json:"notes,omitempty"`
	ClientMaxConnsPerHost int           `json:"client_max_conns_per_host,omitempty"`
	Stages                []StageReport `json:"stages"`
	Accounting            []Accounting  `json:"accounting"`
	History               *HistoryStats `json:"claim_history_insert,omitempty"`
	CeilingHighestStable  float64       `json:"ceiling_highest_stable_per_s"`
	CeilingFirstUnstable  float64       `json:"ceiling_first_unstable_per_s"`
	CeilingLowerBoundOnly bool          `json:"ceiling_lower_bound_only"`
}

// MaxConnsPerHost bounds the shared transport (TKT-92): without it a 3,000/s
// sweep stage could open 12k concurrent connections (rate×4 in-flight cap).
// 4096 preserves the rate×4 Little's-law headroom through the observed 600/s
// knee; above ~1024/s a saturated bound surfaces as client errors or drops,
// which CeilingInconclusive turns into a generator verdict — never a published
// server ceiling.
const MaxConnsPerHost = 4096

// NewClient is the load-generator HTTP client: aggressive connection reuse (at
// thousands of attempts/min the default 2 idle conns per host turns the client
// into the bottleneck), the TKT-92 transport bound, and a fail-closed redirect
// policy (TKT-93): a redirect is delivered as the 3xx response itself for the
// attempt to classify — never a followed hop, so a custom header (the internal
// token) can never be re-sent by the transport (ADR-028 posture).
func NewClient() *http.Client {
	return &http.Client{
		Transport:     &http.Transport{MaxIdleConns: 1024, MaxIdleConnsPerHost: 1024, MaxConnsPerHost: MaxConnsPerHost},
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ClassifyHold409 discriminates a hold 409 body (TKT-93): only the bare
// {"error":…} shape — what problem() in services/inventory/internal/api/server.go
// emits for capacity sellout (ErrUnavailable), ErrConflict and ErrIdempotency —
// counts as an expected capacity rejection. A body carrying any machine-readable
// code (today slot_archived / slot_closed) is not sellout, and anything
// unrecognized — a code, no error field, unparseable — fails closed as
// KindServerError per ADR-028's drift posture:
// server-error is the class that cannot be silently blessed as capacity
// evidence. Cost accepted at plan: a new inventory 409 code breaks the harness
// until taught here.
func ClassifyHold409(body []byte) string {
	var b struct {
		Error *string `json:"error"`
		Code  string  `json:"code"`
	}
	if json.Unmarshal(body, &b) != nil || b.Error == nil || b.Code != "" {
		return KindServerError
	}
	return KindRejected
}

// ValidTransitionBody reports whether a finalize/confirm 200 body is the claim
// object in the target state (TKT-93): parseable JSON whose status equals the
// step's target. Status only — full-shape validation is the contract
// middleware's job (ADR-028); this is malformed-success-body detection, which
// the taxonomy classifies server-side (KindServerError doc above).
func ValidTransitionBody(body []byte, wantStatus string) bool {
	var b struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(body, &b) == nil && b.Status == wantStatus
}

func NewReport(ticket, profile, gitSHA string) *Report {
	r := &Report{Ticket: ticket, Profile: profile, GitSHA: gitSHA}
	r.Host.OS, r.Host.Arch, r.Host.CPUs = runtime.GOOS, runtime.GOARCH, runtime.NumCPU()
	return r
}
