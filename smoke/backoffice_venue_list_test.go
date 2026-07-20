//go:build smoke

package smoke_test

import (
	"net/http"
	"strings"
	"testing"
)

// US-018 / TKT-101: the back-office venue list, end to end through the real
// stack. Two proofs in one flow:
//   1. catalog's GET /public/venues returns the seeded organizer's venues at
//      the ADR-004 hours tier (proves the migration + handler + scoping — the
//      catalog contract itself is validated in-process, not by the smoke
//      contract gate, which allowlists inventory/commerce/payments/access);
//   2. GET /admin/ renders those venues (proves the gateway /admin/ route +
//      the back-office SSR consuming the generated contract).
//
// The seeded organizer (organizerID, catalog_publication_test.go) and venues
// are migrations (0002 organizer, 0008 venues) — deterministic by construction,
// so the assertions name a specific venue.
const seededVenueName = "La Grande Salle" // migrations/0008_seed_default_venues.sql

func TestBackofficeVenueReadHoursTier(t *testing.T) {
	// One fetch: header and body asserted on the SAME response (getWithHeaders
	// carries a 10s timeout and runs the contract check).
	code, body, hdr := getWithHeaders(t,
		gatewayURL+"/api/catalog/public/venues?organizer_id="+organizerID)
	if code != http.StatusOK {
		t.Fatalf("venue read: status %d: %s", code, body)
	}
	// ADR-004 hours tier — long-lived venue geometry, not the events minutes tier.
	if got := hdr.Get("Cache-Control"); got != "public, max-age=3600, s-maxage=3600" {
		t.Fatalf("venue read must carry the ADR-004 hours tier, got %q", got)
	}
	if !strings.Contains(string(body), seededVenueName) {
		t.Fatalf("venue read must list the seeded venue %q; body=%s", seededVenueName, body)
	}
}

func TestBackofficeShellServedThroughGateway(t *testing.T) {
	code, body := get(t, gatewayURL+"/admin/", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /admin/: status %d: %s", code, body)
	}
	page := string(body)
	// The shell rendered the venue list from the catalog read (COS4).
	if !strings.Contains(page, seededVenueName) {
		t.Fatalf("/admin/ must render the seeded venue %q; page=%.800s", seededVenueName, page)
	}
}
