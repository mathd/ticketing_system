package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// TKT-220 / ADR-049. Customer sign-in must not tell a caller which addresses are
// registered, and a status code cannot prove that: the unknown-account and the
// wrong-password paths can return byte-identical 401s while differing by an order
// of magnitude in wall-clock, which is exactly what an enumerator measures.
//
// So the proof is a CALL COUNT through the comparison seam — the unknown-address
// path must perform the same number of KDF comparisons, against a hash of the
// same cost, as the known-address path.
//
// customerDummyHash is a FIXED LITERAL, generated out of band and pasted into
// customer.go. It is deliberately not produced by hashCustomerPassword (the code
// under test): a fixture built from the implementation encodes the property it
// claims to prove, so a regression in the cost constant would move the dummy with
// it and this test would keep passing while the timing signal came back
// (docs/learnings/2026-08-03-a-fixture-too-small-cannot-show-the-negative.md).
func TestAuthenticateCustomerComparesOnceForUnknownAndKnownAccounts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		email string
	}{
		{"unknown address", "nobody@example.test"},
		{"known address, wrong password", "buyer@example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			restore := swapCompareCustomerHashAndPassword(func(hashed, password []byte) error {
				calls++
				cost, err := bcrypt.Cost(hashed)
				if err != nil {
					t.Fatalf("compared against a value that is not a bcrypt hash: %v", err)
				}
				if cost != customerBcryptCost {
					t.Fatalf("compared against cost %d, want %d — both paths must cost the same", cost, customerBcryptCost)
				}
				return bcrypt.CompareHashAndPassword(hashed, password)
			})
			defer restore()

			lookup := newFakeCustomerLookup()
			lookup.add("buyer@example.test", mustHashCustomer(t, "correct horse battery"))

			_, err := authenticateCustomer(context.Background(), lookup, tc.email, "wrong password")
			if !errors.Is(err, ErrCustomerCredentialsInvalid) {
				t.Fatalf("want ErrCustomerCredentialsInvalid, got %v", err)
			}
			if calls != 1 {
				t.Fatalf("KDF comparisons = %d, want exactly 1 — an unknown address must not skip the comparison", calls)
			}
		})
	}
}

// Even a caller who somehow submits the dummy password itself must be refused:
// `found` gates the answer, not the comparison's verdict.
func TestAuthenticateCustomerRefusesTheDummyPassword(t *testing.T) {
	lookup := newFakeCustomerLookup()

	_, err := authenticateCustomer(context.Background(), lookup, "nobody@example.test", customerDummyPassword)
	if !errors.Is(err, ErrCustomerCredentialsInvalid) {
		t.Fatalf("the dummy password must never authenticate anyone, got %v", err)
	}
}

// OpenAPI's maxLength counts CHARACTERS; bcrypt's 72 counts BYTES. 72 multibyte
// characters therefore clear the contract and are three times over bcrypt's
// limit — and past it bcrypt returns ErrPasswordTooLong WITHOUT doing the work,
// which strips away the only thing masking the cost difference between "row
// found, columns scanned" and sql.ErrNoRows.
//
// So this asserts the DATABASE IS NEVER TOUCHED, not merely that the answer is
// "invalid": a version that looked up first and refused afterwards returns the
// same error and leaks the same timing.
func TestAuthenticateCustomerRefusesOverlongPasswordsWithoutTouchingTheDatabase(t *testing.T) {
	multibyte := strings.Repeat("é", 72)
	if len([]rune(multibyte)) != 72 {
		t.Fatalf("fixture must be exactly 72 CHARACTERS to clear the contract, got %d", len([]rune(multibyte)))
	}
	if len(multibyte) <= bcryptMaxPasswordBytes {
		t.Fatalf("fixture must exceed bcrypt's %d-BYTE bound, got %d", bcryptMaxPasswordBytes, len(multibyte))
	}

	lookup := newFakeCustomerLookup()
	lookup.add("buyer@example.test", mustHashCustomer(t, "correct horse battery"))

	_, err := authenticateCustomer(context.Background(), lookup, "buyer@example.test", multibyte)
	if !errors.Is(err, ErrCustomerCredentialsInvalid) {
		t.Fatalf("want ErrCustomerCredentialsInvalid, got %v", err)
	}
	if lookup.calls != 0 {
		t.Fatalf("database lookups = %d, want 0 — refusal must happen before the lookup", lookup.calls)
	}
}

// The address is stored twice on purpose: `email` keeps what the buyer typed so a
// page can show it back, `email_key` is the normalized form every lookup goes
// through. Conflating them is the failure where registration succeeds and the
// account can never sign in.
func TestCustomerEmailKeyNormalizesButAuthenticationReturnsTheOriginalSpelling(t *testing.T) {
	const typed = "  Buyer.Person@Example.TEST  "
	const key = "buyer.person@example.test"

	if got := customerEmailKey(typed); got != key {
		t.Fatalf("customerEmailKey(%q) = %q, want %q", typed, got, key)
	}

	lookup := newFakeCustomerLookup()
	lookup.addWithDisplay(key, strings.TrimSpace(typed), mustHashCustomer(t, "correct horse battery"))

	// Sign-in spells it differently again — a third casing, more padding.
	account, err := authenticateCustomer(context.Background(), lookup, "\tBUYER.PERSON@example.test ", "correct horse battery")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if account.Email != strings.TrimSpace(typed) {
		t.Fatalf("account.Email = %q, want the ORIGINAL spelling %q", account.Email, strings.TrimSpace(typed))
	}
}

func TestHashCustomerPasswordRefusesWhatBcryptWouldNotFaithfullyStore(t *testing.T) {
	for _, tc := range []struct {
		name     string
		password string
	}{
		{"empty", ""},
		// Past 72 bytes bcrypt ignores the tail, so a 200-byte password and its
		// 72-byte prefix would authenticate each other. Refuse rather than store
		// a credential weaker than it looks.
		{"longer than bcrypt's byte bound", strings.Repeat("a", bcryptMaxPasswordBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := hashCustomerPassword(tc.password); !errors.Is(err, ErrCustomerPasswordUnusable) {
				t.Fatalf("want ErrCustomerPasswordUnusable, got %v", err)
			}
		})
	}
}

func TestHashCustomerPasswordSaltsSoTwoIdenticalPasswordsDiffer(t *testing.T) {
	first, err := hashCustomerPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	second, err := hashCustomerPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two hashes of the same password are identical — the KDF is not salting")
	}
	if strings.Contains(first, "correct horse battery") {
		t.Fatal("the stored value contains the plaintext")
	}
}

// --- fake lookup ---

type fakeCustomerLookup struct {
	byKey map[string]customerCredential
	calls int
}

func newFakeCustomerLookup() *fakeCustomerLookup {
	return &fakeCustomerLookup{byKey: map[string]customerCredential{}}
}

func (f *fakeCustomerLookup) add(email, hash string) {
	f.addWithDisplay(customerEmailKey(email), email, hash)
}

func (f *fakeCustomerLookup) addWithDisplay(key, display, hash string) {
	f.byKey[key] = customerCredential{
		Account:      CustomerAccount{ID: uuid.New(), Email: display},
		PasswordHash: hash,
	}
}

func (f *fakeCustomerLookup) lookupCustomerCredential(_ context.Context, emailKey string) (customerCredential, error) {
	f.calls++
	c, ok := f.byKey[emailKey]
	if !ok {
		return customerCredential{}, errCustomerNotFound
	}
	return c, nil
}

func mustHashCustomer(t *testing.T, password string) string {
	t.Helper()
	h, err := hashCustomerPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
