//go:build smoke

package smoke_test

// TKT-88: cross-service smoke coverage for multi-event admission flows.
//
// Beyond the per-ticket TDD each implementation ticket owes, these drive the
// admission lifecycle end-to-end across catalog -> inventory -> commerce ->
// access on the running stack and assert the trail + integrity-coverage
// effects that only appear once every service participates. Every scenario is
// grounded in ADR-025 (occurrence identity, offline reconciliation) and
// ADR-021 (verify-lifecycle RequireCoverage, run by scripts/smoke.sh's tail
// over the histories these tests generate).
//
// Non-vacuity (a coverage ticket's central risk): the behavior already exists
// (TKT-84/85/87 merged), so a new test passing immediately is expected. Each
// assertion here is written to fail if the behavior regresses — proven during
// authoring by disposable, uncommitted mutation (plan §"TDD and non-vacuity"),
// not by observing an initial red.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// admissionSuffix is a unique per-test fixture discriminator. The smoke stack
// is shared and these tests are not parallel; a distinct suffix keeps venues,
// events and idempotency keys from colliding across scenarios.
func admissionSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// issuedTicket is one delivered ticket from the access bundle.
type issuedTicket struct {
	QRPayload string `json:"qr_payload"`
}

type ticketHistoryEvent struct {
	Type       string    `json:"type"`
	Sequence   *int64    `json:"sequence"`
	OccurredAt time.Time `json:"occurred_at"`
}

// issueSingleEntryTickets runs a full checkout on a fresh single-entry offer and
// returns the two issued tickets once access has delivered them. It reuses the
// shared checkout drivers so the issuance path is identical to the feature
// tests; only the assertions here differ.
// wantSingleEntryTickets is how many tickets one issueSingleEntryTickets
// checkout yields — inherited from reserveCheckout's fixed quantity of 2
// (checkout_test.go). Named here so a change to that fixture surfaces as a
// clear constant mismatch rather than a bare "issued tickets = N, want 2".
const wantSingleEntryTickets = 2

func issueSingleEntryTickets(t *testing.T, suffix string) (guestRef string, tickets []issuedTicket) {
	t.Helper()
	_, ticketType := setupCheckoutOffer(t, suffix)
	// reserveCheckout reserves quantity 2 and asserts the 2×1250=2500 total, so
	// this order issues wantSingleEntryTickets tickets.
	reservation := reserveCheckout(t, ticketType, "tkt88-reserve-"+suffix)
	code, body := postWithKey(t, gatewayURL+"/api/commerce/orders", "tkt88-order-"+suffix, map[string]any{
		"reservation_id": reservation["reservation_id"], "name": "TKT88 Buyer", "email": "tkt88-" + suffix + "@example.test", "payment_token": "fake-ok",
	})
	if code != http.StatusOK {
		t.Fatalf("checkout %d %s", code, body)
	}
	var completed struct {
		GuestOrderRef string `json:"guest_order_ref"`
	}
	if err := json.Unmarshal(body, &completed); err != nil {
		t.Fatal(err)
	}
	if completed.GuestOrderRef == "" {
		t.Fatalf("checkout omitted guest_order_ref: %s", body)
	}
	retry(t, 15*time.Second, func() error {
		code, body, _ := getWithHeaders(t, gatewayURL+"/api/access/orders/"+completed.GuestOrderRef+"/tickets")
		if code != http.StatusOK {
			return fmt.Errorf("ticket bundle %d %s", code, body)
		}
		var bundle struct {
			Tickets []issuedTicket `json:"tickets"`
		}
		if err := json.Unmarshal(body, &bundle); err != nil {
			return err
		}
		if len(bundle.Tickets) != wantSingleEntryTickets {
			return fmt.Errorf("issued tickets = %d, want %d", len(bundle.Tickets), wantSingleEntryTickets)
		}
		tickets = bundle.Tickets
		return nil
	})
	// Wait for issued+delivered to settle on both tickets before any assertion
	// starts from the trail — delivered is appended asynchronously.
	retry(t, 15*time.Second, func() error {
		code, body, _ := getWithHeaders(t, gatewayURL+"/api/access/orders/"+completed.GuestOrderRef+"/tickets")
		if code != http.StatusOK {
			return fmt.Errorf("ticket bundle %d %s", code, body)
		}
		var bundle struct {
			Tickets []struct {
				History []ticketHistoryEvent `json:"history"`
			} `json:"tickets"`
		}
		if err := json.Unmarshal(body, &bundle); err != nil {
			return err
		}
		for _, tk := range bundle.Tickets {
			if len(tk.History) < 2 || tk.History[0].Type != "issued" || tk.History[1].Type != "delivered" {
				return fmt.Errorf("ticket trail not settled: %#v", tk.History)
			}
		}
		return nil
	})
	return completed.GuestOrderRef, tickets
}

// ticketHistory reads the authoritative-order lifecycle history for the first
// ticket of a guest order (single-ticket fixtures issue one ticket per order).
func ticketHistory(t *testing.T, guestRef string) []ticketHistoryEvent {
	t.Helper()
	code, body, _ := getWithHeaders(t, gatewayURL+"/api/access/orders/"+guestRef+"/tickets")
	if code != http.StatusOK {
		t.Fatalf("ticket bundle %d %s", code, body)
	}
	var bundle struct {
		Tickets []struct {
			History []ticketHistoryEvent `json:"history"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(body, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Tickets) == 0 {
		t.Fatalf("no tickets for %s", guestRef)
	}
	return bundle.Tickets[0].History
}

// assertHistoryTypes asserts the ticket's history is exactly want (by type), in
// authoritative sequence order, with contiguous sequences 1..N. occurred_at is
// claimed physical time and may be non-monotonic; order is by sequence only.
func assertHistoryTypes(t *testing.T, history []ticketHistoryEvent, want []string) {
	t.Helper()
	if len(history) != len(want) {
		t.Fatalf("history length = %d, want %d: %#v", len(history), len(want), history)
	}
	for i, e := range history {
		if e.Type != want[i] {
			t.Fatalf("history[%d].type = %q, want %q (full: %#v)", i, e.Type, want[i], history)
		}
		if e.Sequence == nil || *e.Sequence != int64(i+1) {
			t.Fatalf("history[%d].sequence = %v, want %d", i, e.Sequence, i+1)
		}
	}
}

// assertIntegrityCoverage asserts every lifecycle event of the ticket owning
// this guest order has a matching lifecycle_event_integrity row (the in-Go
// half of ADR-021 RequireCoverage; the closing scripts/smoke.sh verify-lifecycle
// run is the authoritative pass over the whole trail).
func assertIntegrityCoverage(t *testing.T, ctx context.Context, guestRef string) {
	t.Helper()
	conn := accessConn(t, ctx)
	defer func() { _ = conn.Close(ctx) }()
	var missing int
	err := conn.QueryRow(ctx, `
		SELECT count(*) FROM lifecycle_events e
		WHERE e.ticket_id IN (SELECT id FROM tickets WHERE guest_order_ref = $1)
		AND NOT EXISTS (
			SELECT 1 FROM lifecycle_event_integrity i
			WHERE i.event_id = e.id AND i.ticket_id = e.ticket_id
		)`, guestRef).Scan(&missing)
	if err != nil {
		t.Fatalf("integrity coverage query: %v", err)
	}
	if missing != 0 {
		t.Fatalf("lifecycle events without an integrity row = %d, want 0", missing)
	}
}

// alarmEnvelope is the decoded data payload of an alarm-outbox row, covering
// both the admission-conflict (offline duplicate_admit) and the policy-conflict
// (pass raise/withdraw) alarm classes.
type alarmEnvelope struct {
	Schema int `json:"schema"`
	Data   struct {
		TicketID     string `json:"ticket_id"`
		OccurrenceID string `json:"occurrence_id"`
		ConflictID   string `json:"conflict_id"`
		Rule         string `json:"rule"`
		Status       string `json:"status"`
		Version      int    `json:"version"`
	} `json:"data"`
}

// ticketID resolves the single ticket id for a guest order (single-ticket
// fixtures) via the access tickets table.
func ticketID(t *testing.T, ctx context.Context, guestRef string) string {
	t.Helper()
	conn := accessConn(t, ctx)
	defer func() { _ = conn.Close(ctx) }()
	var id string
	if err := conn.QueryRow(ctx, `SELECT id::text FROM tickets WHERE guest_order_ref=$1`, guestRef).Scan(&id); err != nil {
		t.Fatalf("resolve ticket id for %s: %v", guestRef, err)
	}
	return id
}

// ticketIDByEvent resolves the ticket id owning a lifecycle event (the event id
// is the occurrence id). Used when a guest order issued multiple tickets and the
// assertion must target the specific one an occurrence landed on.
func ticketIDByEvent(t *testing.T, ctx context.Context, eventID string) string {
	t.Helper()
	conn := accessConn(t, ctx)
	defer func() { _ = conn.Close(ctx) }()
	var id string
	if err := conn.QueryRow(ctx, `SELECT ticket_id::text FROM lifecycle_events WHERE id=$1`, eventID).Scan(&id); err != nil {
		t.Fatalf("resolve ticket id for event %s: %v", eventID, err)
	}
	return id
}

// alarmEnvelopesFor loads every alarm-outbox envelope for a subject+ticket,
// decoded. Alarms are owed in the same transaction as the trail write, so they
// exist synchronously once the API call that produced them returns; publication
// only flips published_at and never deletes the row (ADR-021 0003), so a query
// by subject+ticket sees raised and withdrawn transitions regardless of
// delivery state.
func alarmEnvelopesFor(t *testing.T, ctx context.Context, subject, ticket string) []alarmEnvelope {
	t.Helper()
	conn := accessConn(t, ctx)
	defer func() { _ = conn.Close(ctx) }()
	// ORDER BY created_at is insertion order for stable output, NOT an
	// authoritative sequence (the outbox has no sequence column). Callers assert
	// per-occurrence counts and raise/withdraw pairs, never positional order
	// across occurrences — do not add such an assertion on top of this ordering.
	rows, err := conn.Query(ctx, `SELECT envelope::text FROM lifecycle_integrity_alarm_outbox WHERE subject=$1 AND envelope->'data'->>'ticket_id'=$2 ORDER BY created_at`, subject, ticket)
	if err != nil {
		t.Fatalf("query alarm outbox: %v", err)
	}
	defer rows.Close()
	var out []alarmEnvelope
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan alarm envelope: %v", err)
		}
		var env alarmEnvelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			t.Fatalf("decode alarm envelope: %v", err)
		}
		out = append(out, env)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate alarm outbox: %v", err)
	}
	return out
}

func scanBody(qr, occurrence string, occurredAt time.Time, direction string) map[string]any {
	body := map[string]any{"qr_payload": qr}
	if occurrence != "" {
		body["occurrence_id"] = occurrence
		body["occurred_at"] = occurredAt.UTC().Format(time.RFC3339Nano)
	}
	if direction != "" {
		body["direction"] = direction
	}
	return body
}

// TestOccurrenceReplayIsDistinguishable is COS-1: an occurrence id is an
// idempotency key. A live scan replayed with the same occurrence returns an
// explicit replay result (never a bare accepted, ADR-025 §D3), and a reconciled
// occurrence replayed through the batch endpoint reports synced with the stored
// time unchanged and no new event appended.
func TestOccurrenceReplayIsDistinguishable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	suffix := admissionSuffix()
	guestRef, tickets := issueSingleEntryTickets(t, suffix)

	occ := uuid.NewString()
	occurredAt := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Microsecond)

	// First live scan: accepted, and NOT a bare acceptance's opposite — the
	// replay field is absent on a first-time result.
	code, body := postWithKey(t, gatewayURL+"/api/access/scans", "tkt88-replay-first-"+suffix, scanBody(tickets[0].QRPayload, occ, occurredAt, ""))
	if code != http.StatusOK {
		t.Fatalf("first scan %d %s", code, body)
	}
	var first struct {
		Decision  string    `json:"decision"`
		Replay    *bool     `json:"replay"`
		ScannedAt time.Time `json:"scanned_at"`
	}
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}
	if first.Decision != "accepted" || first.Replay != nil || first.ScannedAt.IsZero() {
		t.Fatalf("first scan result = %s (replay must be absent first time)", body)
	}

	// Replay the identical occurrence: still 200 accepted, but distinguishably
	// flagged replay:true with the identical stored timestamp.
	code, body = postWithKey(t, gatewayURL+"/api/access/scans", "tkt88-replay-second-"+suffix, scanBody(tickets[0].QRPayload, occ, occurredAt, ""))
	if code != http.StatusOK {
		t.Fatalf("replay scan %d %s", code, body)
	}
	var replay struct {
		Decision  string    `json:"decision"`
		Replay    *bool     `json:"replay"`
		ScannedAt time.Time `json:"scanned_at"`
	}
	if err := json.Unmarshal(body, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Decision != "accepted" || replay.Replay == nil || !*replay.Replay {
		t.Fatalf("replay must be a distinguishable accepted (replay:true), got %s", body)
	}
	// time.Equal is safe here: the fixture truncates to microseconds and the
	// value round-trips through RFC3339Nano JSON and a timestamptz column, both
	// microsecond-precision, so no monotonic-clock or sub-microsecond drift can
	// make two equal stored instants compare unequal.
	if !replay.ScannedAt.Equal(first.ScannedAt) {
		t.Fatalf("replay scanned_at = %s, want %s (idempotent, same stored time)", replay.ScannedAt, first.ScannedAt)
	}

	// The replay appended nothing: exactly one redeemed on the trail.
	assertHistoryTypes(t, ticketHistory(t, guestRef), []string{"issued", "delivered", "redeemed"})
	assertIntegrityCoverage(t, ctx, guestRef)
}

// TestOfflineReconciliationOutOfOrderAndConflicts is COS-3/4: an offline batch
// of distinct occurrences reconciled out of chronological order records the
// first as the redemption and each further single-entry admission as a
// duplicate_admit (ADR-025 §D2/§D6), in REQUEST order regardless of claimed
// time, echoing each occurrence id verbatim. N=2 additional admissions is the
// smallest meaningful plural conflict chain.
func TestOfflineReconciliationOutOfOrderAndConflicts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	suffix := admissionSuffix()
	guestRef, tickets := issueSingleEntryTickets(t, suffix)
	qr := tickets[0].QRPayload

	base := time.Now().UTC().Add(-90 * time.Minute).Truncate(time.Microsecond)
	// Claimed times deliberately out of order relative to request order: the
	// first REQUESTED occurrence (base+20m) becomes the redemption, proving
	// reconciliation records by arrival, not by timestamp.
	type occ struct {
		id      string
		claimed time.Time
		wantRes string
	}
	occs := []occ{
		{uuid.NewString(), base.Add(20 * time.Minute), "recorded"},
		{uuid.NewString(), base, "conflict"},
		{uuid.NewString(), base.Add(10 * time.Minute), "conflict"},
	}
	occurrences := make([]map[string]any, 0, len(occs))
	for _, o := range occs {
		occurrences = append(occurrences, map[string]any{
			"qr_payload": qr, "occurrence_id": o.id, "occurred_at": o.claimed.Format(time.RFC3339Nano),
		})
	}

	code, body := postWithKey(t, gatewayURL+"/api/access/scans/reconciliations", "tkt88-reconcile-"+suffix, map[string]any{"occurrences": occurrences})
	if code != http.StatusOK {
		t.Fatalf("reconcile %d %s", code, body)
	}
	var resp struct {
		Results []struct {
			OccurrenceID string    `json:"occurrence_id"`
			Result       string    `json:"result"`
			OccurredAt   time.Time `json:"occurred_at"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != len(occs) {
		t.Fatalf("results = %d, want %d: %s", len(resp.Results), len(occs), body)
	}
	for i, o := range occs {
		if resp.Results[i].OccurrenceID != o.id {
			t.Fatalf("results[%d].occurrence_id = %q, want %q (verbatim, request order)", i, resp.Results[i].OccurrenceID, o.id)
		}
		if resp.Results[i].Result != o.wantRes {
			t.Fatalf("results[%d].result = %q, want %q", i, resp.Results[i].Result, o.wantRes)
		}
		// The stored time is the device-claimed time, not server time: reconciliation
		// records what the gate reported (ADR-025 §D5), so a handler that stamped
		// server time here would break the record.
		if !resp.Results[i].OccurredAt.Equal(o.claimed) {
			t.Fatalf("results[%d].occurred_at = %s, want claimed %s (device time preserved)", i, resp.Results[i].OccurredAt, o.claimed)
		}
	}

	// Authoritative history: one redemption then two duplicate_admit, contiguous
	// sequences 1..5. occurred_at values remain the claimed (non-monotonic) times.
	assertHistoryTypes(t, ticketHistory(t, guestRef), []string{"issued", "delivered", "redeemed", "duplicate_admit", "duplicate_admit"})

	// DB shape: exactly one redeemed, two duplicate_admit for this ticket, and
	// each stored lifecycle event is keyed by its occurrence id with the claimed
	// device time (proves reconciliation neither relabeled nor reordered).
	conn := accessConn(t, ctx)
	defer func() { _ = conn.Close(ctx) }()
	var redeemed, dupes int
	if err := conn.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE event_type='redeemed'),
			count(*) FILTER (WHERE event_type='duplicate_admit')
		FROM lifecycle_events
		WHERE ticket_id IN (SELECT id FROM tickets WHERE guest_order_ref=$1)`,
		guestRef).Scan(&redeemed, &dupes); err != nil {
		t.Fatalf("count admission events: %v", err)
	}
	if redeemed != 1 || dupes != 2 {
		t.Fatalf("admission events redeemed=%d duplicate_admit=%d, want 1 and 2", redeemed, dupes)
	}
	// First-arriving occurrence is the stored redeemed; the other two are the
	// stored duplicate_admit rows — all under their own occurrence id and claimed time.
	for i, o := range occs {
		var eventType string
		var occurredAt time.Time
		if err := conn.QueryRow(ctx, `SELECT event_type, occurred_at FROM lifecycle_events WHERE id=$1`, o.id).Scan(&eventType, &occurredAt); err != nil {
			t.Fatalf("occurrence %s (%d) has no lifecycle event: %v", o.id, i, err)
		}
		wantType := "duplicate_admit"
		if i == 0 {
			wantType = "redeemed"
		}
		if eventType != wantType {
			t.Fatalf("occurrence %s event_type = %q, want %q", o.id, eventType, wantType)
		}
		if !occurredAt.UTC().Equal(o.claimed) {
			t.Fatalf("occurrence %s occurred_at = %s, want claimed %s", o.id, occurredAt.UTC(), o.claimed)
		}
	}
	assertIntegrityCoverage(t, ctx, guestRef)

	// F1 (ADR-025 §D6): every offline conflict owes a durable admission-conflict
	// alarm keyed by its occurrence — removing oweConflictAlarm must fail here,
	// not slip through as a green run. The redemption (occs[0]) is not a conflict
	// and owes no alarm.
	// Exactly one alarm per conflicting occurrence, zero for the redemption, and
	// no alarms for any other occurrence on this ticket (duplicate or misrouted
	// operator alarms are themselves the defect). Resolve the ticket from the
	// redeemed occurrence, NOT from the guest order: the order issued two tickets
	// and only this one was reconciled, so a guest-ref lookup could pick the
	// other ticket.
	ticket := ticketIDByEvent(t, ctx, occs[0].id)
	retry(t, 15*time.Second, func() error {
		conflictAlarms := alarmEnvelopesFor(t, ctx, "platform.access.admission-conflict.alarm", ticket)
		alarmCountByOcc := map[string]int{}
		for _, a := range conflictAlarms {
			if a.Schema != 1 {
				return fmt.Errorf("admission-conflict alarm schema = %d, want 1", a.Schema)
			}
			alarmCountByOcc[a.Data.OccurrenceID]++
		}
		for _, o := range occs[1:] {
			if alarmCountByOcc[o.id] != 1 {
				return fmt.Errorf("admission-conflict alarms for conflicting occurrence %s = %d, want exactly 1 (all: %v)", o.id, alarmCountByOcc[o.id], alarmCountByOcc)
			}
		}
		if alarmCountByOcc[occs[0].id] != 0 {
			return fmt.Errorf("redemption occurrence %s owes %d conflict alarms, want 0", occs[0].id, alarmCountByOcc[occs[0].id])
		}
		if len(alarmCountByOcc) != len(occs)-1 {
			return fmt.Errorf("admission-conflict alarms reference %d distinct occurrences, want %d (all: %v)", len(alarmCountByOcc), len(occs)-1, alarmCountByOcc)
		}
		return nil
	})

	// COS-1 (reconcile half): replaying a recorded occurrence is synced, appends
	// nothing, and the history does not grow.
	code, body = postWithKey(t, gatewayURL+"/api/access/scans/reconciliations", "tkt88-reconcile-replay-"+suffix, map[string]any{
		"occurrences": []map[string]any{{"qr_payload": qr, "occurrence_id": occs[0].id, "occurred_at": occs[0].claimed.Format(time.RFC3339Nano)}},
	})
	if code != http.StatusOK {
		t.Fatalf("reconcile replay %d %s", code, body)
	}
	var replayResp struct {
		Results []struct {
			OccurrenceID string    `json:"occurrence_id"`
			Result       string    `json:"result"`
			OccurredAt   time.Time `json:"occurred_at"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &replayResp); err != nil {
		t.Fatal(err)
	}
	if len(replayResp.Results) != 1 || replayResp.Results[0].Result != "synced" {
		t.Fatalf("reconcile replay must be synced, got %s", body)
	}
	// The replay echoes the queried occurrence verbatim (scanner queue
	// correlation) and returns the UNCHANGED stored time, not a fresh one.
	if replayResp.Results[0].OccurrenceID != occs[0].id {
		t.Fatalf("reconcile replay occurrence_id = %q, want %q (verbatim echo)", replayResp.Results[0].OccurrenceID, occs[0].id)
	}
	if !replayResp.Results[0].OccurredAt.Equal(occs[0].claimed) {
		t.Fatalf("reconcile replay occurred_at = %s, want unchanged stored %s", replayResp.Results[0].OccurredAt, occs[0].claimed)
	}
	assertHistoryTypes(t, ticketHistory(t, guestRef), []string{"issued", "delivered", "redeemed", "duplicate_admit", "duplicate_admit"})
}

// setupPassOffer publishes a fresh multi/requires_exit operating-day pass offer
// with relative dates and returns (slotID/performanceID, ticketTypeID) once
// access has projected the multi policy. It is a fresh post-policy ticket, never
// a seeded or backfilled row.
func setupPassOffer(t *testing.T, ctx context.Context, suffix string) (slotID, ticketType string) {
	t.Helper()
	catalog := gatewayURL + "/api/catalog"
	venue := created(t, catalog+"/venues", map[string]any{"organizer_id": organizerID, "name": "Pass Arena " + suffix, "ga_capacity": 50})
	event := created(t, catalog+"/events", map[string]any{"organizer_id": organizerID, "name": map[string]string{"fr": "Passe " + suffix, "en": "Pass " + suffix}})
	// Operating day relative to now (TKT-93: never a fixed date).
	operatingDate := time.Now().UTC().AddDate(0, 0, 30).Format("2006-01-02")
	perf := created(t, catalog+"/performances", map[string]any{
		"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"],
		"kind": "operating_day", "operating_date": operatingDate, "opens_at": "10:00", "closes_at": "22:00", "timezone": "UTC",
		"re_entry": map[string]any{"mode": "multi", "requires_exit": true},
	})
	tt := created(t, catalog+"/ticket-types", map[string]any{
		"organizer_id": organizerID, "performance_id": perf["id"],
		"name": map[string]string{"fr": "Passe", "en": "Pass"}, "price": map[string]any{"amount": 1250, "currency": "EUR"},
	})
	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/publish", catalog, perf["id"]), nil); code != http.StatusOK {
		t.Fatalf("publish pass %d %s", code, body)
	}
	slotID = fmt.Sprint(perf["id"])
	// Wait for inventory availability AND the access multi-policy projection.
	retry(t, 30*time.Second, func() error {
		code, body, _ := getWithHeaders(t, fmt.Sprintf("%s/api/inventory/slots/%s/availability?organizer_id=%s", gatewayURL, slotID, organizerID))
		if code != http.StatusOK {
			return fmt.Errorf("inventory availability %d %s", code, body)
		}
		return nil
	})
	conn := accessConn(t, ctx)
	defer func() { _ = conn.Close(ctx) }()
	retry(t, 30*time.Second, func() error {
		var mode string
		var requiresExit bool
		err := conn.QueryRow(ctx, `SELECT mode, requires_exit FROM slot_re_entry_policies WHERE slot_id=$1`, slotID).Scan(&mode, &requiresExit)
		if err != nil {
			return fmt.Errorf("policy not projected yet: %w", err)
		}
		if mode != "multi" || !requiresExit {
			return fmt.Errorf("policy mode=%s requires_exit=%v, want multi/true", mode, requiresExit)
		}
		return nil
	})
	return slotID, fmt.Sprint(tt["id"])
}

// issuePassTicket checks out a single pass ticket and returns its guest ref and
// QR credential once access delivers it.
func issuePassTicket(t *testing.T, suffix, ticketType string) (guestRef, qr string) {
	t.Helper()
	code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", "tkt88-pass-reserve-"+suffix, map[string]any{
		"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 1,
	})
	if code != http.StatusCreated {
		t.Fatalf("pass reserve %d %s", code, body)
	}
	var reservation map[string]any
	if err := json.Unmarshal(body, &reservation); err != nil {
		t.Fatal(err)
	}
	code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", "tkt88-pass-order-"+suffix, map[string]any{
		"reservation_id": reservation["reservation_id"], "name": "TKT88 Pass Buyer", "email": "tkt88-pass-" + suffix + "@example.test", "payment_token": "fake-ok",
	})
	if code != http.StatusOK {
		t.Fatalf("pass checkout %d %s", code, body)
	}
	var completed struct {
		GuestOrderRef string `json:"guest_order_ref"`
	}
	if err := json.Unmarshal(body, &completed); err != nil {
		t.Fatal(err)
	}
	retry(t, 15*time.Second, func() error {
		code, body, _ := getWithHeaders(t, gatewayURL+"/api/access/orders/"+completed.GuestOrderRef+"/tickets")
		if code != http.StatusOK {
			return fmt.Errorf("pass bundle %d %s", code, body)
		}
		var bundle struct {
			Tickets []issuedTicket `json:"tickets"`
		}
		if err := json.Unmarshal(body, &bundle); err != nil {
			return err
		}
		if len(bundle.Tickets) != 1 {
			return fmt.Errorf("pass tickets = %d, want 1", len(bundle.Tickets))
		}
		qr = bundle.Tickets[0].QRPayload
		return nil
	})
	// The delivered lifecycle event is appended asynchronously after the ticket
	// first appears in the bundle; wait for issued+delivered to settle so scan
	// assertions start from a known trail.
	retry(t, 15*time.Second, func() error {
		history := ticketHistory(t, completed.GuestOrderRef)
		if len(history) < 2 || history[0].Type != "issued" || history[1].Type != "delivered" {
			return fmt.Errorf("pass ticket trail not settled: %#v", history)
		}
		return nil
	})
	return completed.GuestOrderRef, qr
}

// TestPassEntryExitAndDerivedConflictWithdrawal is COS-6 (TKT-87 pass flows): a
// multi/requires_exit pass records typed entry/exit (never redeemed or
// duplicate_admit), enforces not_inside / exit_required live denials, and a
// reconciliation-raised exit_required derived conflict is WITHDRAWN when a live
// exit later closes the open entry (ADR-025 §D2 raise/withdraw).
func TestPassEntryExitAndDerivedConflictWithdrawal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	suffix := admissionSuffix()
	_, ticketType := setupPassOffer(t, ctx, suffix)
	guestRef, qr := issuePassTicket(t, suffix, ticketType)

	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)

	// (1) Exit while outside: not_inside, 409, nothing appended.
	code, body := postWithKey(t, gatewayURL+"/api/access/scans", "tkt88-pass-exit-outside-"+suffix,
		scanBody(qr, uuid.NewString(), base, "exit"))
	if code != http.StatusConflict {
		t.Fatalf("exit-while-outside status = %d %s, want 409", code, body)
	}
	assertRejectReason(t, body, "not_inside")
	assertHistoryTypes(t, ticketHistory(t, guestRef), []string{"issued", "delivered"})

	// (2) Entry: accepted, appends entry.
	entryOcc := uuid.NewString()
	code, body = postWithKey(t, gatewayURL+"/api/access/scans", "tkt88-pass-entry-"+suffix,
		scanBody(qr, entryOcc, base.Add(5*time.Minute), "entry"))
	if code != http.StatusOK {
		t.Fatalf("pass entry %d %s", code, body)
	}
	assertHistoryTypes(t, ticketHistory(t, guestRef), []string{"issued", "delivered", "entry"})

	// (2b) Identity-before-denial (ADR-025 §D3 binding order): replaying the
	// SAME entry occurrence while inside must resolve as a replay BEFORE the
	// exit_required policy denial — an implementation that evaluated policy first
	// would misdeny a lost-response retry. Same occurrence, same direction: 200,
	// replay:true, original stored time, no append.
	code, body = postWithKey(t, gatewayURL+"/api/access/scans", "tkt88-pass-entry-replay-"+suffix,
		scanBody(qr, entryOcc, base.Add(5*time.Minute), "entry"))
	if code != http.StatusOK {
		t.Fatalf("entry replay while inside = %d %s, want 200 (identity before denial)", code, body)
	}
	var entryReplay struct {
		Decision  string    `json:"decision"`
		Replay    *bool     `json:"replay"`
		ScannedAt time.Time `json:"scanned_at"`
	}
	if err := json.Unmarshal(body, &entryReplay); err != nil {
		t.Fatal(err)
	}
	if entryReplay.Decision != "accepted" || entryReplay.Replay == nil || !*entryReplay.Replay {
		t.Fatalf("entry replay must be a distinguishable accepted (replay:true), got %s", body)
	}
	if !entryReplay.ScannedAt.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("entry replay scanned_at = %s, want original %s", entryReplay.ScannedAt, base.Add(5*time.Minute))
	}
	// The same occurrence in the OTHER direction is a collision, not a replay:
	// direction is part of the occurrence's identity binding (§D3).
	code, body = postWithKey(t, gatewayURL+"/api/access/scans", "tkt88-pass-entry-as-exit-"+suffix,
		scanBody(qr, entryOcc, base.Add(5*time.Minute), "exit"))
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("entry occurrence resent as exit = %d %s, want 422 occurrence_collision", code, body)
	}
	assertRejectReason(t, body, "occurrence_collision")
	assertHistoryTypes(t, ticketHistory(t, guestRef), []string{"issued", "delivered", "entry"})

	// (3) Second entry while inside: exit_required, 409, nothing appended.
	code, body = postWithKey(t, gatewayURL+"/api/access/scans", "tkt88-pass-entry-again-"+suffix,
		scanBody(qr, uuid.NewString(), base.Add(10*time.Minute), "entry"))
	if code != http.StatusConflict {
		t.Fatalf("second-entry-while-inside status = %d %s, want 409", code, body)
	}
	assertRejectReason(t, body, "exit_required")
	assertHistoryTypes(t, ticketHistory(t, guestRef), []string{"issued", "delivered", "entry"})

	// (4) Reconcile a distinct offline ENTRY: a second open entry raises the
	// exit_required derived conflict for that occurrence.
	reconEntryOcc := uuid.NewString()
	code, body = postWithKey(t, gatewayURL+"/api/access/scans/reconciliations", "tkt88-pass-recon-entry-"+suffix, map[string]any{
		"occurrences": []map[string]any{{"qr_payload": qr, "occurrence_id": reconEntryOcc, "occurred_at": base.Add(20 * time.Minute).Format(time.RFC3339Nano), "event_type": "entry"}},
	})
	if code != http.StatusOK {
		t.Fatalf("reconcile entry %d %s", code, body)
	}
	assertReconcileResult(t, body, reconEntryOcc, "recorded")
	conn := accessConn(t, ctx)
	defer func() { _ = conn.Close(ctx) }()
	retry(t, 15*time.Second, func() error {
		var status string
		var version int
		err := conn.QueryRow(ctx, `SELECT status, version FROM pass_policy_conflicts WHERE ticket_id IN (SELECT id FROM tickets WHERE guest_order_ref=$1) AND rule='exit_required'`, guestRef).Scan(&status, &version)
		if err != nil {
			return fmt.Errorf("conflict not raised yet: %w", err)
		}
		if status != "raised" {
			return fmt.Errorf("conflict status=%s v%d, want raised", status, version)
		}
		return nil
	})

	// (5) Live exit WITHDRAWS the reconciliation-raised exit_required, with an
	// incremented version. The exit's claimed time (+8m) is deliberately BETWEEN
	// the live entry (+5m) and the reconciled entry (+20m): DerivePolicyConflicts
	// walks facts ordered by claimed time (conflicts.go:orderFacts) and raises
	// exit_required for an entry that lands while already inside. With the exit
	// at +8m the ordered walk is entry(+5m,inside)→exit(+8m,outside)→
	// reconEntry(+20m,NOT inside) — so the reconciled entry no longer conflicts
	// and the derived set is empty → withdrawn. This ordering-dependence is the
	// point: an exit placed AFTER the reconciled entry (e.g. +25m) would leave
	// reconEntry landing while-inside and the conflict would correctly STAY
	// raised. The withdrawal is a property of the timeline, not of any exit.
	code, body = postWithKey(t, gatewayURL+"/api/access/scans", "tkt88-pass-exit-"+suffix,
		scanBody(qr, uuid.NewString(), base.Add(8*time.Minute), "exit"))
	if code != http.StatusOK {
		t.Fatalf("pass exit %d %s", code, body)
	}
	retry(t, 15*time.Second, func() error {
		var status string
		var version int
		err := conn.QueryRow(ctx, `SELECT status, version FROM pass_policy_conflicts WHERE ticket_id IN (SELECT id FROM tickets WHERE guest_order_ref=$1) AND rule='exit_required'`, guestRef).Scan(&status, &version)
		if err != nil {
			return err
		}
		if status != "withdrawn" || version < 2 {
			return fmt.Errorf("conflict status=%s v%d, want withdrawn v>=2", status, version)
		}
		return nil
	})

	// F2 (ADR-025 §D2): the raise and the withdrawal each owe a durable
	// policy-conflict alarm — the mutable pass_policy_conflicts projection alone
	// could regress while operators receive neither transition. Assert the
	// outbox carries both transitions for the same conflict identity, with the
	// withdrawal at a higher version than the raise. Retry: the withdrawal alarm
	// is owed in the exit's transaction, observable once committed.
	// The conflict is keyed to the offending fact's occurrence (conflicts.go:104):
	// the reconciliation entry that opened the second entry is reconEntryOcc, so
	// both transitions carry it. Assert exactly one raised (v1) and one withdrawn
	// (v2), both schema 1, both bound to reconEntryOcc and the same conflict id —
	// a schema-0 or misattributed alarm must fail here.
	retry(t, 15*time.Second, func() error {
		alarms := alarmEnvelopesFor(t, ctx, "platform.access.admission-policy-conflict.alarm", ticketID(t, ctx, guestRef))
		var raised, withdrawn []alarmEnvelope
		for _, a := range alarms {
			if a.Data.Rule != "exit_required" {
				continue
			}
			switch a.Data.Status {
			case "raised":
				raised = append(raised, a)
			case "withdrawn":
				withdrawn = append(withdrawn, a)
			}
		}
		if len(raised) != 1 || len(withdrawn) != 1 {
			return fmt.Errorf("policy-conflict transitions: raised=%d withdrawn=%d, want exactly 1 each", len(raised), len(withdrawn))
		}
		r, w := raised[0], withdrawn[0]
		for _, a := range []alarmEnvelope{r, w} {
			if a.Schema != 1 {
				return fmt.Errorf("policy-conflict alarm %s schema = %d, want 1", a.Data.Status, a.Schema)
			}
			if a.Data.OccurrenceID != reconEntryOcc {
				return fmt.Errorf("policy-conflict %s occurrence_id = %q, want reconEntryOcc %q", a.Data.Status, a.Data.OccurrenceID, reconEntryOcc)
			}
		}
		if r.Data.ConflictID == "" || r.Data.ConflictID != w.Data.ConflictID {
			return fmt.Errorf("raise/withdraw conflict_id mismatch: %q vs %q", r.Data.ConflictID, w.Data.ConflictID)
		}
		if r.Data.Version != 1 || w.Data.Version != 2 {
			return fmt.Errorf("versions raised=%d withdrawn=%d, want 1 then 2", r.Data.Version, w.Data.Version)
		}
		return nil
	})

	// Final history: typed entry/exit only, contiguous, no redeemed/duplicate_admit.
	history := ticketHistory(t, guestRef)
	assertHistoryTypes(t, history, []string{"issued", "delivered", "entry", "entry", "exit"})
	for _, e := range history {
		if e.Type == "redeemed" || e.Type == "duplicate_admit" {
			t.Fatalf("pass ticket must never mint %q: %#v", e.Type, history)
		}
	}
	assertIntegrityCoverage(t, ctx, guestRef)
}

func assertRejectReason(t *testing.T, body []byte, want string) {
	t.Helper()
	var r struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	if r.Decision != "rejected" || r.Reason != want {
		t.Fatalf("reject reason = %q (decision %q), want %q: %s", r.Reason, r.Decision, want, body)
	}
}

func assertReconcileResult(t *testing.T, body []byte, occurrence, want string) {
	t.Helper()
	var resp struct {
		Results []struct {
			OccurrenceID string `json:"occurrence_id"`
			Result       string `json:"result"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	for _, r := range resp.Results {
		if r.OccurrenceID == occurrence {
			if r.Result != want {
				t.Fatalf("reconcile result for %s = %q, want %q", occurrence, r.Result, want)
			}
			return
		}
	}
	t.Fatalf("occurrence %s not in reconcile results: %s", occurrence, body)
}
