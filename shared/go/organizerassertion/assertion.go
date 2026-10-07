// Package organizerassertion verifies the organizer assertion that catalog mints
// for a signed-in staff member (ADR-058, v2 since TKT-287).
//
// The wire format is
//
//	v2.<kid>.<staff id>.<organizer id>.<unix expiry>.<signature>
//
// where the signature is Ed25519 over the exact bytes of the first five fields
// joined by dots, raw-URL base64. Catalog alone holds the private key and mints
// (services/catalog/internal/api/assertion.go). Any service verifies with the
// public key, which grants nothing on its own: a process that holds only this
// package's keys can check an assertion and cannot make one. That is the whole
// reason the format is asymmetric (TKT-287 D1).
//
// This package holds VERIFICATION material only. Signing stays in catalog, so a
// service importing it gains no minting path in code either.
package organizerassertion

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Version prefixes every token. A token of any other version is refused, never
// misparsed: the HMAC v1 format has no transition window (TKT-287 D4).
const Version = "v2"

// KIDNamespace prefixes every key id, so a key from another keyring (access's
// QR keys use `access-qr/`) cannot be configured here by mistake.
const KIDNamespace = "catalog-org/"

// signatureChars is an Ed25519 signature (64 bytes) in unpadded base64url.
const signatureChars = 86

var (
	canonicalUUID   = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
	canonicalExpiry = regexp.MustCompile(`^[0-9]{1,19}$`)
)

// ErrInvalid is the ONLY verification failure reported. Expired, forged,
// malformed and unknown-key are one answer: a caller probing the difference
// learns which of its guesses was structurally right.
var ErrInvalid = errors.New("invalid organizer assertion")

// Scope is what a verified assertion authorises: which staff member, acting for
// which organizer. Nothing in it comes from anywhere but the signed bytes.
type Scope struct {
	StaffID     uuid.UUID
	OrganizerID uuid.UUID
}

// Verifier holds public keys by kid. The zero value and a nil pointer refuse
// every token.
type Verifier struct{ keys map[string]ed25519.PublicKey }

// ValidKID reports whether kid is a well-formed key id: the namespace, then 1–64
// characters of [A-Za-z0-9_-]. No dot, because the dot is the field separator.
func ValidKID(kid string) bool {
	rest, ok := strings.CutPrefix(kid, KIDNamespace)
	if !ok || rest == "" || len(rest) > 64 {
		return false
	}
	for _, c := range rest {
		if !kidChar(c) {
			return false
		}
	}
	return true
}

func kidChar(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		return true
	}
	return false
}

// NewVerifier builds a verifier from kid → public key. It refuses an empty set,
// a malformed kid and a key of the wrong length.
func NewVerifier(keys map[string]ed25519.PublicKey) (*Verifier, error) {
	if len(keys) == 0 {
		return nil, errors.New("organizer assertion keyring is empty")
	}
	copied := make(map[string]ed25519.PublicKey, len(keys))
	for kid, key := range keys {
		if !ValidKID(kid) {
			return nil, fmt.Errorf("invalid organizer assertion key id %q", kid)
		}
		if len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("organizer assertion public key %q is not an Ed25519 public key", kid)
		}
		copied[kid] = append(ed25519.PublicKey(nil), key...)
	}
	return &Verifier{keys: copied}, nil
}

// ParseKeyring parses `kid=<raw-standard-base64 public key>` entries separated
// by commas — the same shape as access's ACCESS_QR_PUBLIC_KEYS.
func ParseKeyring(raw string) (*Verifier, error) {
	keys := make(map[string]ed25519.PublicKey)
	for _, item := range strings.Split(raw, ",") {
		kid, encoded, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || encoded == "" {
			return nil, errors.New("invalid organizer assertion keyring entry")
		}
		key, err := base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("organizer assertion public key %q is not raw-standard base64", kid)
		}
		if _, duplicate := keys[kid]; duplicate {
			return nil, fmt.Errorf("duplicate organizer assertion key id %q", kid)
		}
		keys[kid] = ed25519.PublicKey(key)
	}
	return NewVerifier(keys)
}

// Verify returns the scope the token names, or ErrInvalid.
//
// Order matters: the signature is checked BEFORE any field is trusted, because
// every field — the expiry above all — is attacker-controlled until then.
func (v *Verifier) Verify(token string, now time.Time) (Scope, error) {
	if v == nil {
		return Scope{}, ErrInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 6 || parts[0] != Version {
		return Scope{}, ErrInvalid
	}
	key, ok := v.keys[parts[1]]
	if !ok {
		return Scope{}, ErrInvalid
	}
	// Exactly the documented 86 characters, decoded STRICTLY: the default decoder
	// ignores CR/LF and the unused trailing bits of the last character, which would
	// give one signature several accepted spellings.
	if len(parts[5]) != signatureChars {
		return Scope{}, ErrInvalid
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return Scope{}, ErrInvalid
	}
	if !ed25519.Verify(key, []byte(strings.Join(parts[:5], ".")), sig) {
		return Scope{}, ErrInvalid
	}
	// The contract's syntax, checked BEFORE the lenient parsers: uuid.Parse also
	// takes the compact, braced and urn forms, and strconv.ParseInt takes a sign.
	// Catalog never mints those, so only the private-key holder could produce one,
	// but a verifier that accepts more than the contract declares is a second
	// definition of the format.
	if !canonicalUUID.MatchString(parts[2]) || !canonicalUUID.MatchString(parts[3]) || !canonicalExpiry.MatchString(parts[4]) {
		return Scope{}, ErrInvalid
	}
	staffID, err := uuid.Parse(parts[2])
	if err != nil {
		return Scope{}, ErrInvalid
	}
	organizerID, err := uuid.Parse(parts[3])
	if err != nil {
		return Scope{}, ErrInvalid
	}
	// The nil uuid is what a zero value looks like, not an identity. Nothing
	// legitimate mints one.
	if staffID == uuid.Nil || organizerID == uuid.Nil {
		return Scope{}, ErrInvalid
	}
	expiry, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return Scope{}, ErrInvalid
	}
	if !now.Before(time.Unix(expiry, 0)) {
		return Scope{}, ErrInvalid
	}
	return Scope{StaffID: staffID, OrganizerID: organizerID}, nil
}
