package api

// Organizer assertions (TKT-245). See ADR-058.
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
// authorizes exactly one thing — naming the organizer a catalog write is for — and
// it is a BEARER token until it expires. ADR-021's question, answered: this stops
// a caller holding the staff-write credential from naming an organizer it has no
// session for. It stops nothing at all against someone holding the signing key,
// the back office's memory, or the database.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrOrganizerAssertionInvalid is the ONLY verification failure reported. Expired,
// forged, malformed and truncated are one answer: a caller probing the difference
// learns which of their guesses was structurally right, and none of the four
// should ever reach a well-behaved back office.
var ErrOrganizerAssertionInvalid = errors.New("invalid organizer assertion")

// organizerAssertionVersion prefixes every token. It costs three bytes and it is
// what makes changing the format later a migration rather than a mystery: an old
// token presented after a format change is refused as invalid, not misparsed.
const organizerAssertionVersion = "v1"

// organizerAssertionKey is the HMAC key. Catalog-only, and deliberately not
// INTERNAL_SERVICE_TOKEN (one shared value opening every service's internal
// surface) or CATALOG_STAFF_WRITE_TOKEN (the credential this assertion is
// presented ALONGSIDE — a key equal to it would let anyone who can write mint
// their own tenancy). main.go refuses to start when it equals either.
type organizerAssertionKey string

// organizerScope is what a verified assertion authorises. It is filled by the
// authentication func and read by the handlers; nothing in it ever comes from the
// request body.
//
// It carries the staff member as well as the organizer. Only the organizer is
// load-bearing today — catalog enforces no roles at all, they live in the back
// office (web/backoffice/src/lib/authorization.ts) — but the staff id is 36 signed
// bytes that save a canonical-format migration the first time anything needs the
// principal, and both fields are immutable per staff row (migration 0015).
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

// mintOrganizerAssertion produces `v1.<staff id>.<organizer id>.<unix expiry>.<mac>`.
//
// The payload is signed, not encrypted: a holder can read which staff member,
// which organizer, and when it dies. That is fine — they are that staff member —
// and it keeps the token debuggable. What they cannot do is change any field,
// because the MAC covers the exact bytes that are compared on the way back in.
func mintOrganizerAssertion(key organizerAssertionKey, staffID, organizerID uuid.UUID, expiresAt time.Time) string {
	payload := strings.Join([]string{
		organizerAssertionVersion,
		staffID.String(),
		organizerID.String(),
		strconv.FormatInt(expiresAt.Unix(), 10),
	}, ".")
	return payload + "." + organizerAssertionMAC(key, payload)
}

func organizerAssertionMAC(key organizerAssertionKey, payload string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyOrganizerAssertion returns the scope the token names, or
// ErrOrganizerAssertionInvalid.
//
// Order matters: the MAC is checked BEFORE the expiry is trusted, because the
// expiry is attacker-controlled until the signature says otherwise. Checking
// expiry first would mean parsing and believing an unauthenticated number — and an
// attacker who could pick it would simply pick one far in the future.
func verifyOrganizerAssertion(key organizerAssertionKey, token string, now time.Time) (organizerScope, error) {
	if len(key) == 0 {
		// Fail closed on an unconfigured key. Startup already refuses that, so this
		// defends against a future construction path that forgets — without it, an
		// empty key would still produce a self-consistent MAC and every forged token
		// would verify.
		return organizerScope{}, ErrOrganizerAssertionInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 5 || parts[0] != organizerAssertionVersion {
		return organizerScope{}, ErrOrganizerAssertionInvalid
	}
	payload := strings.Join(parts[:4], ".")
	// Constant-time: the comparison target is derived from a secret, and an
	// early-exit compare leaks how much of a forged MAC was right.
	if subtle.ConstantTimeCompare([]byte(parts[4]), []byte(organizerAssertionMAC(key, payload))) != 1 {
		return organizerScope{}, ErrOrganizerAssertionInvalid
	}
	staffID, err := uuid.Parse(parts[1])
	if err != nil {
		return organizerScope{}, ErrOrganizerAssertionInvalid
	}
	organizerID, err := uuid.Parse(parts[2])
	if err != nil {
		return organizerScope{}, ErrOrganizerAssertionInvalid
	}
	// The nil uuid is not an identity. It is what a zero value looks like, so
	// accepting it means a construction bug upstream arrives here as an
	// authenticated principal — and downstream it either trips a foreign key as a
	// 503 or, worse, writes rows nobody owns. Nothing legitimate ever mints one.
	if staffID == uuid.Nil || organizerID == uuid.Nil {
		return organizerScope{}, ErrOrganizerAssertionInvalid
	}
	expiry, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return organizerScope{}, ErrOrganizerAssertionInvalid
	}
	if !now.Before(time.Unix(expiry, 0)) {
		return organizerScope{}, ErrOrganizerAssertionInvalid
	}
	return organizerScope{StaffID: staffID, OrganizerID: organizerID}, nil
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
// replacement in the body would reintroduce the shape being removed.
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
// AuthenticateStaff checks for the key before calling this. Keep the empty-key
// return as a defence for direct construction paths; it must never enter a
// StaffPrincipal response.
func (s *Server) mintForStaff(staffID, organizerID uuid.UUID) string {
	if len(s.organizerAssertionKey) == 0 {
		return ""
	}
	return mintOrganizerAssertion(s.organizerAssertionKey, staffID, organizerID, time.Now().Add(OrganizerAssertionTTL))
}
