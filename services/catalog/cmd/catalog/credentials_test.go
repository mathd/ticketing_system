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

// TKT-245, TKT-287. The assertion signing seed is a THIRD value with a third
// blast radius, and the reason it must differ from the staff-write credential is
// sharper than the usual separation argument.
//
// The assertion exists so that holding the write credential does not let a caller
// choose an organizer. If the signing seed WERE the write credential, any holder
// could mint their own assertion for any tenant — the boundary would be exactly
// as absent as before TKT-245, while every header, test and log line said it was
// there. That is the failure this refuses at startup.
//
// The colliding values are VALID Ed25519 seeds, so a decode failure cannot be what
// refuses them: the test reaches the collision guard or nothing.
func TestServerRefusesAnAssertionSeedEqualToACredential(t *testing.T) {
	for _, tc := range []struct{ name, collidesWith string }{
		{"equal to the staff-write credential", "CATALOG_STAFF_WRITE_TOKEN"},
		{"equal to the internal token", "INTERNAL_SERVICE_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const internal = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"   // 32 x 0x01
			const staffWrite = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI" // 32 x 0x02
			t.Setenv("INTERNAL_SERVICE_TOKEN", internal)
			t.Setenv("CATALOG_STAFF_WRITE_TOKEN", staffWrite)
			collided := staffWrite
			if tc.collidesWith == "INTERNAL_SERVICE_TOKEN" {
				collided = internal
			}
			t.Setenv("CATALOG_ORGANIZER_ASSERTION_SIGNING_KEY", collided)
			t.Setenv("CATALOG_ORGANIZER_ASSERTION_KID", "catalog-org/test")
			// Unset, so a guard that failed to fire would fail on the database
			// instead — a different error, which is what this asserts against.
			t.Setenv("DATABASE_URL", "")

			err := run()
			if err == nil {
				t.Fatal("catalog started with the signing seed equal to a credential")
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

// TKT-287 ai-review: the collision is about KEY MATERIAL, not spelling. Each
// credential below is a different string from the seed and decodes to the same
// 32 bytes, so its holder holds the seed. The seed itself is canonical, so the
// test reaches the collision guard and not the decoder.
func TestServerRefusesACredentialThatDecodesToTheAssertionSeed(t *testing.T) {
	const seed = "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc" // 32 x 0x07, canonical raw-standard base64
	for name, alias := range map[string]string{
		"non-canonical trailing bits": "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwd",
		"padded standard base64":      "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc=",
		"hex":                         "0707070707070707070707070707070707070707070707070707070707070707",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("INTERNAL_SERVICE_TOKEN", "0f3d1c9a8b7e6f5d4c3b2a1908f7e6d5")
			t.Setenv("CATALOG_STAFF_WRITE_TOKEN", alias)
			t.Setenv("CATALOG_ORGANIZER_ASSERTION_SIGNING_KEY", seed)
			t.Setenv("CATALOG_ORGANIZER_ASSERTION_KID", "catalog-org/test")
			t.Setenv("DATABASE_URL", "")

			err := run()
			if err == nil || !strings.Contains(err.Error(), "must differ from INTERNAL_SERVICE_TOKEN") {
				t.Fatalf("want the collision refusal, got %v", err)
			}
		})
	}
}

// Each assertion-configuration refusal, with every EARLIER predicate satisfied so
// the case reaches the one it names, and DATABASE_URL unset so a guard that does
// not fire fails on the database instead.
func TestServerRefusesUnusableAssertionConfiguration(t *testing.T) {
	const validSeed = "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc" // 32 x 0x07
	for _, tc := range []struct{ name, seed, kid, want string }{
		{"missing seed", "", "catalog-org/test", "CATALOG_ORGANIZER_ASSERTION_SIGNING_KEY required"},
		{"seed not base64", "this-is-long-enough-but-is-not-base64-at-all!!", "catalog-org/test", "not canonical raw-standard base64"},
		{"seed with non-canonical trailing bits", "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwd", "catalog-org/test", "not canonical raw-standard base64"},
		{"seed of the wrong length", "CAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI", "catalog-org/test", "32-byte Ed25519 seed"},
		{"missing kid", validSeed, "", "key id must be"},
		{"kid outside the namespace", validSeed, "access-qr/test", "key id must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("INTERNAL_SERVICE_TOKEN", "0f3d1c9a8b7e6f5d4c3b2a1908f7e6d5")
			t.Setenv("CATALOG_STAFF_WRITE_TOKEN", "1a2b3c4d5e6f70819293a4b5c6d7e8f9")
			t.Setenv("CATALOG_ORGANIZER_ASSERTION_SIGNING_KEY", tc.seed)
			t.Setenv("CATALOG_ORGANIZER_ASSERTION_KID", tc.kid)
			t.Setenv("DATABASE_URL", "")

			err := run()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
			if tc.seed != "" && strings.Contains(err.Error(), tc.seed) {
				t.Fatalf("the error echoes the seed: %v", err)
			}
		})
	}
}

// The HMAC key the v1 format used is gone (TKT-287 D4). A deployment that still
// sets it, and nothing else, must fail on the missing seed rather than start.
func TestServerDoesNotStartOnTheRetiredHMACKeyAlone(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_TOKEN", "0f3d1c9a8b7e6f5d4c3b2a1908f7e6d5")
	t.Setenv("CATALOG_STAFF_WRITE_TOKEN", "1a2b3c4d5e6f70819293a4b5c6d7e8f9")
	t.Setenv("CATALOG_ORGANIZER_ASSERTION_KEY", "9f8e7d6c5b4a39281706f5e4d3c2b1a0")
	t.Setenv("CATALOG_ORGANIZER_ASSERTION_SIGNING_KEY", "")

	err := run()
	if err == nil || !strings.Contains(err.Error(), "CATALOG_ORGANIZER_ASSERTION_SIGNING_KEY required") {
		t.Fatalf("want a CATALOG_ORGANIZER_ASSERTION_SIGNING_KEY configuration error, got %v", err)
	}
}
