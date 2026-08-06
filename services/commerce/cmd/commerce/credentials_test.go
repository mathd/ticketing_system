package main

import (
	"strings"
	"testing"
)

// Commerce holds three credentials with three different blast radii:
// INTERNAL_SERVICE_TOKEN opens every service's internal surface,
// COMMERCE_STAFF_WRITE_TOKEN opens the staff refund, and
// COMMERCE_CUSTOMER_ASSERTION_KEY signs proofs that a checkout belongs to a
// customer. They are only three credentials if they hold three values, and
// nothing else in the system compares them — identical values look configured.
//
// The pairs are enumerated EXHAUSTIVELY rather than each new credential being
// checked against the first, because a credential added to the wiring and not to
// the check is the one whose separation is never verified (TKT-221 plan-review F2).
func TestCredentialsAreDistinctRefusesEveryCollidingPair(t *testing.T) {
	const shared = "the-same-value"

	for _, tc := range []struct {
		name                              string
		internal, staffWrite, assertion   string
		wantMentions                      []string
	}{
		{"staff write == internal", shared, shared, "assertion", []string{"COMMERCE_STAFF_WRITE_TOKEN", "INTERNAL_SERVICE_TOKEN"}},
		{"assertion key == internal", shared, "staff", shared, []string{"COMMERCE_CUSTOMER_ASSERTION_KEY", "INTERNAL_SERVICE_TOKEN"}},
		{"assertion key == staff write", "internal", shared, shared, []string{"COMMERCE_CUSTOMER_ASSERTION_KEY", "COMMERCE_STAFF_WRITE_TOKEN"}},
		{"all three the same", shared, shared, shared, []string{"INTERNAL_SERVICE_TOKEN"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := credentialsAreDistinct(tc.internal, tc.staffWrite, tc.assertion)
			if err == nil {
				t.Fatal("a colliding pair was accepted — the separation boundary is gone while looking configured")
			}
			for _, want := range tc.wantMentions {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal must name which pair collided; %q does not mention %s", err, want)
				}
			}
			// Never echo a credential, even a rejected one: startup errors are logged.
			if strings.Contains(err.Error(), shared) {
				t.Fatalf("the refusal echoes a credential value: %v", err)
			}
		})
	}
}

func TestCredentialsAreDistinctAcceptsThreeDifferentValues(t *testing.T) {
	if err := credentialsAreDistinct("internal", "staff-write", "assertion-key"); err != nil {
		t.Fatalf("three distinct credentials must be accepted: %v", err)
	}
}
