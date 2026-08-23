//go:build smoke

package smoke_test

import (
	"encoding/json"
	"io"
	"net/http"
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
