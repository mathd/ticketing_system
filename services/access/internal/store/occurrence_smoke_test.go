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

// assertConflictAlarmPIIFloor pins the payload-schema constraint (ADR-025 §D9,
// amended TKT-119: bounded identifiers, enums and operational scalars — no free
// text, no nested objects) on the bytes actually owed to the operator — the
// persisted outbox envelope, not a struct the producer happens to use today.
//
// What this can and cannot prove: it pins the exact key set and decodes every value
// into the scalar it is contracted to be, so a new field or a nested object cannot
// arrive unnoticed. It is a producer-schema check on honest changes. It cannot prove
// the absence of PII in general, and it constrains no one with write access to the
// database (ADR-021 §The trust boundary).
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
	wantData := sortedContractKeys(conflictAlarmContract)

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
	// device_occurred_at becoming free text). Decode every value — envelope
	// level too, not just data — into the scalar it is contracted to be, so an
	// object, an array or a prose string fails even though the key set is
	// untouched (ai-review K2, second pass P2).
	var alarmID, ticketID uuid.UUID
	decodeAlarmScalar(t, "id", top["id"], &alarmID)
	decodeAlarmScalar(t, "occurred_at", top["occurred_at"], new(time.Time))
	decodeAlarmScalar(t, "schema", top["schema"], new(int))
	var subject string
	decodeAlarmScalar(t, "type", top["type"], &subject)
	if subject != SubjectAdmissionConflictAlarm {
		t.Fatalf("conflict alarm type = %q, want %q (ADR-009 §5: type is stable across variants)", subject, SubjectAdmissionConflictAlarm)
	}
	for _, key := range wantData {
		raw := data[key]
		switch conflictAlarmContract[key] {
		case reflect.TypeOf(uuid.UUID{}):
			decodeAlarmScalar(t, "data."+key, raw, &ticketID)
		case reflect.TypeOf(time.Time{}):
			decodeAlarmScalar(t, "data."+key, raw, new(time.Time))
		default:
			decodeAlarmScalar(t, "data."+key, raw, new(bool))
		}
	}

	assertConflictAlarmDataTagsPinned(t, wantData)
}

// decodeAlarmScalar decodes one alarm value into the scalar it is contracted to
// be, rejecting JSON null explicitly. The explicit null check is load-bearing:
// encoding/json treats null as "leave the target untouched" for every type used
// here, so a null sails through uuid.UUID, time.Time and bool alike and the
// decode proves nothing (second-pass finding P1). A nulled value is also how a
// pointer-to-struct field would look in this fixture while carrying a populated
// object in production.
func decodeAlarmScalar(t *testing.T, label string, raw json.RawMessage, into any) {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		t.Fatalf("conflict alarm %s is %q — a null value is not a bounded identifier and defeats every scalar decode (ADR-025 §D9)", label, string(raw))
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("conflict alarm %s = %s, want a bare %T: %v", label, raw, into, err)
	}
}

// conflictAlarmContract is the hand-written contract for the admission-conflict
// alarm's data payload: every field the schema-1 payload may carry, and the
// scalar Go type it must carry it as. Hand-written on purpose — derived from
// conflictAlarmData it would encode the very property it exists to prove
// (ADR-017), which is what makes a fixture unable to fail.
var conflictAlarmContract = map[string]reflect.Type{
	"alarm_id":           reflect.TypeOf(uuid.UUID{}),
	"organizer_id":       reflect.TypeOf(uuid.UUID{}),
	"ticket_id":          reflect.TypeOf(uuid.UUID{}),
	"occurrence_id":      reflect.TypeOf(uuid.UUID{}),
	"device_occurred_at": reflect.TypeOf(time.Time{}),
	"skew_flagged":       reflect.TypeOf(false),
}

func sortedContractKeys(m map[string]reflect.Type) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
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
// It also pins each field's Go TYPE against the same hand-written contract.
// Without that, a field could be re-typed to a pointer-to-struct carrying its
// own `,omitempty` members: nil in this fixture it marshals as null, sails past
// the wire pin and the scalar decode, and carries a populated object in
// production. Pinning the top-level types forbids nested types outright, which
// is why this walk does not need to recurse (second-pass finding P1).
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
		if want, ok := conflictAlarmContract[name]; ok && field.Type != want {
			t.Fatalf("conflictAlarmData.%s is %s, want %s — this contract carries bounded scalars only; a composite type can hide fields the wire pin never sees (ADR-025 §D9)", field.Name, field.Type, want)
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
	// UpTo(5), not Up() — see TestRepeatableAdmissionMigrationIsIrreversible:
	// Down rolls back only the head, so Up-to-head never tested 0005 (A1).
	if _, err := provider.UpTo(ctx, 5); err != nil {
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

// TKT-269 — an offline admit of a REFUNDED ticket must not be silent.
//
// TKT-157 refuses a refunded ticket at a LIVE gate (postgres.go, scan.go). An
// offline scanner cannot know about the refund, admits the holder, and
// reconciliation then records that admission faithfully — chain verifying
// clean, nobody informed. ADR-038 §4 states the gap and hands it here.
//
// The invariant, said without naming the implementation: AN OFFLINE ADMIT OF A
// VOIDED TICKET IS NEVER SILENT. It is still recorded — reconciliation is
// recording, not deciding (ADR-025 §D2/§D6), and the person is already inside —
// but it owes an operational alarm on the admission-conflict class, which means
// "the chain is valid and the world disagreed with it", NOT the integrity class,
// which means "the chain is suspect" (ADR-038 §4 rejected that explicitly).

// refundOwnTicket voids the single-ticket order issueTicket minted. issueTicket
// gives each ticket its own order id, so quantity 1 selects exactly this ticket
// (plan A3). The refund goes through the real path, never a hand-written insert,
// so the `refunded` event participates in the signed chain.
func refundOwnTicket(t *testing.T, ctx context.Context, st *Postgres, s seeded) {
	t.Helper()
	if _, err := st.RefundOrderTickets(ctx, s.id.OrganizerID, s.id.OrderID, uuid.New(), 1); err != nil {
		t.Fatal(err)
	}
}

func conflictAlarmCount(t *testing.T, ctx context.Context, db *sql.DB) int {
	t.Helper()
	return countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox WHERE subject=$1`, SubjectAdmissionConflictAlarm)
}

// A — branch (a): single-entry, refunded, no prior redemption. The occurrence is
// recorded exactly as an unrefunded one would be, AND owes one conflict alarm.
func TestReconcileRefundedTicketRecordsRedemptionAndOwesAlarm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	refundOwnTicket(t, ctx, st, s)

	// The refunded row as it stands BEFORE reconciliation. AC2/AC3: the alarm is
	// additive — reconciliation must not touch the fact that preceded it.
	var refundedIDBefore uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT id FROM lifecycle_events WHERE ticket_id=$1 AND event_type='refunded'`, s.ticketID).Scan(&refundedIDBefore); err != nil {
		t.Fatalf("refunded row before reconcile: %v", err)
	}

	occ := uuid.New()
	offlineAt := deviceTime().Add(3 * time.Minute)
	result, err := st.ReconcileAdmission(ctx, s.reconcileInput(occ, offlineAt))
	if err != nil {
		t.Fatal(err)
	}

	// Recorded, not refused and not downgraded: the person is already inside and
	// dropping the occurrence would falsify the trail (ADR-038 §4, AC2).
	if result.Outcome != ReconcileRecorded {
		t.Fatalf("refunded offline admit outcome = %s, want recorded — reconciliation records, it does not decide", result.Outcome)
	}
	var occurredAt time.Time
	if err := db.QueryRowContext(ctx, `SELECT occurred_at FROM lifecycle_events WHERE id=$1 AND ticket_id=$2 AND event_type='redeemed'`, occ, s.ticketID).Scan(&occurredAt); err != nil {
		t.Fatalf("redeemed row keyed on the occurrence: %v", err)
	}
	if !occurredAt.Equal(offlineAt) {
		t.Fatalf("redeemed at %v, want the device-claimed %v", occurredAt, offlineAt)
	}
	if n := countEvents(t, ctx, db, s.ticketID, "duplicate_admit"); n != 0 {
		t.Fatalf("%d duplicate_admit rows; a first admission is a redemption, refunded or not", n)
	}

	// The alarm: exactly one, on the admission-conflict class.
	if n := conflictAlarmCount(t, ctx, db); n != 1 {
		t.Fatalf("%d admission-conflict alarms, want exactly 1 — an offline admit of a voided ticket is never silent", n)
	}
	var envelope []byte
	if err := db.QueryRowContext(ctx, `SELECT envelope FROM lifecycle_integrity_alarm_outbox WHERE subject=$1`, SubjectAdmissionConflictAlarm).Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	var alarm struct {
		Schema int `json:"schema"`
		Data   struct {
			TicketID     uuid.UUID `json:"ticket_id"`
			OccurrenceID uuid.UUID `json:"occurrence_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(envelope, &alarm); err != nil {
		t.Fatal(err)
	}
	if alarm.Schema != 1 || alarm.Data.TicketID != s.ticketID || alarm.Data.OccurrenceID != occ {
		t.Fatalf("alarm envelope = %+v, want schema-1 naming this ticket and occurrence", alarm)
	}
	// The payload is frozen by a byte-exact golden and this key-set floor: the
	// refunded case reuses the contract, it does not extend it (ADR-017/§D9).
	assertConflictAlarmPIIFloor(t, envelope)

	// AC2/AC3 — additive: the earlier refunded fact is untouched.
	var refundedIDAfter uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT id FROM lifecycle_events WHERE ticket_id=$1 AND event_type='refunded'`, s.ticketID).Scan(&refundedIDAfter); err != nil {
		t.Fatalf("refunded row after reconcile: %v", err)
	}
	if refundedIDAfter != refundedIDBefore {
		t.Fatalf("refunded event id changed %s -> %s; the alarm is additive, it never rewrites the fact", refundedIDBefore, refundedIDAfter)
	}
	if err := st.VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify-lifecycle after a refunded reconcile: %v", err)
	}

	// AC4 — replay: synced, no second event, no second alarm.
	again, err := st.ReconcileAdmission(ctx, s.reconcileInput(occ, offlineAt))
	if err != nil {
		t.Fatal(err)
	}
	if again.Outcome != ReconcileSynced {
		t.Fatalf("replay outcome = %s, want synced", again.Outcome)
	}
	if n := countEvents(t, ctx, db, s.ticketID, "redeemed"); n != 1 {
		t.Fatalf("%d redeemed rows after replay, want 1", n)
	}
	if n := conflictAlarmCount(t, ctx, db); n != 1 {
		t.Fatalf("%d alarms after replay, want 1 — a retry is the same admission", n)
	}
}

// B — branch (b): single-entry, refunded, WITH a prior redemption. This branch
// already owed a conflict alarm before TKT-269, so "an alarm exists" proves
// nothing here. What this test constrains is the new guard's PLACEMENT: hoisting
// the refunded check above the prior-redeemed lookup would owe a SECOND alarm for
// one occurrence (plan A1). The count is the assertion that bites.
func TestReconcileRefundedDuplicateOwesExactlyOneAlarm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	occA, occB := uuid.New(), uuid.New()

	// Redeem live FIRST, then refund: RefundOrderTickets selects on
	// not-yet-refunded and never excludes a redeemed ticket, so this state is
	// reachable in production.
	if _, err := st.Redeem(ctx, occurrenceRedeemInput(s, occA)); err != nil {
		t.Fatal(err)
	}
	refundOwnTicket(t, ctx, st, s)

	result, err := st.ReconcileAdmission(ctx, s.reconcileInput(occB, deviceTime().Add(5*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ReconcileConflict {
		t.Fatalf("outcome = %s, want conflict — the trace already held an admission", result.Outcome)
	}
	if n := countEvents(t, ctx, db, s.ticketID, "duplicate_admit"); n != 1 {
		t.Fatalf("%d duplicate_admit rows, want 1", n)
	}
	if n := conflictAlarmCount(t, ctx, db); n != 1 {
		t.Fatalf("%d admission-conflict alarms, want exactly 1 — one occurrence owes one alarm, and a refunded check placed above the prior-redeemed lookup would owe a second", n)
	}

	again, err := st.ReconcileAdmission(ctx, s.reconcileInput(occB, deviceTime().Add(5*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if again.Outcome != ReconcileSynced {
		t.Fatalf("replay outcome = %s, want synced", again.Outcome)
	}
	if n := conflictAlarmCount(t, ctx, db); n != 1 {
		t.Fatalf("%d alarms after replay, want 1", n)
	}
}

// D — branch (d): refunded AND a broken chain. The quarantine path deliberately
// owes NO conflict alarm: the integrity class owns broken chains and every live
// scan of this ticket already raises it. A refunded guard that fires before
// verifyTicketChain would mint one here, which is why the ticket being refunded
// is load-bearing in this fixture rather than decoration.
func TestReconcileRefundedBrokenChainRecordsQuarantineWithoutConflictAlarm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	refundOwnTicket(t, ctx, st, s)
	corruptChain(t, ctx, db, s.ticketID)

	occ := uuid.New()
	result, err := st.ReconcileAdmission(ctx, s.reconcileInput(occ, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ReconcileRecorded {
		t.Fatalf("outcome = %s, want recorded", result.Outcome)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE occurrence_id=$1`, occ); n != 1 {
		t.Fatalf("%d quarantine-side records, want 1 — the occurrence lands somewhere", n)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type IN ('redeemed','duplicate_admit')`, s.ticketID); n != 0 {
		t.Fatal("reconciliation appended onto an unverified chain")
	}
	if n := conflictAlarmCount(t, ctx, db); n != 0 {
		t.Fatalf("%d admission-conflict alarms on a broken chain, want 0 — the integrity class owns a suspect chain (ADR-021 §D6), and a refunded guard firing before verifyTicketChain would mint one here", n)
	}
}

// E — atomicity. On the success path both artifacts exist whether or not they
// share a transaction, so no happy-path test can tell one transaction from two.
// Reject the outbox insert and assert NEITHER artifact committed.
func TestReconcileRefundedAlarmAndAppendCommitTogether(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	refundOwnTicket(t, ctx, st, s)

	if _, err := db.ExecContext(ctx, `
		CREATE FUNCTION reject_alarm() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'alarm outbox rejected (TKT-269 atomicity probe)'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER reject_alarm_trigger BEFORE INSERT ON lifecycle_integrity_alarm_outbox
		FOR EACH ROW EXECUTE FUNCTION reject_alarm();`); err != nil {
		t.Fatal(err)
	}

	occ := uuid.New()
	if _, err := st.ReconcileAdmission(ctx, s.reconcileInput(occ, deviceTime())); err == nil {
		t.Fatal("reconcile succeeded while the alarm insert was rejected; the alarm is owed, not best-effort")
	}

	if n := countEvents(t, ctx, db, s.ticketID, "redeemed"); n != 0 {
		t.Fatalf("%d redeemed rows committed while its owed alarm failed — the append and the alarm are one transaction", n)
	}
	if n := conflictAlarmCount(t, ctx, db); n != 0 {
		t.Fatalf("%d alarms committed, want 0", n)
	}
	// The fact that preceded the failed reconcile is untouched.
	if n := countEvents(t, ctx, db, s.ticketID, "refunded"); n != 1 {
		t.Fatalf("%d refunded rows, want 1 — a rolled-back reconcile must not disturb it", n)
	}
}

// TKT-270 — an offline admit of an EXCHANGED ticket must not be silent either.
//
// The sibling of TKT-269 above, and deliberately NOT a copy of it. TKT-166 /
// ADR-039 refuse an exchanged ticket at a LIVE gate for a reason that is not the
// refund reason: an exchanged ticket has a LIVE REPLACEMENT somewhere, so
// admitting the original would admit the exchange twice — and the replacement
// can be admitted legitimately at another gate the same night. Offline, the
// scanner cannot know, admits the holder, and reconciliation recorded an
// ordinary redemption with the chain verifying clean and nobody informed.
//
// The invariant, said without naming the implementation: AN OFFLINE ADMIT OF A
// COMMERCIALLY VOIDED TICKET IS NEVER SILENT — and ONE PHYSICAL ADMISSION OWES
// EXACTLY ONE ALARM, however many voiding facts apply.
//
// Which ADR governs, since this is the one place the ticket is not TKT-269 with
// a word changed: ADR-025 §D2/§D6. NOT ADR-038 — its Consequences narrow the
// fail-open exception to "tickets a refund has voided", so it does not reach
// exchanges; and ADR-039 explains the live refusal without defining
// reconciliation at all. Per ADR-021 this is honest-writer consistency, not
// tamper-evidence: a writer with database access can delete the voiding fact.

// exchangeOwnTicket voids the single-ticket order issueTicket minted, through
// the real SwitchExchange path — never a hand-written lifecycle_events insert.
// That is not fastidiousness: every lifecycle event goes through
// appendLifecycle, `access verify-lifecycle` asserts one-to-one coverage in the
// gate, and a direct insert reads as tampering.
//
// One source ticket and one replacement satisfy the whole-order rule — an
// exchange has no partial form (TKT-158, exchanges.go:140) — and issueTicket
// gives each ticket its own order, so the source order is exactly this ticket.
func exchangeOwnTicket(t *testing.T, ctx context.Context, st *Postgres, s seeded) {
	t.Helper()
	if err := st.SwitchExchange(ctx, SwitchExchangeInput{
		EventID:       uuid.New(),
		ExchangeID:    uuid.New(),
		SourceOrderID: s.id.OrderID,
		OrganizerID:   s.id.OrganizerID,
		Tickets:       replacementTickets(uuid.New(), s.id.OrganizerID, s.id.SlotID, 1),
	}); err != nil {
		t.Fatalf("seed the exchange: %v", err)
	}
}

// conflictAlarmsFor counts admission-conflict alarms naming a specific ticket
// and occurrence, by decoding the envelope — the outbox has no ticket_id column,
// so the identity is only inside the payload.
//
// conflictAlarmCount, which every earlier test uses, counts by SUBJECT ALONE.
// The second ai-review pass on TKT-270 was right that this is weaker than it
// reads: exchangeOwnTicket issues a REPLACEMENT ticket, so an implementation
// that alarmed on the wrong ticket would keep the total at one and pass. The
// count still matters — it is what catches a second alarm being owed — so the
// tests that care about identity assert BOTH: the right ticket is named, and no
// other alarm exists beside it.
func conflictAlarmsFor(t *testing.T, ctx context.Context, db *sql.DB, ticketID, occurrenceID uuid.UUID) int {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT envelope FROM lifecycle_integrity_alarm_outbox WHERE subject=$1`, SubjectAdmissionConflictAlarm)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	matching := 0
	for rows.Next() {
		var envelope []byte
		if err := rows.Scan(&envelope); err != nil {
			t.Fatal(err)
		}
		var alarm struct {
			Data struct {
				TicketID     uuid.UUID `json:"ticket_id"`
				OccurrenceID uuid.UUID `json:"occurrence_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(envelope, &alarm); err != nil {
			t.Fatal(err)
		}
		if alarm.Data.TicketID == ticketID && alarm.Data.OccurrenceID == occurrenceID {
			matching++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return matching
}

// requireVoidingFacts asserts the seeds actually landed BEFORE the reconcile
// under test runs. Without it a seed that silently stopped working would leave
// the test asserting an alarm count on a ticket carrying fewer voiding facts
// than it names — passing for the wrong reason, which is the failure mode these
// tests exist to catch in the production code.
func requireVoidingFacts(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID, types ...string) {
	t.Helper()
	for _, eventType := range types {
		if n := countEvents(t, ctx, db, ticketID, eventType); n != 1 {
			t.Fatalf("%d %s rows before reconciling, want 1 — the seed this test's whole claim "+
				"rests on did not land, so nothing below would mean what it says", n, eventType)
		}
	}
}

// A′ — branch (a) for exchanges: single-entry, exchanged, no prior redemption.
// Recorded exactly as an unexchanged one would be, AND owes one conflict alarm.
func TestReconcileExchangedTicketRecordsRedemptionAndOwesAlarm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	exchangeOwnTicket(t, ctx, st, s)
	requireVoidingFacts(t, ctx, db, s.ticketID, "exchanged")

	occ := uuid.New()
	offlineAt := deviceTime().Add(3 * time.Minute)
	result, err := st.ReconcileAdmission(ctx, s.reconcileInput(occ, offlineAt))
	if err != nil {
		t.Fatal(err)
	}

	// Recorded, not refused and not downgraded: the person is already inside and
	// dropping the occurrence would falsify the trail (ADR-025 §D2/§D6).
	if result.Outcome != ReconcileRecorded {
		t.Fatalf("exchanged offline admit outcome = %s, want recorded — reconciliation records, it does not decide", result.Outcome)
	}
	var occurredAt time.Time
	if err := db.QueryRowContext(ctx, `SELECT occurred_at FROM lifecycle_events WHERE id=$1 AND ticket_id=$2 AND event_type='redeemed'`, occ, s.ticketID).Scan(&occurredAt); err != nil {
		t.Fatalf("redeemed row keyed on the occurrence: %v", err)
	}
	if !occurredAt.Equal(offlineAt) {
		t.Fatalf("redeemed at %v, want the device-claimed %v", occurredAt, offlineAt)
	}
	if n := countEvents(t, ctx, db, s.ticketID, "duplicate_admit"); n != 0 {
		t.Fatalf("%d duplicate_admit rows; a first admission is a redemption, exchanged or not", n)
	}
	if n := conflictAlarmCount(t, ctx, db); n != 1 {
		t.Fatalf("%d admission-conflict alarms, want exactly 1 — an offline admit of a voided ticket is never silent", n)
	}
}

// B′ — branch (b) for exchanges. Read the note: this test pins PLACEMENT, not
// the guard's presence.
//
// The duplicate path already owes an alarm of its own (reconcile.go, the
// duplicate_admit branch), so DELETING the exchanged guard leaves this green.
// That is expected and is why the discriminating mutation for guard presence is
// A′ and F′, not this one. What this catches is the exchanged check hoisted
// ABOVE the prior-redeemed lookup: one occurrence would then owe two alarms.
//
// Construction order is forced. SwitchExchange refuses a source ticket that has
// already been admitted (ErrSourceTicketsAlreadyAdmitted, exchanges.go:165), so
// the exchange cannot follow the first admission — unlike the refunded case
// above, which redeems first. Redo the analysis, do not port it.
func TestReconcileExchangedDuplicateOwesExactlyOneAdditionalAlarm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	exchangeOwnTicket(t, ctx, st, s)
	requireVoidingFacts(t, ctx, db, s.ticketID, "exchanged")

	occA, occB := uuid.New(), uuid.New()
	if _, err := st.ReconcileAdmission(ctx, s.reconcileInput(occA, deviceTime().Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	before := conflictAlarmCount(t, ctx, db)

	result, err := st.ReconcileAdmission(ctx, s.reconcileInput(occB, deviceTime().Add(5*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ReconcileConflict {
		t.Fatalf("outcome = %s, want conflict — the trace already held an admission", result.Outcome)
	}
	if n := countEvents(t, ctx, db, s.ticketID, "duplicate_admit"); n != 1 {
		t.Fatalf("%d duplicate_admit rows, want 1", n)
	}
	// The DELTA, not the total: occurrence A legitimately owed one of its own.
	if got := conflictAlarmCount(t, ctx, db) - before; got != 1 {
		t.Fatalf("occurrence B added %d admission-conflict alarms, want exactly 1 — one occurrence "+
			"owes one alarm, and an exchanged check placed above the prior-redeemed lookup would owe a second", got)
	}
	// And the one it added names occurrence B, not the replacement ticket or a
	// second alarm for occurrence A — a delta of one is satisfiable by an alarm
	// raised for the wrong subject.
	if got := conflictAlarmsFor(t, ctx, db, s.ticketID, occB); got != 1 {
		t.Fatalf("%d admission-conflict alarms name this ticket and occurrence B, want exactly 1", got)
	}
}

// D′ — branch (d) for exchanges: exchanged AND a broken chain. The quarantine
// path owes NO conflict alarm — the integrity class owns a suspect chain
// (ADR-021 §D6) and every live scan of this ticket already raises it. The
// exchange seed is load-bearing here rather than decoration: an exchanged guard
// firing before verifyTicketChain would mint one, and without the seed nothing
// in this fixture could tell that apart from an ordinary broken chain.
func TestReconcileExchangedBrokenChainRecordsQuarantineWithoutConflictAlarm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	exchangeOwnTicket(t, ctx, st, s)
	requireVoidingFacts(t, ctx, db, s.ticketID, "exchanged")
	corruptChain(t, ctx, db, s.ticketID)

	occ := uuid.New()
	result, err := st.ReconcileAdmission(ctx, s.reconcileInput(occ, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ReconcileRecorded {
		t.Fatalf("outcome = %s, want recorded", result.Outcome)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE occurrence_id=$1`, occ); n != 1 {
		t.Fatalf("%d quarantine-side records, want 1 — the occurrence lands somewhere", n)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type IN ('redeemed','duplicate_admit')`, s.ticketID); n != 0 {
		t.Fatal("reconciliation appended onto an unverified chain")
	}
	if n := conflictAlarmCount(t, ctx, db); n != 0 {
		t.Fatalf("%d admission-conflict alarms on a broken chain, want 0 — the integrity class owns a "+
			"suspect chain (ADR-021 §D6), and an exchanged guard firing before verifyTicketChain would mint one", n)
	}
}

// F′ — the case that does not exist in TKT-269 at all, and the reason this
// ticket is not a copy-paste: a ticket BOTH refunded AND exchanged.
//
// The invariant, stated without naming the implementation, and the source of
// the expected count — derived from the requirement, never from a run:
//
//	ONE PHYSICAL OFFLINE ADMISSION OWES EXACTLY ONE ADMISSION-CONFLICT ALARM,
//	HOWEVER MANY COMMERCIAL VOIDING FACTS APPLY TO THE TICKET.
//
// Two independent alarm-owning branches would satisfy every other test in this
// file and fail only this one, with a count of two. That is the mutation it
// exists to catch.
//
// Construction order is forced and was verified in both directions:
// SwitchExchange refuses an already-VOIDED source (ErrSourceTicketsAlreadyVoided,
// exchanges.go:151), so refund-first is impossible; RefundOrderTickets selects on
// refundThatVoided alone (refunds.go:120), so an exchanged ticket is still
// refundable. Exchange, then refund.
// F′ deliberately CANNOT detect removal of the exchanged guard: with both facts
// present, ticketRefunded still answers true and TKT-269's half still owes the
// one alarm this asserts. That is not a hole — it is what makes F′ specifically
// about the COUNT. Guard PRESENCE is A′'s job, on a fixture with no refund in it
// at all, and A′ is the only test that can do it.
//
// An earlier revision of this file added a second exchanged-only test here on
// the theory that A′ and it covered "opposite ends of the two-fact space". They
// did not: the fixtures were identical, so it was A′ with a different name, and
// the second ai-review pass was right to call it a duplicate. Deleted. The
// lesson is worth the comment — a test that FEELS like it adds a dimension adds
// nothing unless you can name the state it reaches that no other test does.
func TestReconcileRefundedAndExchangedTicketOwesExactlyOneAlarm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	exchangeOwnTicket(t, ctx, st, s)
	refundOwnTicket(t, ctx, st, s)
	requireVoidingFacts(t, ctx, db, s.ticketID, "exchanged", "refunded")

	occ := uuid.New()
	result, err := st.ReconcileAdmission(ctx, s.reconcileInput(occ, deviceTime().Add(3*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ReconcileRecorded {
		t.Fatalf("outcome = %s, want recorded — two voiding facts do not change that reconciliation records", result.Outcome)
	}
	if n := countEvents(t, ctx, db, s.ticketID, "redeemed"); n != 1 {
		t.Fatalf("%d redeemed rows, want 1 — one admission is one redemption", n)
	}
	// Both directions. The COUNT is what catches two independent alarm-owning
	// branches; the IDENTITY is what stops an alarm raised for the replacement
	// ticket — which exchangeOwnTicket also issues — from satisfying the count.
	if n := conflictAlarmsFor(t, ctx, db, s.ticketID, occ); n != 1 {
		t.Fatalf("%d admission-conflict alarms naming this ticket and occurrence, want exactly 1: one "+
			"physical offline admission owes one admission-conflict alarm, however many commercial "+
			"voiding facts apply to the ticket. Two independent alarm-owning branches would produce 2 here", n)
	}
	if n := conflictAlarmCount(t, ctx, db); n != 1 {
		t.Fatalf("%d admission-conflict alarms in total, want 1 — one was owed for this admission and "+
			"nothing else in this fixture owes one", n)
	}
}

// E′ — atomicity for exchanges. On the success path both artifacts exist whether
// or not they share a transaction, so no happy-path test can tell one
// transaction from two. Reject the outbox insert and assert NEITHER committed.
// The seam is the database transaction because that IS the mechanism — a mock
// callback would pin the harness rather than the contract.
func TestReconcileExchangedAlarmAndAppendCommitTogether(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	exchangeOwnTicket(t, ctx, st, s)
	requireVoidingFacts(t, ctx, db, s.ticketID, "exchanged")

	if _, err := db.ExecContext(ctx, `
		CREATE FUNCTION reject_alarm() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'alarm outbox rejected (TKT-270 atomicity probe)'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER reject_alarm_trigger BEFORE INSERT ON lifecycle_integrity_alarm_outbox
		FOR EACH ROW EXECUTE FUNCTION reject_alarm();`); err != nil {
		t.Fatal(err)
	}

	occ := uuid.New()
	if _, err := st.ReconcileAdmission(ctx, s.reconcileInput(occ, deviceTime())); err == nil {
		t.Fatal("reconcile succeeded while the alarm insert was rejected; the alarm is owed, not best-effort")
	}
	if n := countEvents(t, ctx, db, s.ticketID, "redeemed"); n != 0 {
		t.Fatalf("%d redeemed rows committed while its owed alarm failed — the append and the alarm are one transaction", n)
	}
	if n := conflictAlarmCount(t, ctx, db); n != 0 {
		t.Fatalf("%d alarms committed, want 0", n)
	}
	// The fact that preceded the failed reconcile is untouched.
	if n := countEvents(t, ctx, db, s.ticketID, "exchanged"); n != 1 {
		t.Fatalf("%d exchanged rows, want 1 — a rolled-back reconcile must not disturb it", n)
	}
}

// A reconciliation-learned occurrence replays as ACCEPTED, not as a degraded admission
// (TKT-299).
//
// The two kinds of quarantine row mean different things and only one of them records a
// live admission decision:
//
//   - `admitted_at` SET — ADR-021 §D6 let someone through on a chain that did not verify.
//   - `admitted_at` NULL — reconciliation recorded that an occurrence physically happened
//     offline. ADR-025 §D2: reconciliation is *recording, not deciding*. Nothing was
//     admitted; the row exists because the broken chain refused an append.
//
// `replayByOccurrence` never read `event_type` and labelled BOTH `DecisionAdmittedDegraded`,
// while its two siblings (`replayAdmissionOccurrence`, `reconcileReplay`) distinguish them.
// Three near-identical helpers, one of them wrong.
//
// Impact is honestly internal today: accepted responses do not carry `Decision` on the wire,
// so nothing external changed. It matters because the label is what any later caller or
// operator view would branch on, and because a degraded admission is a §D6 event that owes
// an alarm — a fact this row is not.
func TestReconciledOccurrenceReplaysAcceptedNotDegraded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)
	s := issueTicket(t, ctx, st, uuid.New())
	genuine := genuineHash(t, ctx, db, s.ticketID)

	// Offline occurrence reconciled while the chain is broken: recorded quarantine-side
	// with admitted_at NULL, because appending onto an unverified predecessor would poison
	// the chain.
	corruptChain(t, ctx, db, s.ticketID)
	occ := uuid.New()
	if _, err := st.ReconcileAdmission(ctx, s.reconcileInput(occ, deviceTime())); err != nil {
		t.Fatal(err)
	}
	// The fixture, asserted rather than assumed: exactly one quarantine row, and its
	// admitted_at is NULL. If reconciliation ever started setting it, this test would still
	// pass while testing the other case entirely.
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE ticket_id=$1 AND admitted_at IS NULL`, s.ticketID); n != 1 {
		t.Fatalf("reconciliation-learned rows with a NULL admitted_at = %d, want 1 — the fixture does not hold", n)
	}

	repairChain(t, ctx, db, s.ticketID, genuine)
	retry, err := st.Redeem(ctx, occurrenceRedeemInput(s, occ))
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Accepted || !retry.Replayed {
		t.Fatalf("retry = %+v, want the reconciled occurrence replayed", retry)
	}
	if retry.Decision != DecisionAccepted {
		t.Fatalf("decision = %q, want %q: nobody was admitted on a broken chain here — "+
			"reconciliation RECORDED an occurrence that already happened (ADR-025 §D2), and "+
			"labelling it a degraded admission claims a §D6 event that never occurred",
			retry.Decision, DecisionAccepted)
	}
}

// Reconciliation's prior-admission check must consult the UNION too (TKT-299).
//
// ADR-025 §D2 says admission history is the union of the trace and the quarantine record,
// and that *admission decisions* — not only readers — must consult it. Reconciliation's
// "has this ticket already been admitted?" lookup read `lifecycle_events` alone, so a prior
// §D6 degraded admission was invisible to it.
//
// The consequence is not a caught error: the singleton index on `redeemed` never fires,
// because the degraded admission left NO `redeemed` row to collide with. Reconciliation
// therefore concluded "no prior admission", appended a fresh `redeemed`, and returned
// `recorded`. One physical person, admitted once at the gate and once again in the record,
// with no conflict alarm raised for the second.
//
// The rule this asserts, stated without naming the implementation: a ticket admitted once —
// by ANY route the system records — and then reconciled for a DIFFERENT occurrence has a
// conflict, and the second occurrence is recorded as a refused duplicate, never as the
// redemption. The expected values below come from that sentence, not from a run.
func TestReconcileAfterADegradedAdmissionIsAConflict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)
	s := issueTicket(t, ctx, st, uuid.New())
	genuine := genuineHash(t, ctx, db, s.ticketID)

	// The holder goes through the door on a chain that does not verify: ADR-021 §D6 admits
	// once, recorded ONLY on the quarantine side.
	corruptChain(t, ctx, db, s.ticketID)
	first, err := st.Redeem(ctx, occurrenceRedeemInput(s, uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Accepted || first.Decision != DecisionAdmittedDegraded {
		t.Fatalf("fixture: first scan = %+v, want a degraded admission", first)
	}
	repairChain(t, ctx, db, s.ticketID, genuine)

	// The fixture, asserted: the admission exists only in quarantine, so a trail-only
	// lookup is genuinely blind to it here.
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, s.ticketID); n != 0 {
		t.Fatalf("trail redemptions = %d, want 0 — the fixture does not exercise the union", n)
	}
	degradedAlarms := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox`)

	// A DIFFERENT occurrence arrives from an offline scanner.
	result, err := st.ReconcileAdmission(ctx, s.reconcileInput(uuid.New(), deviceTime().Add(5*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ReconcileConflict {
		t.Fatalf("outcome = %v, want %v: the holder was already admitted (quarantine-side), so a "+
			"second physical occurrence is a conflict — recording it as the redemption gives one "+
			"person two admissions and no alarm", result.Outcome, ReconcileConflict)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, s.ticketID); n != 0 {
		t.Fatalf("redeemed events = %d, want 0: the redemption already happened at the gate and "+
			"lives in quarantine; reconciliation must not mint a second admission record", n)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='duplicate_admit'`, s.ticketID); n != 1 {
		t.Fatalf("duplicate_admit events = %d, want 1: the second occurrence is a refused duplicate", n)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox`); n != degradedAlarms+1 {
		t.Fatalf("alarms = %d, want %d: one physical conflict owes exactly one conflict alarm, "+
			"on top of the integrity alarm the degraded admission already owed", n, degradedAlarms+1)
	}
}

// A `duplicate_admit` occurrence must never replay as an accepted admission (TKT-299
// ai-review, second pass).
//
// Reconciliation appends `duplicate_admit` with the scanner's own occurrence id when an
// offline occurrence arrives for a ticket that was already admitted: the record says "this
// scan was refused". Retrying that same occurrence at a live gate must not come back
// `Accepted: true` — the API reads that as permission to open the door, and nobody was ever
// admitted on it.
//
// The regression this pins is specific and was self-inflicted. Binding the replay to a
// direction (the previous review round's fix) was done by copying the matcher from
// `replayAdmissionOccurrence`, which accepts `duplicate_admit` as an entry — correctly, for
// the pass path, where a refused duplicate is still a physical entry that consumed
// allowance. On THIS path the result becomes the gate's decision, so the two matchers must
// differ. Both chain states are covered because the healthy path and the degraded path
// reach the replay through different callers.
func TestDuplicateAdmitOccurrenceNeverReplaysAsAccepted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		break_ bool
	}{
		{name: "healthy chain"},
		{name: "broken chain", break_: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			db := migratedDB(t, ctx)
			st := New(db, testConfig(t))
			s := issueTicket(t, ctx, st, uuid.New())

			if _, err := st.Redeem(ctx, occurrenceRedeemInput(s, uuid.New())); err != nil {
				t.Fatal(err)
			}
			// A late offline occurrence for an already-admitted ticket: recorded as a
			// refused duplicate, carrying the scanner's occurrence id.
			dup := uuid.New()
			res, err := st.ReconcileAdmission(ctx, s.reconcileInput(dup, deviceTime().Add(5*time.Minute)))
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != ReconcileConflict {
				t.Fatalf("fixture: reconcile outcome = %v, want %v", res.Outcome, ReconcileConflict)
			}
			if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE id=$1 AND event_type='duplicate_admit'`, dup); n != 1 {
				t.Fatalf("fixture: duplicate_admit rows for the occurrence = %d, want 1", n)
			}
			if tc.break_ {
				corruptChain(t, ctx, db, s.ticketID)
			}

			// Asserted to the EXACT outcome, not merely "not accepted". A test satisfied by
			// any error blesses whatever the code happens to do — including an unrelated
			// failure — and the third ai-review pass caught this assertion doing exactly
			// that in its first form.
			//
			// The outcome pinned here is `origin/main`'s, unchanged by this ticket: a
			// duplicate_admit occurrence is answered ErrOccurrenceCollision, which the API
			// maps to 422. That is a deliberate pre-existing choice, documented at the
			// switch this ticket replaced — "its original outcome was a conflict recording,
			// not an acceptance this result shape can honestly replay". Whether a distinct
			// non-accepted decision would serve a scanner better is a real question and
			// TKT-299 does not own it; what this test guarantees is that the answer never
			// silently becomes "accepted".
			out, err := st.Redeem(ctx, occurrenceRedeemInput(s, dup))
			if !errors.Is(err, ErrOccurrenceCollision) {
				t.Fatalf("err = %v, want ErrOccurrenceCollision — a REFUSED duplicate must not "+
					"replay as an admission, and must fail for THAT reason rather than any other",
					err)
			}
			if out.Accepted {
				t.Fatalf("a REFUSED duplicate replayed as an accepted admission (decision=%q): "+
					"its original outcome was a conflict recording, and the gate must not open "+
					"on it", out.Decision)
			}
			if out.Decision != "" || out.Replayed || !out.OccurredAt.IsZero() {
				t.Fatalf("a refused replay must return the zero result, got %+v", out)
			}
		})
	}
}
