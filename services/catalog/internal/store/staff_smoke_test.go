//go:build smoke

package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TKT-190 US-B1 against real Postgres. staff_test.go proves the KDF-timing
// property over an in-memory lookup; these prove the SQL — the migration, the
// uniqueness that makes sign-in-by-identifier unambiguous, the FK, and the fact
// that nothing readable in the row resembles the password.

func TestStaffAccountRoundTripAuthenticates(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)

	acct, err := st.CreateStaffAccount(ctx, StaffAccountInput{
		OrganizerID: seatMapOrg, Identifier: "Ada@Example.TEST", Role: "admin", Password: "correct horse",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if acct.ID == uuid.Nil {
		t.Fatal("provision must return the new id")
	}

	// Sign-in normalizes, so the operator's capitalization at provisioning time
	// does not decide what the human has to type months later.
	got, err := st.AuthenticateStaff(ctx, "  ada@example.test  ", "correct horse")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != acct.ID || got.OrganizerID != seatMapOrg {
		t.Fatalf("principal = %+v, want id %s org %s", got, acct.ID, seatMapOrg)
	}
	if got.Role != "admin" {
		t.Fatalf("role must round-trip even though nothing enforces it yet, got %q", got.Role)
	}

	if _, err := st.AuthenticateStaff(ctx, "ada@example.test", "wrong"); !errors.Is(err, ErrStaffCredentialsInvalid) {
		t.Fatalf("wrong password: want ErrStaffCredentialsInvalid, got %v", err)
	}
	if _, err := st.AuthenticateStaff(ctx, "nobody@example.test", "correct horse"); !errors.Is(err, ErrStaffCredentialsInvalid) {
		t.Fatalf("unknown identifier: want ErrStaffCredentialsInvalid, got %v", err)
	}
}

// The stored row must not contain the password in any form a dump would reveal.
// Asserting "it is a bcrypt string" is not the same claim: a column holding the
// plaintext ALSO fails to verify, silently, and only at sign-in time.
func TestStaffAccountStoresNoPlaintext(t *testing.T) {
	ctx, db, st, _ := seatMapSmokeStore(t)

	const password = "a-very-distinctive-passphrase"
	if _, err := st.CreateStaffAccount(ctx, StaffAccountInput{
		OrganizerID: seatMapOrg, Identifier: "leak@example.test", Role: "admin", Password: password,
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}

	var hash string
	if err := db.QueryRowContext(ctx,
		`SELECT password_hash FROM staff_accounts WHERE identifier_key = 'leak@example.test'`).Scan(&hash); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(hash, password) {
		t.Fatalf("the stored credential contains the plaintext: %q", hash)
	}
	if !strings.HasPrefix(hash, "$2") {
		t.Fatalf("stored credential is not a bcrypt modular-crypt string: %q", hash)
	}
}

// Sign-in resolves by identifier alone, so two accounts differing only by case
// would make "who is signing in?" ambiguous. The database refuses the second one
// rather than leaving the answer to whichever row the planner returns first.
func TestStaffIdentifierIsUniqueAcrossCaseAndOrganizers(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)

	if _, err := st.CreateStaffAccount(ctx, StaffAccountInput{
		OrganizerID: seatMapOrg, Identifier: "dup@example.test", Role: "admin", Password: "first",
	}); err != nil {
		t.Fatalf("first provision: %v", err)
	}
	if _, err := st.CreateStaffAccount(ctx, StaffAccountInput{
		OrganizerID: seatMapOrg, Identifier: "DUP@Example.test", Role: "admin", Password: "second",
	}); !errors.Is(err, ErrStaffIdentifierTaken) {
		t.Fatalf("case-variant duplicate: want ErrStaffIdentifierTaken, got %v", err)
	}
	// And the first account's password is untouched — a refused provision must
	// never be a password reset in disguise.
	if _, err := st.AuthenticateStaff(ctx, "dup@example.test", "first"); err != nil {
		t.Fatalf("the original credential must survive a refused re-provision: %v", err)
	}
}

func TestStaffAccountRequiresAKnownOrganizer(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)

	_, err := st.CreateStaffAccount(ctx, StaffAccountInput{
		OrganizerID: uuid.New(), Identifier: "orphan@example.test", Role: "admin", Password: "pw",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown organizer: want ErrNotFound, got %v", err)
	}
}

// The Down guard refuses to discard credentials. Rolling 0015 back with accounts
// present would lock every staff member out with no record of who existed.
func TestStaffAccountsMigrationRefusesToDropPopulatedTable(t *testing.T) {
	ctx, _, st, provider := seatMapSmokeStore(t)

	if _, err := st.CreateStaffAccount(ctx, StaffAccountInput{
		OrganizerID: seatMapOrg, Identifier: "guard@example.test", Role: "admin", Password: "pw",
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := provider.DownTo(ctx, versionBeforeStaffAccounts); err == nil {
		t.Fatal("rolling 0015 back with accounts present must fail loudly, not drop them")
	}
}
