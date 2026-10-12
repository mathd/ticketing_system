//go:build smoke

package smoke_test

// TKT-337: the provisioned checkout-path dashboard is live on the composed stack.
//
// This test reads the dashboard that Grafana serves, NOT a copy of its queries. It runs
// every panel target through Grafana's own /api/ds/query, so Grafana expands
// $__rate_interval and resolves each datasource uid. Those two decisions belong to
// Grafana, and a test that expanded them itself would prove only its own expansion. The
// test replaces only the two template variables, which Grafana expands in the browser
// and never on the server.
//
// It generates its own traffic, and it asks of each piece what would notice its absence:
//
//   - Two fake-auth-hold checkouts, at least checkoutDashboardFailureGap apart. Payments
//     answers each one 500 and commerce answers 202 payment_unknown. A new
//     status="500" series starts at 1, and rate() cannot see that first increment, so
//     ONE failure can leave the panel at 0 for ever. The second failure, in a later
//     export, is what makes the payments 5xx value positive. The 5xx query has a zero
//     fallback for services with traffic, so only a POSITIVE payments value proves the
//     numerator's status selector works.
//   - One paid checkout with the test's own sampled traceparent. The checkout-traces
//     panel query must return THAT trace id, and Tempo must return its commerce
//     POST /orders span and its payments /internal/charges span.
//   - The two failed checkouts also carry their own traceparents, and the payment-failure
//     panel query must return at least one of them.
//
// No t.Parallel(): TestOnsaleLoadProof resets pg_stat_statements and measures a window,
// and this test must not put traffic into it. For the same reason a cleanup makes the
// recovery runner resolve both payment_unknown orders before the test ends
// (drainPaymentUnknownOnCleanup).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var grafanaURL = env("SMOKE_GRAFANA_URL", "http://localhost:3000")

const (
	checkoutDashboardUID = "ticketing-checkout-path"

	// The panel titles this test requires. A panel renamed in the JSON fails here, and
	// that is intended: the README and the operator procedure name these panels.
	panelRequestRate    = "Request rate by service"
	panelErrorRate      = "5xx rate by service"
	panelLatency        = "Latency by service (p50, p95, p99)"
	panelCheckoutTraces = "Checkout traces: commerce POST /orders"
	panelPaymentErrors  = "Payment failures: payments spans with status 5xx"

	// checkoutDashboardFailureGap is more than two 5 s metric export intervals, so the
	// second failed payment lands in a later export than the first.
	checkoutDashboardFailureGap = 12 * time.Second
)

// checkoutPathServices are the four services the dashboard compares, in the order the
// README names them. The `service` variable has exactly these values.
var checkoutPathServices = []string{"gateway", "inventory", "commerce", "payments"}

// dashboardPanel is the part of a Grafana panel this test reads. Targets stay raw so the
// test sends Grafana each target exactly as the dashboard stores it.
type dashboardPanel struct {
	Title      string           `json:"title"`
	Type       string           `json:"type"`
	Datasource *datasourceRef   `json:"datasource"`
	Targets    []map[string]any `json:"targets"`
	Panels     []dashboardPanel `json:"panels"`
}

type datasourceRef struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

type checkoutDashboard struct {
	Meta struct {
		Provisioned           bool   `json:"provisioned"`
		ProvisionedExternalID string `json:"provisionedExternalId"`
	} `json:"meta"`
	Dashboard struct {
		UID    string           `json:"uid"`
		Title  string           `json:"title"`
		Panels []dashboardPanel `json:"panels"`
	} `json:"dashboard"`
}

func TestCheckoutDashboardProvisionedAndLive(t *testing.T) {
	dash := fetchCheckoutDashboard(t)
	panels := checkoutDashboardPanels(t, dash)
	assertDashboardDatasources(t, panels)

	// Traffic. Three fresh offers: each checkout reserves 2 of a venue's 5 seats, and a
	// payment_unknown order keeps its seats until recovery runs.
	_, holdOffer1 := setupCheckoutOffer(t, "dash-hold-1-"+uuid.NewString()[:8])
	_, paidOffer := setupCheckoutOffer(t, "dash-paid-"+uuid.NewString()[:8])
	_, holdOffer2 := setupCheckoutOffer(t, "dash-hold-2-"+uuid.NewString()[:8])

	windowStart := time.Now().Add(-30 * time.Second)
	holdTrace1, holdOrder1 := tracedCheckout(t, holdOffer1, "fake-auth-hold", http.StatusAccepted, "payment_unknown")
	drainPaymentUnknownOnCleanup(t, holdOrder1)
	firstFailure := time.Now()
	paidTrace, _ := tracedCheckout(t, paidOffer, "fake-ok", http.StatusOK, "completed")
	if wait := checkoutDashboardFailureGap - time.Since(firstFailure); wait > 0 {
		time.Sleep(wait)
	}
	holdTrace2, holdOrder2 := tracedCheckout(t, holdOffer2, "fake-auth-hold", http.StatusAccepted, "payment_unknown")
	drainPaymentUnknownOnCleanup(t, holdOrder2)

	// Metrics: every Prometheus target, through Grafana, until the signature appears.
	// 120 s covers several 5 s exports of every service, the gateway included.
	retry(t, 120*time.Second, func() error {
		return checkPrometheusPanels(t, panels)
	})

	// Traces: every Tempo target, through Grafana, over a window that holds this test's
	// checkouts and little else.
	retry(t, 90*time.Second, func() error {
		from, to := windowStart, time.Now().Add(30*time.Second)
		ids, err := tempoPanelTraceIDs(t, panels[panelCheckoutTraces], from, to)
		if err != nil {
			return err
		}
		if !slices.Contains(ids, paidTrace) {
			return fmt.Errorf("%q returned %d trace(s), not the paid checkout's trace %s: %v", panelCheckoutTraces, len(ids), paidTrace, ids)
		}
		ids, err = tempoPanelTraceIDs(t, panels[panelPaymentErrors], from, to)
		if err != nil {
			return err
		}
		if !slices.Contains(ids, holdTrace1) && !slices.Contains(ids, holdTrace2) {
			return fmt.Errorf("%q returned %d trace(s), neither failed checkout's trace (%s, %s): %v", panelPaymentErrors, len(ids), holdTrace1, holdTrace2, ids)
		}
		return nil
	})

	// The drill-down target: the trace the panel lists is a real checkout, with spans.
	retry(t, 60*time.Second, func() error {
		spans, err := tempoTraceServerSpans(t, paidTrace)
		if err != nil {
			return err
		}
		for _, want := range []string{"commerce POST /orders", "payments POST /internal/charges"} {
			if !slices.Contains(spans, want) {
				return fmt.Errorf("trace %s has server spans %v, missing %q", paidTrace, spans, want)
			}
		}
		return nil
	})
}

// fetchCheckoutDashboard polls for the provisioned dashboard. Grafana loads file
// providers at start-up, so a missing mount answers 404 for the whole poll.
func fetchCheckoutDashboard(t *testing.T) checkoutDashboard {
	t.Helper()
	var dash checkoutDashboard
	retry(t, 60*time.Second, func() error {
		code, body := grafanaRequest(t, http.MethodGet, "/api/dashboards/uid/"+checkoutDashboardUID, nil)
		if code != http.StatusOK {
			return fmt.Errorf("GET dashboard %s: %d %.300s", checkoutDashboardUID, code, body)
		}
		dash = checkoutDashboard{}
		return json.Unmarshal(body, &dash)
	})
	if dash.Dashboard.UID != checkoutDashboardUID {
		t.Fatalf("dashboard uid = %q, want %q", dash.Dashboard.UID, checkoutDashboardUID)
	}
	if !dash.Meta.Provisioned {
		t.Fatalf("dashboard %s is not provisioned (meta.provisioned=false): it was saved by hand, not loaded from deploy/grafana", checkoutDashboardUID)
	}
	return dash
}

// checkoutDashboardPanels flattens rows and requires each named panel exactly once.
func checkoutDashboardPanels(t *testing.T, dash checkoutDashboard) map[string]dashboardPanel {
	t.Helper()
	byTitle := map[string]dashboardPanel{}
	var walk func([]dashboardPanel)
	walk = func(ps []dashboardPanel) {
		for _, p := range ps {
			if len(p.Targets) > 0 {
				if _, dup := byTitle[p.Title]; dup {
					t.Fatalf("two panels have the title %q", p.Title)
				}
				byTitle[p.Title] = p
			}
			walk(p.Panels)
		}
	}
	walk(dash.Dashboard.Panels)
	for _, title := range []string{panelRequestRate, panelErrorRate, panelLatency, panelCheckoutTraces, panelPaymentErrors} {
		if _, ok := byTitle[title]; !ok {
			t.Fatalf("dashboard %s has no panel %q (panels with queries: %v)", checkoutDashboardUID, title, mapsKeys(byTitle))
		}
	}
	if n := len(byTitle[panelLatency].Targets); n != 3 {
		t.Fatalf("%q has %d targets, want 3 (p50, p95, p99)", panelLatency, n)
	}
	return byTitle
}

// assertDashboardDatasources requires every target to name a datasource uid that
// Grafana has, with the type the panel expects.
func assertDashboardDatasources(t *testing.T, panels map[string]dashboardPanel) {
	t.Helper()
	code, body := grafanaRequest(t, http.MethodGet, "/api/datasources", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/datasources: %d %.300s", code, body)
	}
	var list []datasourceRef
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	known := map[string]string{}
	for _, ds := range list {
		known[ds.UID] = ds.Type
	}
	wantType := map[string]string{
		panelRequestRate: "prometheus", panelErrorRate: "prometheus", panelLatency: "prometheus",
		panelCheckoutTraces: "tempo", panelPaymentErrors: "tempo",
	}
	for title, want := range wantType {
		for _, target := range panels[title].Targets {
			ref := targetDatasource(panels[title], target)
			if ref.UID == "" || known[ref.UID] != want {
				t.Fatalf("%q target %v uses datasource %+v; Grafana has %v, want a %s datasource", title, target["refId"], ref, known, want)
			}
		}
	}
}

func targetDatasource(p dashboardPanel, target map[string]any) datasourceRef {
	if raw, ok := target["datasource"].(map[string]any); ok {
		return datasourceRef{Type: fmt.Sprint(raw["type"]), UID: fmt.Sprint(raw["uid"])}
	}
	if p.Datasource != nil {
		return *p.Datasource
	}
	return datasourceRef{}
}

// checkPrometheusPanels runs each Prometheus target through Grafana and checks the
// values a reader of the panel would see at the newest point.
func checkPrometheusPanels(t *testing.T, panels map[string]dashboardPanel) error {
	t.Helper()
	to := time.Now()
	from := to.Add(-10 * time.Minute)
	for _, title := range []string{panelRequestRate, panelErrorRate, panelLatency} {
		for _, target := range panels[title].Targets {
			last, err := prometheusPanelLastValues(t, panels[title], target, from, to)
			if err != nil {
				return fmt.Errorf("%q %v: %w", title, target["refId"], err)
			}
			for _, service := range checkoutPathServices {
				v, ok := last[service]
				if !ok {
					return fmt.Errorf("%q %v: no series for %s (got %v)", title, target["refId"], service, last)
				}
				switch title {
				case panelRequestRate, panelLatency:
					if !(v > 0) || math.IsInf(v, 0) {
						return fmt.Errorf("%q %v: %s = %v, want a finite value above 0", title, target["refId"], service, v)
					}
				case panelErrorRate:
					switch service {
					case "payments":
						if !(v > 0) {
							return fmt.Errorf("%q: payments = %v, want above 0 after two failed payments", title, v)
						}
					case "commerce", "gateway":
						// Commerce answers a failed payment with 202 payment_unknown, so
						// a payment failure must not show as a commerce or gateway 5xx.
						if v != 0 {
							return fmt.Errorf("%q: %s = %v, want 0", title, service, v)
						}
					}
				}
			}
		}
	}
	return nil
}

// templateVariable finds a Grafana template variable reference other than the
// built-in $__ ones, which the server expands.
var templateVariable = regexp.MustCompile(`\$(\{[^}_][^}]*\}|[A-Za-z][A-Za-z0-9_]*)`)

// expandDashboardVariables does what the browser does for the dashboard's default
// selection: every service, method POST.
func expandDashboardVariables(s string) (string, error) {
	s = strings.ReplaceAll(s, "${service:regex}", "("+strings.Join(checkoutPathServices, "|")+")")
	s = strings.ReplaceAll(s, "${method:regex}", "POST")
	if left := templateVariable.FindString(s); left != "" {
		return "", fmt.Errorf("unexpanded template variable %s in %q", left, s)
	}
	return s, nil
}

// grafanaQuery posts one target, with its variables expanded, to /api/ds/query.
func grafanaQuery(t *testing.T, p dashboardPanel, target map[string]any, from, to time.Time) (map[string]any, error) {
	t.Helper()
	q := map[string]any{}
	for k, v := range target {
		q[k] = v
	}
	for _, key := range []string{"expr", "query"} {
		if s, ok := q[key].(string); ok {
			expanded, err := expandDashboardVariables(s)
			if err != nil {
				return nil, err
			}
			q[key] = expanded
		}
	}
	ref := targetDatasource(p, target)
	q["datasource"] = map[string]string{"type": ref.Type, "uid": ref.UID}
	q["intervalMs"] = 15000
	q["maxDataPoints"] = 240
	payload := map[string]any{
		"queries": []any{q},
		"from":    strconv.FormatInt(from.UnixMilli(), 10),
		"to":      strconv.FormatInt(to.UnixMilli(), 10),
	}
	code, body := grafanaRequest(t, http.MethodPost, "/api/ds/query", payload)
	if code != http.StatusOK {
		return nil, fmt.Errorf("ds/query: %d %.500s", code, body)
	}
	var out struct {
		Results map[string]map[string]any `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	res, ok := out.Results[fmt.Sprint(target["refId"])]
	if !ok {
		return nil, fmt.Errorf("ds/query answered no result for refId %v: %.300s", target["refId"], body)
	}
	if e, ok := res["error"]; ok && e != nil && e != "" {
		return nil, fmt.Errorf("ds/query error: %v", e)
	}
	return res, nil
}

type grafanaFrame struct {
	Schema struct {
		Fields []struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"fields"`
	} `json:"schema"`
	Data struct {
		Values [][]any `json:"values"`
	} `json:"data"`
}

func resultFrames(res map[string]any) ([]grafanaFrame, error) {
	raw, err := json.Marshal(res["frames"])
	if err != nil {
		return nil, err
	}
	var frames []grafanaFrame
	return frames, json.Unmarshal(raw, &frames)
}

// prometheusPanelLastValues returns, per service_name, the newest non-null value.
func prometheusPanelLastValues(t *testing.T, p dashboardPanel, target map[string]any, from, to time.Time) (map[string]float64, error) {
	t.Helper()
	res, err := grafanaQuery(t, p, target, from, to)
	if err != nil {
		return nil, err
	}
	frames, err := resultFrames(res)
	if err != nil {
		return nil, err
	}
	last := map[string]float64{}
	for _, f := range frames {
		if len(f.Schema.Fields) < 2 || len(f.Data.Values) < 2 {
			continue
		}
		service := f.Schema.Fields[1].Labels["service_name"]
		vals := f.Data.Values[1]
		for i := len(vals) - 1; i >= 0; i-- {
			if v, ok := numeric(vals[i]); ok {
				last[service] = v
				break
			}
		}
	}
	return last, nil
}

func numeric(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x)
	case string: // Grafana sends NaN and +Inf as strings in some encodings
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil && !math.IsNaN(f)
	}
	return 0, false
}

// tempoPanelTraceIDs runs a panel's TraceQL through Grafana and returns the trace ids.
func tempoPanelTraceIDs(t *testing.T, p dashboardPanel, from, to time.Time) ([]string, error) {
	t.Helper()
	var ids []string
	for _, target := range p.Targets {
		res, err := grafanaQuery(t, p, target, from, to)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", p.Title, err)
		}
		frames, err := resultFrames(res)
		if err != nil {
			return nil, err
		}
		for _, f := range frames {
			for i, field := range f.Schema.Fields {
				if field.Name != "traceID" || i >= len(f.Data.Values) {
					continue
				}
				for _, v := range f.Data.Values[i] {
					ids = append(ids, normalizeTraceID(fmt.Sprint(v)))
				}
			}
		}
	}
	return ids, nil
}

// normalizeTraceID left-pads to 32 hex digits: Tempo drops leading zeros.
func normalizeTraceID(id string) string {
	id = strings.ToLower(id)
	if len(id) < 32 {
		id = strings.Repeat("0", 32-len(id)) + id
	}
	return id
}

// tempoTraceServerSpans fetches a trace through Grafana's Tempo datasource and returns
// its server spans as "<service> <method> <url.path>".
func tempoTraceServerSpans(t *testing.T, traceID string) ([]string, error) {
	t.Helper()
	code, body := grafanaRequest(t, http.MethodGet, "/api/datasources/proxy/uid/tempo/api/v2/traces/"+traceID, nil)
	if code != http.StatusOK {
		return nil, fmt.Errorf("GET trace %s: %d %.300s", traceID, code, body)
	}
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
						Kind       string `json:"kind"`
						Attributes []attr `json:"attributes"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		} `json:"trace"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
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
	var spans []string
	for _, rs := range doc.Trace.ResourceSpans {
		service := value(rs.Resource.Attributes, "service.name")
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				if s.Kind != "SPAN_KIND_SERVER" {
					continue
				}
				spans = append(spans, fmt.Sprintf("%s %s %s", service, value(s.Attributes, "http.request.method"), value(s.Attributes, "url.path")))
			}
		}
	}
	return spans, nil
}

// newTraceparent returns a sampled W3C traceparent and its trace id.
func newTraceparent() (header, traceID string) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	traceID = hex.EncodeToString(b[:16])
	return fmt.Sprintf("00-%s-%s-01", traceID, hex.EncodeToString(b[16:])), traceID
}

// tracedCheckout reserves and checks out one offer through the gateway, with the
// checkout request carrying a fresh sampled traceparent. It returns the trace id and
// the order id.
func tracedCheckout(t *testing.T, ticketType, paymentToken string, wantCode int, wantStatus string) (string, string) {
	t.Helper()
	reservation := reserveCheckout(t, ticketType, "dash-reserve-"+uuid.NewString())
	traceparent, traceID := newTraceparent()
	code, body := postWithKeyAndHeaders(t, gatewayURL+"/api/commerce/orders", "dash-order-"+uuid.NewString(),
		map[string]string{"traceparent": traceparent},
		map[string]any{"reservation_id": reservation["reservation_id"], "name": "Dashboard Buyer",
			"email": "dashboard@example.test", "payment_token": paymentToken})
	if code != wantCode {
		t.Fatalf("checkout with %s = %d %s, want %d", paymentToken, code, body, wantCode)
	}
	var out struct {
		OrderID string `json:"order_id"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != wantStatus {
		t.Fatalf("checkout with %s: status %q, want %q", paymentToken, out.Status, wantStatus)
	}
	return traceID, out.OrderID
}

// drainPaymentUnknownOnCleanup makes the recovery runner resolve a payment_unknown order
// before the test ends, and waits for it. Left alone, the runner resolves the order
// after its 2-minute grace period, releases the seats, and so writes claim_history rows
// at a time no test controls. TestOnsaleLoadProof's full profile counts EVERY
// claim_history INSERT in its 3-minute window, so a late release can read as a defect
// in the load proof. A full-profile run of this test without the cleanup counted 27001
// INSERTs, want 27000; that release is the probable cause, not a proven one. The ageing
// UPDATE is the one TestRecoveryDrainsPaymentUnknownViaStatusAndVoid uses; the runner's
// own predicates stay untouched.
func drainPaymentUnknownOnCleanup(t *testing.T, orderID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		db := commerceDB(t, ctx)
		tag, err := db.Exec(ctx, `UPDATE orders SET updated_at=now()-interval '10 minutes',
			recovery_next_attempt_at=now() WHERE id=$1`, orderID)
		if err != nil || tag.RowsAffected() != 1 {
			t.Errorf("age payment_unknown order %s for recovery: %v (rows=%d)", orderID, err, tag.RowsAffected())
			return
		}
		retry(t, 60*time.Second, func() error {
			code, body, _ := getWithHeaders(t, gatewayURL+"/api/commerce/orders/"+orderID)
			if code != http.StatusOK {
				return fmt.Errorf("order %s = %d %.200s", orderID, code, body)
			}
			var state struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(body, &state); err != nil {
				return err
			}
			if state.Status != "timeout" {
				return fmt.Errorf("order %s status %q, want timeout (recovery not done)", orderID, state.Status)
			}
			return nil
		})
	})
}

// postWithKeyAndHeaders is postWithKey with extra request headers. It validates the
// response against the service contract, as every smoke helper that receives a service
// response must (TKT-95).
func postWithKeyAndHeaders(t *testing.T, url, key string, hdr map[string]string, body any) (int, []byte) {
	t.Helper()
	code, out, req, header, err := doPostWithHeaders(url, key, hdr, body)
	if err != nil {
		t.Fatal(err)
	}
	validateServiceResponse(t, req, code, header, out)
	return code, out
}

// doPostWithHeaders sends the request and returns what the two wrappers validate.
func doPostWithHeaders(url, key string, hdr map[string]string, body any) (int, []byte, *http.Request, http.Header, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, nil, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, nil, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, nil, nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, nil, fmt.Errorf("POST %s: read body: %w", url, err)
	}
	return resp.StatusCode, out, resp.Request, resp.Header, nil
}

// grafanaRequest calls Grafana with no credentials: the lgtm image enables anonymous
// access, so the test needs and stores no password.
func grafanaRequest(t *testing.T, method, path string, payload any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, grafanaURL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func mapsKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
