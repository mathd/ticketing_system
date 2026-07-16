package lifecycle

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// KeyNamespace scopes lifecycle signing material. ADR-021 §D4 mandates key
// material distinct from the QR namespace (`access-qr/`, see
// internal/ticket/token.go) so a leaked credential-signing key does not also
// authorize rewriting history. The two namespaces never mix: this package
// refuses anything outside its own, and token.go refuses anything outside
// `access-qr/`.
const KeyNamespace = "access-lifecycle/"

// Signer holds the active lifecycle private key. It is deliberately absent from
// the verification path: `access verify-lifecycle` builds a Keyring and no
// Signer at all, which is what makes ADR-021 §D4's "a third party can verify the
// trail without holding the power to write it" true rather than aspirational.
type Signer struct {
	private ed25519.PrivateKey
	kid     string
}

// Keyring holds lifecycle public keys, current and historical. Rotation retains
// the old ones so pre-rotation heads and epoch signatures stay verifiable
// (ADR-021 §D5).
type Keyring struct{ keys map[string]ed25519.PublicKey }

// validKID checks a key id is namespaced and cannot smuggle a delimiter.
//
// The delimiter check is not tidiness. Key ids are signed INSIDE the canonical
// head and checkpoint forms, which are newline-separated, so a kid containing a
// newline shifts every field after it: "kid\n<hash>" and "kid" + a different
// hash can canonicalize to the same bytes, and a signature over one verifies as
// the other. `=` and `,` would separately misparse the keyring, which splits on
// exactly those. Reaching this needs operator-controlled configuration rather
// than the database-write adversary, so it is robustness rather than a live
// hole — but an ambiguous canonical form is worth none of the argument.
func validKID(kid string) error {
	if !strings.HasPrefix(kid, KeyNamespace) || kid == KeyNamespace {
		return fmt.Errorf("lifecycle key id %q must use the %s namespace", kid, KeyNamespace)
	}
	if strings.ContainsAny(kid, "\n\r=,") {
		return fmt.Errorf("lifecycle key id %q may not contain a newline, '=' or ',': it is signed inside the canonical form", kid)
	}
	return nil
}

// NewSigner loads the active lifecycle seed. Mirrors the injected seed + kid
// idiom already established for QR credentials (token.go:32-44) rather than
// inventing a second configuration shape.
func NewSigner(seedBase64, kid string) (*Signer, error) {
	if err := validKID(kid); err != nil {
		return nil, fmt.Errorf("ACCESS_LIFECYCLE_KID: %w", err)
	}
	seed, err := base64.RawStdEncoding.DecodeString(seedBase64)
	if err != nil {
		return nil, fmt.Errorf("decode ACCESS_LIFECYCLE_PRIVATE_KEY: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("ACCESS_LIFECYCLE_PRIVATE_KEY must be an Ed25519 seed")
	}
	return &Signer{private: ed25519.NewKeyFromSeed(seed), kid: kid}, nil
}

// KeyID is the key id every head and checkpoint this signer produces is bound to.
func (s *Signer) KeyID() string { return s.kid }

// SignHead signs a ticket head. Used both for the live head and, at rotation,
// for the outgoing key's epoch signature (ADR-021 §D5) — the same bytes either
// way, which is why the epoch signature verifies with the ordinary head path.
func (s *Signer) SignHead(ticketID uuid.UUID, sequence int64, headHash []byte) []byte {
	return ed25519.Sign(s.private, CanonicalHead(ticketID, sequence, s.kid, headHash))
}

// SignCheckpoint signs a checkpoint, naming this signer's key inside the signed
// bytes. c is a copy, so a caller cannot persist one key id while having signed
// under another.
func (s *Signer) SignCheckpoint(c Checkpoint) []byte {
	c.KeyID = s.kid
	return ed25519.Sign(s.private, CanonicalCheckpoint(c))
}

// NewKeyring parses the lifecycle keyring: a comma-separated list of
// "access-lifecycle/<kid>=<base64 raw Ed25519 public key>".
//
// Unlike the QR verifier (token.go:64), no active kid is required: verification
// is an offline, public-key-only operation over history that may be signed
// entirely under retired keys. Callers that also sign (the server, the backfill)
// check Has(activeKID) themselves.
func NewKeyring(raw string) (*Keyring, error) {
	keys := make(map[string]ed25519.PublicKey)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		kid, encoded, ok := strings.Cut(item, "=")
		if !ok || encoded == "" {
			return nil, fmt.Errorf("invalid ACCESS_LIFECYCLE_PUBLIC_KEYS entry %q", item)
		}
		if err := validKID(kid); err != nil {
			return nil, fmt.Errorf("ACCESS_LIFECYCLE_PUBLIC_KEYS: %w", err)
		}
		key, err := base64.RawStdEncoding.DecodeString(encoded)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid lifecycle public key for %q", kid)
		}
		if _, duplicate := keys[kid]; duplicate {
			return nil, fmt.Errorf("duplicate lifecycle key %q", kid)
		}
		keys[kid] = ed25519.PublicKey(key)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("ACCESS_LIFECYCLE_PUBLIC_KEYS must list at least one %s key", KeyNamespace)
	}
	return &Keyring{keys: keys}, nil
}

// Has reports whether the keyring can verify signatures made under kid.
func (k *Keyring) Has(kid string) bool { return len(k.keys[kid]) == ed25519.PublicKeySize }

func (k *Keyring) verify(kid string, message, sig []byte) error {
	key := k.keys[kid]
	if len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("unknown lifecycle key id %q", kid)
	}
	if !ed25519.Verify(key, message, sig) {
		return fmt.Errorf("invalid lifecycle signature under key %q", kid)
	}
	return nil
}

// VerifyHead checks a head signature. keyID comes from the stored row, so an
// unknown key id is itself a finding (ADR-021 §Threat model lists it among the
// cryptographically detected cases).
func (k *Keyring) VerifyHead(ticketID uuid.UUID, sequence int64, keyID string, headHash, sig []byte) error {
	return k.verify(keyID, CanonicalHead(ticketID, sequence, keyID, headHash), sig)
}

// VerifyCheckpoint checks a checkpoint signature under the key id the checkpoint
// itself names.
func (k *Keyring) VerifyCheckpoint(c Checkpoint, sig []byte) error {
	return k.verify(c.KeyID, CanonicalCheckpoint(c), sig)
}
