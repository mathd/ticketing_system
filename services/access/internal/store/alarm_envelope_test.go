package store

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// Wire-compatibility goldens for the three alarm classes (TKT-126).
//
// The integrity alarm golden pins schema 2, which added the stable reason and
// disposition fields. The fixture is a literal rather than a value generated
// from the envelope type, so a change to the type cannot update the expected
// bytes at the same time. The other two alarm goldens pin their current schema 1
// envelopes.
//
// These rows land in `lifecycle_integrity_alarm_outbox` and are part of the
// access integrity record (ADR-021). ADR-025 §D9 (amended TKT-119) constrains
// the alarm payload to bounded identifiers, enums and operational scalars.

var (
	alarmGoldID           = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	alarmGoldOrganizerID  = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	alarmGoldTicketID     = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	alarmGoldSlotID       = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	alarmGoldOccurrenceID = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	alarmGoldConflictID   = uuid.MustParse("66666666-6666-4666-8666-666666666666")

	alarmGoldOccurred = time.Date(2026, 7, 20, 12, 34, 56, 123456789, time.UTC)
)

func TestWireGoldenPolicyConflictAlarm(t *testing.T) {
	body, err := policyConflictAlarmEnvelope(alarmGoldID, alarmGoldOccurred, policyConflictAlarmData{
		AlarmID: alarmGoldID, ConflictID: alarmGoldConflictID, OrganizerID: alarmGoldOrganizerID,
		TicketID: alarmGoldTicketID, SlotID: alarmGoldSlotID, Rule: "exit_required",
		OccurrenceID: alarmGoldOccurrenceID, Status: "raised", Version: 2, Revisable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"11111111-1111-4111-8111-111111111111","type":"platform.access.admission-policy-conflict.alarm","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":1,"data":{"alarm_id":"11111111-1111-4111-8111-111111111111","conflict_id":"66666666-6666-4666-8666-666666666666","organizer_id":"22222222-2222-4222-8222-222222222222","ticket_id":"33333333-3333-4333-8333-333333333333","slot_id":"44444444-4444-4444-8444-444444444444","rule":"exit_required","occurrence_id":"55555555-5555-4555-8555-555555555555","status":"raised","version":2,"revisable":true}}`
	assertAlarmGolden(t, want, body)
}

func TestWireGoldenIntegrityAlarm(t *testing.T) {
	body, err := integrityAlarmEnvelope(alarmGoldID, alarmGoldOccurred, alarmData{
		AlarmID: alarmGoldID, OrganizerID: alarmGoldOrganizerID, TicketID: alarmGoldTicketID,
		Reason: AlarmReasonHeadMismatch, Disposition: string(DecisionAdmittedDegraded),
		Mode: string(ModeNormal), OccurredAt: alarmGoldOccurred,
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"11111111-1111-4111-8111-111111111111","type":"platform.access.lifecycle-integrity.alarm","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":2,"data":{"alarm_id":"11111111-1111-4111-8111-111111111111","organizer_id":"22222222-2222-4222-8222-222222222222","ticket_id":"33333333-3333-4333-8333-333333333333","reason":"head_mismatch","disposition":"admitted_degraded","mode":"normal","occurred_at":"2026-07-20T12:34:56.123456789Z"}}`
	assertAlarmGolden(t, want, body)
}

func TestWireGoldenAdmissionConflictAlarm(t *testing.T) {
	body, err := admissionConflictAlarmEnvelope(alarmGoldID, alarmGoldOccurred, conflictAlarmData{
		AlarmID: alarmGoldID, OrganizerID: alarmGoldOrganizerID, TicketID: alarmGoldTicketID,
		OccurrenceID: alarmGoldOccurrenceID, DeviceOccurredAt: alarmGoldOccurred, SkewFlagged: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"11111111-1111-4111-8111-111111111111","type":"platform.access.admission-conflict.alarm","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":1,"data":{"alarm_id":"11111111-1111-4111-8111-111111111111","organizer_id":"22222222-2222-4222-8222-222222222222","ticket_id":"33333333-3333-4333-8333-333333333333","occurrence_id":"55555555-5555-4555-8555-555555555555","device_occurred_at":"2026-07-20T12:34:56.123456789Z","skew_flagged":true}}`
	assertAlarmGolden(t, want, body)
}

func assertAlarmGolden(t *testing.T, want string, got []byte) {
	t.Helper()
	if string(got) != want {
		t.Fatalf("alarm wire bytes changed (TKT-126 forbids it)\n got: %s\nwant: %s", got, want)
	}
}
