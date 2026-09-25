package store

import (
	"database/sql"
	"errors"
	"testing"
)

func TestAlarmReasonFor(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want AlarmReason
	}{
		{"missing integrity row", &verifierError{code: AlarmReasonMissingIntegrityRow, err: errors.New("detail")}, AlarmReasonMissingIntegrityRow},
		{"unsupported canonical version", &verifierError{code: AlarmReasonUnsupportedCanonicalVersion, err: errors.New("detail")}, AlarmReasonUnsupportedCanonicalVersion},
		{"sequence gap", &verifierError{code: AlarmReasonSequenceGap, err: errors.New("detail")}, AlarmReasonSequenceGap},
		{"broken chain link", &verifierError{code: AlarmReasonBrokenChainLink, err: errors.New("detail")}, AlarmReasonBrokenChainLink},
		{"entry hash mismatch", &verifierError{code: AlarmReasonEntryHashMismatch, err: errors.New("detail")}, AlarmReasonEntryHashMismatch},
		{"orphan integrity row", &verifierError{code: AlarmReasonOrphanIntegrityRow, err: errors.New("detail")}, AlarmReasonOrphanIntegrityRow},
		{"missing head", &verifierError{code: AlarmReasonMissingHead, err: errors.New("detail")}, AlarmReasonMissingHead},
		{"head without events", &verifierError{code: AlarmReasonHeadWithoutEvents, err: errors.New("detail")}, AlarmReasonHeadWithoutEvents},
		{"head mismatch", &verifierError{code: AlarmReasonHeadMismatch, err: errors.New("detail")}, AlarmReasonHeadMismatch},
		{"missing keyring", &verifierError{code: AlarmReasonMissingKeyring, err: errors.New("detail")}, AlarmReasonMissingKeyring},
		{"unknown key", &verifierError{code: AlarmReasonUnknownKey, err: errors.New("detail")}, AlarmReasonUnknownKey},
		{"invalid head signature", &verifierError{code: AlarmReasonInvalidHeadSignature, err: errors.New("detail")}, AlarmReasonInvalidHeadSignature},
		{"query failure", sql.ErrConnDone, AlarmReasonVerificationUnavailable},
		{"unclassified verifier failure", errors.New("private diagnostic"), AlarmReasonVerificationUnclassified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := alarmReasonFor(tt.err); got != tt.want {
				t.Fatalf("alarmReasonFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAlarmReasonVocabulary(t *testing.T) {
	want := []AlarmReason{
		AlarmReasonMissingIntegrityRow,
		AlarmReasonUnsupportedCanonicalVersion,
		AlarmReasonSequenceGap,
		AlarmReasonBrokenChainLink,
		AlarmReasonEntryHashMismatch,
		AlarmReasonOrphanIntegrityRow,
		AlarmReasonMissingHead,
		AlarmReasonHeadWithoutEvents,
		AlarmReasonHeadMismatch,
		AlarmReasonMissingKeyring,
		AlarmReasonUnknownKey,
		AlarmReasonInvalidHeadSignature,
		AlarmReasonVerificationUnavailable,
		AlarmReasonVerificationUnclassified,
		AlarmReasonLegacyQuarantine,
	}
	if got := alarmReasonVocabulary(); !sameAlarmReasons(got, want) {
		t.Fatalf("alarm reason vocabulary = %v, want %v", got, want)
	}
}

func TestInvalidAlarmReasonIsRejected(t *testing.T) {
	if err := validateAlarmReason(AlarmReason("private diagnostic")); err == nil {
		t.Fatal("validateAlarmReason accepted an unknown value")
	}
}

func TestVerifierErrorKeepsOriginalText(t *testing.T) {
	const detail = "sequence gap at 4 (expected 3) on ticket 123"
	err := &verifierError{code: AlarmReasonSequenceGap, err: errors.New(detail)}
	if got := err.Error(); got != detail {
		t.Fatalf("Error() = %q, want %q", got, detail)
	}
	if !errors.Is(err, err.err) {
		t.Fatal("verifierError does not unwrap its diagnostic")
	}
}
