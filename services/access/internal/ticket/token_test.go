package ticket

import (
	"crypto/ed25519"
	"encoding/base64"
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
	parts := strings.Split(token, ".")
	parts[1] = "A" + parts[1][1:]
	if _, err := Verify(strings.Join(parts, "."), map[string]ed25519.PublicKey{"test-v1": private.Public().(ed25519.PublicKey)}); err == nil {
		t.Fatal("tampered token accepted")
	}
}
