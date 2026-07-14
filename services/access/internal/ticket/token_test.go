package ticket

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSignedPayloadRejectsTamperingAndUnknownKey(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	signer, err := New(base64.RawStdEncoding.EncodeToString(seed), "test-v1")
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Payload(uuid.New(), uuid.New(), uuid.New(), uuid.New(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	private := ed25519.NewKeyFromSeed(seed)
	if _, err := Verify(token, map[string]ed25519.PublicKey{"test-v1": private.Public().(ed25519.PublicKey)}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := Verify(token, map[string]ed25519.PublicKey{}); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := Verify(corruptSignature(t, token), map[string]ed25519.PublicKey{"test-v1": private.Public().(ed25519.PublicKey)}); err == nil {
		t.Fatal("tampered token accepted")
	}
}

func corruptSignature(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) == 0 {
		t.Fatalf("decode signature: %v", err)
	}
	signature[0] ^= 1
	parts[2] = base64.RawURLEncoding.EncodeToString(signature)
	return strings.Join(parts, ".")
}

func TestVerifierRequiresDedicatedActiveKey(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	private := ed25519.NewKeyFromSeed(seed)
	public := base64.RawStdEncoding.EncodeToString(private.Public().(ed25519.PublicKey))
	keyID := "access-qr/test-v1"
	verifier, err := NewVerifier(keyID+"="+public, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = verifier.Verify("not-a-ticket"); err == nil {
		t.Fatal("invalid token accepted")
	}
	if _, err = NewVerifier("", keyID); err == nil {
		t.Fatal("missing keyring accepted")
	}
	if _, err = NewVerifier(keyID+"="+public, "other"); err == nil {
		t.Fatal("unqualified active key accepted")
	}
}

func TestVerifyRejectsWrongCredentialType(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	private := ed25519.NewKeyFromSeed(seed)
	claims := Claims{Version: 1, TicketID: uuid.New(), OrderID: uuid.New(), OrganizerID: uuid.New(), SlotID: uuid.New(), IssuedAt: time.Now().Unix()}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","kid":"access-qr/test-v1","typ":"OTHER"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encoded := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	token := encoded + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(encoded)))
	if _, err = Verify(token, map[string]ed25519.PublicKey{"access-qr/test-v1": private.Public().(ed25519.PublicKey)}); err == nil {
		t.Fatal("wrong credential type accepted")
	}
}
