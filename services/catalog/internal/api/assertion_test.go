package api

// Organizer assertion tests (TKT-245).
//
// Every expectation here is derived from the REQUIREMENT, never from watching what
// the code does (AGENTS.md: a green test can bless the defect). The invariant each
// case pins is stated in one sentence without naming the implementation, so a test
// that starts passing for a new reason is visible as a changed sentence.

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testAssertionKey = organizerAssertionKey("catalog-organizer-assertion-test-key")

// A minted assertion names the organizer and staff member it was minted for, and
// nothing else can be read back out of it.
func TestOrganizerAssertionRoundTrips(t *testing.T) {
	staffID, orgID := uuid.New(), uuid.New()
	now := time.Now()

	token := mintOrganizerAssertion(testAssertionKey, staffID, orgID, now.Add(time.Hour))

	got, err := verifyOrganizerAssertion(testAssertionKey, token, now)
	if err != nil {
		t.Fatalf("verify a freshly minted assertion = %v, want nil", err)
	}
	if got.OrganizerID != orgID {
		t.Errorf("organizer = %s, want %s", got.OrganizerID, orgID)
	}
	if got.StaffID != staffID {
		t.Errorf("staff = %s, want %s", got.StaffID, staffID)
	}
}

// The signature covers every field, so no field can be changed by its holder.
//
// Table-driven over each mutable position rather than one "tampering" case: a
// single case proves the MAC covers ONE field, and the defect this refuses is a
// payload assembled so that some field falls outside it.
func TestOrganizerAssertionRefusesEveryTamperedField(t *testing.T) {
	staffID, orgID := uuid.New(), uuid.New()
	now := time.Now()
	token := mintOrganizerAssertion(testAssertionKey, staffID, orgID, now.Add(time.Hour))
	parts := strings.Split(token, ".")

	// Rebuild the token with one field replaced, keeping the original MAC.
	swap := func(idx int, val string) string {
		mutated := append([]string(nil), parts...)
		mutated[idx] = val
		return strings.Join(mutated, ".")
	}

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"a different staff member", swap(1, uuid.New().String())},
		{"a different organizer", swap(2, uuid.New().String())},
		{"an expiry pushed into the future", swap(3, strconv.FormatInt(now.Add(100*time.Hour).Unix(), 10))},
		{"a forged mac", swap(4, "not-the-real-mac")},
		{"a different version", swap(0, "v2")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := verifyOrganizerAssertion(testAssertionKey, tc.token, now); err == nil {
				t.Fatal("verify accepted a tampered assertion, want refusal")
			}
		})
	}
}

// A token signed with a different key is not this catalog's to trust.
func TestOrganizerAssertionRefusesAnotherKeysSignature(t *testing.T) {
	staffID, orgID := uuid.New(), uuid.New()
	now := time.Now()

	token := mintOrganizerAssertion("a-different-signing-key-entirely", staffID, orgID, now.Add(time.Hour))

	if _, err := verifyOrganizerAssertion(testAssertionKey, token, now); err == nil {
		t.Fatal("verify accepted an assertion signed by another key, want refusal")
	}
}

// An assertion stops being valid the instant it expires -- not a moment after.
//
// The boundary is asserted exactly, because "expired" is the one property whose
// off-by-one is invisible in normal use and only shows up as a session that
// outlives its credential.
func TestOrganizerAssertionExpiryBoundaryIsExact(t *testing.T) {
	staffID, orgID := uuid.New(), uuid.New()
	now := time.Now().Truncate(time.Second)
	expiry := now.Add(time.Hour)
	token := mintOrganizerAssertion(testAssertionKey, staffID, orgID, expiry)

	if _, err := verifyOrganizerAssertion(testAssertionKey, token, expiry.Add(-time.Second)); err != nil {
		t.Errorf("one second before expiry = %v, want valid", err)
	}
	if _, err := verifyOrganizerAssertion(testAssertionKey, token, expiry); err == nil {
		t.Error("at the expiry instant the assertion verified, want refusal")
	}
	if _, err := verifyOrganizerAssertion(testAssertionKey, token, expiry.Add(time.Second)); err == nil {
		t.Error("one second after expiry the assertion verified, want refusal")
	}
}

// An expired assertion cannot be revived by rewriting its expiry, because the
// signature covers that field.
//
// NOT a test of the internal check ORDER. The MAC-before-expiry ordering in
// verifyOrganizerAssertion is real and deliberate (an unauthenticated number must
// not be parsed and believed), but it is not observable from out here: both orders
// return the same single error for every bad token, by construction, so any test
// claiming to pin the order would pass under either. Writing one and watching a
// reordering mutant survive is how this comment came to exist -- the assertion was
// pinning the mechanism, not the rule.
//
// What IS observable, and what actually matters to a holder, is this: the one
// field they have a motive to change cannot be changed. The ordering is left to
// the code comment and to review, where it belongs.
func TestOrganizerAssertionExpiryCannotBeExtendedByItsHolder(t *testing.T) {
	now := time.Now()
	staffID, orgID := uuid.New(), uuid.New()

	// A token that has already died in the holder's hands.
	expired := mintOrganizerAssertion(testAssertionKey, staffID, orgID, now.Add(-time.Minute))
	if _, err := verifyOrganizerAssertion(testAssertionKey, expired, now); err == nil {
		t.Fatal("precondition: the token must start out expired")
	}

	// The holder rewrites the expiry far into the future, keeping everything else.
	parts := strings.Split(expired, ".")
	parts[3] = strconv.FormatInt(now.Add(1000*time.Hour).Unix(), 10)

	if _, err := verifyOrganizerAssertion(testAssertionKey, strings.Join(parts, "."), now); err == nil {
		t.Fatal("an expired assertion was revived by rewriting its expiry; the MAC does not cover that field")
	}
}

// A structurally broken token is refused rather than misread.
func TestOrganizerAssertionRefusesMalformedTokens(t *testing.T) {
	now := time.Now()
	valid := mintOrganizerAssertion(testAssertionKey, uuid.New(), uuid.New(), now.Add(time.Hour))

	for _, tc := range []struct{ name, token string }{
		{"empty", ""},
		{"whitespace", "   "},
		{"too few parts", "v1.abc.def"},
		{"too many parts", valid + ".extra"},
		{"truncated mid-token", valid[:len(valid)/2]},
		{"not a uuid in the staff position", "v1.not-a-uuid." + uuid.New().String() + ".9999999999.mac"},
		{"a non-numeric expiry", "v1." + uuid.New().String() + "." + uuid.New().String() + ".soon.mac"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := verifyOrganizerAssertion(testAssertionKey, tc.token, now); err == nil {
				t.Fatalf("verify accepted %q, want refusal", tc.token)
			}
		})
	}
}

// The nil uuid is a zero value, not an identity: a construction bug upstream must
// not arrive here as an authenticated principal.
func TestOrganizerAssertionRefusesTheNilUUID(t *testing.T) {
	now := time.Now()

	for _, tc := range []struct {
		name           string
		staffID, orgID uuid.UUID
	}{
		{"nil organizer", uuid.New(), uuid.Nil},
		{"nil staff", uuid.Nil, uuid.New()},
		{"both nil", uuid.Nil, uuid.Nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := mintOrganizerAssertion(testAssertionKey, tc.staffID, tc.orgID, now.Add(time.Hour))
			if _, err := verifyOrganizerAssertion(testAssertionKey, token, now); err == nil {
				t.Fatal("verify accepted a nil uuid as a principal, want refusal")
			}
		})
	}
}

// An unconfigured key verifies NOTHING, rather than verifying everything.
//
// Without the guard an empty key still produces a self-consistent MAC, so every
// forged token would verify -- the one configuration nobody exercises being the
// one that admits everybody.
func TestOrganizerAssertionWithNoKeyConfiguredRefusesEverything(t *testing.T) {
	now := time.Now()
	staffID, orgID := uuid.New(), uuid.New()

	// Self-consistent under the empty key: minted and verified with the same "".
	selfConsistent := mintOrganizerAssertion("", staffID, orgID, now.Add(time.Hour))
	if _, err := verifyOrganizerAssertion("", selfConsistent, now); err == nil {
		t.Fatal("an unkeyed verifier accepted a token, want refusal")
	}

	valid := mintOrganizerAssertion(testAssertionKey, staffID, orgID, now.Add(time.Hour))
	if _, err := verifyOrganizerAssertion("", valid, now); err == nil {
		t.Fatal("an unkeyed verifier accepted a validly signed token, want refusal")
	}
}

// Every refusal is the same refusal: a caller probing the difference learns which
// of its guesses was structurally right.
func TestOrganizerAssertionRefusalsAreIndistinguishable(t *testing.T) {
	now := time.Now()
	staffID, orgID := uuid.New(), uuid.New()
	valid := mintOrganizerAssertion(testAssertionKey, staffID, orgID, now.Add(time.Hour))
	expired := mintOrganizerAssertion(testAssertionKey, staffID, orgID, now.Add(-time.Hour))
	forged := mintOrganizerAssertion("another-key", staffID, orgID, now.Add(time.Hour))

	for _, tc := range []struct{ name, token string }{
		{"expired", expired},
		{"forged", forged},
		{"malformed", "v1.garbage"},
		{"truncated", valid[:10]},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verifyOrganizerAssertion(testAssertionKey, tc.token, now)
			if err == nil {
				t.Fatalf("%s verified, want refusal", tc.name)
			}
			if !errors.Is(err, ErrOrganizerAssertionInvalid) {
				t.Fatalf("%s refused with %v, want the single %v -- a distinguishable refusal is an oracle",
					tc.name, err, ErrOrganizerAssertionInvalid)
			}
		})
	}
}

// The token discloses no secret: it is signed, not encrypted, and the holder is
// the staff member it names. What must NOT appear is the key.
func TestOrganizerAssertionDoesNotCarryTheKey(t *testing.T) {
	token := mintOrganizerAssertion(testAssertionKey, uuid.New(), uuid.New(), time.Now().Add(time.Hour))

	if strings.Contains(token, string(testAssertionKey)) {
		t.Fatal("the minted assertion contains the signing key")
	}
}

// The role is deliberately NOT part of the payload.
//
// ADR-042 snapshots role at sign-in and warns it goes stale when a role-change
// surface lands; signing it would make catalog authoritative about something it
// cannot refresh. Organizer and staff id are immutable per staff row, so they are
// safe to sign. This test pins the ABSENCE, which is a design constraint that would
// otherwise be invisible to a future reader adding "just one more field".
func TestOrganizerAssertionPayloadCarriesOnlyImmutableIdentity(t *testing.T) {
	staffID, orgID := uuid.New(), uuid.New()
	expiry := time.Now().Add(time.Hour)

	parts := strings.Split(mintOrganizerAssertion(testAssertionKey, staffID, orgID, expiry), ".")

	if len(parts) != 5 {
		t.Fatalf("assertion has %d parts, want exactly 5 (version, staff, organizer, expiry, mac); "+
			"a new field is a canonical-format change, not a test update", len(parts))
	}
	if parts[0] != organizerAssertionVersion {
		t.Errorf("version = %q, want %q", parts[0], organizerAssertionVersion)
	}
	if parts[1] != staffID.String() || parts[2] != orgID.String() {
		t.Errorf("payload = %q/%q, want staff %s and organizer %s", parts[1], parts[2], staffID, orgID)
	}
	if parts[3] != strconv.FormatInt(expiry.Unix(), 10) {
		t.Errorf("expiry = %q, want %d", parts[3], expiry.Unix())
	}
}
