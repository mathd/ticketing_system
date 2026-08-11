package store

// Back-office staff accounts (TKT-190, US-B1). Catalog owns organizers and
// tenant-scoped configuration (ADR-002), so the humans who administer an
// organizer live here too — no sixth service, no database on the gateway.
// See ADR-042 for the placement decision and its adversary boundary.
//
// This file owns credential storage and verification ONLY. Roles are persisted
// but never interpreted here; TKT-191 defines and enforces role semantics.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

var (
	// ErrStaffCredentialsInvalid is the ONLY authentication failure this package
	// reports. An unknown identifier and a wrong password are the same error by
	// construction, so no caller can accidentally branch them apart and leak
	// which accounts exist (COS-4).
	ErrStaffCredentialsInvalid = errors.New("invalid staff credentials")

	// ErrStaffIdentifierTaken reports a provisioning collision. Provisioning is
	// create-only: an existing account is never silently overwritten, so a typo
	// in the bootstrap command cannot reset a live account's password.
	ErrStaffIdentifierTaken = errors.New("staff identifier already provisioned")

	// ErrStaffPasswordUnusable reports a password bcrypt cannot faithfully store.
	ErrStaffPasswordUnusable = errors.New("staff password unusable")

	// ErrStaffInvalidInput reports a provisioning argument the schema would
	// reject anyway, caught before the KDF so a typo costs no work factor.
	ErrStaffInvalidInput = errors.New("invalid staff account input")
)

// staffBcryptCost is the KDF work factor. Both the real and the dummy hash are
// pinned to it: a dummy of a different cost would restore the timing signal
// COS-4 exists to remove.
const staffBcryptCost = bcrypt.DefaultCost // 10

// bcryptMaxPasswordBytes is bcrypt's hard input bound. Past it the algorithm
// silently ignores the tail, so a 200-byte password and its 72-byte prefix would
// authenticate each other. Refuse rather than store a credential weaker than it
// looks.
const bcryptMaxPasswordBytes = 72

// staffDummyPassword / staffDummyHash are the fixed comparison target for an
// unknown identifier. The hash is a LITERAL, generated once out of band and
// pasted here — never produced by hashPassword at init. A fixture derived from
// the code under test encodes the property it is meant to prove: it would track
// a regression in the cost constant instead of failing on it (TKT-61's
// fixture-too-small trap). staff_test.go asserts its cost independently.
const (
	staffDummyPassword = "tkt-190 dummy comparison password"
	staffDummyHash     = "$2a$10$E9yELIafSmhNMdM6EDHQFuiJxaMj7rTIkv8UDUFNr9Sffsfb7PskG"
)

// compareHashAndPassword is the KDF seam. Production always uses bcrypt; tests
// swap it to COUNT comparisons, which is the only way to prove the unknown-account
// path does not short-circuit past the KDF. Asserting equal status codes cannot
// prove it — the two paths can return identical bytes and still differ by an
// order of magnitude in wall-clock, which is exactly what an enumerator measures.
var compareHashAndPassword = bcrypt.CompareHashAndPassword

// swapCompareHashAndPassword installs a comparison function and returns a restore
// func. Test-only helper; it lives here rather than in _test.go so the seam and
// its single legitimate use are read together.
func swapCompareHashAndPassword(fn func(hashed, password []byte) error) func() {
	prev := compareHashAndPassword
	compareHashAndPassword = fn
	return func() { compareHashAndPassword = prev }
}

// StaffAccount is a back-office principal. It carries no password material:
// nothing above the store ever sees a hash.
type StaffAccount struct {
	ID          uuid.UUID
	OrganizerID uuid.UUID
	Identifier  string
	Role        string
}

// StaffAccountInput provisions one account. Password is plaintext in memory for
// the length of the call and is never logged, returned, or stored.
type StaffAccountInput struct {
	OrganizerID uuid.UUID
	Identifier  string
	Role        string
	Password    string
}

// staffCredential is the internal join of a principal and its stored hash. It
// never leaves this package.
type staffCredential struct {
	Account      StaffAccount
	PasswordHash string
}

// staffLookup is the seam between credential verification (pure, testable
// without Postgres) and the query that resolves an identifier. The real
// implementation is *Postgres; staff_test.go substitutes an in-memory one.
type staffLookup interface {
	lookupStaffCredential(ctx context.Context, identifierKey string) (staffCredential, error)
}

// staffIdentifierKey is the normalized lookup form. Sign-in is by identifier
// alone, so the key is UNIQUE across the whole table rather than per organizer:
// two organizers holding "admin@example.com" would make "who is signing in?"
// ambiguous, and v1 has a single organizer anyway. Multi-organizer sign-in needs
// an organizer selector at the login form, which is out of scope here.
// NormalizeStaffIdentifier exposes the lookup key to the API layer, which needs
// to rate-limit per identifier (TKT-195). It delegates rather than duplicating,
// and that is the point: a limiter keyed on its OWN idea of normalization would
// give "Ada" and "ada" separate budgets, so an attacker grinding one account
// would simply vary the case. Two normalizers is the bypass.
func NormalizeStaffIdentifier(identifier string) string { return staffIdentifierKey(identifier) }

func staffIdentifierKey(identifier string) string {
	return strings.ToLower(strings.TrimSpace(identifier))
}

// hashPassword produces the stored credential. bcrypt salts internally, so two
// provisions of the same password yield different hashes.
func hashPassword(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("%w: empty", ErrStaffPasswordUnusable)
	}
	if len(password) > bcryptMaxPasswordBytes {
		return "", fmt.Errorf("%w: longer than %d bytes, which bcrypt would silently truncate",
			ErrStaffPasswordUnusable, bcryptMaxPasswordBytes)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), staffBcryptCost)
	if err != nil {
		// Deliberately not %w-wrapping bcrypt's error: it is the only remaining
		// failure mode and wrapping invites a caller to inspect it.
		return "", fmt.Errorf("%w: %v", ErrStaffPasswordUnusable, err)
	}
	return string(h), nil
}

// authenticateStaff verifies a credential in constant shape: exactly one KDF
// comparison whether or not the identifier resolves, against a hash of the same
// cost either way. The unknown-identifier branch compares against staffDummyHash
// and then discards the result — even a caller who somehow submits
// staffDummyPassword is refused, because `found` gates the answer, not the
// comparison's verdict.
func authenticateStaff(ctx context.Context, s staffLookup, identifier, password string) (StaffAccount, error) {
	// Refuse an over-long password BEFORE the lookup, and without touching the
	// database. bcrypt returns ErrPasswordTooLong *without doing the work* past
	// 72 bytes, so letting such an input through would skip the KDF — and the
	// KDF is the only thing masking the cost difference between a row that was
	// found (five columns scanned and allocated) and sql.ErrNoRows. That is the
	// enumeration oracle this function exists to close, reopened by a password
	// nobody can log in with anyway.
	//
	// The contract's maxLength cannot catch this: OpenAPI counts CHARACTERS and
	// bcrypt counts BYTES, so 72 multibyte characters pass validation and are
	// three times over the limit. Refusing here is uniform across identifiers —
	// every caller gets the same immediate answer — so it introduces no oracle
	// of its own.
	if len(password) > bcryptMaxPasswordBytes {
		return StaffAccount{}, ErrStaffCredentialsInvalid
	}

	cred, err := s.lookupStaffCredential(ctx, staffIdentifierKey(identifier))
	found := err == nil
	switch {
	case found:
	case errors.Is(err, ErrNotFound):
		cred = staffCredential{PasswordHash: staffDummyHash}
	default:
		return StaffAccount{}, fmt.Errorf("lookup staff credential: %w", err)
	}

	matched := compareHashAndPassword([]byte(cred.PasswordHash), []byte(password)) == nil
	if !found || !matched {
		return StaffAccount{}, ErrStaffCredentialsInvalid
	}
	return cred.Account, nil
}

// --- Postgres ---

func (p *Postgres) lookupStaffCredential(ctx context.Context, identifierKey string) (staffCredential, error) {
	var c staffCredential
	err := p.db.QueryRowContext(ctx, `
		SELECT id, organizer_id, identifier, role, password_hash
		  FROM staff_accounts
		 WHERE identifier_key = $1`, identifierKey).
		Scan(&c.Account.ID, &c.Account.OrganizerID, &c.Account.Identifier, &c.Account.Role, &c.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return staffCredential{}, ErrNotFound
	}
	if err != nil {
		return staffCredential{}, fmt.Errorf("lookup staff account: %w", err)
	}
	return c, nil
}

// AuthenticateStaff resolves a staff principal from an identifier and password.
func (p *Postgres) AuthenticateStaff(ctx context.Context, identifier, password string) (StaffAccount, error) {
	return authenticateStaff(ctx, p, identifier, password)
}

// CreateStaffAccount provisions one account. Create-only: a colliding identifier
// is ErrStaffIdentifierTaken, never an overwrite.
func (p *Postgres) CreateStaffAccount(ctx context.Context, in StaffAccountInput) (StaffAccount, error) {
	identifier := strings.TrimSpace(in.Identifier)
	role := strings.TrimSpace(in.Role)
	if identifier == "" {
		return StaffAccount{}, fmt.Errorf("%w: empty identifier", ErrStaffInvalidInput)
	}
	if role == "" {
		return StaffAccount{}, fmt.Errorf("%w: empty role", ErrStaffInvalidInput)
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		return StaffAccount{}, err
	}
	acct := StaffAccount{OrganizerID: in.OrganizerID, Identifier: identifier, Role: role}
	err = p.db.QueryRowContext(ctx, `
		INSERT INTO staff_accounts (organizer_id, identifier, identifier_key, role, password_hash)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		in.OrganizerID, acct.Identifier, staffIdentifierKey(identifier), acct.Role, hash).Scan(&acct.ID)
	switch {
	case isUniqueViolation(err):
		return StaffAccount{}, ErrStaffIdentifierTaken
	case isFKViolation(err):
		return StaffAccount{}, fmt.Errorf("organizer: %w", ErrNotFound)
	case err != nil:
		return StaffAccount{}, fmt.Errorf("insert staff account: %w", err)
	}
	return acct, nil
}
