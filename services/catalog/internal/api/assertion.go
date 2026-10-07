package api

// Organizer assertions (TKT-245; Ed25519 v2 since TKT-287). See ADR-058.
//
// The problem this exists for: ADR-042 put staff accounts in catalog and the
// SESSION in the back-office process, and the two never speak about a specific
// signed-in staff member afterwards. So when a write arrives, catalog has no way
// to know which organizer it is for — and every unsafe operation took
// `organizer_id` from the request body and believed it. The back office passed its
// session's organizer and never a form value, but that is a discipline in one
// codebase, not a boundary catalog can enforce (services/catalog/internal/api/
// server.go, and ADR-053, which states the assumption rather than implying a
// boundary that is not there).
//
// The two obvious answers are both wrong, and commerce already refused both for
// the identically shaped customer problem (ADR-049 § TKT-221): an `organizer_id`
// in the body is exactly what we are removing, and a per-organizer credential
// authenticates a MACHINE confined to one tenant — which is the right shape for a
// reseller (ADR-056) and the wrong one for an interactive tool where the tenant
// is a property of the signed-in human, not of the process.
//
// So catalog signs a statement it can verify later without storing anything:
// "this is staff member S, acting for organizer O, until T". The staff member
// earns it by presenting their password; the back office keeps it in its
// in-process session, server-side only, and forwards it on the writes it proxies.
//
// What it is NOT: a session, a refresh token, or a general-purpose credential. It
// authorizes exactly one thing — naming the organizer a write is for — and it is
// a BEARER token until it expires. ADR-021's question, answered: this stops a
// caller holding a staff-write credential from naming an organizer it has no
// session for. It stops nothing at all against someone holding the private key,
// the back office's memory, or the database.
//
// Since TKT-287 the signature is Ed25519, not HMAC. Catalog alone holds the
// private key. A service that holds only the public key can verify the same
// token (shared/go/organizerassertion) and mint nothing; commerce is that service
// from TKT-287 part 2. The verifier lives in the shared package so every
// verifier parses one canonical form; the
// signer lives here, so no other service has a minting path even in code.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"ticketing/shared/organizerassertion"
)

// ErrOrganizerAssertionInvalid is the ONLY verification failure reported. Expired,
// forged, malformed and truncated are one answer: a caller probing the difference
// learns which of its guesses was structurally right, and none of the four should
// ever reach a well-behaved back office.
var ErrOrganizerAssertionInvalid = organizerassertion.ErrInvalid

// OrganizerAssertionSigner holds catalog's Ed25519 private key, its key id, and
// the verifier for its own public key. Catalog verifies with the key derived from
// its seed, so the two can never disagree. There is no second keyring: ADR-058
// has no rotation overlap, and rotating invalidates every live assertion.
type OrganizerAssertionSigner struct {
	private  ed25519.PrivateKey
	kid      string
	verifier *organizerassertion.Verifier
}

// NewOrganizerAssertionSigner decodes a raw-standard-base64 Ed25519 seed (the
// `access keygen` format) and a `catalog-org/` key id.
//
// Errors name the problem and never echo the seed.
func NewOrganizerAssertionSigner(seedBase64, kid string) (*OrganizerAssertionSigner, error) {
	seed, err := DecodeOrganizerAssertionSeed(seedBase64)
	if err != nil {
		return nil, err
	}
	if !organizerassertion.ValidKID(kid) {
		return nil, fmt.Errorf("organizer assertion key id must be %q followed by 1-64 of [A-Za-z0-9_-]", organizerassertion.KIDNamespace)
	}
	private := ed25519.NewKeyFromSeed(seed)
	verifier, err := organizerassertion.NewVerifier(map[string]ed25519.PublicKey{
		kid: private.Public().(ed25519.PublicKey),
	})
	if err != nil {
		return nil, err
	}
	return &OrganizerAssertionSigner{private: private, kid: kid, verifier: verifier}, nil
}

// DecodeOrganizerAssertionSeed decodes a 32-byte Ed25519 seed in canonical
// raw-standard base64, and accepts NO other spelling of it. Go's decoder, even in
// Strict mode, ignores CR and LF, and its default mode also ignores the unused
// trailing bits of the last character, so the decoded bytes are re-encoded and
// compared with the input. One seed therefore has exactly one accepted string.
func DecodeOrganizerAssertionSeed(seedBase64 string) ([]byte, error) {
	seed, err := base64.RawStdEncoding.DecodeString(seedBase64)
	if err != nil || base64.RawStdEncoding.EncodeToString(seed) != seedBase64 {
		return nil, errors.New("organizer assertion signing key is not canonical raw-standard base64")
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("organizer assertion signing key must decode to a %d-byte Ed25519 seed", ed25519.SeedSize)
	}
	return seed, nil
}

// EncodesOrganizerAssertionSeed reports whether a holder of value holds the seed:
// value IS the 32 seed bytes, or decodes to them as base64 or hex. Base64 is
// tried once, after normalizing every spelling Go or a shell would also accept —
// whitespace removed, the URL-safe alphabet folded into the standard one, padding
// dropped, unused trailing bits ignored — rather than variant by variant, so a
// mixed or reformatted alias cannot fall between two checks (TKT-287 ai-review).
//
// Name the limit: this catches a credential that IS the seed in a common text
// form, which is what a misconfiguration produces. It cannot catch an operator
// who deliberately derives one secret from the other in an encoding of their own
// (base32, a KDF); no comparison can, and that is not the adversary this guard
// is for.
func EncodesOrganizerAssertionSeed(value string, seed []byte) bool {
	if bytes.Equal([]byte(value), seed) {
		return true
	}
	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, value)
	normalized := strings.NewReplacer("-", "+", "_", "/", "=", "").Replace(compact)
	if decoded, err := base64.RawStdEncoding.DecodeString(normalized); err == nil && bytes.Equal(decoded, seed) {
		return true
	}
	decoded, err := hex.DecodeString(compact)
	return err == nil && bytes.Equal(decoded, seed)
}

// organizerScope is what a verified assertion authorises. It is filled by the
// authentication func and read by the handlers; nothing in it ever comes from the
// request body.
//
// It carries the staff member as well as the organizer. Only the organizer is
// load-bearing today — catalog enforces no roles at all, they live in the back
// office (web/backoffice/src/lib/authorization.ts) — but both fields are
// immutable per staff row (migration 0015).
//
// The ROLE is deliberately absent, and that absence is load-bearing: ADR-042
// snapshots role at sign-in and warns it goes stale the day a role-change surface
// lands. Signing it would make catalog authoritative about a fact it cannot
// refresh, so a demoted staff member would carry their old role until the token
// expired. A test pins the payload shape so a future field is a deliberate
// canonical-format change rather than an accident.
type organizerScope struct {
	StaffID     uuid.UUID
	OrganizerID uuid.UUID
}

// mintOrganizerAssertion produces
// `v2.<kid>.<staff id>.<organizer id>.<unix expiry>.<signature>`.
//
// The payload is signed, not encrypted: a holder can read which staff member,
// which organizer, and when it dies. That is fine — they are that staff member —
// and it keeps the token debuggable. What they cannot do is change any field,
// because the signature covers the exact bytes the verifier checks.
func mintOrganizerAssertion(s *OrganizerAssertionSigner, staffID, organizerID uuid.UUID, expiresAt time.Time) string {
	payload := strings.Join([]string{
		organizerassertion.Version,
		s.kid,
		staffID.String(),
		organizerID.String(),
		strconv.FormatInt(expiresAt.Unix(), 10),
	}, ".")
	return payload + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.private, []byte(payload)))
}

// verifyOrganizerAssertion returns the scope the token names, or
// ErrOrganizerAssertionInvalid. A nil signer refuses everything: a catalog that
// cannot check an assertion must not admit one.
func verifyOrganizerAssertion(s *OrganizerAssertionSigner, token string, now time.Time) (organizerScope, error) {
	if s == nil {
		return organizerScope{}, ErrOrganizerAssertionInvalid
	}
	scope, err := s.verifier.Verify(token, now)
	if err != nil {
		return organizerScope{}, ErrOrganizerAssertionInvalid
	}
	return organizerScope{StaffID: scope.StaffID, OrganizerID: scope.OrganizerID}, nil
}

// OrganizerAssertionTTL is how long a minted assertion lives.
//
// It is EXACTLY the back office's session lifetime, and the equality is the point:
// if the assertion were shorter, a staff member who signed in and worked through a
// long authoring session would be refused mid-edit, with no way back except
// signing in again. The back office cannot re-mint one — it holds the principal,
// not the password.
//
// Equal TTLs are NOT sufficient on their own, and the back office is where that is
// handled. Catalog mints at T1 on ITS clock; the back office stamps its session at
// T2 after the round trip, so a session created at T2 with an 8h assertion
// outlives the assertion by the round trip plus any clock skew — surfacing near the
// boundary as a 401 on a session the back office still believes is live. The back
// office therefore parses this expiry back out and clamps its session to
// min(SESSION_TTL_MS, assertion lifetime); see web/backoffice/src/lib/session.ts.
// The same shape, for the same reason, as the storefront's customer assertion.
//
// Keep this equal to SESSION_TTL_MS in web/backoffice/src/lib/session.ts. They are
// two constants in two languages; there is no mechanism that enforces the
// equality, which is why it is written down in both places and in ADR-058.
const OrganizerAssertionTTL = 8 * time.Hour

// organizerAssertionHeader carries the token. A header, not the body: the whole
// point is that the request body cannot name an organizer, so putting the
// replacement in the body would reintroduce the shape being removed. Commerce
// takes the same header name in TKT-287 part 2 (D6), so the back office forwards
// one session value to both.
const organizerAssertionHeader = "X-Catalog-Organizer-Assertion"

// organizerAssertionSecurityScheme is the securityScheme name in the contract.
// Compared against what the document declares by a test in write_credential_test.go:
// a scheme renamed in the spec and not here would stop matching and the guard would
// refuse everything, which is at least loud — but a HEADER renamed in the spec and
// not here would silently read a header nobody sends.
const organizerAssertionSecurityScheme = "CatalogOrganizerAssertion"

// mintForStaff is the one place an assertion is created, so the TTL cannot drift
// between call sites.
//
// AuthenticateStaff checks for the signer before calling this. Keep the nil
// return as a defence for direct construction paths; it must never enter a
// StaffPrincipal response.
func (s *Server) mintForStaff(staffID, organizerID uuid.UUID) string {
	if s.organizerAssertions == nil {
		return ""
	}
	return mintOrganizerAssertion(s.organizerAssertions, staffID, organizerID, time.Now().Add(OrganizerAssertionTTL))
}
