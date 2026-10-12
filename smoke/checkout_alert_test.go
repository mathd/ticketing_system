//go:build smoke

package smoke_test

// TKT-338: the provisioned checkout-path 5xx alert fires and resolves on the composed stack.
//
// This test reads the rule that Grafana serves and the alert state that Grafana computes. It
// does not read deploy/grafana/provisioning/alerting/checkout-5xx.yaml. A rule file that
// Grafana does not load, or loads and cannot evaluate, therefore fails here.
//
// Sequence:
//
//  1. The provisioning API serves rule ticketing-checkout-5xx from a file (provenance
//     "file"), not paused, with for 0s and the expected states for no data and errors.
//  2. One paid checkout. It makes each of the four services record a POST, so that each
//     has the zero-fallback series and thus an alert instance. In the gate, earlier tests
//     have already done this, so the gate cannot see if this checkout is removed. It is
//     here so that `-run TestCheckoutAlertFiresAndResolves` also works alone on a new stack.
//  3. Baseline: an evaluation later than the start of this test finds the rule healthy,
//     evaluated every 10 s, with exactly four alert instances, one for each checkout-path
//     service, all Normal. A state with a reason, such as "Normal (NoData)", is not Normal.
//     On a stack that an earlier run used, this waits until the earlier failures leave the
//     1-minute window.
//  4. Two fake-auth-hold checkouts. Payments answers each one 500, and commerce answers
//     202 payment_unknown. A new status="500" series starts at 1, and rate() cannot see
//     that first increment, so the second failure, in a later sample, is what makes the
//     payments rate positive. The test sends the second failure only after the payments
//     counter under the rule's OWN 5xx selector has grown by 1 in Prometheus, and at least
//     checkoutDashboardFailureGap after it. A broken status selector fails here with a
//     clear message.
//  5. Firing: payments is Alerting and the other three are Normal, within 60 s. The test
//     reads the state before it checks the counters, so a slow counter check cannot make
//     it miss the Alerting interval. If payments does not fire, the message includes the
//     counter check.
//  6. The counters under the rule's 5xx selector: payments grew by exactly 2, and the
//     other three services did not grow.
//  7. Resolution: payments is Normal again after an evaluation later than the one that
//     made it fire, the rule is still present and not paused, within 120 s. An instance
//     that disappears is not a resolution: the test requires all four instances. An
//     instance that is "Normal (NoData)" is not a resolution either.
//  8. The counters again, after resolution: payments grew by exactly 2 in total.
//
// Grafana sends no notification for this rule: the image's only contact point is the
// receiver "empty", which has no integration. The alert shows only in Grafana's alerting
// page and API (docs/verification/checkout-dashboard/README.md).
//
// No t.Parallel(), for the reason TestCheckoutDashboardProvisionedAndLive gives: this test
// must not put traffic into TestOnsaleLoadProof's measured windows. tracedCheckout arms the
// recovery cleanup before it submits each order. The file sorts before
// checkout_dashboard_test.go, so this test runs before the dashboard test, and it ends only
// after payments is Normal again.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	checkoutAlertRuleUID = "ticketing-checkout-5xx"

	// checkoutAlertInterval is the rule group's evaluation interval in seconds. It is
	// also the image's minimum (unified_alerting.min_interval).
	checkoutAlertInterval = 10

	// Instance states in Grafana's Prometheus-compatible rules API.
	alertStateNormal   = "Normal"
	alertStateAlerting = "Alerting"
)

// checkoutAlertProvisionedRule is the part of the provisioning API's rule that this test reads.
type checkoutAlertProvisionedRule struct {
	UID          string `json:"uid"`
	Title        string `json:"title"`
	RuleGroup    string `json:"ruleGroup"`
	Condition    string `json:"condition"`
	For          string `json:"for"`
	NoDataState  string `json:"noDataState"`
	ExecErrState string `json:"execErrState"`
	Provenance   string `json:"provenance"`
	IsPaused     bool   `json:"isPaused"`
	Data         []struct {
		RefID string         `json:"refId"`
		Model map[string]any `json:"model"`
	} `json:"data"`
}

// checkoutAlertState is the rule and its alert instances, as Grafana's rules API answers.
type checkoutAlertState struct {
	Interval       int
	Health         string
	LastError      string
	IsPaused       bool
	LastEvaluation time.Time
	Instances      map[string]string // service_name -> state
	Unlabelled     []string          // states of instances with no service_name
}

func TestCheckoutAlertFiresAndResolves(t *testing.T) {
	testStart := time.Now()
	rule := fetchCheckoutAlertRule(t)
	errorSelector := checkoutAlertErrorSelector(t, rule)

	_, paidOffer := setupCheckoutOffer(t, "alert-paid-"+uuid.NewString()[:8])
	_, holdOffer1 := setupCheckoutOffer(t, "alert-hold-1-"+uuid.NewString()[:8])
	_, holdOffer2 := setupCheckoutOffer(t, "alert-hold-2-"+uuid.NewString()[:8])

	// Step 2: see the file comment. The gate cannot notice if this checkout is removed.
	tracedCheckout(t, paidOffer, "fake-ok", http.StatusOK, "completed")

	// Step 3: the baseline. 120 s covers a payments instance that an earlier run left
	// Alerting (it resolves about 60 s after its last failure) and a first export. The
	// evaluation must be later than the start of this test: after a restart, Grafana
	// serves the instance states that it stored, with health ok, until it evaluates the
	// rule again. Those states are not evidence about the rule that is loaded now.
	retry(t, 120*time.Second, func() error {
		s, err := readCheckoutAlertState(t)
		if err != nil {
			return err
		}
		if !s.LastEvaluation.After(testStart) {
			return fmt.Errorf("no evaluation since the test started at %s (last %s)", testStart.UTC().Format(time.RFC3339), s.LastEvaluation.UTC().Format(time.RFC3339))
		}
		return s.require(alertStateNormal, "")
	})

	// Step 4: the induced failures. The second failure is sent only after the payments
	// counter under the rule's own 5xx selector has grown by exactly 1 (see
	// assertCheckoutAlertCounters for why exactly). A sleep alone does
	// not make two samples: if the payments export stalls across the gap, Prometheus first
	// sees a new 500 series at 2, and rate() never goes above 0.
	trafficStart := time.Now()
	tracedCheckout(t, holdOffer1, "fake-auth-hold", http.StatusAccepted, "payment_unknown")
	firstFailure := time.Now()
	retry(t, 60*time.Second, func() error {
		return assertCheckoutAlertCounters(t, errorSelector, trafficStart, 1)
	})
	if wait := checkoutDashboardFailureGap - time.Since(firstFailure); wait > 0 {
		time.Sleep(wait)
	}
	tracedCheckout(t, holdOffer2, "fake-auth-hold", http.StatusAccepted, "payment_unknown")
	secondFailure := time.Now()

	// Step 5: Firing. The test reads the state first and checks the counters only after,
	// so slow requests cannot make the test start to look after the alert has resolved.
	var fired checkoutAlertState
	if err := poll(60*time.Second, 2*time.Second, func() error {
		s, err := readCheckoutAlertState(t)
		if err != nil {
			return err
		}
		if err := s.require(alertStateNormal, "payments"); err != nil {
			return err
		}
		fired = s
		return nil
	}); err != nil {
		counters := "the counters moved as expected"
		if cerr := assertCheckoutAlertCounters(t, errorSelector, trafficStart, 2); cerr != nil {
			counters = cerr.Error()
		}
		t.Fatalf("payments did not fire within 60 s of the second failure: %v; counters: %s", err, counters)
	}
	firedAt := time.Now()
	t.Logf("payments Alerting %.0f s after the first failure, %.0f s after the second (evaluation %s)",
		firedAt.Sub(firstFailure).Seconds(), firedAt.Sub(secondFailure).Seconds(), fired.LastEvaluation.UTC().Format(time.RFC3339))

	// Step 6: both failures reached Prometheus under the rule's 5xx selector, and the other
	// three services had no 5xx. The alert can fire on the first failure when the payments
	// 500 series already exists, so the second increment can arrive after the Firing state.
	// 60 s, the same budget as the first-failure wait: Firing is already recorded.
	retry(t, 60*time.Second, func() error {
		return assertCheckoutAlertCounters(t, errorSelector, trafficStart, 2)
	})

	// Step 7: resolution, after a later evaluation.
	retry(t, 120*time.Second, func() error {
		s, err := readCheckoutAlertState(t)
		if err != nil {
			return err
		}
		if !s.LastEvaluation.After(fired.LastEvaluation) {
			return fmt.Errorf("no evaluation after the one that fired (%s)", fired.LastEvaluation.UTC().Format(time.RFC3339))
		}
		return s.require(alertStateNormal, "")
	})
	t.Logf("payments Normal again %.0f s after it was seen Alerting, %.0f s after the second failure",
		time.Since(firedAt).Seconds(), time.Since(secondFailure).Seconds())

	// Step 8: the counters again, now about a minute after the second failure, so many
	// exports later. Step 6 can pass before the second failure is exported: if a payments 5xx
	// from another source arrived in the gap, the growth is already exactly 2 there. Here
	// that 5xx makes the growth 3, and the test fails.
	if err := assertCheckoutAlertCounters(t, errorSelector, trafficStart, 2); err != nil {
		t.Fatalf("after resolution: %v", err)
	}
}

// require checks the rule's health and settings, and that there are exactly four instances,
// one for each checkout-path service. The instance named by alerting must be Alerting, and
// every other instance must have the state want. An empty alerting name means no instance
// may be Alerting.
func (s checkoutAlertState) require(want, alerting string) error {
	if s.Health != "ok" || s.LastError != "" {
		return fmt.Errorf("rule %s health %q, last error %q, want ok", checkoutAlertRuleUID, s.Health, s.LastError)
	}
	if s.IsPaused {
		return fmt.Errorf("rule %s is paused", checkoutAlertRuleUID)
	}
	if s.Interval != checkoutAlertInterval {
		return fmt.Errorf("rule group interval %d s, want %d s", s.Interval, checkoutAlertInterval)
	}
	if len(s.Unlabelled) > 0 {
		return fmt.Errorf("rule %s has %d instance(s) with no service_name (states %v); want one instance for each service", checkoutAlertRuleUID, len(s.Unlabelled), s.Unlabelled)
	}
	got := mapsKeys(s.Instances)
	if !slices.Equal(got, slices.Sorted(slices.Values(checkoutPathServices))) {
		return fmt.Errorf("rule %s has instances for %v, want exactly %v (states %v)", checkoutAlertRuleUID, got, checkoutPathServices, s.Instances)
	}
	for service, state := range s.Instances {
		// Grafana adds a reason to the state when it did not evaluate a value: with
		// noDataState OK, an instance whose query returns nothing keeps its labels and
		// becomes "Normal (NoData)", and lastEvaluation still advances. Such a state is not
		// a resolution or a baseline. The value field cannot tell the two apart: Grafana
		// sends an empty value for every Normal instance. The exact comparison below also
		// refuses such a state; this check gives the reason in the message.
		if strings.Contains(state, "(") {
			return fmt.Errorf("instance %s is %q: a state with a reason (NoData, Error) is not an evaluated result (states %v)", service, state, s.Instances)
		}
		expected := want
		if service == alerting {
			expected = alertStateAlerting
		}
		if state != expected {
			return fmt.Errorf("instance %s is %q, want %q (states %v)", service, state, expected, s.Instances)
		}
	}
	return nil
}

// fetchCheckoutAlertRule polls the provisioning API for the rule and checks the settings
// that Grafana serves. Grafana reads alerting provisioning files at start-up, so a missing
// mount answers 404 for the whole poll.
func fetchCheckoutAlertRule(t *testing.T) checkoutAlertProvisionedRule {
	t.Helper()
	var rule checkoutAlertProvisionedRule
	retry(t, 60*time.Second, func() error {
		code, body := grafanaRequest(t, http.MethodGet, "/api/v1/provisioning/alert-rules/"+checkoutAlertRuleUID, nil)
		if code != http.StatusOK {
			return fmt.Errorf("GET alert rule %s: %d %.300s", checkoutAlertRuleUID, code, body)
		}
		rule = checkoutAlertProvisionedRule{}
		return json.Unmarshal(body, &rule)
	})
	checks := []struct{ name, got, want string }{
		{"uid", rule.UID, checkoutAlertRuleUID},
		{"provenance", rule.Provenance, "file"},
		{"ruleGroup", rule.RuleGroup, "checkout-local"},
		{"condition", rule.Condition, "C"},
		{"for", rule.For, "0s"},
		{"noDataState", rule.NoDataState, "OK"},
		{"execErrState", rule.ExecErrState, "Error"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Fatalf("alert rule %s: %s = %q, want %q", checkoutAlertRuleUID, c.name, c.got, c.want)
		}
	}
	if rule.IsPaused {
		t.Fatalf("alert rule %s is paused", checkoutAlertRuleUID)
	}
	return rule
}

// checkoutAlertErrorSelector returns the 5xx metric selector of the rule's Prometheus query.
// It does not require the zero-fallback selector: the instance checks see a missing
// fallback, because then the services with no 5xx series have no instance.
func checkoutAlertErrorSelector(t *testing.T, rule checkoutAlertProvisionedRule) string {
	t.Helper()
	var expr string
	for _, d := range rule.Data {
		if d.RefID == "A" {
			expr, _ = d.Model["expr"].(string)
		}
	}
	if expr == "" {
		t.Fatalf("alert rule %s has no query A with an expr", checkoutAlertRuleUID)
	}
	var errors []string
	for _, s := range promSelector.FindAllString(expr, -1) {
		if strings.Contains(s, "http_response_status_code") {
			errors = append(errors, s)
		}
	}
	if len(errors) != 1 {
		t.Fatalf("alert rule %s query A has %d selectors with a status matcher, want 1: %s", checkoutAlertRuleUID, len(errors), expr)
	}
	return errors[0]
}

// assertCheckoutAlertCounters compares the counters under the rule's 5xx selector at
// trafficStart and now: payments must have grown by exactly wantPayments (this test's
// failed payments so far), and the other three services must not have grown. The raw
// counter sees the first increment that rate() cannot see. The counter has no label for each
// request, so "exactly" is what ties the growth to this test: a payments 5xx from another
// source, such as a background worker, makes the growth too large and fails the test instead
// of standing in for one of this test's failures.
func assertCheckoutAlertCounters(t *testing.T, selector string, trafficStart time.Time, wantPayments float64) error {
	t.Helper()
	total := "sum by (service_name) (" + selector + ")"
	before, err := prometheusInstantByService(t, total, trafficStart)
	if err != nil {
		return err
	}
	after, err := prometheusInstantByService(t, total, time.Now())
	if err != nil {
		return err
	}
	if d := after["payments"] - before["payments"]; d != wantPayments {
		return fmt.Errorf("rule 5xx selector %s: payments grew by %v since %s, want exactly %v (this test's failed payments)", selector, d, trafficStart.UTC().Format(time.RFC3339), wantPayments)
	}
	for _, svc := range checkoutPathServices {
		if svc != "payments" && after[svc] != before[svc] {
			return fmt.Errorf("rule 5xx selector %s: %s grew from %v to %v, want no 5xx", selector, svc, before[svc], after[svc])
		}
	}
	return nil
}

// readCheckoutAlertState reads the rule's state from Grafana's Prometheus-compatible rules
// API, filtered to the rule's uid.
func readCheckoutAlertState(t *testing.T) (checkoutAlertState, error) {
	t.Helper()
	path := "/api/prometheus/grafana/api/v1/rules?rule_uid=" + url.QueryEscape(checkoutAlertRuleUID)
	code, body := grafanaRequest(t, http.MethodGet, path, nil)
	if code != http.StatusOK {
		return checkoutAlertState{}, fmt.Errorf("GET %s: %d %.300s", path, code, body)
	}
	var out struct {
		Data struct {
			Groups []struct {
				Interval int `json:"interval"`
				Rules    []struct {
					UID            string    `json:"uid"`
					Health         string    `json:"health"`
					LastError      string    `json:"lastError"`
					IsPaused       bool      `json:"isPaused"`
					LastEvaluation time.Time `json:"lastEvaluation"`
					Alerts         []struct {
						Labels map[string]string `json:"labels"`
						State  string            `json:"state"`
					} `json:"alerts"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return checkoutAlertState{}, err
	}
	var found []checkoutAlertState
	for _, g := range out.Data.Groups {
		for _, r := range g.Rules {
			if r.UID != checkoutAlertRuleUID {
				continue
			}
			s := checkoutAlertState{Interval: g.Interval, Health: r.Health, LastError: r.LastError,
				IsPaused: r.IsPaused, LastEvaluation: r.LastEvaluation, Instances: map[string]string{}}
			for _, a := range r.Alerts {
				service, ok := a.Labels["service_name"]
				if !ok {
					s.Unlabelled = append(s.Unlabelled, a.State)
					continue
				}
				if prev, dup := s.Instances[service]; dup {
					return checkoutAlertState{}, fmt.Errorf("two instances for service %s (%q, %q)", service, prev, a.State)
				}
				s.Instances[service] = a.State
			}
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		return checkoutAlertState{}, fmt.Errorf("rules API lists rule %s %d times, want once: %.300s", checkoutAlertRuleUID, len(found), body)
	}
	return found[0], nil
}
