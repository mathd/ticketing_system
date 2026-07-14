//go:build smoke

// Catalog publication end-to-end: create venue/event/performance/ticket type
// through the gateway, publish, assert the domain event on the PLATFORM
// stream (consumer created BEFORE publishing, correlated by performance id),
// then assert the storefront renders it in FR and EN with ADR-004 cache
// tiers and without re-fetching inside the TTL.
package smoke_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const organizerID = "00000000-0000-0000-0000-000000000001" // seeded (ADR-008)

func postJSON(t *testing.T, url string, body any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	validateServiceResponse(t, resp.Request, resp.StatusCode, resp.Header, out)
	return resp.StatusCode, out
}

func getWithHeaders(t *testing.T, url string) (int, []byte, http.Header) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	validateServiceResponse(t, resp.Request, resp.StatusCode, resp.Header, body)
	return resp.StatusCode, body, resp.Header
}

func created(t *testing.T, url string, in any) map[string]any {
	t.Helper()
	code, body := postJSON(t, url, in)
	if code != http.StatusCreated {
		t.Fatalf("POST %s: status %d: %s", url, code, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return out
}

func TestCatalogPublicationAndStorefront(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	suffix := func() string {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		return hex.EncodeToString(b)
	}()
	nameFR, nameEN := "Nuit Électrique "+suffix, "Electric Night "+suffix

	// -- create the fixture through the public contract (AC1) --
	venue := created(t, catalog+"/venues", map[string]any{
		"organizer_id": organizerID, "name": "Le Zénith", "ga_capacity": 500,
	})
	event := created(t, catalog+"/events", map[string]any{
		"organizer_id": organizerID,
		"name":         map[string]string{"fr": nameFR, "en": nameEN},
		"description":  map[string]string{"fr": "Une soirée électro.", "en": "An electro night."},
	})
	perf := created(t, catalog+"/performances", map[string]any{
		"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"],
		"starts_at": "2026-09-18T17:30:00Z", "timezone": "Europe/Paris",
	})
	created(t, catalog+"/ticket-types", map[string]any{
		"organizer_id": organizerID, "performance_id": perf["id"],
		"name":  map[string]string{"fr": "Admission générale", "en": "General admission"},
		"price": map[string]any{"amount": 4550, "currency": "EUR"},
	})

	// Tenancy is enforced through the real stack: a venue owned by an
	// unknown organizer id cannot be wired to this event (AC5).
	if code, _ := postJSON(t, catalog+"/performances", map[string]any{
		"organizer_id": "11111111-1111-1111-1111-111111111111",
		"event_id":     event["id"], "venue_id": venue["id"],
		"starts_at": "2026-09-18T17:30:00Z", "timezone": "Europe/Paris",
	}); code != http.StatusBadRequest && code != http.StatusNotFound {
		t.Fatalf("cross-organizer performance: want 400/404, got %d", code)
	}

	// -- consumer BEFORE publish: no deliver-all false pass (AC2) --
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx := t.Context()
	stream, err := js.Stream(ctx, "PLATFORM")
	if err != nil {
		t.Fatalf("PLATFORM stream: %v", err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "smoke-catalog-publication",
		FilterSubject: "platform.catalog.performance.published",
		DeliverPolicy: jetstream.DeliverNewPolicy,
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}

	// -- publish (AC2); idempotent on re-POST --
	publishURL := fmt.Sprintf("%s/performances/%v/publish", catalog, perf["id"])
	if code, body := postJSON(t, publishURL, nil); code != http.StatusOK {
		t.Fatalf("publish: status %d: %s", code, body)
	}
	if code, _ := postJSON(t, publishURL, nil); code != http.StatusOK {
		t.Fatalf("re-publish must stay 200, got %d", code)
	}

	// -- the domain event, correlated to THIS performance (AC2) --
	var envelope struct {
		ID         string    `json:"id"`
		Type       string    `json:"type"`
		OccurredAt time.Time `json:"occurred_at"`
		Schema     int       `json:"schema"`
		Data       struct {
			PerformanceID string `json:"performance_id"`
			EventID       string `json:"event_id"`
			OrganizerID   string `json:"organizer_id"`
			Capacity      int32  `json:"capacity"`
		} `json:"data"`
	}
	msg, err := cons.Next(jetstream.FetchMaxWait(15 * time.Second))
	if err != nil {
		t.Fatalf("performance.published not received: %v", err)
	}
	_ = msg.Ack()
	if err := json.Unmarshal(msg.Data(), &envelope); err != nil {
		t.Fatalf("envelope: %v (%s)", err, msg.Data())
	}
	if envelope.Type != "platform.catalog.performance.published" ||
		envelope.Schema != 2 || envelope.ID == "" || envelope.Data.Capacity != 500 ||
		envelope.Data.PerformanceID != perf["id"] ||
		envelope.Data.EventID != event["id"] ||
		envelope.Data.OrganizerID != organizerID {
		t.Fatalf("envelope mismatch: %+v (want performance %v)", envelope, perf["id"])
	}

	// -- public reads carry the minutes tier (AC3) and the data (AC1) --
	code, body, hdr := getWithHeaders(t, catalog+"/public/events?locale=fr")
	if code != http.StatusOK {
		t.Fatalf("public list: status %d: %s", code, body)
	}
	if cc := hdr.Get("Cache-Control"); cc != "public, max-age=300, s-maxage=300" {
		t.Fatalf("public list Cache-Control = %q, want the ADR-004 minutes tier", cc)
	}
	if !strings.Contains(string(body), nameFR) {
		t.Fatalf("public list (fr) must contain %q: %.300s", nameFR, body)
	}

	// The committed contract is served byte-identical through the gateway (ADR-009).
	specOnDisk, err := os.ReadFile("../services/catalog/api/openapi.yaml")
	if err != nil {
		t.Fatalf("read committed spec: %v", err)
	}
	code, served, _ := getWithHeaders(t, catalog+"/openapi.yaml")
	if code != http.StatusOK || !bytes.Equal(served, specOnDisk) {
		t.Fatalf("served spec differs from committed contract (status %d, %d vs %d bytes)",
			code, len(served), len(specOnDisk))
	}

	// -- storefront renders FR and EN with localized content, date and
	//    money formats, through the gateway (AC2, AC4) --
	frURL := gatewayURL + "/fr/events"
	code, page, hdr := getWithHeaders(t, frURL)
	if code != http.StatusOK {
		t.Fatalf("storefront fr: status %d", code)
	}
	html := string(page)
	for _, want := range []string{nameFR, "Le Zénith", "45,50", "vendredi 18 septembre 2026", "19:30"} {
		if !strings.Contains(html, want) {
			t.Fatalf("fr list page missing %q:\n%.600s", want, html)
		}
	}
	if cc := hdr.Get("Cache-Control"); !strings.HasPrefix(cc, "public, max-age=") {
		t.Fatalf("fr page Cache-Control = %q, want the minutes tier", cc)
	}

	// No re-fetch inside the TTL (AC3): the second hit is served from the
	// SSR page-data cache — its data age advances and the outgoing TTL is
	// the REMAINING freshness, so page+data staleness never stacks.
	time.Sleep(2 * time.Second)
	code, _, hdr = getWithHeaders(t, frURL)
	if code != http.StatusOK {
		t.Fatalf("storefront fr (second hit): status %d", code)
	}
	var age, maxAge int
	if _, err := fmt.Sscanf(hdr.Get("X-Page-Data-Age"), "%d", &age); err != nil || age < 1 {
		t.Fatalf("second hit X-Page-Data-Age = %q, want >= 1 (data served from cache)", hdr.Get("X-Page-Data-Age"))
	}
	if _, err := fmt.Sscanf(hdr.Get("Cache-Control"), "public, max-age=%d", &maxAge); err != nil || maxAge >= 300 || maxAge <= 0 {
		t.Fatalf("second hit Cache-Control = %q, want remaining freshness < 300", hdr.Get("Cache-Control"))
	}

	code, page, _ = getWithHeaders(t, gatewayURL+"/en/events")
	if code != http.StatusOK {
		t.Fatalf("storefront en: status %d", code)
	}
	html = string(page)
	for _, want := range []string{nameEN, "€45.50", "Friday, September 18, 2026"} {
		if !strings.Contains(html, want) {
			t.Fatalf("en list page missing %q:\n%.600s", want, html)
		}
	}

	// Detail page carries the ticket type at its localized name and price.
	code, page, hdr = getWithHeaders(t, fmt.Sprintf("%s/en/events/%v", gatewayURL, event["id"]))
	if code != http.StatusOK {
		t.Fatalf("storefront detail: status %d", code)
	}
	html = string(page)
	for _, want := range []string{nameEN, "General admission", "€45.50"} {
		if !strings.Contains(html, want) {
			t.Fatalf("en detail page missing %q:\n%.600s", want, html)
		}
	}
	if cc := hdr.Get("Cache-Control"); !strings.HasPrefix(cc, "public, max-age=") {
		t.Fatalf("detail page Cache-Control = %q", cc)
	}

	// Unpublished (draft) performances stay invisible: a fresh event with a
	// draft-only performance must not appear on the public list.
	draftEvent := created(t, catalog+"/events", map[string]any{
		"organizer_id": organizerID,
		"name":         map[string]string{"fr": "Brouillon " + suffix, "en": "Draft " + suffix},
	})
	created(t, catalog+"/performances", map[string]any{
		"organizer_id": organizerID, "event_id": draftEvent["id"], "venue_id": venue["id"],
		"starts_at": "2026-10-01T18:00:00Z", "timezone": "Europe/Paris",
	})
	code, body, _ = getWithHeaders(t, catalog+"/public/events?locale=en")
	if code != http.StatusOK || strings.Contains(string(body), "Draft "+suffix) {
		t.Fatalf("draft performances must not be publicly listed (status %d)", code)
	}

	// Add a second published slot to the original event. Archiving the first
	// must retain this mixed event with only its remaining published slot.
	secondPerf := created(t, catalog+"/performances", map[string]any{
		"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"],
		"starts_at": "2026-09-19T17:30:00Z", "timezone": "Europe/Paris",
	})
	created(t, catalog+"/ticket-types", map[string]any{
		"organizer_id": organizerID, "performance_id": secondPerf["id"],
		"name":  map[string]string{"fr": "Deuxième soir", "en": "Second night"},
		"price": map[string]any{"amount": 5000, "currency": "EUR"},
	})
	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/publish", catalog, secondPerf["id"]), nil); code != http.StatusOK {
		t.Fatalf("publish second slot: status %d: %s", code, body)
	}

	// Consumer exists before archive (DeliverNew): receiving the event cannot
	// be a false pass from an older stream message.
	archiveConsumer, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "smoke-catalog-archive-" + suffix,
		FilterSubject: "platform.catalog.performance.archived",
		DeliverPolicy: jetstream.DeliverNewPolicy,
	})
	if err != nil {
		t.Fatalf("archive consumer: %v", err)
	}

	archiveURL := fmt.Sprintf("%s/performances/%v/archive", catalog, perf["id"])
	type archiveResult struct {
		code int
		body []byte
		err  error
	}
	results := make(chan archiveResult, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, archiveURL, nil)
			if err != nil {
				results <- archiveResult{err: err}
				return
			}
			resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
			if err != nil {
				results <- archiveResult{err: err}
				return
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			results <- archiveResult{code: resp.StatusCode, body: body, err: err}
		}()
	}
	wg.Wait()
	close(results)
	var derivedIDs []string
	for result := range results {
		if result.err != nil || result.code != http.StatusOK {
			t.Fatalf("concurrent archive: status=%d err=%v body=%s", result.code, result.err, result.body)
		}
		var archived struct {
			ID         string    `json:"id"`
			Status     string    `json:"status"`
			ArchivedAt time.Time `json:"archived_at"`
		}
		if err := json.Unmarshal(result.body, &archived); err != nil {
			t.Fatalf("decode concurrent archive: %v (%s)", err, result.body)
		}
		if archived.Status != "archived" || archived.ArchivedAt.IsZero() {
			t.Fatalf("archive response = %+v", archived)
		}
		key := "platform.catalog.performance.archived:" + archived.ID + ":" + archived.ArchivedAt.UTC().Format(time.RFC3339Nano)
		derivedIDs = append(derivedIDs, uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String())
	}
	if len(derivedIDs) != 2 || derivedIDs[0] != derivedIDs[1] {
		t.Fatalf("raced archive event ids differ: %v", derivedIDs)
	}

	archiveMsg, err := archiveConsumer.Next(jetstream.FetchMaxWait(15 * time.Second))
	if err != nil {
		t.Fatalf("performance.archived not received: %v", err)
	}
	_ = archiveMsg.Ack()
	var archivedEnvelope struct {
		ID, Type string
		Schema   int
		Data     struct {
			PerformanceID string `json:"performance_id"`
			EventID       string `json:"event_id"`
			OrganizerID   string `json:"organizer_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(archiveMsg.Data(), &archivedEnvelope); err != nil {
		t.Fatalf("archive envelope: %v (%s)", err, archiveMsg.Data())
	}
	if archivedEnvelope.ID != derivedIDs[0] || archivedEnvelope.Type != "platform.catalog.performance.archived" ||
		archivedEnvelope.Schema != 2 || archivedEnvelope.Data.PerformanceID != perf["id"] ||
		archivedEnvelope.Data.EventID != event["id"] || archivedEnvelope.Data.OrganizerID != organizerID {
		t.Fatalf("archive envelope mismatch: %+v", archivedEnvelope)
	}
	if _, err := archiveConsumer.Next(jetstream.FetchMaxWait(500 * time.Millisecond)); err == nil {
		t.Fatal("concurrent archive produced more than one stream message")
	}
	if code, _ := postJSON(t, archiveURL, nil); code != http.StatusOK {
		t.Fatalf("idempotent re-archive: want 200, got %d", code)
	}

	// Fresh catalog reads bypass the already-warmed storefront page-data
	// cache. The cache may remain stale for its declared 300-second TTL.
	code, body, _ = getWithHeaders(t, catalog+"/public/events?locale=en&fresh="+suffix)
	if code != http.StatusOK || strings.Contains(string(body), fmt.Sprint(perf["id"])) ||
		!strings.Contains(string(body), fmt.Sprint(secondPerf["id"])) {
		t.Fatalf("mixed event filtering failed (status %d): %.600s", code, body)
	}
	code, body, _ = getWithHeaders(t, fmt.Sprintf("%s/public/events/%v?locale=en&fresh=%s", catalog, event["id"], suffix))
	if code != http.StatusOK || strings.Contains(string(body), fmt.Sprint(perf["id"])) ||
		!strings.Contains(string(body), fmt.Sprint(secondPerf["id"])) {
		t.Fatalf("mixed event detail filtering failed (status %d): %.600s", code, body)
	}

	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/archive", catalog, secondPerf["id"]), nil); code != http.StatusOK {
		t.Fatalf("archive final slot: status %d: %s", code, body)
	}
	code, body, _ = getWithHeaders(t, catalog+"/public/events?locale=en&fresh=all-"+suffix)
	if code != http.StatusOK || strings.Contains(string(body), nameEN) {
		t.Fatalf("all-archived event remains listed (status %d): %.600s", code, body)
	}
	code, body, _ = getWithHeaders(t, fmt.Sprintf("%s/public/events/%v?locale=en&fresh=all-%s", catalog, event["id"], suffix))
	if code != http.StatusNotFound {
		t.Fatalf("all-archived event detail: want 404, got %d: %s", code, body)
	}
}
