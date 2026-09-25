package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"ticketing/shared/domainevent"
)

// The three alarm classes use the shared platform envelope (ADR-033). Each
// service keeps its payload type. Integrity alarms use schema 2 because their
// reason field now carries a fixed code (ADR-017 §3). The goldens in
// alarm_envelope_test.go pin the wire bytes.

func policyConflictAlarmEnvelope(id uuid.UUID, occurred time.Time, data policyConflictAlarmData) ([]byte, error) {
	return json.Marshal(domainevent.Envelope[policyConflictAlarmData]{
		ID: id, Type: SubjectAdmissionPolicyConflictAlarm, OccurredAt: occurred, Schema: 1, Data: data,
	})
}

func integrityAlarmEnvelope(id uuid.UUID, occurred time.Time, data alarmData) ([]byte, error) {
	if err := validateAlarmReason(data.Reason); err != nil {
		return nil, err
	}
	return json.Marshal(domainevent.Envelope[alarmData]{
		ID: id, Type: SubjectIntegrityAlarm, OccurredAt: occurred, Schema: 2, Data: data,
	})
}

func admissionConflictAlarmEnvelope(id uuid.UUID, occurred time.Time, data conflictAlarmData) ([]byte, error) {
	return json.Marshal(domainevent.Envelope[conflictAlarmData]{
		ID: id, Type: SubjectAdmissionConflictAlarm, OccurredAt: occurred, Schema: 1, Data: data,
	})
}
