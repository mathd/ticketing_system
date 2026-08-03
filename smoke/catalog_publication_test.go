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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
			Kind          string `json:"kind"`
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
	// Schema stays 2: kind is an additive, backward-compatible field (US-009 /
	// ADR-009) so inventory's Schema-2 consumer keeps provisioning unchanged.
	if envelope.Type != "platform.catalog.performance.published" ||
		envelope.Schema != 2 || envelope.ID == "" || envelope.Data.Capacity != 500 ||
		envelope.Data.Kind != "performance" ||
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
			// TKT-95: was a raw client — the only reason it bypassed the validating
			// helpers was that they t.Fatal'd, which is illegal here. postWithKeyAsync
			// contract-validates and reports with t.Error instead. No idempotency key:
			// the point of this block is two concurrent unkeyed archives.
			code, body := postWithKeyAsync(t, archiveURL, "", nil)
			results <- archiveResult{code: code, body: body}
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

// TestSeatedPublicationCoexistsWithGA (TKT-103, updated TKT-80) drives seated + GA
// through the real stack: author and publish a seat map (asserting the
// seat_map.published event), create a seated performance referencing that published
// version, publish it and assert the schema-4 fork on the stream, then create a GA
// performance at the same venue and assert it still publishes at schema 2. Inventory
// provisions a GA pool for the GA slot and a SEATED pool (inventory_kind='seated',
// carrying the seat map) for the seated slot — the seat-level claim path is TKT-80.
func TestSeatedPublicationCoexistsWithGA(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	suffixBytes := make([]byte, 4)
	_, _ = rand.Read(suffixBytes)
	suffix := hex.EncodeToString(suffixBytes)

	venue := created(t, catalog+"/venues", map[string]any{
		"organizer_id": organizerID, "name": "Seated Hall " + suffix, "ga_capacity": 400,
	})
	event := created(t, catalog+"/events", map[string]any{
		"organizer_id": organizerID,
		"name":         map[string]string{"fr": "Récital " + suffix, "en": "Recital " + suffix},
	})

	// -- author a draft seat map: map -> section -> row -> seat --
	seatMap := created(t, catalog+"/venues/"+fmt.Sprint(venue["id"])+"/seat-maps", map[string]any{
		"organizer_id": organizerID, "name": "Main floor " + suffix,
	})
	section := created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/sections", map[string]any{
		"organizer_id": organizerID, "name": "Orchestra", "position": 1,
	})
	row := created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/rows", map[string]any{
		"organizer_id": organizerID, "section_id": section["id"], "label": "A", "position": 1,
	})
	created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/seats", map[string]any{
		"organizer_id": organizerID, "row_id": row["id"], "label": "1", "position": 1,
	})

	// -- consumer BEFORE publishing the map: assert the seat_map.published event --
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
	mapCons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "smoke-seatmap-published-" + suffix,
		FilterSubject: "platform.catalog.seat_map.published",
		DeliverPolicy: jetstream.DeliverNewPolicy,
	})
	if err != nil {
		t.Fatalf("seat_map consumer: %v", err)
	}
	pubCons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "smoke-seated-published-" + suffix,
		FilterSubject: "platform.catalog.performance.published",
		DeliverPolicy: jetstream.DeliverNewPolicy,
	})
	if err != nil {
		t.Fatalf("published consumer: %v", err)
	}

	// -- publish the seat map (idempotent); a published version is immutable --
	mapPublishURL := catalog + "/seat-maps/" + fmt.Sprint(seatMap["id"]) + "/publish"
	if code, body := postJSON(t, mapPublishURL, nil); code != http.StatusOK {
		t.Fatalf("publish seat map: %d %s", code, body)
	}
	if code, body := postJSON(t, mapPublishURL, nil); code != http.StatusOK {
		t.Fatalf("re-publish seat map must stay 200, got %d %s", code, body)
	}
	// A published version rejects further authoring (immutability, COS-1).
	if code, _ := postJSON(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/sections", map[string]any{
		"organizer_id": organizerID, "name": "Balcony", "position": 2,
	}); code != http.StatusNotFound {
		t.Fatalf("authoring a published map must be refused with 404, got %d", code)
	}

	mapMsg, err := mapCons.Next(jetstream.FetchMaxWait(15 * time.Second))
	if err != nil {
		t.Fatalf("seat_map.published not received: %v", err)
	}
	_ = mapMsg.Ack()
	var mapEnv struct {
		Type   string `json:"type"`
		Schema int    `json:"schema"`
		Data   struct {
			SeatMapID   string `json:"seat_map_id"`
			OrganizerID string `json:"organizer_id"`
			VenueID     string `json:"venue_id"`
			Version     int32  `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(mapMsg.Data(), &mapEnv); err != nil {
		t.Fatalf("seat_map envelope: %v (%s)", err, mapMsg.Data())
	}
	if mapEnv.Type != "platform.catalog.seat_map.published" || mapEnv.Schema != 1 ||
		mapEnv.Data.SeatMapID != fmt.Sprint(seatMap["id"]) || mapEnv.Data.VenueID != fmt.Sprint(venue["id"]) ||
		mapEnv.Data.OrganizerID != organizerID || mapEnv.Data.Version != 1 {
		t.Fatalf("seat_map envelope mismatch: %+v", mapEnv)
	}

	// -- create a SEATED performance referencing the published map version --
	seated := created(t, catalog+"/performances", map[string]any{
		"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"],
		"starts_at": "2026-10-01T20:00:00Z", "timezone": "Europe/Paris",
		"seat_map_id": seatMap["id"],
	})
	if seated["seat_map_id"] != seatMap["id"] {
		t.Fatalf("seated performance must echo the map reference, got %+v", seated["seat_map_id"])
	}
	// A seated reference to the STILL-draft state is impossible now (it's
	// published), but a reference to a draft map in a fresh venue is a 409.
	draftMap := created(t, catalog+"/venues/"+fmt.Sprint(venue["id"])+"/seat-maps", map[string]any{
		"organizer_id": organizerID, "name": "Draft " + suffix,
	})
	if code, _ := postJSON(t, catalog+"/performances", map[string]any{
		"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"],
		"starts_at": "2026-10-02T20:00:00Z", "timezone": "Europe/Paris",
		"seat_map_id": draftMap["id"],
	}); code != http.StatusConflict {
		t.Fatalf("seated ref to a draft map must be 409, got %d", code)
	}

	created(t, catalog+"/ticket-types", map[string]any{
		"organizer_id": organizerID, "performance_id": seated["id"],
		"name":  map[string]string{"fr": "Place assise", "en": "Reserved seat"},
		"price": map[string]any{"amount": 8000, "currency": "EUR"},
	})
	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/publish", catalog, seated["id"]), nil); code != http.StatusOK {
		t.Fatalf("publish seated: %d %s", code, body)
	}

	// -- the seated publication forks to schema 4 with the map reference (COS-3) --
	seatedEnv := awaitPublishedEnvelope(t, pubCons, seated["id"])
	if seatedEnv.Schema != 4 || seatedEnv.Data.SeatMapID != fmt.Sprint(seatMap["id"]) {
		t.Fatalf("seated publication envelope: schema=%d seat_map_id=%q (want 4/%v)",
			seatedEnv.Schema, seatedEnv.Data.SeatMapID, seatMap["id"])
	}

	// -- a GA performance at the same venue still publishes at schema 2 (coexistence, COS-2) --
	ga := created(t, catalog+"/performances", map[string]any{
		"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"],
		"starts_at": "2026-10-03T20:00:00Z", "timezone": "Europe/Paris",
	})
	created(t, catalog+"/ticket-types", map[string]any{
		"organizer_id": organizerID, "performance_id": ga["id"],
		"name":  map[string]string{"fr": "Admission générale", "en": "General admission"},
		"price": map[string]any{"amount": 4550, "currency": "EUR"},
	})
	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/publish", catalog, ga["id"]), nil); code != http.StatusOK {
		t.Fatalf("publish GA: %d %s", code, body)
	}
	gaEnv := awaitPublishedEnvelope(t, pubCons, ga["id"])
	if gaEnv.Schema != 2 || gaEnv.Data.SeatMapID != "" {
		t.Fatalf("GA publication must stay schema 2 without a seat map ref, got schema=%d seat_map_id=%q",
			gaEnv.Schema, gaEnv.Data.SeatMapID)
	}

	// -- inventory provisions the GA pool but NOT the seated one (COS-4) --
	db, err := pgx.Connect(ctx, fmt.Sprintf("postgres://inventory:inventory@%s/inventory", pgHostPort))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	var gaPools int
	for i := 0; i < 40; i++ {
		if err := db.QueryRow(ctx, `SELECT count(*) FROM inventory_pools WHERE slot_id=$1`, ga["id"]).Scan(&gaPools); err == nil && gaPools == 1 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if gaPools != 1 {
		t.Fatalf("GA slot must provision exactly one pool, got %d", gaPools)
	}
	// Inventory now provisions a SEATED pool for the seated slot (TKT-80): distinct from a
	// GA quantity pool (inventory_kind='seated'), carrying the seat map so seat-level holds
	// can pin. The GA path stays untouched (the GA slot above got a plain pool).
	var seatedKind, seatedMapID string
	for i := 0; i < 40; i++ {
		if err := db.QueryRow(ctx, `SELECT inventory_kind, COALESCE(seat_map_id::text,'') FROM inventory_pools WHERE slot_id=$1`, seated["id"]).Scan(&seatedKind, &seatedMapID); err == nil && seatedKind == "seated" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if seatedKind != "seated" {
		t.Fatalf("seated slot must provision a seated pool (TKT-80), got kind=%q", seatedKind)
	}
	if seatedMapID != fmt.Sprint(seatMap["id"]) {
		t.Fatalf("seated pool must carry its seat map reference, got %q want %v", seatedMapID, seatMap["id"])
	}

	// Inventory stays ready: a known seated variant must not latch it unready.
	code, _, _ := getWithHeaders(t, inventoryURL+"/readyz")
	if code != http.StatusOK {
		t.Fatalf("inventory /readyz = %d after a seated publication; the schema-4 arm must not latch unready", code)
	}

	// -- TKT-172: the buyer-facing read path, end to end through the gateway --
	//
	// Two halves that only mean something together: catalog says WHICH map a slot is
	// seated against, inventory says which of its seats are gone. Neither alone lets a
	// storefront render a picker, and before this ticket neither existed publicly.
	//
	// Driven through the gateway (not the service port) because that is the only path
	// a buyer has, and the 200 here is what registers getSeatOccupancy with the smoke
	// coverage gate — inventory is in smokeCoverageGatedServices, so an undriven
	// operation fails the suite by itself.
	detailCode, detailBody, _ := getWithHeaders(t, catalog+"/public/events/"+fmt.Sprint(event["id"])+"?locale=fr")
	if detailCode != http.StatusOK {
		t.Fatalf("public event detail = %d %s", detailCode, detailBody)
	}
	var detail struct {
		Performances []map[string]json.RawMessage `json:"performances"`
	}
	if err := json.Unmarshal(detailBody, &detail); err != nil {
		t.Fatalf("public detail decode: %v (%s)", err, detailBody)
	}
	var sawSeated, sawGA bool
	for _, perf := range detail.Performances {
		var id string
		_ = json.Unmarshal(perf["id"], &id)
		raw, present := perf["seat_map_id"]
		switch id {
		case fmt.Sprint(seated["id"]):
			var mapID string
			if !present {
				t.Fatalf("the seated performance must publish its seat_map_id — payload: %s", detailBody)
			}
			_ = json.Unmarshal(raw, &mapID)
			if mapID != fmt.Sprint(seatMap["id"]) {
				t.Fatalf("public seat_map_id = %q want %v", mapID, seatMap["id"])
			}
			sawSeated = true
		case fmt.Sprint(ga["id"]):
			// Omitted, not null: presence IS the seated signal a storefront reads.
			if present {
				t.Fatalf("a GA performance must omit seat_map_id, got %s — payload: %s", raw, detailBody)
			}
			sawGA = true
		}
	}
	if !sawSeated || !sawGA {
		t.Fatalf("public detail must list both slots (seated=%v ga=%v): %s", sawSeated, sawGA, detailBody)
	}

	occupancyURL := fmt.Sprintf("%s/api/inventory/slots/%v/seat-occupancy?organizer_id=%s",
		gatewayURL, seated["id"], organizerID)
	occCode, occBody, occHeaders := getWithHeaders(t, occupancyURL)
	if occCode != http.StatusOK {
		t.Fatalf("seat occupancy before any hold = %d %s", occCode, occBody)
	}
	if cc := occHeaders.Get("Cache-Control"); cc != "public, max-age=5, s-maxage=5" {
		t.Fatalf("seat occupancy Cache-Control = %q, want ADR-004's seconds tier", cc)
	}
	var occ struct {
		SlotID         string   `json:"slot_id"`
		SeatMapID      string   `json:"seat_map_id"`
		OfferingStatus string   `json:"offering_status"`
		Remaining      int      `json:"remaining_capacity"`
		Unavailable    []string `json:"unavailable_seat_identities"`
	}
	if err := json.Unmarshal(occBody, &occ); err != nil {
		t.Fatalf("seat occupancy decode: %v (%s)", err, occBody)
	}
	if occ.SlotID != fmt.Sprint(seated["id"]) || occ.SeatMapID != fmt.Sprint(seatMap["id"]) {
		t.Fatalf("seat occupancy ids = %+v, want slot %v / map %v", occ, seated["id"], seatMap["id"])
	}
	if occ.OfferingStatus != "open" {
		t.Fatalf("seat occupancy offering_status = %q want open", occ.OfferingStatus)
	}
	if len(occ.Unavailable) != 0 {
		t.Fatalf("a freshly published seated slot has nothing held, got %v", occ.Unavailable)
	}
	// An empty seat list is not the same claim as "you can buy here": the pool's
	// aggregate headroom is a separate gate the claim path enforces (ai-review).
	if occ.Remaining <= 0 {
		t.Fatalf("a freshly published seated slot must report headroom, got remaining_capacity=%d", occ.Remaining)
	}
	// The two refusals stay distinguishable: a GA slot has no seats (409), an unknown
	// slot does not exist (404). Collapsed, a picker cannot tell them apart.
	if code, body, _ := getWithHeaders(t, fmt.Sprintf("%s/api/inventory/slots/%v/seat-occupancy?organizer_id=%s",
		gatewayURL, ga["id"], organizerID)); code != http.StatusConflict {
		t.Fatalf("seat occupancy on a GA slot = %d %s, want 409", code, body)
	}
	if code, body, _ := getWithHeaders(t, fmt.Sprintf("%s/api/inventory/slots/%s/seat-occupancy?organizer_id=%s",
		gatewayURL, uuid.NewString(), organizerID)); code != http.StatusNotFound {
		t.Fatalf("seat occupancy on an unknown slot = %d %s, want 404", code, body)
	}

	// -- TKT-80 end-to-end: a real seat hold pins the seat in catalog over HTTP --
	// This exercises the wired cross-service path no unit test covers: main.go →
	// CatalogResolver.PinSeats → catalog's internal batch-pin endpoint → store.PinSeats
	// (hold-then-pin, AC3). The seated pool was provisioned above; Orchestra/A/1 is the
	// one authored seat. postSeatHold contract-validates the 201 and records happy-path
	// coverage for createSeatHold (TestCommittedServiceContractsAreComplete).
	hcode, hbody := postSeatHold(t, "seat-e2e-"+suffix, map[string]any{
		"organizer_id": organizerID, "slot_id": seated["id"],
		"seat_identities": []string{"Orchestra/A/1"},
		"ticket_type_id":  "11111111-1111-1111-1111-111111111111",
		"unit_amount":     3000, "currency": "EUR",
	})
	if hcode != http.StatusCreated {
		t.Fatalf("seat hold = %d %s want 201", hcode, hbody)
	}
	var held struct {
		HoldID string   `json:"hold_id"`
		Seats  []string `json:"seats"`
	}
	if err := json.Unmarshal(hbody, &held); err != nil || held.HoldID == "" || len(held.Seats) != 1 {
		t.Fatalf("seat hold response: %v (%s)", err, hbody)
	}
	catDB, err := pgx.Connect(ctx, fmt.Sprintf("postgres://catalog:catalog@%s/catalog", pgHostPort))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = catDB.Close(ctx) }()
	var pins int
	for i := 0; i < 20; i++ {
		if err := catDB.QueryRow(ctx, `SELECT count(*) FROM seat_map_pins WHERE seat_identity=$1 AND pinned_by=$2`,
			"Orchestra/A/1", "hold:"+held.HoldID).Scan(&pins); err == nil && pins == 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if pins != 1 {
		t.Fatalf("seat hold must pin Orchestra/A/1 in catalog as hold:%s, got %d pins", held.HoldID, pins)
	}

	// TKT-172: the held seat is now what the picker must render as taken. This is the
	// assertion that ties the read to the claim path — the same seat, one call apart.
	_, occBody2, _ := getWithHeaders(t, occupancyURL)
	var occ2 struct {
		Unavailable []string `json:"unavailable_seat_identities"`
	}
	if err := json.Unmarshal(occBody2, &occ2); err != nil {
		t.Fatalf("seat occupancy decode: %v (%s)", err, occBody2)
	}
	if len(occ2.Unavailable) != 1 || occ2.Unavailable[0] != "Orchestra/A/1" {
		t.Fatalf("seat occupancy after the hold = %v, want [Orchestra/A/1]", occ2.Unavailable)
	}
	// A second hold on the same seat is DB-rejected (per-seat no-oversell, AC1) end-to-end.
	if dcode, dbody := postSeatHold(t, "seat-e2e-dup-"+suffix, map[string]any{
		"organizer_id": organizerID, "slot_id": seated["id"],
		"seat_identities": []string{"Orchestra/A/1"},
		"ticket_type_id":  "11111111-1111-1111-1111-111111111111",
		"unit_amount":     3000, "currency": "EUR",
	}); dcode != http.StatusConflict {
		t.Fatalf("double seat hold = %d %s want 409", dcode, dbody)
	}
}

// postSeatHold POSTs a seat hold through the gateway with an idempotency key and
// contract-validates the response (recording happy-path coverage, like postJSON).
func postSeatHold(t *testing.T, key string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, gatewayURL+"/api/inventory/holds/seats", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST seat hold: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	validateServiceResponse(t, resp.Request, resp.StatusCode, resp.Header, out)
	return resp.StatusCode, out
}

// awaitPublishedEnvelope pulls performance.published messages until one
// correlates to slotID, tolerating raced events from other tests, and returns
// the seated-aware envelope (schema + seat_map_id).
func awaitPublishedEnvelope(t *testing.T, cons jetstream.Consumer, slotID any) publishedEnvelope {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := cons.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			continue
		}
		_ = msg.Ack()
		var env publishedEnvelope
		if err := json.Unmarshal(msg.Data(), &env); err != nil {
			t.Fatalf("published envelope decode: %v (%s)", err, msg.Data())
		}
		if env.Data.PerformanceID == fmt.Sprint(slotID) {
			return env
		}
	}
	t.Fatalf("performance.published not received for slot %v", slotID)
	return publishedEnvelope{}
}

type publishedEnvelope struct {
	Type   string `json:"type"`
	Schema int    `json:"schema"`
	Data   struct {
		PerformanceID string `json:"performance_id"`
		SeatMapID     string `json:"seat_map_id"`
		// TKT-183's fork half. Read as a POINTER so the test can tell "absent" from
		// "false": `omitempty` means a schema-4 payload omits it entirely, and that
		// distinction is the whole contract — schema and flag are one fact.
		OrphanPrevention *bool `json:"orphan_prevention_enabled"`
	} `json:"data"`
}

func TestSeriesSeasonPublicationAndStorefrontGrouping(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	suffixBytes := make([]byte, 4)
	_, _ = rand.Read(suffixBytes)
	suffix := hex.EncodeToString(suffixBytes)
	venue := created(t, catalog+"/venues", map[string]any{
		"organizer_id": organizerID, "name": "Series Hall", "ga_capacity": 300,
	})
	event := created(t, catalog+"/events", map[string]any{
		"organizer_id": organizerID,
		"name":         map[string]string{"fr": "Événement " + suffix, "en": "Event " + suffix},
	})
	performanceIDs := make([]string, 0, 2)
	for i, startsAt := range []string{"2026-11-01T19:00:00Z", "2026-11-02T19:00:00Z"} {
		perf := created(t, catalog+"/performances", map[string]any{
			"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"],
			"starts_at": startsAt, "timezone": "America/Toronto",
		})
		performanceIDs = append(performanceIDs, fmt.Sprint(perf["id"]))
		created(t, catalog+"/ticket-types", map[string]any{
			"organizer_id": organizerID, "performance_id": perf["id"],
			"name":  map[string]string{"fr": fmt.Sprintf("Soir %d", i+1), "en": fmt.Sprintf("Night %d", i+1)},
			"price": map[string]any{"amount": 3000 + i*500, "currency": "CAD"},
		})
	}
	seriesName := "Autumn run " + suffix
	series := created(t, catalog+"/series", map[string]any{
		"organizer_id": organizerID, "event_id": event["id"],
		"name": map[string]string{"fr": "Série automne " + suffix, "en": seriesName},
	})
	for i, id := range performanceIDs {
		if code, body := postJSON(t, fmt.Sprintf("%s/series/%v/performances", catalog, series["id"]), map[string]any{
			"performance_id": id, "position": i + 1,
		}); code != http.StatusOK {
			t.Fatalf("attach member %d: %d %s", i, code, body)
		}
	}
	season := created(t, catalog+"/seasons", map[string]any{
		"organizer_id": organizerID,
		"name":         map[string]string{"fr": "Saison " + suffix, "en": "Season " + suffix},
	})
	if code, body := postJSON(t, fmt.Sprintf("%s/seasons/%v/series", catalog, season["id"]), map[string]any{"series_id": series["id"]}); code != http.StatusOK {
		t.Fatalf("attach season series: %d %s", code, body)
	}
	if code, body := postJSON(t, fmt.Sprintf("%s/seasons/%v/events", catalog, season["id"]), map[string]any{"event_id": event["id"]}); code != http.StatusOK {
		t.Fatalf("attach duplicate season event path: %d %s", code, body)
	}

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(t.Context(), "PLATFORM")
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := stream.CreateOrUpdateConsumer(t.Context(), jetstream.ConsumerConfig{
		Durable: "smoke-series-publish-" + suffix, FilterSubject: "platform.catalog.performance.published",
		DeliverPolicy: jetstream.DeliverNewPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	publishURL := fmt.Sprintf("%s/series/%v/publish", catalog, series["id"])
	if code, body := postJSON(t, publishURL, nil); code != http.StatusOK {
		t.Fatalf("series publish: %d %s", code, body)
	}
	seen := map[string]bool{}
	for range 2 {
		msg, err := consumer.Next(jetstream.FetchMaxWait(15 * time.Second))
		if err != nil {
			t.Fatalf("series member event: %v", err)
		}
		var envelope struct {
			Data struct {
				PerformanceID string `json:"performance_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(msg.Data(), &envelope); err != nil {
			t.Fatal(err)
		}
		seen[envelope.Data.PerformanceID] = true
		_ = msg.Ack()
	}
	for _, id := range performanceIDs {
		if !seen[id] {
			t.Fatalf("missing publication for %s: %v", id, seen)
		}
	}
	if code, body := postJSON(t, publishURL, nil); code != http.StatusOK {
		t.Fatalf("idempotent series publish: %d %s", code, body)
	}
	if _, err := consumer.Next(jetstream.FetchMaxWait(500 * time.Millisecond)); err == nil {
		t.Fatal("idempotent series publish emitted another event")
	}

	code, body, headers := getWithHeaders(t, fmt.Sprintf("%s/public/seasons/%v?locale=en", catalog, season["id"]))
	if code != http.StatusOK || strings.Count(string(body), fmt.Sprintf(`"id":"%v"`, event["id"])) != 1 {
		t.Fatalf("season event dedupe: %d %s", code, body)
	}
	if headers.Get("Cache-Control") != "public, max-age=300, s-maxage=300" {
		t.Fatalf("season cache tier = %q", headers.Get("Cache-Control"))
	}
	code, page, _ := getWithHeaders(t, fmt.Sprintf("%s/en/events/%v", gatewayURL, event["id"]))
	if code != http.StatusOK || strings.Count(string(page), seriesName) != 1 {
		t.Fatalf("storefront series heading: %d %.800s", code, page)
	}
	if strings.Index(string(page), "Night 1") > strings.Index(string(page), "Night 2") {
		t.Fatalf("storefront ignored series position: %.800s", page)
	}

	archiveConsumer, err := stream.CreateOrUpdateConsumer(t.Context(), jetstream.ConsumerConfig{
		Durable: "smoke-series-archive-" + suffix, FilterSubject: "platform.catalog.performance.archived",
		DeliverPolicy: jetstream.DeliverNewPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	archiveURL := fmt.Sprintf("%s/series/%v/archive", catalog, series["id"])
	if code, body := postJSON(t, archiveURL, nil); code != http.StatusOK {
		t.Fatalf("series archive: %d %s", code, body)
	}
	archived := map[string]bool{}
	for range 2 {
		msg, err := archiveConsumer.Next(jetstream.FetchMaxWait(15 * time.Second))
		if err != nil {
			t.Fatalf("series archive member event: %v", err)
		}
		var envelope struct {
			Data struct {
				PerformanceID string `json:"performance_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(msg.Data(), &envelope); err != nil {
			t.Fatal(err)
		}
		archived[envelope.Data.PerformanceID] = true
		_ = msg.Ack()
	}
	for _, id := range performanceIDs {
		if !archived[id] {
			t.Fatalf("missing archive event for %s: %v", id, archived)
		}
	}
	if code, body := postJSON(t, archiveURL, nil); code != http.StatusOK {
		t.Fatalf("idempotent series archive: %d %s", code, body)
	}
	if _, err := archiveConsumer.Next(jetstream.FetchMaxWait(500 * time.Millisecond)); err == nil {
		t.Fatal("idempotent series archive emitted another event")
	}
	if code, body, _ := getWithHeaders(t, fmt.Sprintf("%s/public/events/%v?locale=en", catalog, event["id"])); code != http.StatusNotFound {
		t.Fatalf("all-archived series event remains public: %d %s", code, body)
	}
	if code, body, _ := getWithHeaders(t, fmt.Sprintf("%s/public/seasons/%v?locale=en", catalog, season["id"])); code != http.StatusNotFound {
		t.Fatalf("season with no published events: %d %s", code, body)
	}
}

func TestFestivalPublicationSharedCapacityAndPublicGrouping(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	suffixBytes := make([]byte, 4)
	_, _ = rand.Read(suffixBytes)
	suffix := hex.EncodeToString(suffixBytes)
	venue := created(t, catalog+"/venues", map[string]any{
		"organizer_id": organizerID, "name": "Festival Grounds " + suffix, "ga_capacity": 250,
	})
	event := created(t, catalog+"/events", map[string]any{
		"organizer_id": organizerID,
		"name":         map[string]string{"fr": "Festival " + suffix, "en": "Festival " + suffix},
	})
	dayIDs := make([]string, 0, 2)
	for i, date := range []string{"2026-08-01", "2026-08-02"} {
		day := created(t, catalog+"/performances", map[string]any{
			"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"],
			"kind": "festival_day", "operating_date": date,
			"opens_at": "12:00", "closes_at": "23:00", "timezone": "America/Toronto",
		})
		dayIDs = append(dayIDs, fmt.Sprint(day["id"]))
		created(t, catalog+"/ticket-types", map[string]any{
			"organizer_id": organizerID, "performance_id": day["id"],
			"name":  map[string]string{"fr": fmt.Sprintf("Jour %d", i+1), "en": fmt.Sprintf("Day %d", i+1)},
			"price": map[string]any{"amount": 7500, "currency": "CAD"},
		})
	}
	festival := created(t, catalog+"/festivals", map[string]any{
		"organizer_id":    organizerID,
		"name":            map[string]string{"fr": "Festival partagé " + suffix, "en": "Shared festival " + suffix},
		"shared_capacity": 1000,
	})
	for _, dayID := range dayIDs {
		if code, body := postJSON(t, fmt.Sprintf("%s/festivals/%v/days", catalog, festival["id"]), map[string]any{"performance_id": dayID}); code != http.StatusOK {
			t.Fatalf("attach festival day: %d %s", code, body)
		}
	}

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(t.Context(), "PLATFORM")
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := stream.CreateOrUpdateConsumer(t.Context(), jetstream.ConsumerConfig{
		Durable: "smoke-festival-publish-" + suffix, FilterSubject: "platform.catalog.performance.published",
		DeliverPolicy: jetstream.DeliverNewPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	archiveConsumer, err := stream.CreateOrUpdateConsumer(t.Context(), jetstream.ConsumerConfig{
		Durable: "smoke-festival-archive-" + suffix, FilterSubject: "platform.catalog.performance.archived",
		DeliverPolicy: jetstream.DeliverNewPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	publishURL := fmt.Sprintf("%s/festivals/%v/publish", catalog, festival["id"])
	if code, body := postJSON(t, publishURL, nil); code != http.StatusOK {
		t.Fatalf("festival publish: %d %s", code, body)
	}
	seen := map[string]bool{}
	for range 2 {
		msg, err := consumer.Next(jetstream.FetchMaxWait(15 * time.Second))
		if err != nil {
			t.Fatalf("festival publication event: %v", err)
		}
		var envelope struct {
			Schema int `json:"schema"`
			Data   struct {
				PerformanceID   string `json:"performance_id"`
				Kind            string `json:"kind"`
				CapacityGroupID string `json:"capacity_group_id"`
				SharedCapacity  int32  `json:"shared_capacity"`
			} `json:"data"`
		}
		if err := json.Unmarshal(msg.Data(), &envelope); err != nil {
			t.Fatal(err)
		}
		_ = msg.Ack()
		if envelope.Schema != 3 || envelope.Data.Kind != "festival_day" || envelope.Data.CapacityGroupID != fmt.Sprint(festival["id"]) || envelope.Data.SharedCapacity != 1000 {
			t.Fatalf("festival envelope = %+v", envelope)
		}
		seen[envelope.Data.PerformanceID] = true
	}
	for _, dayID := range dayIDs {
		if !seen[dayID] {
			t.Fatalf("missing festival day event for %s: %v", dayID, seen)
		}
	}

	// The two real publication events are consumed by inventory and converge on
	// exactly one pool keyed by the festival id.
	db, err := pgx.Connect(t.Context(), fmt.Sprintf("postgres://inventory:inventory@%s/inventory", pgHostPort))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(t.Context()) }()
	var poolCount int
	var poolCapacity int32
	for i := 0; i < 40; i++ {
		err = db.QueryRow(t.Context(), `SELECT count(*),COALESCE(max(capacity),0) FROM inventory_pools WHERE slot_id=$1`, festival["id"]).Scan(&poolCount, &poolCapacity)
		if err == nil && poolCount == 1 && poolCapacity == 1000 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil || poolCount != 1 || poolCapacity != 1000 {
		t.Fatalf("festival inventory pools=%d capacity=%d err=%v, want one pool of 1000", poolCount, poolCapacity, err)
	}
	code, body, headers := getWithHeaders(t, fmt.Sprintf("%s/public/festivals/%v?locale=en", catalog, festival["id"]))
	if code != http.StatusOK || headers.Get("Cache-Control") != "public, max-age=300, s-maxage=300" {
		t.Fatalf("public festival: %d cache=%q %s", code, headers.Get("Cache-Control"), body)
	}
	for _, dayID := range dayIDs {
		if !strings.Contains(string(body), dayID) {
			t.Fatalf("public festival missing day %s: %s", dayID, body)
		}
	}
	// TKT-76 AC3: the group is the pool — one internal adjustment moves the shared
	// capacity, and public availability reflects it within its ADR-004 5s tier.
	adjustURL := fmt.Sprintf("%s/internal/slots/%v/capacity-adjustments", inventoryURL, festival["id"])
	if code, body := internalJSON(t, http.MethodPost, adjustURL, "fest-adjust-"+suffix, map[string]any{
		"organizer_id": organizerID, "capacity": 800, "actor": "staff:amy", "reason": "festival resize",
	}); code != http.StatusCreated {
		t.Fatalf("festival adjust: %d %s", code, body)
	}
	code, body, headers = getWithHeaders(t, fmt.Sprintf("%s/api/inventory/slots/%v/availability?organizer_id=%s", gatewayURL, festival["id"], organizerID))
	if code != http.StatusOK || headers.Get("Cache-Control") != "public, max-age=5, s-maxage=5" {
		t.Fatalf("festival availability after adjust: %d cache=%q %s", code, headers.Get("Cache-Control"), body)
	}
	var avail struct{ Capacity, Available int32 }
	if err := json.Unmarshal(body, &avail); err != nil {
		t.Fatal(err)
	}
	if avail.Capacity != 800 || avail.Available != 800 {
		t.Fatalf("festival group adjustment not reflected: %+v", avail)
	}

	archiveURL := fmt.Sprintf("%s/festivals/%v/archive", catalog, festival["id"])
	if code, body := postJSON(t, archiveURL, nil); code != http.StatusOK {
		t.Fatalf("festival archive: %d %s", code, body)
	}
	archived := map[string]bool{}
	for range 2 {
		msg, err := archiveConsumer.Next(jetstream.FetchMaxWait(15 * time.Second))
		if err != nil {
			t.Fatalf("festival archive event: %v", err)
		}
		var envelope struct {
			Schema int `json:"schema"`
			Data   struct {
				PerformanceID   string `json:"performance_id"`
				CapacityGroupID string `json:"capacity_group_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(msg.Data(), &envelope); err != nil {
			t.Fatal(err)
		}
		_ = msg.Ack()
		if envelope.Schema != 3 || envelope.Data.CapacityGroupID != fmt.Sprint(festival["id"]) {
			t.Fatalf("festival archive envelope = %+v", envelope)
		}
		archived[envelope.Data.PerformanceID] = true
	}
	for _, dayID := range dayIDs {
		if !archived[dayID] {
			t.Fatalf("missing festival archive event for %s: %v", dayID, archived)
		}
	}
	if code, body, _ := getWithHeaders(t, fmt.Sprintf("%s/public/festivals/%v?locale=en", catalog, festival["id"])); code != http.StatusNotFound {
		t.Fatalf("archived festival remains public: %d %s", code, body)
	}
}

// TestTypedDaySlotPublication drives a non-performance slot (operating_day)
// through the real stack (US-009): create + price + publish, assert the Schema-2
// envelope carries its kind + capacity, inventory provisions the same pool
// shape (no fork in the claim path — ADR-005), the public read exposes the
// DST-correct derived opening instant (the nullable starts_at / COALESCE path),
// and a weather closure emits its domain event with the kind + version.
func TestTypedDaySlotPublication(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	suffix := func() string {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		return hex.EncodeToString(b)
	}()
	nameFR, nameEN := "Journée Parc "+suffix, "Park Day "+suffix

	venue := created(t, catalog+"/venues", map[string]any{
		"organizer_id": organizerID, "name": "La Ronde", "ga_capacity": 800,
	})
	event := created(t, catalog+"/events", map[string]any{
		"organizer_id": organizerID,
		"name":         map[string]string{"fr": nameFR, "en": nameEN},
	})
	// operating_day: no starts_at; carries the operating window + multi re-entry.
	slot := created(t, catalog+"/performances", map[string]any{
		"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"],
		"kind": "operating_day", "operating_date": "2026-08-01",
		"opens_at": "10:00", "closes_at": "22:00", "timezone": "America/Toronto",
		"re_entry": map[string]any{"mode": "multi", "requires_exit": true},
	})
	if slot["kind"] != "operating_day" || slot["starts_at"] != nil {
		t.Fatalf("operating_day create echo wrong: %+v", slot)
	}
	created(t, catalog+"/ticket-types", map[string]any{
		"organizer_id": organizerID, "performance_id": slot["id"],
		"name":  map[string]string{"fr": "Passeport jour", "en": "Day pass"},
		"price": map[string]any{"amount": 9000, "currency": "CAD"},
	})

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
	pubCons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "smoke-typed-day-published-" + suffix,
		FilterSubject: "platform.catalog.performance.published",
		DeliverPolicy: jetstream.DeliverNewPolicy,
	})
	if err != nil {
		t.Fatalf("published consumer: %v", err)
	}
	closeCons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "smoke-typed-day-closed-" + suffix,
		FilterSubject: "platform.catalog.performance.closed",
		DeliverPolicy: jetstream.DeliverNewPolicy,
	})
	if err != nil {
		t.Fatalf("closed consumer: %v", err)
	}

	// publish
	publishURL := fmt.Sprintf("%s/performances/%v/publish", catalog, slot["id"])
	if code, body := postJSON(t, publishURL, nil); code != http.StatusOK {
		t.Fatalf("publish: %d %s", code, body)
	}

	// -- the publication envelope carries kind + capacity at Schema 2 (AC3) --
	pubEnv := awaitSlotEnvelope(t, pubCons, "platform.catalog.performance.published", slot["id"])
	if pubEnv.Schema != 2 || pubEnv.Data.Kind != "operating_day" || pubEnv.Data.Capacity != 800 {
		t.Fatalf("publication envelope: schema=%d kind=%q capacity=%d (want 2/operating_day/800)",
			pubEnv.Schema, pubEnv.Data.Kind, pubEnv.Data.Capacity)
	}

	// -- inventory provisions the pool identically regardless of kind (no fork) --
	availURL := fmt.Sprintf("%s/api/inventory/slots/%v/availability?organizer_id=%s", gatewayURL, slot["id"], organizerID)
	var provisioned bool
	for i := 0; i < 40; i++ {
		if code, _, _ := getWithHeaders(t, availURL); code == http.StatusOK {
			provisioned = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !provisioned {
		t.Fatal("inventory never provisioned a pool for the operating_day slot")
	}

	// -- public read exposes the DST-correct derived opening instant --
	// 10:00 America/Toronto on 2026-08-01 (EDT, UTC-4) == 14:00:00Z.
	code, body, _ := getWithHeaders(t, fmt.Sprintf("%s/public/events/%v?locale=fr", catalog, event["id"]))
	if code != http.StatusOK {
		t.Fatalf("public detail: %d %s", code, body)
	}
	var detail struct {
		Performances []struct {
			Id       string `json:"id"`
			StartsAt string `json:"starts_at"`
		} `json:"performances"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	var found bool
	for _, p := range detail.Performances {
		if p.Id == slot["id"] {
			found = true
			if p.StartsAt != "2026-08-01T14:00:00Z" {
				t.Fatalf("derived opening instant = %q, want 2026-08-01T14:00:00Z (10:00 EDT)", p.StartsAt)
			}
		}
	}
	if !found {
		t.Fatalf("operating_day slot absent from public detail: %s", body)
	}

	// -- weather closure emits its domain event with kind + version (AC4) --
	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/close", catalog, slot["id"]),
		map[string]any{"reason": "storm"}); code != http.StatusOK {
		t.Fatalf("close: %d %s", code, body)
	}
	closeEnv := awaitSlotEnvelope(t, closeCons, "platform.catalog.performance.closed", slot["id"])
	if closeEnv.Data.Kind != "operating_day" || closeEnv.Data.Version != 1 {
		t.Fatalf("closed envelope: kind=%q version=%d (want operating_day/1)", closeEnv.Data.Kind, closeEnv.Data.Version)
	}
}

type slotEnvelope struct {
	Type   string `json:"type"`
	Schema int    `json:"schema"`
	Data   struct {
		PerformanceID string `json:"performance_id"`
		Kind          string `json:"kind"`
		Capacity      int32  `json:"capacity"`
		Version       int32  `json:"closure_version"`
	} `json:"data"`
}

// awaitSlotEnvelope pulls messages until one correlates to slotID, so a raced
// event from another test does not mislead the assertion.
func awaitSlotEnvelope(t *testing.T, cons jetstream.Consumer, subject string, slotID any) slotEnvelope {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := cons.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			continue
		}
		_ = msg.Ack()
		var env slotEnvelope
		if err := json.Unmarshal(msg.Data(), &env); err != nil {
			t.Fatalf("envelope decode: %v (%s)", err, msg.Data())
		}
		if env.Data.PerformanceID == fmt.Sprint(slotID) {
			return env
		}
	}
	t.Fatalf("%s not received for slot %v", subject, slotID)
	return slotEnvelope{}
}

// TestSeatMapVersioningReadsAndEdit drives the five catalog operations TKT-95 absorbed
// from TKT-111 — the TKT-105 surface no smoke test exercised: editSeatMap,
// getPublicSeatMapGeometry, listSeatMapVersions, listVenueSeatMaps and
// updateVenueGaCapacity. All five go through validating helpers, so the running stack's
// responses are checked against the committed catalog contract (the value here is
// real gateway + store + migration execution; catalog stays deliberately outside the
// smoke coverage gate per ADR-030, and this test does not touch that scope).
//
// It owns its venue and seat-map family on purpose, rather than borrowing
// TestSeatedPublicationCoexistsWithGA's: an edit advances the family's current version
// and updateVenueGaCapacity rewrites the venue, and that test's later assertions read
// both. A shared fixture would have made this test's setup silently change that one's
// meaning.
//
// The edit is deliberately NON-orphaning — it keeps Orchestra/A/1 and adds
// Orchestra/A/2 — and runs on a family no sale or hold has pinned. ADR-029 hard-rejects
// an orphaning edit (409); that rejection path and the edit-vs-sale advisory lock are
// TKT-104's own tests, not this one's. What is asserted here is ADR-029's
// honest-writer consistency: the predecessor version stays readable and unchanged. That
// is NOT a tamper-evidence claim — a writer with catalog DB access can still rewrite
// either version.
//
// Must not call t.Parallel(): it publishes a seat map, and
// TestSeatedPublicationCoexistsWithGA's DeliverNew consumer on
// platform.catalog.seat_map.published would then be able to receive this test's event
// instead of its own.
func TestSeatMapVersioningReadsAndEdit(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	suffixBytes := make([]byte, 4)
	_, _ = rand.Read(suffixBytes)
	suffix := hex.EncodeToString(suffixBytes)
	const hoursTier = "public, max-age=3600, s-maxage=3600" // ADR-004

	venue := created(t, catalog+"/venues", map[string]any{
		"organizer_id": organizerID, "name": "Versioning Hall " + suffix, "ga_capacity": 100,
	})
	venueID := fmt.Sprint(venue["id"])

	// -- author and publish version 1: Orchestra / A / 1 --
	seatMap := created(t, catalog+"/venues/"+venueID+"/seat-maps", map[string]any{
		"organizer_id": organizerID, "name": "Versioned floor " + suffix,
	})
	v1ID := fmt.Sprint(seatMap["id"])
	section := created(t, catalog+"/seat-maps/"+v1ID+"/sections", map[string]any{
		"organizer_id": organizerID, "name": "Orchestra", "position": 1,
	})
	row := created(t, catalog+"/seat-maps/"+v1ID+"/rows", map[string]any{
		"organizer_id": organizerID, "section_id": section["id"], "label": "A", "position": 1,
	})
	created(t, catalog+"/seat-maps/"+v1ID+"/seats", map[string]any{
		"organizer_id": organizerID, "row_id": row["id"], "label": "1", "position": 1,
	})
	// -- TKT-107: while v1 is still a draft, the geometry read is no-store. This is
	// the one real-stack proof that the status-driven tier survives the gateway and
	// real Postgres; the draft/published/mixed/empty branches themselves are covered
	// at the handler seam (TestSeatMapReadCacheTierByStatus), which runs through the
	// same ADR-028 response validator.
	if _, body, hdr := getWithHeaders(t, catalog+"/public/seat-maps/"+v1ID); hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("draft geometry cache tier %q, want no-store; body=%s", hdr.Get("Cache-Control"), body)
	}

	if code, body := postJSON(t, catalog+"/seat-maps/"+v1ID+"/publish", nil); code != http.StatusOK {
		t.Fatalf("publish seat map: %d %s", code, body)
	}

	// -- editSeatMap: full replacement geometry keeping Orchestra/A/1, adding A/2 --
	editCode, editBody := postJSON(t, catalog+"/seat-maps/"+v1ID+"/edit", map[string]any{
		"organizer_id": organizerID,
		"sections": []map[string]any{{
			"name": "Orchestra", "position": 1,
			"rows": []map[string]any{{
				"label": "A", "position": 1,
				"seats": []map[string]any{
					{"label": "1", "position": 1},
					{"label": "2", "position": 2},
				},
			}},
		}},
	})
	if editCode != http.StatusCreated {
		t.Fatalf("edit seat map: %d %s", editCode, editBody)
	}
	var edited struct {
		ID          string `json:"id"`
		VenueID     string `json:"venue_id"`
		Version     int32  `json:"version"`
		Status      string `json:"status"`
		PublishedAt string `json:"published_at"`
	}
	if err := json.Unmarshal(editBody, &edited); err != nil {
		t.Fatalf("decode edit: %v (%s)", err, editBody)
	}
	if edited.Version != 2 || edited.Status != "published" || edited.PublishedAt == "" {
		t.Fatalf("edit must publish version 2, got %+v", edited)
	}
	if edited.ID == v1ID {
		t.Fatalf("edit must create a NEW version row, got the predecessor id %s", v1ID)
	}
	if edited.VenueID != venueID {
		t.Fatalf("edited version venue = %q, want %q", edited.VenueID, venueID)
	}
	v2ID := edited.ID

	// -- getPublicSeatMapGeometry: the predecessor is unchanged, the successor has both seats --
	for _, want := range []struct {
		id       string
		version  int32
		seatIDs  []string
		describe string
	}{
		{v1ID, 1, []string{"Orchestra/A/1"}, "predecessor stays immutable"},
		{v2ID, 2, []string{"Orchestra/A/1", "Orchestra/A/2"}, "successor carries the edit"},
	} {
		code, body, hdr := getWithHeaders(t, catalog+"/public/seat-maps/"+want.id)
		if code != http.StatusOK {
			t.Fatalf("geometry %s (%s): %d %s", want.id, want.describe, code, body)
		}
		if got := hdr.Get("Cache-Control"); got != hoursTier {
			t.Fatalf("geometry cache tier %q, want %q", got, hoursTier)
		}
		var geometry struct {
			Map struct {
				ID      string `json:"id"`
				Version int32  `json:"version"`
			} `json:"map"`
			Sections []struct {
				Rows []struct {
					Seats []struct {
						SeatIdentity string `json:"seat_identity"`
					} `json:"seats"`
				} `json:"rows"`
			} `json:"sections"`
		}
		if err := json.Unmarshal(body, &geometry); err != nil {
			t.Fatalf("decode geometry: %v (%s)", err, body)
		}
		if geometry.Map.Version != want.version {
			t.Fatalf("geometry %s version = %d, want %d", want.id, geometry.Map.Version, want.version)
		}
		var identities []string
		for _, s := range geometry.Sections {
			for _, r := range s.Rows {
				for _, seat := range r.Seats {
					identities = append(identities, seat.SeatIdentity)
				}
			}
		}
		if !slices.Equal(identities, want.seatIDs) {
			t.Fatalf("geometry %s (%s) seats = %v, want %v", want.id, want.describe, identities, want.seatIDs)
		}
	}

	// -- listSeatMapVersions: any version resolves the family, newest first --
	code, body, hdr := getWithHeaders(t, catalog+"/public/seat-maps/"+v1ID+"/versions")
	if code != http.StatusOK {
		t.Fatalf("versions: %d %s", code, body)
	}
	if got := hdr.Get("Cache-Control"); got != hoursTier {
		t.Fatalf("versions cache tier %q, want %q", got, hoursTier)
	}
	var history struct {
		CurrentVersion int32 `json:"current_version"`
		Versions       []struct {
			ID          string `json:"id"`
			Version     int32  `json:"version"`
			Status      string `json:"status"`
			PublishedAt string `json:"published_at"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &history); err != nil {
		t.Fatalf("decode versions: %v (%s)", err, body)
	}
	if history.CurrentVersion != 2 {
		t.Fatalf("current_version = %d, want 2 (the version an edit targets)", history.CurrentVersion)
	}
	if len(history.Versions) != 2 ||
		history.Versions[0].Version != 2 || history.Versions[0].ID != v2ID ||
		history.Versions[1].Version != 1 || history.Versions[1].ID != v1ID {
		t.Fatalf("versions must be newest-first [2,1] with distinct ids, got %+v", history.Versions)
	}
	for _, v := range history.Versions {
		if v.PublishedAt == "" {
			t.Fatalf("version %d has no published_at: %+v", v.Version, v)
		}
	}

	// -- listVenueSeatMaps: summaries for this venue's family, both versions --
	code, body, hdr = getWithHeaders(t, catalog+"/public/venues/"+venueID+"/seat-maps")
	if code != http.StatusOK {
		t.Fatalf("venue seat maps: %d %s", code, body)
	}
	if got := hdr.Get("Cache-Control"); got != hoursTier {
		t.Fatalf("venue seat maps cache tier %q, want %q", got, hoursTier)
	}
	var list struct {
		SeatMaps []struct {
			ID      string `json:"id"`
			Version int32  `json:"version"`
		} `json:"seat_maps"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode venue seat maps: %v (%s)", err, body)
	}
	found := map[string]int32{}
	for _, m := range list.SeatMaps {
		found[m.ID] = m.Version
	}
	if found[v1ID] != 1 || found[v2ID] != 2 {
		t.Fatalf("venue seat maps must list both versions (v1=1, v2=2), got %+v", list.SeatMaps)
	}

	// -- updateVenueGaCapacity: GA capacity is writable after venue creation --
	code, body = postJSON(t, catalog+"/venues/"+venueID+"/ga-capacity", map[string]any{
		"organizer_id": organizerID, "ga_capacity": 450,
	})
	if code != http.StatusOK {
		t.Fatalf("update ga capacity: %d %s", code, body)
	}
	var updated struct {
		ID         string `json:"id"`
		GaCapacity int32  `json:"ga_capacity"`
	}
	if err := json.Unmarshal(body, &updated); err != nil {
		t.Fatalf("decode ga capacity: %v (%s)", err, body)
	}
	if updated.ID != venueID || updated.GaCapacity != 450 {
		t.Fatalf("ga capacity update = %+v, want id %s capacity 450", updated, venueID)
	}
}
