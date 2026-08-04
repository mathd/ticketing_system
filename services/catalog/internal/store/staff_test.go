package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// TKT-190 COS-4: "a wrong password and an unknown account are indistinguishable
// to the caller ... no short-circuit that skips the KDF when the account does
// not exist."
//
// Status codes alone cannot prove this — the two paths can return byte-identical
// 401s while differing by an order of magnitude in wall-clock, which is what a
// credential-enumeration attack reads. So the proof is a *call count* through the
// comparison seam: the unknown-identifier path must perform exactly as many KDF
// comparisons as the known-identifier path, against a hash of the same cost.
//
// staffDummyHash is deliberately a FIXED LITERAL and is NOT generated from
// hashPassword (the code under test). A fixture built from the implementation
// encodes the property it claims to prove: if hashPassword ever regressed to
// cost 4, a generated dummy would silently regress with it and this test would
// keep passing while the timing signal reappeared (the fixture-too-small trap,
// TKT-61 / docs/learnings/2026-08-03-a-fixture-too-small-cannot-show-the-negative.md).
func TestAuthenticateStaffComparesOnceForUnknownAndKnownAccounts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		identifier string
	}{
		{"unknown identifier", "nobody@example.test"},
		{"known identifier, wrong password", "boxoffice@example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			restore := swapCompareHashAndPassword(func(hashed, password []byte) error {
				calls++
				if cost, err := bcrypt.Cost(hashed); err != nil {
					t.Fatalf("compared against a value that is not a bcrypt hash: %v", err)
				} else if cost != staffBcryptCost {
					t.Fatalf("compared against cost %d, want %d — the two paths must cost the same", cost, staffBcryptCost)
				}
				return bcrypt.CompareHashAndPassword(hashed, password)
			})
			defer restore()

			s := newFakeStaffLookup()
			s.add("boxoffice@example.test", mustHash(t, "correct horse"))

			_, err := authenticateStaff(context.Background(), s, tc.identifier, "wrong password")
			if !errors.Is(err, ErrStaffCredentialsInvalid) {
				t.Fatalf("want ErrStaffCredentialsInvalid, got %v", err)
			}
			if calls != 1 {
				t.Fatalf("KDF comparisons = %d, want exactly 1 — an unknown account must not skip the comparison", calls)
			}
		})
	}
}

// ai-review F2. OpenAPI's maxLength counts CHARACTERS; bcrypt's 72 counts BYTES.
// So 72 multibyte characters clear the contract and are ~3x over bcrypt's limit,
// and bcrypt then returns ErrPasswordTooLong *without doing the work* — which
// strips away the only thing masking the cost difference between "row found,
// five columns scanned" and sql.ErrNoRows. An enumerator averaging that raw
// difference over many samples gets the account list, using a password that can
// never log anyone in.
//
// The fix is refusal before the lookup, so this asserts the DATABASE IS NEVER
// TOUCHED — not merely that the answer is "invalid". A version that looked up
// first and refused afterwards returns the same error and leaks the same timing.
func TestAuthenticateStaffRefusesOverlongPasswordsWithoutTouchingTheDatabase(t *testing.T) {
	// 72 characters, 216 bytes: passes maxLength: 72, fails bcrypt's byte bound.
	multibyte := strings.Repeat("é", 72)
	if len([]rune(multibyte)) != 72 {
		t.Fatalf("fixture must be exactly 72 CHARACTERS to clear the contract, got %d", len([]rune(multibyte)))
	}
	if len(multibyte) <= bcryptMaxPasswordBytes {
		t.Fatalf("fixture must exceed %d BYTES to reach the bug, got %d", bcryptMaxPasswordBytes, len(multibyte))
	}

	for _, tc := range []struct{ name, identifier string }{
		{"known identifier", "boxoffice@example.test"},
		{"unknown identifier", "nobody@example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeStaffLookup()
			s.add("boxoffice@example.test", mustHash(t, "correct horse"))

			var compares int
			restore := swapCompareHashAndPassword(func(hashed, password []byte) error {
				compares++
				return bcrypt.CompareHashAndPassword(hashed, password)
			})
			defer restore()

			_, err := authenticateStaff(context.Background(), s, tc.identifier, multibyte)
			if !errors.Is(err, ErrStaffCredentialsInvalid) {
				t.Fatalf("want ErrStaffCredentialsInvalid, got %v", err)
			}
			if s.lookups != 0 {
				t.Fatalf("the database was queried %d times; an over-long password must be refused "+
					"before the lookup, or the lookup's cost is measurable", s.lookups)
			}
			if compares != 0 {
				t.Fatalf("bcrypt was called %d times for an input it cannot hash", compares)
			}
		})
	}
}

// The dummy-hash path must not become an authentication bypass: even if the
// caller's password happens to match the dummy hash's plaintext, the answer is
// still invalid credentials.
func TestAuthenticateStaffRejectsThePasswordThatMatchesTheDummyHash(t *testing.T) {
	s := newFakeStaffLookup()
	_, err := authenticateStaff(context.Background(), s, "nobody@example.test", staffDummyPassword)
	if !errors.Is(err, ErrStaffCredentialsInvalid) {
		t.Fatalf("matching the dummy hash must not authenticate anyone: got %v", err)
	}
}

func TestAuthenticateStaffAcceptsTheCorrectPassword(t *testing.T) {
	s := newFakeStaffLookup()
	want := uuid.New()
	org := uuid.New()
	s.addAccount("boxoffice@example.test", mustHash(t, "correct horse"), StaffAccount{
		ID: want, OrganizerID: org, Identifier: "boxoffice@example.test", Role: "admin"})

	got, err := authenticateStaff(context.Background(), s, "boxoffice@example.test", "correct horse")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != want || got.OrganizerID != org {
		t.Fatalf("principal = %+v, want id %s org %s", got, want, org)
	}
}

// The lookup key is normalized so an operator who provisions "Box.Office@Example.TEST"
// can sign in as "boxoffice@example.test" — and, more importantly, so two accounts
// differing only by case cannot both exist and make login ambiguous.
func TestStaffIdentifierKeyNormalizes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Box.Office@Example.TEST", "box.office@example.test"},
		{"  spaced@example.test  ", "spaced@example.test"},
		{"already@example.test", "already@example.test"},
	} {
		if got := staffIdentifierKey(tc.in); got != tc.want {
			t.Fatalf("staffIdentifierKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// bcrypt silently ignores everything past 72 bytes, so a 200-character password
// and its 72-byte prefix would authenticate each other. Refuse at the door
// rather than store a credential that is weaker than it looks.
func TestHashPasswordRefusesOverlongAndEmptyPasswords(t *testing.T) {
	if _, err := hashPassword(strings.Repeat("a", 73)); !errors.Is(err, ErrStaffPasswordUnusable) {
		t.Fatalf("73-byte password must be refused, got %v", err)
	}
	if _, err := hashPassword(""); !errors.Is(err, ErrStaffPasswordUnusable) {
		t.Fatalf("empty password must be refused, got %v", err)
	}
	if _, err := hashPassword(strings.Repeat("a", 72)); err != nil {
		t.Fatalf("72-byte password is within bcrypt's input bound: %v", err)
	}
}

// Salting is what makes two identical passwords indistinguishable in the dump.
func TestHashPasswordSaltsEachHash(t *testing.T) {
	a, err := hashPassword("same password")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hashPassword("same password")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical — the KDF is not salting")
	}
	for _, h := range []string{a, b} {
		if strings.Contains(h, "same password") {
			t.Fatalf("hash contains the plaintext: %q", h)
		}
		if err := bcrypt.CompareHashAndPassword([]byte(h), []byte("same password")); err != nil {
			t.Fatalf("hash does not verify: %v", err)
		}
	}
}

func mustHash(t *testing.T, password string) string {
	t.Helper()
	h, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

// fakeStaffLookup stands in for the SQL lookup so the KDF-timing property is
// tested without Postgres; staff_smoke_test.go proves the real query.
type fakeStaffLookup struct {
	byKey map[string]staffCredential
	// lookups counts queries, so a test can prove the database was never
	// reached — not merely that the answer came back wrong.
	lookups int
}

func newFakeStaffLookup() *fakeStaffLookup {
	return &fakeStaffLookup{byKey: map[string]staffCredential{}}
}

func (f *fakeStaffLookup) add(identifier, hash string) {
	f.addAccount(identifier, hash, StaffAccount{ID: uuid.New(), OrganizerID: uuid.New(),
		Identifier: identifier, Role: "admin"})
}

func (f *fakeStaffLookup) addAccount(identifier, hash string, acct StaffAccount) {
	f.byKey[staffIdentifierKey(identifier)] = staffCredential{Account: acct, PasswordHash: hash}
}

func (f *fakeStaffLookup) lookupStaffCredential(_ context.Context, key string) (staffCredential, error) {
	f.lookups++
	c, ok := f.byKey[key]
	if !ok {
		return staffCredential{}, ErrNotFound
	}
	return c, nil
}
