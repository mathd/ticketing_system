//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// The voided-ticket feed through the running stack (TKT-162, ADR-066).
//
// This exists for two reasons and it is worth separating them, because only one
// is about correctness. The first is ADR-030's coverage gate: `access` is in the
// gated service list with an empty allowlist, so a documented 2xx operation that
// is never exercised here fails the build. The second is that the feed's whole
// point is to be reachable BY A SCANNER — through the gateway, with a device
// token, past the request validator — and every other test of it stops short of
// that boundary.
//
// What is deliberately NOT asserted here: the tenant scoping and the scan plan.
// Both live in the store's smoke tests, against real PostgreSQL, because that is
// the tier the mechanism lives at — an assertion here would be one tier above the
// SQL that enforces it.
func TestVoidedTicketFeedIsReachableByAnEnrolledScanner(t *testing.T) {
	url := gatewayURL + "/api/access/scans/voided-tickets"

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", url, err)
	}
	req.Header.Set("X-Scanner-Token", scannerToken())

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	// Records the operation for the coverage gate AND checks the response against
	// the contract — a drifted body counts as covered and is then reported as
	// drift, so it can never produce a green run.
	validateServiceResponse(t, resp.Request, resp.StatusCode, resp.Header, body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s", url, resp.StatusCode, body)
	}

	var page struct {
		TicketIDs  []string `json:"ticket_ids"`
		NextCursor *string  `json:"next_cursor"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode feed: %v (%s)", err, body)
	}
	if page.TicketIDs == nil {
		t.Fatalf("ticket_ids is absent or null: %s", body)
	}
	// next_cursor is required-and-nullable, and null here means "this is the whole
	// list". A scanner uses exactly that to decide whether its view is complete,
	// so an absent field would be a silent "keep polling" or a silent "you are
	// done" depending on how carelessly it was read.
	if page.NextCursor != nil && *page.NextCursor == "" {
		t.Fatalf("next_cursor is an empty string; it must be a cursor or null: %s", body)
	}
}

// An unenrolled caller gets 401, not an empty page.
//
// The distinction is the whole safety property: a scanner that receives an empty
// feed caches it as "nothing is revoked" and admits voided holders until its next
// successful pull. Answering 200 with no ids to a caller we cannot identify would
// be worse than answering nothing at all.
func TestVoidedTicketFeedRefusesAnUnenrolledScanner(t *testing.T) {
	url := gatewayURL + "/api/access/scans/voided-tickets"

	for name, token := range map[string]string{
		"absent": "",
		"wrong":  "not-an-enrolled-token",
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if token != "" {
				req.Header.Set("X-Scanner-Token", token)
			}
			resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", url, err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			validateServiceResponse(t, resp.Request, resp.StatusCode, resp.Header, body)

			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unenrolled (%s) = %d %s, want 401 — an empty page reads as 'nothing is revoked'", name, resp.StatusCode, body)
			}
		})
	}
}

// The abuse telemetry, through the running stack (TKT-272).
//
// This is the WIRING test, and the distinction is the whole point of it. The
// unit tests in services/access/internal/api prove the emitter emits; deleting
// `WithScannerTelemetry(...)` from main.go leaves every one of them green,
// because they build their own Server. That is exactly TKT-202's F3/F7 finding
// — a test that proves the component while the component is not installed. So
// this one asserts against the REAL binary's output: the access container's log,
// and the metric the real meter exported.
//
// It asserts on the device id captured at enrolment rather than one derived
// from the request, because the operator's path is
// `abuse.request` record -> device id -> `access revoke-scanner <id>`, and a
// test that inferred the id from the request would prove the round trip without
// proving the identity is the enrolled one.
func TestFeedAbuseTelemetryNamesTheDeviceAnOperatorWouldRevoke(t *testing.T) {
	deviceID := os.Getenv("SMOKE_SCANNER_DEVICE_ID")
	if deviceID == "" {
		t.Fatal("SMOKE_SCANNER_DEVICE_ID is unset — scripts/smoke.sh must export the enrolled device id")
	}

	url := gatewayURL + "/api/access/scans/voided-tickets"
	code, _ := get(t, url, map[string]string{"X-Scanner-Token": scannerToken()})
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The log carries the device identity. This is the operator's signal and the
	// one sink that must not be lossy.
	//
	// Two SEPARATE assertions over the log, and the separation is the point
	// (ai-review F2). Checking the token only inside the abuse.request line
	// would leave a leak from any OTHER emitter — the request logger, the auth
	// path, a future middleware — green, and the requirement is that the token
	// reaches NO log line. So: find the identity in its own record, and check
	// the absence across every line the request produced.
	retry(t, 20*time.Second, func() error {
		out, err := dockerRun(ctx, "compose", "-p", project, "logs", "--no-color", "access")
		if err != nil {
			return fmt.Errorf("compose logs access: %v: %s", err, out)
		}
		var found bool
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, `"msg":"abuse.request"`) && strings.Contains(line, deviceID) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("no abuse.request record naming device %s in the access log yet", deviceID)
		}
		// Global absence, over the WHOLE log rather than one selected record.
		// Not a retry condition: a leak does not become un-leaked by waiting.
		if strings.Contains(out, scannerToken()) {
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, scannerToken()) {
					t.Fatalf("the scanner token reached the access log: %s", line)
				}
			}
		}
		return nil
	})

	// And the aggregate counter reached Prometheus, which proves ObserveMetrics
	// ran on the real meter rather than a test one.
	retry(t, 30*time.Second, func() error {
		query := neturl.QueryEscape(`access_abuse_requests_total{service_name="access"}`)
		code, body := get(t, promURL+"/api/v1/query?query="+query, nil)
		if code != http.StatusOK || !strings.Contains(string(body), `"result":[{`) {
			return fmt.Errorf("abuse counter not exported yet: %d %s", code, body)
		}
		if strings.Contains(string(body), scannerToken()) {
			return fmt.Errorf("the scanner token appears in exported metrics: %s", body)
		}
		return nil
	})
}
