package api

// Organizer assertion tests (TKT-245; Ed25519 v2 since TKT-287). The parser's
// malformation classes are covered in shared/go/organizerassertion; these pin
// what catalog MINTS and that catalog's verification path is that parser.
//
// Every expectation here is derived from the REQUIREMENT, never from watching what
// the code does (AGENTS.md: a green test can bless the defect). The invariant each
// case pins is stated in one sentence without naming the implementation, so a test
// that starts passing for a new reason is visible as a changed sentence.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/shared/organizerassertion"
)

func assertionSigner(t *testing.T, seedByte byte, kid string) *OrganizerAssertionSigner {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = seedByte
	}
	s, err := NewOrganizerAssertionSigner(base64.RawStdEncoding.EncodeToString(seed), kid)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A minted assertion names the organizer and staff member it was minted for, and
// nothing else can be read back out of it.
func TestOrganizerAssertionRoundTrips(t *testing.T) {
	staffID, orgID := uuid.New(), uuid.New()
	now := time.Now()

	token := mintOrganizerAssertion(testOrganizerAssertionSigner(t), staffID, orgID, now.Add(time.Hour))

	got, err := verifyOrganizerAssertion(testOrganizerAssertionSigner(t), token, now)
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
// single case proves the signature covers ONE field, and the defect this refuses is a
// payload assembled so that some field falls outside it.
func TestOrganizerAssertionRefusesEveryTamperedField(t *testing.T) {
	staffID, orgID := uuid.New(), uuid.New()
	now := time.Now()
	token := mintOrganizerAssertion(testOrganizerAssertionSigner(t), staffID, orgID, now.Add(time.Hour))
	parts := strings.Split(token, ".")

	// Rebuild the token with one field replaced, keeping the original signature.
	swap := func(idx int, val string) string {
		mutated := append([]string(nil), parts...)
		mutated[idx] = val
		return strings.Join(mutated, ".")
	}

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"a different version", swap(0, "v3")},
		{"a different key id", swap(1, "catalog-org/other")},
		{"a different staff member", swap(2, uuid.New().String())},
		{"a different organizer", swap(3, uuid.New().String())},
		{"an expiry pushed into the future", swap(4, strconv.FormatInt(now.Add(100*time.Hour).Unix(), 10))},
		{"a forged signature", swap(5, base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := verifyOrganizerAssertion(testOrganizerAssertionSigner(t), tc.token, now); err == nil {
				t.Fatal("verify accepted a tampered assertion, want refusal")
			}
		})
	}
}

// A token signed with a different key is not this catalog's to trust.
func TestOrganizerAssertionRefusesAnotherKeysSignature(t *testing.T) {
	staffID, orgID := uuid.New(), uuid.New()
	now := time.Now()

	token := mintOrganizerAssertion(assertionSigner(t, 9, testOrganizerAssertionKID), staffID, orgID, now.Add(time.Hour))

	if _, err := verifyOrganizerAssertion(testOrganizerAssertionSigner(t), token, now); err == nil {
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
	token := mintOrganizerAssertion(testOrganizerAssertionSigner(t), staffID, orgID, expiry)

	if _, err := verifyOrganizerAssertion(testOrganizerAssertionSigner(t), token, expiry.Add(-time.Second)); err != nil {
		t.Errorf("one second before expiry = %v, want valid", err)
	}
	if _, err := verifyOrganizerAssertion(testOrganizerAssertionSigner(t), token, expiry); err == nil {
		t.Error("at the expiry instant the assertion verified, want refusal")
	}
	if _, err := verifyOrganizerAssertion(testOrganizerAssertionSigner(t), token, expiry.Add(time.Second)); err == nil {
		t.Error("one second after expiry the assertion verified, want refusal")
	}
}

// An expired assertion cannot be revived by rewriting its expiry, because the
// signature covers that field.
//
// NOT a test of the internal check ORDER. The signature-before-expiry ordering in
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
	expired := mintOrganizerAssertion(testOrganizerAssertionSigner(t), staffID, orgID, now.Add(-time.Minute))
	if _, err := verifyOrganizerAssertion(testOrganizerAssertionSigner(t), expired, now); err == nil {
		t.Fatal("precondition: the token must start out expired")
	}

	// The holder rewrites the expiry far into the future, keeping everything else.
	parts := strings.Split(expired, ".")
	parts[4] = strconv.FormatInt(now.Add(1000*time.Hour).Unix(), 10)

	if _, err := verifyOrganizerAssertion(testOrganizerAssertionSigner(t), strings.Join(parts, "."), now); err == nil {
		t.Fatal("an expired assertion was revived by rewriting its expiry; the signature does not cover that field")
	}
}

// A structurally broken token is refused rather than misread.
func TestOrganizerAssertionRefusesMalformedTokens(t *testing.T) {
	now := time.Now()
	valid := mintOrganizerAssertion(testOrganizerAssertionSigner(t), uuid.New(), uuid.New(), now.Add(time.Hour))

	for _, tc := range []struct{ name, token string }{
		{"empty", ""},
		{"whitespace", "   "},
		{"too few parts", "v2.abc.def"},
		{"too many parts", valid + ".extra"},
		{"truncated mid-token", valid[:len(valid)/2]},
		{"not a uuid in the staff position", "v2." + testOrganizerAssertionKID + ".not-a-uuid." + uuid.New().String() + ".9999999999.sig"},
		{"a non-numeric expiry", "v2." + testOrganizerAssertionKID + "." + uuid.New().String() + "." + uuid.New().String() + ".soon.sig"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := verifyOrganizerAssertion(testOrganizerAssertionSigner(t), tc.token, now); err == nil {
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
			token := mintOrganizerAssertion(testOrganizerAssertionSigner(t), tc.staffID, tc.orgID, now.Add(time.Hour))
			if _, err := verifyOrganizerAssertion(testOrganizerAssertionSigner(t), token, now); err == nil {
				t.Fatal("verify accepted a nil uuid as a principal, want refusal")
			}
		})
	}
}

// An unconfigured signer verifies NOTHING, rather than verifying everything.
func TestOrganizerAssertionWithNoSignerConfiguredRefusesEverything(t *testing.T) {
	valid := mintOrganizerAssertion(testOrganizerAssertionSigner(t), uuid.New(), uuid.New(), time.Now().Add(time.Hour))
	if _, err := verifyOrganizerAssertion(nil, valid, time.Now()); err == nil {
		t.Fatal("an unkeyed verifier accepted a validly signed token, want refusal")
	}
}

// The HMAC v1 format is refused outright: there is no transition window
// (TKT-287 D4), so a v1 token held across the deploy must not verify — and must
// not be misparsed as something else.
func TestALegacyV1OrganizerAssertionIsRefused(t *testing.T) {
	now := time.Now()
	payload := strings.Join([]string{"v1", uuid.NewString(), uuid.NewString(),
		strconv.FormatInt(now.Add(time.Hour).Unix(), 10)}, ".")
	mac := hmac.New(sha256.New, []byte("any-legacy-hmac-key"))
	mac.Write([]byte(payload))
	legacy := payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if _, err := verifyOrganizerAssertion(testOrganizerAssertionSigner(t), legacy, now); err == nil {
		t.Fatal("a v1 HMAC assertion verified after the v2 cutover")
	}
}

// What catalog mints is what another service verifies with ONLY the public key.
// The keyring is built here from the seed by hand — not through the signer — so
// the test cannot pass by catalog agreeing with itself.
func TestACatalogAssertionVerifiesWithOnlyThePublicKey(t *testing.T) {
	seed, err := base64.RawStdEncoding.DecodeString(testOrganizerAssertionSeed)
	if err != nil {
		t.Fatal(err)
	}
	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	keyring, err := organizerassertion.ParseKeyring(testOrganizerAssertionKID + "=" + base64.RawStdEncoding.EncodeToString(public))
	if err != nil {
		t.Fatal(err)
	}
	staffID, orgID := uuid.New(), uuid.New()
	now := time.Now()
	scope, err := keyring.Verify(mintOrganizerAssertion(testOrganizerAssertionSigner(t), staffID, orgID, now.Add(time.Hour)), now)
	if err != nil {
		t.Fatalf("public-key verification of a catalog assertion = %v", err)
	}
	if scope.StaffID != staffID || scope.OrganizerID != orgID {
		t.Fatalf("scope = %+v, want staff %s organizer %s", scope, staffID, orgID)
	}
}

// The signer refuses key material it cannot use, and never echoes the seed.
func TestNewOrganizerAssertionSignerRefusesBadConfiguration(t *testing.T) {
	seed := base64.RawStdEncoding.EncodeToString(make([]byte, ed25519.SeedSize))
	for _, tc := range []struct{ name, seed, kid string }{
		{"seed not base64", "not base64!!", testOrganizerAssertionKID},
		{"padded standard base64", seed + "=", testOrganizerAssertionKID},
		{"seed too short", base64.RawStdEncoding.EncodeToString(make([]byte, 31)), testOrganizerAssertionKID},
		{"seed too long", base64.RawStdEncoding.EncodeToString(make([]byte, 64)), testOrganizerAssertionKID},
		{"empty kid", seed, ""},
		{"kid outside the namespace", seed, "access-qr/test"},
		{"kid with a dot", seed, "catalog-org/a.b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOrganizerAssertionSigner(tc.seed, tc.kid)
			if err == nil {
				t.Fatal("accepted")
			}
			if tc.seed != "" && strings.Contains(err.Error(), tc.seed) {
				t.Fatalf("error echoes the seed: %v", err)
			}
		})
	}
}

// Every refusal is the same refusal: a caller probing the difference learns which
// of its guesses was structurally right.
func TestOrganizerAssertionRefusalsAreIndistinguishable(t *testing.T) {
	now := time.Now()
	staffID, orgID := uuid.New(), uuid.New()
	valid := mintOrganizerAssertion(testOrganizerAssertionSigner(t), staffID, orgID, now.Add(time.Hour))
	expired := mintOrganizerAssertion(testOrganizerAssertionSigner(t), staffID, orgID, now.Add(-time.Hour))
	forged := mintOrganizerAssertion(assertionSigner(t, 9, testOrganizerAssertionKID), staffID, orgID, now.Add(time.Hour))

	for _, tc := range []struct{ name, token string }{
		{"expired", expired},
		{"forged", forged},
		{"malformed", "v2.garbage"},
		{"truncated", valid[:10]},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verifyOrganizerAssertion(testOrganizerAssertionSigner(t), tc.token, now)
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
// the staff member it names. What must NOT appear is the seed.
func TestOrganizerAssertionDoesNotCarryTheKey(t *testing.T) {
	token := mintOrganizerAssertion(testOrganizerAssertionSigner(t), uuid.New(), uuid.New(), time.Now().Add(time.Hour))

	if strings.Contains(token, testOrganizerAssertionSeed) {
		t.Fatal("the minted assertion contains the signing seed")
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

	parts := strings.Split(mintOrganizerAssertion(testOrganizerAssertionSigner(t), staffID, orgID, expiry), ".")

	if len(parts) != 6 {
		t.Fatalf("assertion has %d parts, want exactly 6 (version, kid, staff, organizer, expiry, signature); "+
			"a new field is a canonical-format change, not a test update", len(parts))
	}
	if parts[0] != "v2" {
		t.Errorf("version = %q, want v2", parts[0])
	}
	if parts[1] != testOrganizerAssertionKID {
		t.Errorf("kid = %q, want %q", parts[1], testOrganizerAssertionKID)
	}
	if parts[2] != staffID.String() || parts[3] != orgID.String() {
		t.Errorf("payload = %q/%q, want staff %s and organizer %s", parts[2], parts[3], staffID, orgID)
	}
	if parts[4] != strconv.FormatInt(expiry.Unix(), 10) {
		t.Errorf("expiry = %q, want %d", parts[4], expiry.Unix())
	}
}

// TKT-287 ai-review: a credential "holds the seed" if it IS the seed bytes or
// decodes to them in any common text form. Each alias below is a different string
// from the canonical seed; each must be recognised.
func TestEncodesOrganizerAssertionSeedRecognisesEveryCommonAlias(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 7
	}
	canonical := base64.RawStdEncoding.EncodeToString(seed)
	mixedSeed := []byte{0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef}
	for name, tc := range map[string]struct {
		value string
		seed  []byte
	}{
		"canonical":                   {canonical, seed},
		"padded standard":             {base64.StdEncoding.EncodeToString(seed), seed},
		"non-canonical trailing bits": {canonical[:len(canonical)-1] + "d", seed},
		"lower hex":                   {hex.EncodeToString(seed), seed},
		"upper hex":                   {strings.ToUpper(hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))), bytes.Repeat([]byte{0xab}, 32)},
		"whitespace inside":           {canonical[:10] + " \t" + canonical[10:], seed},
		"the raw seed bytes":          {"0123456789abcdef0123456789abcdef", []byte("0123456789abcdef0123456789abcdef")},
		"0x-prefixed hex":             {"0x" + hex.EncodeToString(seed), seed},
		"colon-separated hex":         {colonHex(seed), seed},
		"base64 behind a prefix":      {"seed:" + canonical, seed},
		"raw bytes inside a string":   {"x-0123456789abcdef0123456789abcdef-y", []byte("0123456789abcdef0123456789abcdef")},
		"url-safe alphabet":           {"------------------------------------------8", mixedSeed},
		"mixed alphabets":             {"++++++++++++++++++++----------------------8", mixedSeed},
	} {
		t.Run(name, func(t *testing.T) {
			if !EncodesOrganizerAssertionSeed(tc.value, tc.seed) {
				t.Fatalf("%q was not recognised as the seed", tc.value)
			}
		})
	}
	// And the negative: a different seed in every form is NOT the seed.
	other := make([]byte, ed25519.SeedSize)
	for _, v := range []string{base64.RawStdEncoding.EncodeToString(other), hex.EncodeToString(other), string(other), "0f3d1c9a8b7e6f5d4c3b2a1908f7e6d5"} {
		if EncodesOrganizerAssertionSeed(v, seed) {
			t.Fatalf("%q was mistaken for the seed", v)
		}
	}
}

// One seed, one accepted string: the decoder refuses every other spelling, CR/LF
// included, which Go's base64 decoder ignores even in Strict mode.
func TestDecodeOrganizerAssertionSeedAcceptsOnlyTheCanonicalSpelling(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 7
	}
	canonical := base64.RawStdEncoding.EncodeToString(seed)
	if got, err := DecodeOrganizerAssertionSeed(canonical); err != nil || !bytes.Equal(got, seed) {
		t.Fatalf("canonical seed: %v", err)
	}
	for name, v := range map[string]string{
		"trailing bits": canonical[:len(canonical)-1] + "d",
		"padded":        canonical + "=",
		"LF inside":     canonical[:20] + "\n" + canonical[20:],
		"CR inside":     canonical[:20] + "\r" + canonical[20:],
		"url alphabet":  base64.RawURLEncoding.EncodeToString([]byte{0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef, 0xbe, 0xfb, 0xef}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeOrganizerAssertionSeed(v); err == nil {
				t.Fatalf("accepted %q", v)
			}
		})
	}
}

func colonHex(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = hex.EncodeToString([]byte{c})
	}
	return strings.Join(parts, ":")
}
