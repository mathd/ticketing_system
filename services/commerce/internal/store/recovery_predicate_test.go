package store

import (
	"strings"
	"testing"
)

// TKT-321 COS2, query construction.
//
// Both composed statements must reference the shared predicate constant exactly once. This checks
// the SQL text, and only that. A reference replaced by an identical literal would still pass it,
// so this is not proof that the two queries agree. The behavioural membership tests in
// recovery_eligible_smoke_test.go are the evidence that they select the same rows.
func TestClaimAndEligibleQueriesComposeTheSharedPredicate(t *testing.T) {
	// strings.Count with an empty needle returns the rune count plus one, so an empty predicate
	// would pass the loop below for an empty statement. Refuse it first.
	if recoveryClaimablePredicate == "" {
		t.Fatal("the shared predicate is empty: the composition check would prove nothing")
	}
	for _, q := range []struct{ name, sql string }{
		{"claimStuckOrdersSQL", claimStuckOrdersSQL},
		{"eligibleUnparkedRecoveryBacklogSQL", eligibleUnparkedRecoveryBacklogSQL},
	} {
		if n := strings.Count(q.sql, recoveryClaimablePredicate); n != 1 {
			t.Errorf("%s contains the shared predicate %d times, want 1: it must compose recoveryClaimablePredicate", q.name, n)
		}
	}
	// The gauge reads the combined statement, not the standalone one. It must compose both
	// aggregate constants once each. This is a text check too; the six-row baseline in
	// recovery_eligible_smoke_test.go reads the combined statement against real rows.
	for _, part := range []struct{ name, sql string }{
		{"eligibleUnparkedRecoveryBacklogSQL", eligibleUnparkedRecoveryBacklogSQL},
		{"parkedRecoveryBacklogSQL", parkedRecoveryBacklogSQL},
	} {
		if n := strings.Count(recoveryBacklogSQL, part.sql); n != 1 {
			t.Errorf("recoveryBacklogSQL contains %s %d times, want 1", part.name, n)
		}
	}
}
