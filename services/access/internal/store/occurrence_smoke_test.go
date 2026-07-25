//go:build smoke

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// occurrence tests — ADR-025 §D3/D5/D6 on the scan and reconciliation paths.
// The occurrence id is an idempotency key, never admission authorization: every
// replay below asserts Replayed explicitly, and every denial asserts nothing
// was appended.

// Pinned once, relative to now: a fixed calendar date sails past
// AdmissionSkewBound 24h after it is written and turns the suite into a time
// bomb (it did — TKT-93 gate run). Truncated to microseconds so equality
// survives the timestamptz round-trip; called repeatedly, so it must be a
// constant within a run.
var deviceTimeBase = time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

func deviceTime() time.Time {
	return deviceTimeBase
}

func repairChain(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID, genuine []byte) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `ALTER TABLE lifecycle_event_integrity DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE lifecycle_event_integrity SET entry_hash=$1 WHERE ticket_id=$2 AND sequence=1`, genuine, ticketID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE lifecycle_event_integrity ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
}

func genuineHash(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) []byte {
	t.Helper()
	var genuine []byte
	if err := db.QueryRowContext(ctx, `SELECT entry_hash FROM lifecycle_event_integrity WHERE ticket_id=$1 AND sequence=1`, ticketID).Scan(&genuine); err != nil {
		t.Fatal(err)
	}
	return genuine
}

func occurrenceRedeemInput(s seeded, occ uuid.UUID) RedeemInput {
	in := s.redeemInput()
	in.OccurrenceID = occ
	in.OccurredAt = deviceTime()
	return in
}

func (s seeded) reconcileInput(occ uuid.UUID, at time.Time) ReconcileOccurrence {
	return ReconcileOccurrence{
		TicketID: s.ticketID, OrderID: s.id.OrderID, OrganizerID: s.id.OrganizerID, SlotID: s.id.SlotID,
		OccurrenceID: occ, OccurredAt: at,
	}
}

func TestRedeemWithOccurrenceUsesScannerIDAndDeviceTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	occ := uuid.New()

	result, err := st.Redeem(ctx, occurrenceRedeemInput(s, occ))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.Decision != DecisionAccepted || result.Replayed {
		t.Fatalf("first occurrence scan = %+v, want a first-time acceptance", result)
	}
	if !result.OccurredAt.Equal(deviceTime()) {
		t.Fatalf("stored time %v, want the device-claimed %v (ADR-025 §D5)", result.OccurredAt, deviceTime())
	}
	var eventID uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT id FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, s.ticketID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if eventID != occ {
		t.Fatalf("redeemed event id = %s, want the scanner's occurrence id %s (ADR-025 §D3: one identity model, no exceptions)", eventID, occ)
	}
}

// The §D3 named trap: a retry of the occurrence that became `redeemed` is
// idempotent success — distinguishable as a replay, never bare accepted, and it
// can never be forged into duplicate_admit evidence of a second admission.
func TestRedeemOccurrenceRetryIsReplayNeverDuplicateAdmit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	occ := uuid.New()

	first, err := st.Redeem(ctx, occurrenceRedeemInput(s, occ))
	if err != nil {
		t.Fatal(err)
	}
	retry, err := st.Redeem(ctx, occurrenceRedeemInput(s, occ))
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Accepted || retry.Decision != DecisionAccepted || !retry.Replayed {
		t.Fatalf("retry = %+v, want a replay-marked acceptance (never bare accepted, ADR-025 §D3)", retry)
	}
	if !retry.OccurredAt.Equal(first.OccurredAt) {
		t.Fatalf("replay reports %v, want the original %v", retry.OccurredAt, first.OccurredAt)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, s.ticketID); n != 1 {
		t.Fatalf("%d redeemed rows after a retry, want 1", n)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='duplicate_admit'`, s.ticketID); n != 0 {
		t.Fatal("a transport retry was forged into duplicate_admit evidence")
	}

	// A DISTINCT occurrence on the live path is a denial, not a conflict event:
	// a connected duplicate that was denied is not an admission (ADR-025 §D2).
	distinct, err := st.Redeem(ctx, occurrenceRedeemInput(s, uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if distinct.Accepted || distinct.Decision != DecisionAlreadyRedeemed || distinct.Replayed {
		t.Fatalf("distinct occurrence = %+v, want already_redeemed denial", distinct)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='duplicate_admit'`, s.ticketID); n != 0 {
		t.Fatal("a live denied duplicate minted a duplicate_admit event")
	}
}

func TestRedeemWithoutOccurrenceKeepsGrandfatheredSemantics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())

	if _, err := st.Redeem(ctx, s.redeemInput()); err != nil {
		t.Fatal(err)
	}
	var eventID uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT id FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, s.ticketID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if want := uuid.NewSHA1(uuid.NameSpaceOID, []byte(s.ticketID.String()+":redeemed")); eventID != want {
		t.Fatalf("old-scanner redeemed id = %s, want the grandfathered deterministic id %s", eventID, want)
	}
	repeat, err := st.Redeem(ctx, s.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Accepted || repeat.Decision != DecisionAlreadyRedeemed || repeat.Replayed {
		t.Fatalf("old-scanner repeat = %+v, want today's already_redeemed exactly", repeat)
	}
}

func TestRedeemOccurrenceValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())

	notV4 := occurrenceRedeemInput(s, uuid.NewSHA1(uuid.NameSpaceOID, []byte("deterministic")))
	if _, err := st.Redeem(ctx, notV4); err == nil {
		t.Fatal("a non-UUIDv4 occurrence id was accepted (ADR-025 §D3)")
	}
	noTime := occurrenceRedeemInput(s, uuid.New())
	noTime.OccurredAt = time.Time{}
	if _, err := st.Redeem(ctx, noTime); err == nil {
		t.Fatal("an occurrence without its claimed admission time was accepted")
	}
}

func TestRedeemOccurrenceCollisionAcrossTickets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	a := issueTicket(t, ctx, st, uuid.New())
	b := issueTicket(t, ctx, st, uuid.New())
	occ := uuid.New()

	if _, err := st.Redeem(ctx, occurrenceRedeemInput(a, occ)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Redeem(ctx, occurrenceRedeemInput(b, occ)); !errors.Is(err, ErrOccurrenceCollision) {
		t.Fatalf("reused occurrence id across tickets: err=%v, want ErrOccurrenceCollision (never another admission's result)", err)
	}
}

// Broken-chain retry (§D3): the degraded admission's occurrence id is persisted
// on the quarantine row, and its retry returns the original result — no second
// admission, no second alarm.
func TestDegradedOccurrenceBrokenChainRetryAndDistinct(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	corruptChain(t, ctx, db, s.ticketID)
	occ := uuid.New()

	first, err := st.Redeem(ctx, occurrenceRedeemInput(s, occ))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Accepted || first.Decision != DecisionAdmittedDegraded || first.Replayed {
		t.Fatalf("first broken-chain scan = %+v, want a first-time degraded admission", first)
	}
	var storedOcc uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT occurrence_id FROM lifecycle_integrity_quarantine WHERE ticket_id=$1 AND admitted_at IS NOT NULL`, s.ticketID).Scan(&storedOcc); err != nil {
		t.Fatal(err)
	}
	if storedOcc != occ {
		t.Fatalf("quarantine row occurrence = %s, want %s (ADR-025 §D3: the identity rule extends to degraded admissions)", storedOcc, occ)
	}
	alarms := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox`)

	retry, err := st.Redeem(ctx, occurrenceRedeemInput(s, occ))
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Accepted || retry.Decision != DecisionAdmittedDegraded || !retry.Replayed || !retry.OccurredAt.Equal(first.OccurredAt) {
		t.Fatalf("broken-chain retry = %+v, want the original degraded admission replayed at %v", retry, first.OccurredAt)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox`); n != alarms {
		t.Fatalf("a replay owed a second alarm (%d -> %d)", alarms, n)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE ticket_id=$1`, s.ticketID); n != 1 {
		t.Fatalf("%d quarantine rows after a retry, want 1", n)
	}

	// A distinct occurrence is a second scan: denied and escalated, as today.
	distinct, err := st.Redeem(ctx, occurrenceRedeemInput(s, uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if distinct.Accepted || distinct.Decision != DecisionIntegrityQuarantined || distinct.Replayed {
		t.Fatalf("distinct occurrence on quarantined ticket = %+v, want denial", distinct)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox`); n != alarms+1 {
		t.Fatalf("distinct-occurrence denial owed no alarm (%d, want %d)", n, alarms+1)
	}
}

// Recovered-chain retry (§D3): after the chain verifies clean again, the
// occurrence that took the one degraded admission still replays its original
// result on the verified path; a distinct occurrence is denied (§D6 admit-once).
func TestRecoveredChainQuarantinedOccurrenceReplays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)
	s := issueTicket(t, ctx, st, uuid.New())
	genuine := genuineHash(t, ctx, db, s.ticketID)
	corruptChain(t, ctx, db, s.ticketID)
	occ := uuid.New()

	first, err := st.Redeem(ctx, occurrenceRedeemInput(s, occ))
	if err != nil {
		t.Fatal(err)
	}
	repairChain(t, ctx, db, s.ticketID, genuine)
	if err := New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("chain did not recover: %v", err)
	}

	alarms := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox`)
	retry, err := st.Redeem(ctx, occurrenceRedeemInput(s, occ))
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Accepted || retry.Decision != DecisionAdmittedDegraded || !retry.Replayed || !retry.OccurredAt.Equal(first.OccurredAt) {
		t.Fatalf("recovered-chain retry = %+v, want the original degraded admission replayed (not a quarantine denial)", retry)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox`); n != alarms {
		t.Fatal("a recovered-chain replay owed an alarm")
	}
	distinct, err := st.Redeem(ctx, occurrenceRedeemInput(s, uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if distinct.Accepted || distinct.Decision != DecisionIntegrityQuarantined {
		t.Fatalf("distinct occurrence after recovery = %+v, want §D6 admit-once denial", distinct)
	}
}

// Plan-final A1: an occurrence recorded in the trail while the chain was
// healthy, retried after the chain later breaks, replays its original result —
// it must not take a fresh degraded admission and double-record one physical
// occurrence across the trace/quarantine union.
func TestDegradedScanReplaysVerifiedRedeemedOccurrence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	occ := uuid.New()

	first, err := st.Redeem(ctx, occurrenceRedeemInput(s, occ))
	if err != nil {
		t.Fatal(err)
	}
	corruptChain(t, ctx, db, s.ticketID)

	retry, err := st.Redeem(ctx, occurrenceRedeemInput(s, occ))
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Accepted || retry.Decision != DecisionAccepted || !retry.Replayed || !retry.OccurredAt.Equal(first.OccurredAt) {
		t.Fatalf("post-breakage retry = %+v, want the original redemption replayed", retry)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE ticket_id=$1`, s.ticketID); n != 0 {
		t.Fatal("a replayed redemption took a degraded admission — one occurrence recorded twice across the union")
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox`); n != 0 {
		t.Fatal("a replayed redemption owed an alarm")
	}
}

func TestReconcileRecordsOfflineRedemption(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	occ := uuid.New()

	result, err := st.ReconcileAdmission(ctx, s.reconcileInput(occ, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ReconcileRecorded || result.SkewFlagged {
		t.Fatalf("reconcile of an unredeemed ticket = %+v, want recorded", result)
	}
	var eventID uuid.UUID
	var occurredAt time.Time
	if err := db.QueryRowContext(ctx, `SELECT id,occurred_at FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, s.ticketID).Scan(&eventID, &occurredAt); err != nil {
		t.Fatal(err)
	}
	if eventID != occ || !occurredAt.Equal(deviceTime()) {
		t.Fatalf("reconciled redemption = %s at %v, want %s at the device time %v", eventID, occurredAt, occ, deviceTime())
	}

	// The occurrence that became redeemed via reconciliation replays on the
	// live path too — one identity model, whichever door the record came in by.
	live, err := st.Redeem(ctx, occurrenceRedeemInput(s, occ))
	if err != nil {
		t.Fatal(err)
	}
	if !live.Accepted || !live.Replayed {
		t.Fatalf("live retry of a reconciled occurrence = %+v, want replay", live)
	}
	// And a reconcile retry is synced, not a second record.
	again, err := st.ReconcileAdmission(ctx, s.reconcileInput(occ, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	if again.Outcome != ReconcileSynced {
		t.Fatalf("reconcile retry = %+v, want synced", again)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1`, s.ticketID); n != 2 {
		t.Fatalf("%d lifecycle rows, want 2 (issued, redeemed)", n)
	}
}

// assertConflictAlarmPIIFloor pins the PII floor (ADR-025 §D9: bounded
// identifiers and enums only) on the bytes actually owed to the operator — the
// persisted outbox envelope, not a struct the producer happens to use today.
// The envelope's top level is an anonymous inline struct inside
// oweConflictAlarm, so the persisted row is the only seam that sees both levels.
//
// The assertion is equality against hand-written literals, never a subset
// check: a subset assertion (the shape this test used before TKT-86, three
// selected fields) cannot fail when a field is ADDED, which is the only
// direction PII travels. And the expected side is written out here rather than
// derived from conflictAlarmData on purpose — a fixture built from the type
// under test encodes the very property it claims to prove (ADR-017).
func assertConflictAlarmPIIFloor(t *testing.T, envelope []byte) {
	t.Helper()
	wantEnvelope := []string{"data", "id", "occurred_at", "schema", "type"}
	wantData := []string{"alarm_id", "device_occurred_at", "occurrence_id", "organizer_id", "skew_flagged", "ticket_id"}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(envelope, &top); err != nil {
		t.Fatalf("conflict alarm envelope is not a JSON object: %v", err)
	}
	if got := sortedKeys(top); !slices.Equal(got, wantEnvelope) {
		t.Fatalf("conflict alarm envelope keys = %v, want exactly %v (ADR-009 §5 envelope shape)", got, wantEnvelope)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(top["data"], &data); err != nil {
		t.Fatalf("conflict alarm data is not a JSON object: %v", err)
	}
	if got := sortedKeys(data); !slices.Equal(got, wantData) {
		t.Fatalf("conflict alarm data keys = %v, want exactly %v — a new field on this contract is a PII decision (ADR-025 §D9) and a schema decision (ADR-017 §3), not a payload tweak", got, wantData)
	}

	// Keys alone are not the floor: an existing key's VALUE can carry the PII
	// instead (organizer_id becoming an object with an operator name in it,
	// device_occurred_at becoming free text). Decode every value into the
	// scalar it is contracted to be — an object, an array or a prose string
	// fails here even though the key set is untouched (ai-review K2).
	for _, key := range []string{"alarm_id", "occurrence_id", "organizer_id", "ticket_id"} {
		var id uuid.UUID
		if err := json.Unmarshal(data[key], &id); err != nil {
			t.Fatalf("conflict alarm data.%s = %s, want a bare UUID: %v", key, data[key], err)
		}
	}
	var deviceOccurredAt time.Time
	if err := json.Unmarshal(data["device_occurred_at"], &deviceOccurredAt); err != nil {
		t.Fatalf("conflict alarm data.device_occurred_at = %s, want a bare RFC3339 timestamp: %v", data["device_occurred_at"], err)
	}
	var skewFlagged bool
	if err := json.Unmarshal(data["skew_flagged"], &skewFlagged); err != nil {
		t.Fatalf("conflict alarm data.skew_flagged = %s, want a bare boolean: %v", data["skew_flagged"], err)
	}

	assertConflictAlarmDataTagsPinned(t, wantData)
}

// assertConflictAlarmDataTagsPinned closes the one gap a wire-level key-set
// assertion structurally cannot see: a `,omitempty` field is ABSENT from the
// bytes whenever the fixture leaves it zero, so the key set is unchanged and
// the pin passes green while production envelopes — where the field is
// populated — carry it. No fixture can catch that, because the fixture is
// exactly what decides whether the field appears. So check it at the source,
// where the fixture has no say (ai-review K1).
//
// The expected side is still the caller's hand-written literal; only the
// observed side is derived from the type. That is the ADR-017 fixture trap
// avoided rather than re-entered — the trap is generating the EXPECTATION from
// the type under test, which is what makes a test unable to fail.
func assertConflictAlarmDataTagsPinned(t *testing.T, want []string) {
	t.Helper()
	typ := reflect.TypeOf(conflictAlarmData{})
	tags := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		field := typ.Field(i)
		name, opts, _ := strings.Cut(field.Tag.Get("json"), ",")
		if opts != "" {
			t.Fatalf("conflictAlarmData.%s carries json option %q — a conditionally-emitted field is invisible to the wire-level key pin whenever this fixture leaves it zero, so the PII floor (ADR-025 §D9) would stop being enforced", field.Name, opts)
		}
		tags = append(tags, name)
	}
	slices.Sort(tags)
	if !slices.Equal(tags, want) {
		t.Fatalf("conflictAlarmData json tags = %v, want exactly %v", tags, want)
	}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func TestReconcileConflictAppendsDuplicateAdmitAndOwesAlarm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	occA, occB := uuid.New(), uuid.New()

	if _, err := st.Redeem(ctx, occurrenceRedeemInput(s, occA)); err != nil {
		t.Fatal(err)
	}
	offlineAt := deviceTime().Add(5 * time.Minute)
	result, err := st.ReconcileAdmission(ctx, s.reconcileInput(occB, offlineAt))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ReconcileConflict {
		t.Fatalf("conflicting reconcile = %+v, want conflict", result)
	}
	var occurredAt time.Time
	if err := db.QueryRowContext(ctx, `SELECT occurred_at FROM lifecycle_events WHERE id=$1 AND ticket_id=$2 AND event_type='duplicate_admit'`, occB, s.ticketID).Scan(&occurredAt); err != nil {
		t.Fatalf("duplicate_admit row: %v", err)
	}
	if !occurredAt.Equal(offlineAt) {
		t.Fatalf("duplicate_admit at %v, want the device-claimed %v", occurredAt, offlineAt)
	}

	var subject string
	var envelope []byte
	if err := db.QueryRowContext(ctx, `SELECT subject,envelope FROM lifecycle_integrity_alarm_outbox`).Scan(&subject, &envelope); err != nil {
		t.Fatalf("conflict alarm outbox row: %v", err)
	}
	if subject != SubjectAdmissionConflictAlarm {
		t.Fatalf("alarm subject = %q, want %q (its own operational class, ADR-025 §D6)", subject, SubjectAdmissionConflictAlarm)
	}
	var alarm struct {
		Schema int `json:"schema"`
		Data   struct {
			TicketID     uuid.UUID `json:"ticket_id"`
			OccurrenceID uuid.UUID `json:"occurrence_id"`
			SkewFlagged  bool      `json:"skew_flagged"`
		} `json:"data"`
	}
	if err := json.Unmarshal(envelope, &alarm); err != nil {
		t.Fatal(err)
	}
	if alarm.Schema != 1 || alarm.Data.TicketID != s.ticketID || alarm.Data.OccurrenceID != occB || alarm.Data.SkewFlagged {
		t.Fatalf("alarm envelope = %+v, want schema-1 with the conflicting occurrence", alarm)
	}
	assertConflictAlarmPIIFloor(t, envelope)

	// Reconcile retry of the conflicting occurrence: synced, single-entry-only
	// stays single — no second duplicate_admit, no second alarm.
	again, err := st.ReconcileAdmission(ctx, s.reconcileInput(occB, offlineAt))
	if err != nil {
		t.Fatal(err)
	}
	if again.Outcome != ReconcileSynced {
		t.Fatalf("conflict retry = %+v, want synced", again)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='duplicate_admit'`, s.ticketID); n != 1 {
		t.Fatalf("%d duplicate_admit rows after retry, want 1", n)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox`); n != 1 {
		t.Fatalf("%d alarms after retry, want 1", n)
	}
}

// Skew is validated as recording, never rejecting (§D5): device time is
// claimed, and dropping a physical admission from the record is the worse
// failure. Out-of-bound skew flags the result and the alarm.
func TestReconcileFlagsSkewWithoutRejecting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	skewed := time.Now().UTC().Add(-48 * time.Hour)

	recorded, err := st.ReconcileAdmission(ctx, s.reconcileInput(uuid.New(), skewed))
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Outcome != ReconcileRecorded || !recorded.SkewFlagged {
		t.Fatalf("skewed non-conflict reconcile = %+v, want recorded with the skew flagged", recorded)
	}

	conflict, err := st.ReconcileAdmission(ctx, s.reconcileInput(uuid.New(), skewed))
	if err != nil {
		t.Fatal(err)
	}
	if conflict.Outcome != ReconcileConflict || !conflict.SkewFlagged {
		t.Fatalf("skewed conflict reconcile = %+v, want conflict with the skew flagged", conflict)
	}
	var envelope []byte
	if err := db.QueryRowContext(ctx, `SELECT envelope FROM lifecycle_integrity_alarm_outbox WHERE subject=$1`, SubjectAdmissionConflictAlarm).Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	var alarm struct {
		Data struct {
			SkewFlagged bool `json:"skew_flagged"`
		} `json:"data"`
	}
	if err := json.Unmarshal(envelope, &alarm); err != nil {
		t.Fatal(err)
	}
	if !alarm.Data.SkewFlagged {
		t.Fatal("conflict alarm does not carry the skew flag")
	}
	// The skew-flagged conflict is the class's second emitted variant. Pin the
	// floor here too: a field populated only on the skew path would otherwise
	// evade the non-skew test entirely (ai-review K3).
	assertConflictAlarmPIIFloor(t, envelope)
}

// Reconciliation of a broken-chain ticket is recording, not deciding (§D2):
// the occurrence lands as a per-occurrence quarantine-side record — repeatable,
// idempotent by occurrence — and mints no conflict alarm (the integrity alarm
// class owns broken chains, and live scans already raise it).
func TestReconcileBrokenChainRecordsQuarantineSide(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	corruptChain(t, ctx, db, s.ticketID)
	occA, occB := uuid.New(), uuid.New()

	first, err := st.ReconcileAdmission(ctx, s.reconcileInput(occA, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != ReconcileRecorded {
		t.Fatalf("broken-chain reconcile = %+v, want recorded", first)
	}
	var occurredAt time.Time
	var admittedAt sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT occurred_at,admitted_at FROM lifecycle_integrity_quarantine WHERE occurrence_id=$1`, occA).Scan(&occurredAt, &admittedAt); err != nil {
		t.Fatalf("quarantine-side record: %v", err)
	}
	if !occurredAt.Equal(deviceTime()) || admittedAt.Valid {
		t.Fatalf("reconciled record: occurred_at=%v admitted_at=%v; want device time and NULL admitted_at (recording, not a live admission)", occurredAt, admittedAt)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type IN ('redeemed','duplicate_admit')`, s.ticketID); n != 0 {
		t.Fatal("reconciliation appended onto an unverified chain")
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox WHERE subject=$1`, SubjectAdmissionConflictAlarm); n != 0 {
		t.Fatal("a broken-chain recording minted a conflict alarm")
	}

	// Per-occurrence means repeatable: a second distinct offline admission of
	// the same broken ticket is recorded too, not swallowed (ADR-025 §D2).
	second, err := st.ReconcileAdmission(ctx, s.reconcileInput(occB, deviceTime().Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if second.Outcome != ReconcileRecorded {
		t.Fatalf("second broken-chain occurrence = %+v, want recorded", second)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE ticket_id=$1`, s.ticketID); n != 2 {
		t.Fatalf("%d quarantine-side records, want 2 — the second admission vanished (the §D2 gap this design closes)", n)
	}
	// Idempotent by occurrence.
	retry, err := st.ReconcileAdmission(ctx, s.reconcileInput(occA, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	if retry.Outcome != ReconcileSynced {
		t.Fatalf("broken-chain reconcile retry = %+v, want synced", retry)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE ticket_id=$1`, s.ticketID); n != 2 {
		t.Fatal("a reconcile retry duplicated a quarantine-side record")
	}
}

func TestReconcileRejectsIdentityMismatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())

	in := s.reconcileInput(uuid.New(), deviceTime())
	in.OrderID = uuid.New()
	if _, err := st.ReconcileAdmission(ctx, in); !errors.Is(err, ErrTicketCredential) {
		t.Fatalf("identity mismatch: err=%v, want ErrTicketCredential", err)
	}
}

// ---- migration 0005 ----

func TestPerOccurrenceQuarantineMigrationStatementOrder(t *testing.T) {
	raw, err := fs.ReadFile(migrationsFS, "migrations/0005_per_occurrence_quarantine.sql")
	if err != nil {
		t.Fatalf("migration 0005 is missing: %v", err)
	}
	sql := string(raw)
	steps := []string{
		"ADD COLUMN occurrence_id uuid",
		"ADD COLUMN occurred_at timestamptz",
		"ALTER COLUMN admitted_at DROP NOT NULL",
		"CREATE UNIQUE INDEX lifecycle_integrity_quarantine_one_admission_uidx",
		"CREATE UNIQUE INDEX lifecycle_integrity_quarantine_occurrence_uidx",
		"ADD COLUMN quarantine_id uuid",
		"DROP CONSTRAINT lifecycle_integrity_quarantine_pkey",
		"ADD PRIMARY KEY (quarantine_id)",
	}
	last := -1
	for _, s := range steps {
		i := strings.Index(sql, s)
		if i < 0 {
			t.Fatalf("migration 0005 lacks %q", s)
		}
		if i < last {
			t.Fatalf("migration 0005 runs %q out of order: the per-ticket guard must exist before the old PK drops", s)
		}
		last = i
	}
	for _, banned := range []string{"CONCURRENTLY", "NOT VALID", "NO TRANSACTION"} {
		if strings.Contains(sql, banned) {
			t.Fatalf("migration 0005 contains %q (ADR-020/ADR-022 forbid it here)", banned)
		}
	}
}

func TestPerOccurrenceQuarantineMigrationCarriesAndEnforces(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.UpTo(ctx, 4); err != nil {
		t.Fatal(err)
	}

	// A pre-0005 quarantine row: one degraded admission, no occurrence protocol.
	ticketID := uuid.New()
	organizerID := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tickets(id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,'signed-credential',now())`,
		ticketID, uuid.New(), uuid.New(), organizerID, uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	admittedAt := time.Date(2026, time.July, 10, 20, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(ctx, `INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,admitted_at) VALUES($1,$2,'seed',$3)`,
		ticketID, organizerID, admittedAt); err != nil {
		t.Fatal(err)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply migration 0005: %v", err)
	}

	// Grandfathered row carried: NULL occurrence (no id existed to claim),
	// admission time intact.
	var occ sql.NullString
	var carried time.Time
	if err := db.QueryRowContext(ctx, `SELECT occurrence_id,admitted_at FROM lifecycle_integrity_quarantine WHERE ticket_id=$1`, ticketID).Scan(&occ, &carried); err != nil {
		t.Fatal(err)
	}
	if occ.Valid || !carried.Equal(admittedAt) {
		t.Fatalf("grandfathered row = occurrence %v at %v, want NULL occurrence and the original %v", occ, carried, admittedAt)
	}

	// Still immutable.
	if _, err := db.ExecContext(ctx, `UPDATE lifecycle_integrity_quarantine SET reason='rewritten' WHERE ticket_id=$1`, ticketID); err == nil {
		t.Fatal("quarantine rows are no longer append-only after 0005")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM lifecycle_integrity_quarantine WHERE ticket_id=$1`, ticketID); err == nil {
		t.Fatal("quarantine rows became deletable after 0005")
	}

	// One LIVE admission per ticket (admitted_at NOT NULL), still enforced.
	if _, err := db.ExecContext(ctx, `INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,admitted_at,occurrence_id) VALUES($1,$2,'second',now(),$3)`,
		ticketID, organizerID, uuid.New()); err == nil {
		t.Fatal("a second live degraded admission row was accepted (§D6 admit-once guard gone)")
	}
	// Reconciliation-learned records (admitted_at NULL) are repeatable per ticket.
	for i := 0; i < 2; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,occurrence_id,occurred_at) VALUES($1,$2,'reconciled',$3,now())`,
			ticketID, organizerID, uuid.New()); err != nil {
			t.Fatalf("reconciliation-learned record %d rejected: %v", i+1, err)
		}
	}
	// Occurrence ids stay unique across the table.
	dup := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,occurrence_id,occurred_at) VALUES($1,$2,'reconciled',$3,now())`,
		ticketID, organizerID, dup); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,occurrence_id,occurred_at) VALUES($1,$2,'reconciled',$3,now())`,
		ticketID, organizerID, dup); err == nil {
		t.Fatal("a duplicated occurrence id was accepted on the quarantine side")
	}
	// Every row carries a time: both NULL is rejected.
	if _, err := db.ExecContext(ctx, `INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,occurrence_id) VALUES($1,$2,'timeless',$3)`,
		ticketID, organizerID, uuid.New()); err == nil {
		t.Fatal("a quarantine row with neither admitted_at nor occurred_at was accepted")
	}
}

func TestPerOccurrenceQuarantineMigrationIsIrreversible(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("migration 0005 rolled back; quarantine history is not protected")
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_class WHERE relname='lifecycle_integrity_quarantine_occurrence_uidx'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("failed down attempt altered the schema (n=%d err=%v)", n, err)
	}
}

// The §D7-style measured obligation for 0005, opt-in like TKT-84's. The
// quarantine table is small by design (one live row per corrupt ticket), so the
// honest representative volume is far below lifecycle_events'; the env var says
// what "representative" meant for the recorded run.
func TestPerOccurrenceQuarantineMigrationRepresentativeVolume(t *testing.T) {
	nStr := os.Getenv("ACCESS_QUARANTINE_MIGRATION_MEASUREMENT_ROWS")
	if nStr == "" {
		t.Skip("ACCESS_QUARANTINE_MIGRATION_MEASUREMENT_ROWS is not set")
	}
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 {
		t.Fatalf("ACCESS_QUARANTINE_MIGRATION_MEASUREMENT_ROWS=%q is not a positive integer", nStr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.UpTo(ctx, 4); err != nil {
		t.Fatal(err)
	}
	// One live quarantine row per seeded ticket — every row matches the
	// one-admission partial index predicate, the worst case for that build.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tickets(id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at)
		SELECT gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'signed-credential',now()
		FROM generate_series(1,$1)`, n); err != nil {
		t.Fatalf("seed tickets: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,admitted_at)
		SELECT id, organizer_id, 'measurement seed', now() FROM tickets`); err != nil {
		t.Fatalf("seed quarantine rows: %v", err)
	}
	var rows int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM lifecycle_integrity_quarantine`).Scan(&rows); err != nil {
		t.Fatal(err)
	}

	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer migrateCancel()
	start := time.Now()
	if _, err := provider.UpTo(migrateCtx, 5); err != nil {
		t.Fatalf("migration 0005 breached the 30s bound at %d quarantine rows: %v", rows, err)
	}
	elapsed := time.Since(start)
	t.Logf("migration 0005: %v for %d quarantine rows", elapsed, rows)
	if elapsed > 15*time.Second {
		t.Logf("WARNING: above the 15s engineering target — ship only with the reduced margin explicitly accepted")
	}
}
