package main

import (
	"strings"
	"testing"
)

// TKT-191, ai-review F1. Two credentials with different blast radii are only
// two credentials if they hold different values. Nothing else in the system
// compares them, so a deployment that set both to the same string would run
// normally while the back office quietly held the key to every service's
// internal surface — configured-looking and wrong.
//
// This drives run() rather than a helper: the guard is only worth anything if it
// is on the path the binary actually takes, before any dependency is contacted.
//
// If this test ever HANGS rather than fails, that is the diagnosis: the guard
// has been moved after the NATS connection, which retries forever by design
// (MaxReconnects(-1)). Verified by disabling the guard — run() blocked instead
// of returning, which is also what proves the assertion is load-bearing.
func TestServerRefusesIdenticalCredentials(t *testing.T) {
	const same = "0f3d1c9a8b7e6f5d4c3b2a1908f7e6d5"
	t.Setenv("INTERNAL_SERVICE_TOKEN", same)
	t.Setenv("CATALOG_STAFF_WRITE_TOKEN", same)
	// Deliberately unset: if the guard did not fire, run() would get this far and
	// fail on the database instead — a different error, which is what this
	// asserts against.
	t.Setenv("DATABASE_URL", "")

	err := run()
	if err == nil {
		t.Fatal("catalog started with both credentials set to the same value")
	}
	if !strings.Contains(err.Error(), "must not equal INTERNAL_SERVICE_TOKEN") {
		t.Fatalf("startup failed for the wrong reason: %v", err)
	}
	if strings.Contains(err.Error(), same) {
		t.Fatalf("the error echoes the credential: %v", err)
	}
}

func TestServerRefusesMissingStaffWriteCredential(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_TOKEN", "0f3d1c9a8b7e6f5d4c3b2a1908f7e6d5")
	t.Setenv("CATALOG_STAFF_WRITE_TOKEN", "")

	err := run()
	if err == nil || !strings.Contains(err.Error(), "CATALOG_STAFF_WRITE_TOKEN required") {
		t.Fatalf("want a CATALOG_STAFF_WRITE_TOKEN configuration error, got %v", err)
	}
}

// TKT-245. The assertion key is a THIRD value with a third blast radius, and the
// reason it must differ from the staff-write credential is sharper than the usual
// separation argument.
//
// The assertion exists so that holding the write credential does not let a caller
// choose an organizer. If the signing key WERE the write credential, any holder
// could mint their own assertion for any tenant — the boundary would be exactly
// as absent as before this ticket, while every header, test and log line said it
// was there. That is the failure this refuses at startup.
func TestServerRefusesAnAssertionKeyEqualToACredential(t *testing.T) {
	for _, tc := range []struct{ name, collidesWith string }{
		{"equal to the staff-write credential", "CATALOG_STAFF_WRITE_TOKEN"},
		{"equal to the internal token", "INTERNAL_SERVICE_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const internal = "0f3d1c9a8b7e6f5d4c3b2a1908f7e6d5"
			const staffWrite = "1a2b3c4d5e6f70819293a4b5c6d7e8f9"
			t.Setenv("INTERNAL_SERVICE_TOKEN", internal)
			t.Setenv("CATALOG_STAFF_WRITE_TOKEN", staffWrite)
			collided := staffWrite
			if tc.collidesWith == "INTERNAL_SERVICE_TOKEN" {
				collided = internal
			}
			t.Setenv("CATALOG_ORGANIZER_ASSERTION_KEY", collided)
			// Unset, so a guard that failed to fire would fail on the database
			// instead — a different error, which is what this asserts against.
			t.Setenv("DATABASE_URL", "")

			err := run()
			if err == nil {
				t.Fatal("catalog started with the signing key equal to a credential")
			}
			if !strings.Contains(err.Error(), "must differ from INTERNAL_SERVICE_TOKEN") {
				t.Fatalf("startup failed for the wrong reason: %v", err)
			}
			if strings.Contains(err.Error(), collided) {
				t.Fatalf("the error echoes the secret: %v", err)
			}
		})
	}
}

func TestServerRefusesMissingAssertionKey(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_TOKEN", "0f3d1c9a8b7e6f5d4c3b2a1908f7e6d5")
	t.Setenv("CATALOG_STAFF_WRITE_TOKEN", "1a2b3c4d5e6f70819293a4b5c6d7e8f9")
	t.Setenv("CATALOG_ORGANIZER_ASSERTION_KEY", "")

	err := run()
	if err == nil || !strings.Contains(err.Error(), "CATALOG_ORGANIZER_ASSERTION_KEY required") {
		t.Fatalf("want a CATALOG_ORGANIZER_ASSERTION_KEY configuration error, got %v", err)
	}
}
