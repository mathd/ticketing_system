package store

import (
	"sort"
	"time"

	"github.com/google/uuid"
)

// SubjectAdmissionPolicyConflictAlarm carries derived pass-policy conflicts
// (ADR-025 §D2). Deliberately a separate class from SubjectAdmissionConflictAlarm:
// that one says "the immutable trail minted duplicate_admit — final"; this one
// says "a projection currently holds — revisable". Each alarm is a raise or a
// withdrawal of one conflict, sharing a deterministic id so consumers upsert.
// Its own schema-1 contract, following ADR-017 thereafter.
const SubjectAdmissionPolicyConflictAlarm = "platform.access.admission-policy-conflict.alarm"

// ReEntryPolicy is access's projection of ADR-005's slot re_entry_policy.
// Catalog owns it; access enforces it at the gate. The zero value is not valid —
// resolve through PolicyForSlot, whose absent-row answer is explicit single.
type ReEntryPolicy struct {
	Mode         string // single | multi | count_limited
	MaxEntries   *int32 // set iff Mode == count_limited
	RequiresExit bool
}

// IsPass reports whether the policy admits through the repeatable entry/exit
// vocabulary rather than the singleton redemption (ADR-025 §D1).
func (p ReEntryPolicy) IsPass() bool { return p.Mode == "multi" || p.Mode == "count_limited" }

// PolicyConflictRule is a derived pass-policy violation class (ADR-025 §D2).
type PolicyConflictRule string

const (
	ConflictEntryLimitReached PolicyConflictRule = "entry_limit_reached"
	ConflictExitRequired      PolicyConflictRule = "exit_required"
)

// PolicyConflict identifies one derived conflict: the rule plus the offending
// occurrence. Per-occurrence identity is what keeps withdrawal targeted — a
// set-keyed identity would orphan its alarm the moment the set changed.
type PolicyConflict struct {
	Rule         PolicyConflictRule
	OccurrenceID uuid.UUID
}

// AdmissionFact is one physical admission the union (trace ∪ quarantine) knows
// about, reduced to what policy evaluation needs. DegradedAdmission marks a
// live §D6 fail-open admission: an un-directioned entry-equivalent — it
// consumes allowance and sets "inside" exactly as an entry does.
type AdmissionFact struct {
	OccurrenceID      uuid.UUID
	Type              AdmissionEventType // entry | exit
	OccurredAt        time.Time
	Sequence          int64 // integrity sequence; 0 for quarantine-side facts
	DegradedAdmission bool
}

// orderFacts sorts by claimed physical time, then integrity sequence, then
// occurrence id. Claimed-time-first is load-bearing (ADR-025 §D2/§D5): the
// integrity sequence is append/reconciliation order, and cross-device sync
// order is not physical order — an exit at gate A syncing after a re-entry at
// gate B must still slot between the entries, or its conflict could never be
// withdrawn. Device time is claimed, not attested; that is why the projection
// stays revisable instead of ever reaching the trail.
func orderFacts(facts []AdmissionFact) []AdmissionFact {
	ordered := append([]AdmissionFact(nil), facts...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if !a.OccurredAt.Equal(b.OccurredAt) {
			return a.OccurredAt.Before(b.OccurredAt)
		}
		if a.Sequence != b.Sequence {
			return a.Sequence < b.Sequence
		}
		return a.OccurrenceID.String() < b.OccurrenceID.String()
	})
	return ordered
}

// DerivePolicyConflicts recomputes the full conflict set for one ticket from
// scratch (ADR-025 §D2: derived, revisable, re-evaluated as late events
// arrive). Pure — the same facts always derive the same set, so re-evaluation
// is idempotent by construction.
func DerivePolicyConflicts(policy ReEntryPolicy, facts []AdmissionFact) []PolicyConflict {
	if !policy.IsPass() {
		return nil
	}
	var conflicts []PolicyConflict
	entries := 0
	inside := false
	for _, f := range orderFacts(facts) {
		switch {
		case f.Type == AdmissionExit:
			inside = false
			continue
		case f.DegradedAdmission, f.Type == AdmissionEntry:
			entries++
			if policy.Mode == "count_limited" && policy.MaxEntries != nil && entries > int(*policy.MaxEntries) {
				conflicts = append(conflicts, PolicyConflict{Rule: ConflictEntryLimitReached, OccurrenceID: f.OccurrenceID})
			} else if policy.RequiresExit && inside && !f.DegradedAdmission {
				conflicts = append(conflicts, PolicyConflict{Rule: ConflictExitRequired, OccurrenceID: f.OccurrenceID})
			}
			inside = true
		}
	}
	return conflicts
}

// EvaluateLiveAdmission answers a live gate decision for a pass ticket from
// the same ordered-facts basis the conflict projection uses — one rule copy,
// shared, so the live verdict and the derived projection cannot drift apart.
// Live denials append nothing; that is what keeps them free to be wrong when a
// late cross-device fact arrives (the reconciled fact is recorded and the
// derived projection re-evaluates — ADR-025 §D2).
func EvaluateLiveAdmission(policy ReEntryPolicy, facts []AdmissionFact, direction AdmissionEventType) Decision {
	entries := 0
	inside := false
	for _, f := range orderFacts(facts) {
		if f.Type == AdmissionExit {
			inside = false
			continue
		}
		entries++
		inside = true
	}
	if direction == AdmissionExit {
		if !inside {
			return DecisionNotInside
		}
		return DecisionAccepted
	}
	if policy.Mode == "count_limited" && policy.MaxEntries != nil && entries >= int(*policy.MaxEntries) {
		return DecisionEntryLimitReached
	}
	if policy.RequiresExit && inside {
		return DecisionExitRequired
	}
	return DecisionAccepted
}

// DiffPolicyConflicts compares the freshly derived set against the stored
// projection state and returns the transitions owed: raises for conflicts that
// are derived but not currently raised (new, or previously withdrawn), and
// withdrawals for raised conflicts no longer derived. Unchanged conflicts owe
// nothing — that is what makes replayed evaluation alarm-silent.
func DiffPolicyConflicts(current map[PolicyConflict]string, derived []PolicyConflict) (raises, withdraws []PolicyConflict) {
	derivedSet := make(map[PolicyConflict]bool, len(derived))
	for _, c := range derived {
		derivedSet[c] = true
		if current[c] != "raised" {
			raises = append(raises, c)
		}
	}
	for c, status := range current {
		if status == "raised" && !derivedSet[c] {
			withdraws = append(withdraws, c)
		}
	}
	return raises, withdraws
}
