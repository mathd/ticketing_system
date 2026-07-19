package store

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func i32(v int32) *int32 { return &v }

// fact builds an AdmissionFact at a claimed physical time offset (minutes).
func fact(t AdmissionEventType, minute int, seq int64) AdmissionFact {
	return AdmissionFact{
		OccurrenceID: uuid.New(),
		Type:         t,
		OccurredAt:   time.Date(2026, 7, 19, 12, minute, 0, 0, time.UTC),
		Sequence:     seq,
	}
}

func TestDerivePolicyConflictsRequiresExit(t *testing.T) {
	policy := ReEntryPolicy{Mode: "multi", RequiresExit: true}

	entry1 := fact(AdmissionEntry, 0, 1)
	entry2 := fact(AdmissionEntry, 10, 2)
	conflicts := DerivePolicyConflicts(policy, []AdmissionFact{entry1, entry2})
	if len(conflicts) != 1 || conflicts[0].Rule != ConflictExitRequired || conflicts[0].OccurrenceID != entry2.OccurrenceID {
		t.Fatalf("conflicts = %+v, want one exit_required on the second entry", conflicts)
	}

	// The §D2 case that forbids trail-minted conflicts: an exit at gate A
	// syncs AFTER the re-entry at gate B but claims an earlier physical time.
	// Re-evaluation must order by claimed time — the conflict is withdrawn.
	lateExit := fact(AdmissionExit, 5, 3) // synced last (highest sequence), physically between the entries
	conflicts = DerivePolicyConflicts(policy, []AdmissionFact{entry1, entry2, lateExit})
	if len(conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none after the late cross-device exit arrives", conflicts)
	}
}

func TestDerivePolicyConflictsCountLimited(t *testing.T) {
	policy := ReEntryPolicy{Mode: "count_limited", MaxEntries: i32(2)}
	e1, e2, e3 := fact(AdmissionEntry, 0, 1), fact(AdmissionEntry, 10, 2), fact(AdmissionEntry, 20, 3)
	x1 := fact(AdmissionExit, 5, 4)

	conflicts := DerivePolicyConflicts(policy, []AdmissionFact{e1, e2})
	if len(conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none at the limit", conflicts)
	}
	// Exits never restore the allowance: max_entries counts lifetime entries.
	conflicts = DerivePolicyConflicts(policy, []AdmissionFact{e1, x1, e2, e3})
	if len(conflicts) != 1 || conflicts[0].Rule != ConflictEntryLimitReached || conflicts[0].OccurrenceID != e3.OccurrenceID {
		t.Fatalf("conflicts = %+v, want one entry_limit_reached on the third entry (physically ordered)", conflicts)
	}
}

func TestDerivePolicyConflictsCountsDegradedAdmission(t *testing.T) {
	// A live degraded admission (ADR-021 §D6) is an un-directioned
	// entry-equivalent in the union: it consumes allowance.
	policy := ReEntryPolicy{Mode: "count_limited", MaxEntries: i32(1)}
	degraded := AdmissionFact{OccurrenceID: uuid.New(), Type: AdmissionEntry, OccurredAt: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC), DegradedAdmission: true}
	e2 := fact(AdmissionEntry, 10, 1)
	conflicts := DerivePolicyConflicts(policy, []AdmissionFact{degraded, e2})
	if len(conflicts) != 1 || conflicts[0].OccurrenceID != e2.OccurrenceID {
		t.Fatalf("conflicts = %+v, want the post-degraded entry over the limit", conflicts)
	}
}

func TestDerivePolicyConflictsIsStable(t *testing.T) {
	// Same facts, any arrival order, equal timestamps: identical conflict set
	// (deterministic tie-breaks), so re-evaluation is idempotent.
	policy := ReEntryPolicy{Mode: "multi", RequiresExit: true}
	e1 := fact(AdmissionEntry, 0, 1)
	e2 := fact(AdmissionEntry, 0, 2) // same claimed minute — sequence breaks the tie
	a := DerivePolicyConflicts(policy, []AdmissionFact{e1, e2})
	b := DerivePolicyConflicts(policy, []AdmissionFact{e2, e1})
	if len(a) != 1 || len(b) != 1 || a[0] != b[0] {
		t.Fatalf("unstable derivation: %+v vs %+v", a, b)
	}
	if a[0].OccurrenceID != e2.OccurrenceID {
		t.Fatalf("tie broken toward %v, want the higher-sequence entry %v", a[0].OccurrenceID, e2.OccurrenceID)
	}
}

func TestEvaluateLiveAdmission(t *testing.T) {
	multiExit := ReEntryPolicy{Mode: "multi", RequiresExit: true}
	counted := ReEntryPolicy{Mode: "count_limited", MaxEntries: i32(2)}
	entryIn := []AdmissionFact{fact(AdmissionEntry, 0, 1)}
	balanced := []AdmissionFact{fact(AdmissionEntry, 0, 1), fact(AdmissionExit, 5, 2)}
	atLimit := []AdmissionFact{fact(AdmissionEntry, 0, 1), fact(AdmissionExit, 5, 2), fact(AdmissionEntry, 10, 3)}

	tests := []struct {
		name   string
		policy ReEntryPolicy
		facts  []AdmissionFact
		dir    AdmissionEventType
		want   Decision
	}{
		{"multi entry accepted", ReEntryPolicy{Mode: "multi"}, entryIn, AdmissionEntry, DecisionAccepted},
		{"requires_exit blocks re-entry while inside", multiExit, entryIn, AdmissionEntry, DecisionExitRequired},
		{"requires_exit re-entry after exit accepted", multiExit, balanced, AdmissionEntry, DecisionAccepted},
		{"exit while inside accepted", multiExit, entryIn, AdmissionExit, DecisionAccepted},
		{"exit while outside denied", multiExit, balanced, AdmissionExit, DecisionNotInside},
		{"exit with no history denied", multiExit, nil, AdmissionExit, DecisionNotInside},
		{"count under limit accepted", counted, entryIn, AdmissionEntry, DecisionAccepted},
		{"count at limit denied, exits restore nothing", counted, atLimit, AdmissionEntry, DecisionEntryLimitReached},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EvaluateLiveAdmission(tt.policy, tt.facts, tt.dir); got != tt.want {
				t.Fatalf("EvaluateLiveAdmission = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestDiffPolicyConflicts(t *testing.T) {
	e1, e2 := uuid.New(), uuid.New()
	derived := []PolicyConflict{
		{Rule: ConflictExitRequired, OccurrenceID: e1},
		{Rule: ConflictEntryLimitReached, OccurrenceID: e2},
	}
	current := map[PolicyConflict]string{
		{Rule: ConflictExitRequired, OccurrenceID: e1}:      "raised",    // unchanged: no transition owed
		{Rule: ConflictExitRequired, OccurrenceID: uuid.New()}: "raised", // no longer derived: withdraw
		{Rule: ConflictEntryLimitReached, OccurrenceID: e2}: "withdrawn", // re-derived after withdrawal: re-raise
	}
	raises, withdraws := DiffPolicyConflicts(current, derived)
	if len(raises) != 1 || raises[0].OccurrenceID != e2 {
		t.Fatalf("raises = %+v, want exactly the re-raised limit conflict", raises)
	}
	if len(withdraws) != 1 || withdraws[0].Rule != ConflictExitRequired {
		t.Fatalf("withdraws = %+v, want exactly the stale exit_required", withdraws)
	}
}
