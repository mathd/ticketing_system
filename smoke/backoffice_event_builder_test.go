//go:build smoke

package smoke_test

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TKT-192 US-B3, through the real stack: an admin authors and publishes a
// sellable event using only the back-office forms.
//
// "Only the forms" is the point of COS-1, so every step here is a form POST
// through the gateway to the Astro SSR layer — not a catalog API call. The one
// exception is the refusal fixtures below, which the builder CANNOT create
// (it authors neither festivals nor archived slots) and which are therefore
// built through the API. A fixture the code under test constructs would only
// prove the code agrees with itself.

// postBuilder submits a builder form to the page URL carrying the progress query
// string, exactly as the browser would after the previous redirect.
func postBuilder(t *testing.T, c *http.Client, form url.Values, eventID, performanceID, ticketTypeID string) *http.Response {
	t.Helper()
	q := url.Values{}
	if eventID != "" {
		q.Set("event", eventID)
	}
	if performanceID != "" {
		q.Set("performance", performanceID)
	}
	if ticketTypeID != "" {
		q.Set("ticket_type", ticketTypeID)
	}
	target := gatewayURL + "/admin/events/new"
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	return postForm(t, c, target, form)
}

// followStep submits one builder form and returns the redirect Location, which
// carries the id the next step needs.
func followStep(t *testing.T, c *http.Client, form url.Values, eventID, performanceID, ticketTypeID string) string {
	t.Helper()
	resp := postBuilder(t, c, form, eventID, performanceID, ticketTypeID)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("%s: status %d, want 303; body=%.400s",
			form.Get("_action"), resp.StatusCode, readBody(t, resp))
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatalf("%s: 303 with no Location", form.Get("_action"))
	}
	return loc
}

func idFrom(t *testing.T, location, key string) string {
	t.Helper()
	re := regexp.MustCompile(fmt.Sprintf(`[?&]%s=([^&]+)`, regexp.QuoteMeta(key)))
	m := re.FindStringSubmatch(location)
	if len(m) != 2 {
		t.Fatalf("no %s in %q", key, location)
	}
	return m[1]
}

func TestBackofficeEventBuilderPublishesASellableEvent(t *testing.T) {
	client := signInAs(t, "admin")
	// Unique names: the storefront list is an ADR-004 minutes-tier read, so a
	// shared fixture name could match a page cached before this run published.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// 1. The event.
	loc := followStep(t, client, url.Values{
		"_action": {"create-event"},
		"name_en": {"Builder Night " + suffix},
		"name_fr": {"Nuit Builder " + suffix},
	}, "", "", "")
	eventID := idFrom(t, loc, "event")

	// 2. The dated slot. RFC 3339 with an explicit offset plus the IANA zone —
	// the builder refuses to guess a zone, so the test does not either.
	loc = followStep(t, client, url.Values{
		"_action":   {"create-performance"},
		"venue_id":  {seededVenueID},
		"starts_at": {time.Now().Add(72 * time.Hour).Format(time.RFC3339)},
		"timezone":  {"Europe/Paris"},
	}, eventID, "", "")
	performanceID := idFrom(t, loc, "performance")

	// 3. The price, in minor units.
	loc = followStep(t, client, url.Values{
		"_action":    {"create-ticket-type"},
		"tt_name_en": {"Standard"},
		"tt_name_fr": {"Standard"},
		"amount":     {"4550"},
		"currency":   {"EUR"},
	}, eventID, performanceID, "")
	ticketTypeID := idFrom(t, loc, "ticket_type")

	// 4. Publish.
	loc = followStep(t, client, url.Values{"_action": {"publish-performance"}},
		eventID, performanceID, ticketTypeID)
	if !strings.Contains(loc, "published=1") {
		t.Fatalf("publish did not report success: %q", loc)
	}

	// COS-2: a buyer can now see it. Asserted from the STOREFRONT render, not
	// the catalog API — the API proves the write, the storefront proves the sale.
	retry(t, 30*time.Second, func() error {
		// `locale` is required by listPublicEvents — the aggregated read returns
		// localized text and will not guess which locale. Omitting it is a 400,
		// not an empty list.
		code, body := get(t, gatewayURL+"/api/catalog/public/events?locale=en", nil)
		if code != http.StatusOK {
			return fmt.Errorf("public events: %d", code)
		}
		if !strings.Contains(string(body), "Builder Night "+suffix) {
			return fmt.Errorf("published event not yet on the public read")
		}
		return nil
	})

	// ...and it is sellable: inventory is provisioned asynchronously from the
	// publication event, so this retries rather than assuming.
	retry(t, 30*time.Second, func() error {
		// ReservationCreate is a strict XOR: exactly three properties, and
		// `performance_id` is not one of them — the ticket type identifies the
		// slot. additionalProperties:false means guessing is a 400, not a
		// tolerated extra field.
		code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", "builder-"+suffix,
			map[string]any{
				"organizer_id":   organizerID,
				"ticket_type_id": ticketTypeID,
				"quantity":       1,
			})
		if code != http.StatusCreated && code != http.StatusOK {
			return fmt.Errorf("reserve: %d: %s", code, body)
		}
		return nil
	})
}

// COS-3: catalog owns the refusal and the builder shows it. The fixture is built
// through the API because the builder cannot author a festival day — a fixture
// the code under test could create would prove only self-consistency.
func TestBackofficeEventBuilderSurfacesAPublishRefusal(t *testing.T) {
	client := signInAs(t, "admin")

	// A slot with no ticket type: reachable through the builder alone (skip the
	// price step), and the refusal an operator will actually hit.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	loc := followStep(t, client, url.Values{
		"_action": {"create-event"},
		"name_en": {"Unpriced " + suffix},
		"name_fr": {"Sans prix " + suffix},
	}, "", "", "")
	eventID := idFrom(t, loc, "event")
	loc = followStep(t, client, url.Values{
		"_action":   {"create-performance"},
		"venue_id":  {seededVenueID},
		"starts_at": {time.Now().Add(96 * time.Hour).Format(time.RFC3339)},
		"timezone":  {"Europe/Paris"},
	}, eventID, "", "")
	performanceID := idFrom(t, loc, "performance")

	// Publish without a price. Catalog refuses (409, "performance has no ticket
	// type"), and the page must RENDER that — 200 with the message, not a 500
	// and not a redirect claiming success.
	resp := postBuilder(t, client, url.Values{"_action": {"publish-performance"}},
		eventID, performanceID, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a refused publish must re-render, got %d; body=%.400s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "performance has no ticket type") {
		t.Fatalf("the refusal message from catalog is not on the page; body=%.600s", body)
	}

	// ai-review F7: a string on the page proves only that a string is on the
	// page — the builder could hard-code it and this would pass. What proves
	// catalog REFUSED is that the slot is still not on sale. Only published
	// slots carrying a ticket type appear on the public read, so absence here is
	// the refusal having actually happened.
	code, listBody := get(t, gatewayURL+"/api/catalog/public/events?locale=en", nil)
	if code != http.StatusOK {
		t.Fatalf("public events: %d", code)
	}
	if strings.Contains(string(listBody), "Unpriced "+suffix) {
		t.Fatalf("the refused slot is on sale anyway — the page reported a refusal that did not happen")
	}
}

// COS-7: the builder is admin-only, and the URL is refused for everyone else —
// not merely unlinked.
func TestBackofficeEventBuilderIsAdminOnly(t *testing.T) {
	for _, role := range []string{"box_office", "finance"} {
		t.Run(role, func(t *testing.T) {
			client := signInAs(t, role)

			list := readBody(t, doRequest(t, client, http.MethodGet, gatewayURL+"/admin/", nil, nil))
			if strings.Contains(list, "/admin/events/new") {
				t.Fatalf("%s is offered a builder link it may not use", role)
			}
			resp := doRequest(t, client, http.MethodGet, gatewayURL+"/admin/events/new", nil, nil)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s reached the builder by URL: %d — hiding the link is not the enforcement",
					role, resp.StatusCode)
			}
		})
	}
}
