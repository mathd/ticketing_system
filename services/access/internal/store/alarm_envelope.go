package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// The three alarm classes each marshalled their envelope inline, as an anonymous
// struct re-declaring the platform envelope fields. They are extracted here so
// the wire bytes have a pure seam a golden test can pin, and so the envelope
// shape lives in one place per package rather than three.

func policyConflictAlarmEnvelope(id uuid.UUID, occurred time.Time, data policyConflictAlarmData) ([]byte, error) {
	return json.Marshal(struct {
		ID         uuid.UUID               `json:"id"`
		Type       string                  `json:"type"`
		OccurredAt time.Time               `json:"occurred_at"`
		Schema     int                     `json:"schema"`
		Data       policyConflictAlarmData `json:"data"`
	}{ID: id, Type: SubjectAdmissionPolicyConflictAlarm, OccurredAt: occurred, Schema: 1, Data: data})
}

func integrityAlarmEnvelope(id uuid.UUID, occurred time.Time, data alarmData) ([]byte, error) {
	return json.Marshal(struct {
		ID         uuid.UUID `json:"id"`
		Type       string    `json:"type"`
		OccurredAt time.Time `json:"occurred_at"`
		Schema     int       `json:"schema"`
		Data       alarmData `json:"data"`
	}{ID: id, Type: SubjectIntegrityAlarm, OccurredAt: occurred, Schema: 1, Data: data})
}

func admissionConflictAlarmEnvelope(id uuid.UUID, occurred time.Time, data conflictAlarmData) ([]byte, error) {
	return json.Marshal(struct {
		ID         uuid.UUID         `json:"id"`
		Type       string            `json:"type"`
		OccurredAt time.Time         `json:"occurred_at"`
		Schema     int               `json:"schema"`
		Data       conflictAlarmData `json:"data"`
	}{ID: id, Type: SubjectAdmissionConflictAlarm, OccurredAt: occurred, Schema: 1, Data: data})
}
