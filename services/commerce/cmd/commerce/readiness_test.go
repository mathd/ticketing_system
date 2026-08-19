package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"ticketing/shared/httpx"
)

// Liveness and readiness answer different questions (TKT-163, ADR-062).
//
// They were the same handler until this ticket. `/healthz` answers "this process is
// working" and is what the container healthcheck probes; `/readyz` additionally answers
// "this deployment is configured to keep its promises". Without ACCESS_URL commerce still
// refunds money and still records the obligation, but nothing — the reconciler included —
// can ever discharge it, and a misconfigured deployment must not take traffic.
//
// The seam is this function rather than the running stack on purpose: the smoke stack always
// sets ACCESS_URL, and unsetting it there to prove this would break every other test sharing
// that stack.

func probe(t *testing.T, checks []httpx.NamedCheck) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	httpx.Healthz("commerce", checks...).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health body: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, body
}

// A healthy process with no ACCESS_URL is LIVE but not READY. Both halves are asserted:
// a readiness rule that also fails liveness would take the whole stack down with it, since
// the gateway waits on commerce being *healthy* before it starts.
func TestMissingAccessURLFailsReadinessButNotLiveness(t *testing.T) {
	liveness := []httpx.NamedCheck{httpx.Check("db", func() error { return nil })}

	if code, _ := probe(t, liveness); code != http.StatusOK {
		t.Fatalf("liveness = %d, want 200: a missing ACCESS_URL must not make the process "+
			"look dead — the container healthcheck probes /healthz and the gateway waits on it", code)
	}

	code, body := probe(t, readinessChecks(liveness, ""))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readiness = %d, want 503: a deployment that can never discharge a refund's "+
			"ticket voiding must not be routed refunds", code)
	}
	checks, _ := body["checks"].(map[string]any)
	if checks["access_configured"] != "unhealthy" {
		t.Fatalf("checks = %v, want access_configured unhealthy: the probe must name what is "+
			"missing, or an operator sees only 'degraded'", checks)
	}
}

// The mirror, and the reason the test above proves something: with ACCESS_URL set, readiness
// passes. A check that refuses everything would satisfy the negative case while breaking
// every deployment.
func TestAConfiguredAccessURLIsReady(t *testing.T) {
	liveness := []httpx.NamedCheck{httpx.Check("db", func() error { return nil })}
	code, body := probe(t, readinessChecks(liveness, "http://access:8080"))
	if code != http.StatusOK {
		t.Fatalf("readiness = %d, want 200 with ACCESS_URL set; body=%v", code, body)
	}
}

// The WIRING, not the rule: the two probe sets reach the two paths.
//
// The tests above prove readinessChecks is correct. None of them notices if run() serves the
// LIVENESS set on both paths — the rule stays right and simply stops being reached, which is
// the shape where a guard tests its mechanism and never tests that anything uses it. So this
// asserts at the router, with the two sets made distinguishable by their verdicts: liveness
// passes, readiness does not, and each path must answer with its own.
func TestReadyzServesTheReadinessSetAndHealthzDoesNot(t *testing.T) {
	liveness := []httpx.NamedCheck{httpx.Check("db", func() error { return nil })}
	r := chi.NewRouter()
	mountHealth(r, liveness, readinessChecks(liveness, "")) // ACCESS_URL unset

	get := func(path string) int {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}

	if got := get("/healthz"); got != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200: the readiness set reached the liveness path, so a "+
			"misconfigured ACCESS_URL would fail the container healthcheck and the gateway's "+
			"depends_on would never let the stack start", got)
	}
	if got := get("/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503: the liveness set reached the readiness path, so the "+
			"access_configured check is never actually consulted by anything", got)
	}
}

// Readiness is a superset of liveness, not a replacement: a failing database must still fail
// readiness. Building the two sets independently is how they drift into disagreeing about
// whether the same process is usable.
func TestReadinessStillFailsOnAFailingLivenessCheck(t *testing.T) {
	liveness := []httpx.NamedCheck{httpx.Check("db", func() error { return errDBDown })}
	code, body := probe(t, readinessChecks(liveness, "http://access:8080"))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readiness = %d, want 503 when the database is down; body=%v", code, body)
	}
}

var errDBDown = errDown("database is down")

type errDown string

func (e errDown) Error() string { return string(e) }
