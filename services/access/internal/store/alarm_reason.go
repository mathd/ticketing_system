package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// AlarmReason is a stable, finite integrity-alarm reason.
type AlarmReason string

const (
	AlarmReasonMissingIntegrityRow         AlarmReason = "missing_integrity_row"
	AlarmReasonUnsupportedCanonicalVersion AlarmReason = "unsupported_canonical_version"
	AlarmReasonSequenceGap                 AlarmReason = "sequence_gap"
	AlarmReasonBrokenChainLink             AlarmReason = "broken_chain_link"
	AlarmReasonEntryHashMismatch           AlarmReason = "entry_hash_mismatch"
	AlarmReasonOrphanIntegrityRow          AlarmReason = "orphan_integrity_row"
	AlarmReasonMissingHead                 AlarmReason = "missing_head"
	AlarmReasonHeadWithoutEvents           AlarmReason = "head_without_events"
	AlarmReasonHeadMismatch                AlarmReason = "head_mismatch"
	AlarmReasonMissingKeyring              AlarmReason = "missing_keyring"
	AlarmReasonUnknownKey                  AlarmReason = "unknown_key"
	AlarmReasonInvalidHeadSignature        AlarmReason = "invalid_head_signature"
	AlarmReasonVerificationUnavailable     AlarmReason = "verification_unavailable"
	AlarmReasonVerificationUnclassified    AlarmReason = "verification_unclassified"
	AlarmReasonLegacyQuarantine            AlarmReason = "legacy_quarantine"
)

var alarmReasons = []AlarmReason{
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

type verifierError struct {
	code AlarmReason
	err  error
}

func (e *verifierError) Error() string { return e.err.Error() }
func (e *verifierError) Unwrap() error { return e.err }

func verificationFailure(code AlarmReason, err error) error {
	return &verifierError{code: code, err: err}
}

func unavailableVerification(err error) error {
	return verificationFailure(AlarmReasonVerificationUnavailable, err)
}

func alarmReasonFor(err error) AlarmReason {
	var verification *verifierError
	if errors.As(err, &verification) {
		if validateAlarmReason(verification.code) == nil {
			return verification.code
		}
	}
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		return AlarmReasonVerificationUnavailable
	}
	return AlarmReasonVerificationUnclassified
}

func validateAlarmReason(reason AlarmReason) error {
	for _, allowed := range alarmReasons {
		if reason == allowed {
			return nil
		}
	}
	return fmt.Errorf("invalid lifecycle integrity alarm reason %q", reason)
}

func alarmReasonVocabulary() []AlarmReason { return append([]AlarmReason(nil), alarmReasons...) }

func sameAlarmReasons(a, b []AlarmReason) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[AlarmReason]int, len(a))
	for _, reason := range a {
		seen[reason]++
	}
	for _, reason := range b {
		seen[reason]--
	}
	for _, count := range seen {
		if count != 0 {
			return false
		}
	}
	return true
}
