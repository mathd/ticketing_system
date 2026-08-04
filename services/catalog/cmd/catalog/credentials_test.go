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
