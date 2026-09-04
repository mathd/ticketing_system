package main

import (
	"strings"
	"testing"
)

// Four credentials produce six unordered pairs. Every pair must be refused when
// its values collide, and the diagnostic must identify the affected privileges.
func TestCredentialsAreDistinctRefusesEveryCollidingPair(t *testing.T) {
	const shared = "the-same-value"

	for _, tc := range []struct {
		name                                      string
		internal, staffWrite, assertion, payments string
		wantMentions                              []string
	}{
		{"staff write == internal", shared, shared, "assertion", "payments", []string{"COMMERCE_STAFF_WRITE_TOKEN", "INTERNAL_SERVICE_TOKEN"}},
		{"assertion key == internal", shared, "staff", shared, "payments", []string{"COMMERCE_CUSTOMER_ASSERTION_KEY", "INTERNAL_SERVICE_TOKEN"}},
		{"assertion key == staff write", "internal", shared, shared, "payments", []string{"COMMERCE_CUSTOMER_ASSERTION_KEY", "COMMERCE_STAFF_WRITE_TOKEN"}},
		{"payments == internal", shared, "staff", "assertion", shared, []string{"PAYMENTS_INTERNAL_TOKEN", "INTERNAL_SERVICE_TOKEN"}},
		{"payments == staff write", "internal", shared, "assertion", shared, []string{"PAYMENTS_INTERNAL_TOKEN", "COMMERCE_STAFF_WRITE_TOKEN"}},
		{"payments == assertion key", "internal", "staff", shared, shared, []string{"PAYMENTS_INTERNAL_TOKEN", "COMMERCE_CUSTOMER_ASSERTION_KEY"}},
		{"all four the same", shared, shared, shared, shared, []string{"INTERNAL_SERVICE_TOKEN"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := credentialsAreDistinct(tc.internal, tc.staffWrite, tc.assertion, tc.payments)
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

func TestCredentialsAreDistinctAcceptsFourDifferentValues(t *testing.T) {
	if err := credentialsAreDistinct("internal", "staff-write", "assertion-key", "payments"); err != nil {
		t.Fatalf("four distinct credentials must be accepted: %v", err)
	}
}
