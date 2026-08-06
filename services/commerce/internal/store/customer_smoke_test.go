//go:build smoke

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Customer accounts against real Postgres (TKT-220 / US-A1, migration 0015).
//
// The unit tests in customer_test.go prove the credential logic through a fake
// lookup. These prove the things only the database can refuse: the uniqueness
// that registration relies on instead of a pre-check, the tripwire on plaintext,
// and a Down that will not silently discard credentials.

// uniqueEmail keeps parallel runs and repeat runs against the same store-test
// database from colliding on the UNIQUE key.
func uniqueEmail(local string) string {
	return local + "+" + uuid.NewString()[:8] + "@example.test"
}

func TestRegisterCustomerRoundTripsThroughTheNormalizedKey(t *testing.T) {
	db, ctx := outboxDB(t)

	typed := "  " + strings.ToUpper(uniqueEmail("Buyer")) + "  "
	account, err := RegisterCustomer(ctx, db, typed, "correct horse battery")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if account.Email != strings.TrimSpace(typed) {
		t.Fatalf("stored display email = %q, want the ORIGINAL spelling %q", account.Email, strings.TrimSpace(typed))
	}

	// Sign in with a different casing and different padding. This is the failure
	// where registration succeeds and the account can never sign in.
	signedIn, err := AuthenticateCustomer(ctx, db, "\t"+strings.ToLower(strings.TrimSpace(typed))+" ", "correct horse battery")
	if err != nil {
		t.Fatalf("authenticate with a differently-spelled address: %v", err)
	}
	if signedIn.ID != account.ID {
		t.Fatalf("authenticated a different account: %s != %s", signedIn.ID, account.ID)
	}

	// And the wrong password on a KNOWN address is the same refusal as an
	// unknown one — asserted here against the real query, not only the fake.
	if _, err := AuthenticateCustomer(ctx, db, typed, "wrong password"); !errors.Is(err, ErrCustomerCredentialsInvalid) {
		t.Fatalf("wrong password: want ErrCustomerCredentialsInvalid, got %v", err)
	}
	if _, err := AuthenticateCustomer(ctx, db, uniqueEmail("nobody"), "wrong password"); !errors.Is(err, ErrCustomerCredentialsInvalid) {
		t.Fatalf("unknown address: want ErrCustomerCredentialsInvalid, got %v", err)
	}
}

// Registration has no SELECT-then-INSERT pre-check, deliberately: that is a race
// two concurrent registrations both win. The UNIQUE constraint is the authority,
// and this pins that its violation is what surfaces — including when the second
// attempt spells the address differently.
func TestRegisterCustomerRefusesADuplicateAddressWhateverItsSpelling(t *testing.T) {
	db, ctx := outboxDB(t)

	email := uniqueEmail("dup")
	if _, err := RegisterCustomer(ctx, db, email, "correct horse battery"); err != nil {
		t.Fatalf("first registration: %v", err)
	}

	for _, spelling := range []string{email, strings.ToUpper(email), "  " + email + "  "} {
		if _, err := RegisterCustomer(ctx, db, spelling, "a different password"); !errors.Is(err, ErrCustomerEmailTaken) {
			t.Fatalf("re-registering %q: want ErrCustomerEmailTaken, got %v", spelling, err)
		}
	}

	// Create-only: the original password still works, so a re-registration
	// cannot be used to reset someone else's account.
	if _, err := AuthenticateCustomer(ctx, db, email, "correct horse battery"); err != nil {
		t.Fatalf("the original credential must survive a duplicate registration attempt: %v", err)
	}
}

// The CHECK is a tripwire, not security. Its job is to make a plaintext password
// written straight into the column fail LOUDLY rather than become a credential
// that silently never matches.
func TestCustomerPasswordHashColumnRefusesPlaintext(t *testing.T) {
	db, ctx := outboxDB(t)

	email := uniqueEmail("plaintext")
	_, err := db.ExecContext(ctx, `
		INSERT INTO customer_accounts (email, email_key, password_hash) VALUES ($1, $2, $3)`,
		email, customerEmailKey(email), "correct horse battery")
	if err == nil {
		t.Fatal("the database accepted a plaintext password_hash — the tripwire is not armed")
	}
	if !strings.Contains(err.Error(), "customer_accounts_password_hash_check") {
		t.Fatalf("refused, but not by the password_hash CHECK: %v", err)
	}
}

// email_key must already be normalized when it is written: the CHECK is what
// stops a direct writer inserting a mixed-case key that no sign-in can ever
// resolve. Adversary named per ADR-021 — this constrains a careless writer with
// database access, not a hostile one, who can drop the constraint.
func TestCustomerEmailKeyColumnRefusesAnUnnormalizedKey(t *testing.T) {
	db, ctx := outboxDB(t)

	email := uniqueEmail("Unnormalized")
	_, err := db.ExecContext(ctx, `
		INSERT INTO customer_accounts (email, email_key, password_hash) VALUES ($1, $2, $3)`,
		email, "  "+strings.ToUpper(email)+"  ", customerDummyHash)
	if err == nil {
		t.Fatal("the database accepted an unnormalized email_key")
	}
	if !strings.Contains(err.Error(), "customer_accounts_email_key_check") {
		t.Fatalf("refused, but not by the email_key CHECK: %v", err)
	}
}

// Rolling 0015 back with accounts present must fail and leave the table intact.
// Run against the MIGRATION database (its own schema) so it cannot disturb the
// store-test database every other test shares.
func TestCustomerAccountsDownRefusesToDiscardCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	email := uniqueEmail("rollback")
	if _, err := RegisterCustomer(ctx, db, email, "correct horse battery"); err != nil {
		t.Fatalf("register: %v", err)
	}

	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("0015 rolled back with an account present — credentials would have been discarded silently")
	}

	// The refusal must leave the row, not half-drop the table.
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM customer_accounts`).Scan(&count); err != nil {
		t.Fatalf("the table did not survive the refused rollback: %v", err)
	}
	if count != 1 {
		t.Fatalf("account count after the refused rollback = %d, want 1", count)
	}
}
