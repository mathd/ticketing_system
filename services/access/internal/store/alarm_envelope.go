package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"ticketing/shared/domainevent"
)

// The three alarm classes each marshalled their envelope inline, as an anonymous
// struct re-declaring the platform envelope fields. They now route through the
// shared declaration (ADR-033); the per-alarm payload types stay here, in the
// service that owns them, and are untouched -- ADR-025 §D9 constrains those
// payloads, and this ticket rewraps the envelope, nothing else. The goldens in
// alarm_envelope_test.go are what make that claim checkable.

func policyConflictAlarmEnvelope(id uuid.UUID, occurred time.Time, data policyConflictAlarmData) ([]byte, error) {
	return json.Marshal(domainevent.Envelope[policyConflictAlarmData]{
		ID: id, Type: SubjectAdmissionPolicyConflictAlarm, OccurredAt: occurred, Schema: 1, Data: data,
	})
}

func integrityAlarmEnvelope(id uuid.UUID, occurred time.Time, data alarmData) ([]byte, error) {
	return json.Marshal(domainevent.Envelope[alarmData]{
		ID: id, Type: SubjectIntegrityAlarm, OccurredAt: occurred, Schema: 1, Data: data,
	})
}

func admissionConflictAlarmEnvelope(id uuid.UUID, occurred time.Time, data conflictAlarmData) ([]byte, error) {
	return json.Marshal(domainevent.Envelope[conflictAlarmData]{
		ID: id, Type: SubjectAdmissionConflictAlarm, OccurredAt: occurred, Schema: 1, Data: data,
	})
}
