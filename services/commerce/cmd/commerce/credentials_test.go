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

// TKT-287. The organizer keyring is required and must parse; a commerce started
// without it would answer every staff operation with 404.
func TestOrganizerAssertionsFromEnvRefusesAMissingOrMalformedKeyring(t *testing.T) {
	for name, raw := range map[string]string{
		"missing":               "",
		"no kid":                "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE",
		"outside the namespace": "access-qr/x=AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE",
		"short key":             "catalog-org/x=AQEB",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(organizerAssertionKeysEnv, raw)
			_, err := organizerAssertionsFromEnv()
			if err == nil || !strings.Contains(err.Error(), organizerAssertionKeysEnv) {
				t.Fatalf("want an error naming %s, got %v", organizerAssertionKeysEnv, err)
			}
		})
	}
	t.Setenv(organizerAssertionKeysEnv, "catalog-org/x=AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE")
	if _, err := organizerAssertionsFromEnv(); err != nil {
		t.Fatalf("a well-formed keyring was refused: %v", err)
	}
}

// The loader is WIRED into startup, before any dependency: run() with every earlier
// credential valid and no keyring fails on the keyring. If the call were removed or
// moved after the NATS connect, run() would block on the broker instead (it retries
// forever), so this test hangs rather than passes — the same tell catalog's startup
// tests document.
func TestCommerceRefusesToStartWithoutTheOrganizerKeyring(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_TOKEN", "0f3d1c9a8b7e6f5d4c3b2a1908f7e6d5aaaa")
	t.Setenv("COMMERCE_STAFF_WRITE_TOKEN", "1a2b3c4d5e6f70819293a4b5c6d7e8f9bbbb")
	t.Setenv("COMMERCE_CUSTOMER_ASSERTION_KEY", "2b3c4d5e6f708192a3b4c5d6e7f8091acccc")
	t.Setenv("PAYMENTS_INTERNAL_TOKEN", "3c4d5e6f708192a3b4c5d6e7f8091a2bdddd")
	t.Setenv(organizerAssertionKeysEnv, "")
	t.Setenv("DATABASE_URL", "")
	err := run()
	if err == nil || !strings.Contains(err.Error(), organizerAssertionKeysEnv+" required") {
		t.Fatalf("want the missing-keyring refusal, got %v", err)
	}
}
