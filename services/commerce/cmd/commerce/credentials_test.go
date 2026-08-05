package main

import (
	"strings"
	"testing"
)

// TKT-194. Commerce now accepts a second credential — the back office's, which
// opens exactly one operation (the staff refund) where INTERNAL_SERVICE_TOKEN
// opens every service's internal surface. Set both to the same string and that
// separation evaporates while the deployment looks configured: the back office,
// an internet-facing SSR process, would be holding the key to every internal
// route in the system under a different name.
//
// Mirrors catalog's guard (TKT-191) deliberately, including this diagnosis: if
// this test ever HANGS rather than fails, the guard has been moved after the
// NATS connection, which retries forever by design.
func TestCommerceRefusesIdenticalCredentials(t *testing.T) {
	const same = "b8a7c6d5e4f3021918273645aabbccdd"
	t.Setenv("INTERNAL_SERVICE_TOKEN", same)
	t.Setenv("COMMERCE_STAFF_WRITE_TOKEN", same)
	// Deliberately unset: if the guard did not fire, run() would reach the
	// database and fail for a different reason, which is what this asserts
	// against.
	t.Setenv("DATABASE_URL", "")

	err := run()
	if err == nil {
		t.Fatal("commerce started with both credentials set to the same value")
	}
	if !strings.Contains(err.Error(), "must not equal INTERNAL_SERVICE_TOKEN") {
		t.Fatalf("startup failed for the wrong reason: %v", err)
	}
	if strings.Contains(err.Error(), same) {
		t.Fatalf("the error echoes the credential: %v", err)
	}
}

// The credential is required, not optional. A commerce that started without it
// would serve every refund request with a 404 — indistinguishable from "no such
// order", so the misconfiguration would surface as a support ticket about a
// missing order rather than as a deployment failure.
func TestCommerceRefusesAMissingStaffCredential(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_TOKEN", "1a2b3c4d5e6f70819a2b3c4d5e6f7081")
	t.Setenv("COMMERCE_STAFF_WRITE_TOKEN", "")
	t.Setenv("DATABASE_URL", "")

	err := run()
	if err == nil {
		t.Fatal("commerce started with no staff credential")
	}
	if !strings.Contains(err.Error(), "COMMERCE_STAFF_WRITE_TOKEN") {
		t.Fatalf("startup failed for the wrong reason: %v", err)
	}
}
