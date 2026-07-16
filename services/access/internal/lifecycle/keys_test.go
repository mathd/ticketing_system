package lifecycle

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func newKeyMaterial(t *testing.T) (seed string, public string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawStdEncoding.EncodeToString(priv.Seed()), base64.RawStdEncoding.EncodeToString(pub)
}

func signerAndKeyring(t *testing.T, kid string) (*Signer, *Keyring) {
	t.Helper()
	seed, pub := newKeyMaterial(t)
	s, err := NewSigner(seed, kid)
	if err != nil {
		t.Fatal(err)
	}
	k, err := NewKeyring(kid + "=" + pub)
	if err != nil {
		t.Fatal(err)
	}
	return s, k
}

func headHash(b byte) []byte {
	h := make([]byte, HashSize)
	h[0] = b
	return h
}

func TestSignedHeadVerifies(t *testing.T) {
	s, k := signerAndKeyring(t, "access-lifecycle/2026-07")
	h := headHash(0xa1)
	sig := s.SignHead(goldTicket, 3, h)
	if err := k.VerifyHead(goldTicket, 3, s.KeyID(), h, sig); err != nil {
		t.Fatalf("freshly signed head did not verify: %v", err)
	}
}

// ADR-021 §D5 binds ticket, sequence, key id and head hash into the signature.
// Each mutation below is an attack the signature must reject; a signature over
// the bare head hash would pass the first two.
func TestVerifyHeadRejectsEveryMutation(t *testing.T) {
	s, k := signerAndKeyring(t, "access-lifecycle/2026-07")
	h := headHash(0xa1)
	sig := s.SignHead(goldTicket, 3, h)

	other := uuid.MustParse("20000000-0000-0000-0000-00000000000f")
	cases := map[string]func() error{
		"replayed onto another ticket": func() error { return k.VerifyHead(other, 3, s.KeyID(), h, sig) },
		"rolled-back sequence":         func() error { return k.VerifyHead(goldTicket, 2, s.KeyID(), h, sig) },
		"substituted head hash":        func() error { return k.VerifyHead(goldTicket, 3, s.KeyID(), headHash(0xa2), sig) },
		"unknown key id":               func() error { return k.VerifyHead(goldTicket, 3, "access-lifecycle/nope", h, sig) },
		"corrupted signature":          func() error { return k.VerifyHead(goldTicket, 3, s.KeyID(), h, append([]byte{}, make([]byte, 64)...)) },
	}
	for name, verify := range cases {
		if err := verify(); err == nil {
			t.Fatalf("%s: verification accepted a head it must reject", name)
		}
	}
}

// The key namespace is the whole point of ADR-021 §D4: a leaked QR signing key
// must not also authorize rewriting history.
func TestSignerRejectsForeignNamespace(t *testing.T) {
	seed, _ := newKeyMaterial(t)
	for _, kid := range []string{"access-qr/2026-07", "2026-07", "", "access-lifecycleX/2026-07"} {
		if _, err := NewSigner(seed, kid); err == nil {
			t.Fatalf("signer accepted kid %q outside the access-lifecycle/ namespace", kid)
		}
	}
}

func TestKeyringRejectsForeignNamespace(t *testing.T) {
	_, pub := newKeyMaterial(t)
	for _, raw := range []string{"access-qr/2026-07=" + pub, "2026-07=" + pub} {
		if _, err := NewKeyring(raw); err == nil {
			t.Fatalf("keyring accepted %q: QR material must never verify lifecycle history", raw)
		}
	}
}

// A key id is signed INSIDE the canonical head and checkpoint forms, which are
// newline-separated. A kid carrying a newline shifts every field after it, so
// "kid\n<hash>" with one head hash and "kid" with another canonicalize to the
// same bytes — one signature verifying as two different heads. '=' and ',' would
// separately misparse the keyring. Reaching this needs operator-controlled
// config, not the database adversary; it is closed anyway because an ambiguous
// canonical form is worth none of the argument.
func TestKeyIDsCannotSmuggleACanonicalDelimiter(t *testing.T) {
	seed, pub := newKeyMaterial(t)
	hostile := map[string]string{
		"newline":         "access-lifecycle/a\n0000000000000000000000000000000000000000000000000000000000000000",
		"carriage return": "access-lifecycle/a\rb",
		"equals":          "access-lifecycle/a=b",
		"comma":           "access-lifecycle/a,b",
	}
	for name, kid := range hostile {
		if _, err := NewSigner(seed, kid); err == nil {
			t.Fatalf("%s: signer accepted a kid that can shift fields in the bytes it signs", name)
		}
		if _, err := NewKeyring(kid + "=" + pub); err == nil {
			t.Fatalf("%s: keyring accepted a kid that can shift fields in the bytes it verifies", name)
		}
	}
}

func TestSignerRejectsMalformedSeed(t *testing.T) {
	for _, seed := range []string{"", "not-base64!!", base64.RawStdEncoding.EncodeToString([]byte("short"))} {
		if _, err := NewSigner(seed, "access-lifecycle/2026-07"); err == nil {
			t.Fatalf("signer accepted malformed seed %q", seed)
		}
	}
}

// Rotation retains historical public keys so pre-rotation heads and epoch
// signatures stay verifiable (ADR-021 §D4/§D5).
func TestKeyringVerifiesUnderRetiredKey(t *testing.T) {
	retiredSeed, retiredPub := newKeyMaterial(t)
	_, currentPub := newKeyMaterial(t)
	retired, err := NewSigner(retiredSeed, "access-lifecycle/2026-06")
	if err != nil {
		t.Fatal(err)
	}
	k, err := NewKeyring("access-lifecycle/2026-06=" + retiredPub + ",access-lifecycle/2026-07=" + currentPub)
	if err != nil {
		t.Fatal(err)
	}
	h := headHash(0xb2)
	sig := retired.SignHead(goldTicket, 9, h)
	if err := k.VerifyHead(goldTicket, 9, "access-lifecycle/2026-06", h, sig); err != nil {
		t.Fatalf("retired key could not verify the epoch it signed: %v", err)
	}
	if !k.Has("access-lifecycle/2026-07") || !k.Has("access-lifecycle/2026-06") {
		t.Fatal("keyring lost a key it parsed")
	}
	if k.Has("access-lifecycle/2026-08") {
		t.Fatal("keyring claims a key it never parsed")
	}
}

func TestKeyringRejectsDuplicateAndEmpty(t *testing.T) {
	_, pub := newKeyMaterial(t)
	if _, err := NewKeyring("access-lifecycle/a=" + pub + ",access-lifecycle/a=" + pub); err == nil {
		t.Fatal("duplicate key id accepted: which key verifies would depend on map order")
	}
	if _, err := NewKeyring(""); err == nil {
		t.Fatal("empty keyring accepted")
	}
	if _, err := NewKeyring("access-lifecycle/a=not-base64!!"); err == nil {
		t.Fatal("malformed public key accepted")
	}
}

func TestSignedCheckpointVerifies(t *testing.T) {
	s, k := signerAndKeyring(t, "access-lifecycle/2026-07")
	c := goldenCheckpoint()
	c.KeyID = s.KeyID()
	sig := s.SignCheckpoint(c)
	if err := k.VerifyCheckpoint(c, sig); err != nil {
		t.Fatalf("freshly signed checkpoint did not verify: %v", err)
	}
	c.Root = headHash(0xff)
	if err := k.VerifyCheckpoint(c, sig); err == nil {
		t.Fatal("checkpoint signature survived a substituted root")
	}
}

// SignCheckpoint names the signing key in the bytes it signs, so a caller
// cannot persist one kid while having signed under another.
func TestSignCheckpointBindsTheSigningKey(t *testing.T) {
	s, k := signerAndKeyring(t, "access-lifecycle/2026-07")
	c := goldenCheckpoint()
	c.KeyID = "access-lifecycle/lies"
	sig := s.SignCheckpoint(c)
	if err := k.VerifyCheckpoint(c, sig); err == nil {
		t.Fatal("checkpoint verified against a key id it was not signed under")
	}
	c.KeyID = s.KeyID()
	if err := k.VerifyCheckpoint(c, sig); err != nil {
		t.Fatalf("checkpoint did not verify under its true signing key: %v", err)
	}
}

// Domain separation, exercised rather than assumed: head bytes and checkpoint
// bytes must not be interchangeable under one key.
func TestHeadSignatureCannotVerifyAsCheckpoint(t *testing.T) {
	s, k := signerAndKeyring(t, "access-lifecycle/2026-07")
	c := goldenCheckpoint()
	c.KeyID = s.KeyID()
	headSig := s.SignHead(goldTicket, 3, headHash(0xa1))
	if err := k.VerifyCheckpoint(c, headSig); err == nil {
		t.Fatal("a head signature verified as a checkpoint: domains are not separated")
	}
}

// verify-lifecycle runs with public keys only (ADR-021 §D7). The keyring is the
// type it uses, so it must be incapable of holding a private key.
func TestKeyringHoldsPublicMaterialOnly(t *testing.T) {
	_, pub := newKeyMaterial(t)
	k, err := NewKeyring("access-lifecycle/2026-07=" + pub)
	if err != nil {
		t.Fatal(err)
	}
	for kid, key := range k.keys {
		if len(key) != ed25519.PublicKeySize {
			t.Fatalf("key %q is %d bytes; a keyring must hold public material only", kid, len(key))
		}
	}
}

func TestKeyringToleratesWhitespaceBetweenEntries(t *testing.T) {
	_, a := newKeyMaterial(t)
	_, b := newKeyMaterial(t)
	k, err := NewKeyring("access-lifecycle/a=" + a + ", access-lifecycle/b=" + b)
	if err != nil {
		t.Fatal(err)
	}
	if !k.Has("access-lifecycle/b") {
		t.Fatal("a padded keyring entry was dropped")
	}
}

func TestKeyNamespaceIsDistinctFromQR(t *testing.T) {
	if !strings.HasPrefix(KeyNamespace, "access-lifecycle/") || strings.HasPrefix(KeyNamespace, "access-qr/") {
		t.Fatalf("KeyNamespace = %q", KeyNamespace)
	}
}
