package organizerassertion

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The tests sign with crypto/ed25519 DIRECTLY, never with catalog's minting code:
// a fixture built from the code under test cannot express a canonical-form
// disagreement (AGENTS.md, ADR-017's trap). Every token below is assembled from
// literal fields so a test can sign a payload that is well signed and semantically
// wrong, and so reach the guard it names.

var (
	staff     = uuid.MustParse("6f1c2a52-5d0e-4c55-9d4e-1b2f3a4b5c6d")
	organizer = uuid.MustParse("0b8e7d6c-5a49-4382-9170-6e5d4c3b2a19")
	now       = time.Unix(1_800_000_000, 0)
	expiry    = now.Add(time.Hour)
)

func key(t *testing.T, b byte) ed25519.PrivateKey {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = b
	}
	return ed25519.NewKeyFromSeed(seed)
}

func pub(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

// sign joins fields with dots and appends an Ed25519 signature over exactly
// those bytes. Fields are strings so a test can put anything in any position.
func sign(k ed25519.PrivateKey, fields ...string) string {
	payload := strings.Join(fields, ".")
	return payload + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(k, []byte(payload)))
}

func valid(k ed25519.PrivateKey, kid string) string {
	return sign(k, "v2", kid, staff.String(), organizer.String(), strconv.FormatInt(expiry.Unix(), 10))
}

func verifier(t *testing.T, keys map[string]ed25519.PublicKey) *Verifier {
	t.Helper()
	v, err := NewVerifier(keys)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustRefuse(t *testing.T, v *Verifier, token string, at time.Time) {
	t.Helper()
	scope, err := v.Verify(token, at)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Verify(%q) = %+v, %v; want ErrInvalid", token, scope, err)
	}
	if scope != (Scope{}) {
		t.Fatalf("a refusal returned a non-zero scope %+v", scope)
	}
}

const kidA = "catalog-org/a"
const kidB = "catalog-org/b"

func TestAValidAssertionVerifiesToExactlyItsIdentities(t *testing.T) {
	k := key(t, 1)
	v := verifier(t, map[string]ed25519.PublicKey{kidA: pub(k)})
	scope, err := v.Verify(valid(k, kidA), now)
	if err != nil {
		t.Fatal(err)
	}
	if scope.StaffID != staff || scope.OrganizerID != organizer {
		t.Fatalf("scope = %+v, want staff %s organizer %s", scope, staff, organizer)
	}
}

// Every field is covered by the signature. Changing any one of them, without
// re-signing, must refuse. The kid case swaps to ANOTHER KNOWN kid so that the
// refusal cannot be explained by "unknown kid" alone: it must come from the
// signature covering the kid bytes (both keys are the same key here).
func TestTamperingWithAnyFieldRefuses(t *testing.T) {
	k := key(t, 1)
	v := verifier(t, map[string]ed25519.PublicKey{kidA: pub(k), kidB: pub(k)})
	token := valid(k, kidA)
	parts := strings.Split(token, ".")
	other := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	sigBytes, _ := base64.RawURLEncoding.DecodeString(parts[5])
	sigBytes[0] ^= 0x01
	cases := map[int]string{
		0: "v3",
		1: kidB,
		2: other.String(),
		3: other.String(),
		4: strconv.FormatInt(expiry.Add(time.Hour).Unix(), 10),
		5: base64.RawURLEncoding.EncodeToString(sigBytes),
	}
	names := []string{"version", "kid", "staff", "organizer", "expiry", "signature"}
	for i, replacement := range cases {
		t.Run(names[i], func(t *testing.T) {
			tampered := append([]string(nil), parts...)
			tampered[i] = replacement
			mustRefuse(t, v, strings.Join(tampered, "."), now)
		})
	}
}

func TestATokenSignedByAnotherKeyUnderAKnownKidRefuses(t *testing.T) {
	v := verifier(t, map[string]ed25519.PublicKey{kidA: pub(key(t, 1))})
	mustRefuse(t, v, valid(key(t, 2), kidA), now)
}

func TestExpiryBoundary(t *testing.T) {
	k := key(t, 1)
	v := verifier(t, map[string]ed25519.PublicKey{kidA: pub(k)})
	token := valid(k, kidA)
	if _, err := v.Verify(token, expiry.Add(-time.Second)); err != nil {
		t.Fatalf("one second before expiry: %v", err)
	}
	mustRefuse(t, v, token, expiry)
	mustRefuse(t, v, token, expiry.Add(time.Second))
}

// Each class of malformed input, signed correctly wherever the class allows it,
// so that the signature check cannot be the thing that refuses it.
func TestMalformedAssertionsRefuse(t *testing.T) {
	k := key(t, 1)
	v := verifier(t, map[string]ed25519.PublicKey{kidA: pub(k)})
	exp := strconv.FormatInt(expiry.Unix(), 10)
	s, o := staff.String(), organizer.String()
	shortSig := strings.Join([]string{"v2", kidA, s, o, exp}, ".") + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(k, []byte("x"))[:63])
	cases := map[string]string{
		// syntax
		"empty":             "",
		"five fields":       strings.Join([]string{"v2", kidA, s, o, exp}, "."),
		"seven fields":      sign(k, "v2", kidA, s, o, exp, "extra"),
		"signature not b64": strings.Join([]string{"v2", kidA, s, o, exp, "!!!!"}, "."),
		"padded b64 sig":    valid(k, kidA) + "=",
		// type
		"staff not uuid":     sign(k, "v2", kidA, "not-a-uuid", o, exp),
		"organizer not uuid": sign(k, "v2", kidA, s, "not-a-uuid", exp),
		"expiry not integer": sign(k, "v2", kidA, s, o, "soon"),
		"expiry fractional":  sign(k, "v2", kidA, s, o, exp+".5"),
		// absence
		"empty kid":       sign(k, "v2", "", s, o, exp),
		"empty staff":     sign(k, "v2", kidA, "", o, exp),
		"empty organizer": sign(k, "v2", kidA, s, "", exp),
		"empty expiry":    sign(k, "v2", kidA, s, o, ""),
		"empty signature": strings.Join([]string{"v2", kidA, s, o, exp, ""}, "."),
		// identity
		"nil staff":     sign(k, "v2", kidA, uuid.Nil.String(), o, exp),
		"nil organizer": sign(k, "v2", kidA, s, uuid.Nil.String(), exp),
		// range
		"zero expiry":          sign(k, "v2", kidA, s, o, "0"),
		"expiry leading space": sign(k, "v2", kidA, s, o, " "+exp),
		"negative expiry":      sign(k, "v2", kidA, s, o, "-1"),
		"overflowing expiry":   sign(k, "v2", kidA, s, o, "99999999999999999999"),
		"short signature":      shortSig,
		// version
		"unknown version": sign(k, "v3", kidA, s, o, exp),
		"empty version":   sign(k, "", kidA, s, o, exp),
		// kid
		"unknown kid": sign(k, "v2", "catalog-org/unknown", s, o, exp),
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) { mustRefuse(t, v, token, now) })
	}
}

// TKT-287 ai-review: one signature, one accepted spelling. Each case keeps the
// signed bytes valid and changes only how the signature or a field is WRITTEN, so
// a lenient decoder or parser would accept it.
func TestNonCanonicalSpellingsRefuse(t *testing.T) {
	k := key(t, 1)
	v := verifier(t, map[string]ed25519.PublicKey{kidA: pub(k)})
	token := valid(k, kidA)
	parts := strings.Split(token, ".")
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := parts[5][len(parts[5])-1]
	// 86 characters carry 516 bits for 512, so the last character's low 4 bits
	// are unused: flipping one leaves the decoded signature identical.
	trailing := parts[5][:len(parts[5])-1] + string(alphabet[strings.IndexByte(alphabet, last)^1])
	if got, _ := base64.RawURLEncoding.DecodeString(trailing); string(got) != func() string {
		b, _ := base64.RawURLEncoding.DecodeString(parts[5])
		return string(b)
	}() {
		t.Fatal("fixture: the trailing-bit spelling must decode to the same signature")
	}
	withSig := func(sig string) string { return strings.Join(append(append([]string(nil), parts[:5]...), sig), ".") }
	exp := strconv.FormatInt(expiry.Unix(), 10)
	compact := strings.ReplaceAll(staff.String(), "-", "")
	cases := map[string]string{
		"signature with unused trailing bits set": withSig(trailing),
		"signature with an embedded newline":      withSig(parts[5][:40] + "\n" + parts[5][40:]),
		"signature with an embedded CR":           withSig(parts[5][:40] + "\r" + parts[5][40:]),
		"compact staff uuid":                      sign(k, "v2", kidA, compact, organizer.String(), exp),
		"braced organizer uuid":                   sign(k, "v2", kidA, staff.String(), "{"+organizer.String()+"}", exp),
		"urn staff uuid":                          sign(k, "v2", kidA, "urn:uuid:"+staff.String(), organizer.String(), exp),
		"signed expiry":                           sign(k, "v2", kidA, staff.String(), organizer.String(), "+"+exp),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) { mustRefuse(t, v, tok, now) })
	}
}

// The HMAC v1 format this replaces (ADR-058) must not verify through any path:
// there is no transition window (TKT-287 D4).
func TestALegacyV1AssertionRefuses(t *testing.T) {
	v := verifier(t, map[string]ed25519.PublicKey{kidA: pub(key(t, 1))})
	payload := strings.Join([]string{"v1", staff.String(), organizer.String(), strconv.FormatInt(expiry.Unix(), 10)}, ".")
	mac := hmac.New(sha256.New, []byte("legacy-key"))
	mac.Write([]byte(payload))
	mustRefuse(t, v, payload+"."+base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), now)
}

// A Verifier that somehow holds no keys must refuse rather than admit.
func TestTheZeroVerifierRefuses(t *testing.T) {
	var v Verifier
	mustRefuse(t, &v, valid(key(t, 1), kidA), now)
	var nilV *Verifier
	mustRefuse(t, nilV, valid(key(t, 1), kidA), now)
}

func TestNewVerifierRefusesBadKeyMaterial(t *testing.T) {
	good := pub(key(t, 1))
	cases := map[string]map[string]ed25519.PublicKey{
		"no keys":        {},
		"short key":      {kidA: good[:31]},
		"kid namespace":  {"access-qr/a": good},
		"kid with dot":   {"catalog-org/a.b": good},
		"bare namespace": {"catalog-org/": good},
	}
	for name, keys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewVerifier(keys); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestParseKeyring(t *testing.T) {
	a, b := pub(key(t, 1)), pub(key(t, 2))
	enc := base64.RawStdEncoding.EncodeToString
	v, err := ParseKeyring(kidA + "=" + enc(a) + ", " + kidB + "=" + enc(b))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(valid(key(t, 2), kidB), now); err != nil {
		t.Fatalf("second kid did not verify: %v", err)
	}
	bad := map[string]string{
		"empty":         "",
		"no equals":     kidA + enc(a),
		"empty key":     kidA + "=",
		"bad base64":    kidA + "=!!!",
		"short key":     kidA + "=" + enc(a[:16]),
		"duplicate kid": kidA + "=" + enc(a) + "," + kidA + "=" + enc(b),
		"bad namespace": "access-qr/a=" + enc(a),
		"trailing sep":  kidA + "=" + enc(a) + ",",
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseKeyring(raw); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
