//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Partner credentials against real Postgres (TKT-240 / ADR-056, migration 0020).
//
// These live at the store tier because the mechanism they assert IS the SQL: the
// revocation predicate is in the WHERE clause, the hash is what the column holds,
// and the scope is what the row returns. An assertion one tier up would prove that
// a fake and a handler agree, and nothing about what ships.

func migratedDB(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

// The plaintext token must not be recoverable from the database, and the stored
// form must not be the token.
//
// Stated as the requirement rather than as "the column equals sha256(token)":
// asserting the specific digest would pin the ALGORITHM, and this test is about
// the property (the secret is not at rest). The digest is pinned by the fact that
// authentication works at all, below.
func TestResellerTokenIsNotRecoverableFromTheDatabase(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	org, reseller := uuid.New(), uuid.New()

	cred, token, err := EnrolResellerCredential(ctx, db, org, reseller, "reseller-acme", "ACME Tickets")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(token) == "" {
		t.Fatal("enrolment returned an empty token; the partner has nothing to present")
	}

	var stored string
	if err := db.QueryRowContext(ctx,
		`SELECT token_hash FROM reseller_credentials WHERE id = $1`, cred.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == token {
		t.Fatal("the token is stored in plaintext: a database dump hands an attacker every partner's credential")
	}
	if strings.Contains(stored, token) || strings.Contains(token, stored) {
		t.Fatalf("the stored form contains the token: stored=%q", stored)
	}

	// And the whole row must not carry it anywhere else — a hash in token_hash is
	// worth nothing if the label or another column echoes the secret.
	var rowText string
	if err := db.QueryRowContext(ctx,
		`SELECT reseller_credentials::text FROM reseller_credentials WHERE id = $1`, cred.ID).Scan(&rowText); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rowText, token) {
		t.Fatal("some column of the credential row contains the plaintext token")
	}
}

// Authentication resolves a live credential and returns THE SCOPE IT WAS ISSUED
// FOR — not merely "yes".
//
// The scope assertion is the point. A credential that authenticates but does not
// carry its organizer and channel is what lets the caller fall back on the
// request's values, which is ADR-053's cross-tenant defect exactly.
func TestAuthenticateReturnsTheScopeTheCredentialWasIssuedFor(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	org, reseller := uuid.New(), uuid.New()

	issued, token, err := EnrolResellerCredential(ctx, db, org, reseller, "reseller-acme", "ACME Tickets")
	if err != nil {
		t.Fatal(err)
	}
	got, err := AuthenticateResellerCredential(ctx, db, token)
	if err != nil {
		t.Fatalf("a freshly issued credential must authenticate: %v", err)
	}
	if got.ID != issued.ID {
		t.Fatalf("resolved credential id = %s, want %s", got.ID, issued.ID)
	}
	if got.OrganizerID != org {
		t.Fatalf("resolved organizer = %s, want %s — a credential that does not carry its own scope forces the caller to trust the request", got.OrganizerID, org)
	}
	if got.ResellerID != reseller {
		t.Fatalf("resolved reseller = %s, want %s — settlement cannot split by an identity that is not returned", got.ResellerID, reseller)
	}
	if got.ChannelCode != "reseller-acme" {
		t.Fatalf("resolved channel = %q, want %q", got.ChannelCode, "reseller-acme")
	}
}

// A revoked credential is refused IMMEDIATELY — on the very next call, with no
// cache to expire and no sweeper to run.
//
// The fixture deliberately authenticates successfully FIRST. Without that, a test
// that only checks the post-revocation refusal cannot distinguish "revocation
// works" from "this token never worked" — it would pass against a broken enrolment,
// a wrong hash, or a typo in the fixture.
func TestARevokedCredentialIsRefusedOnTheVeryNextCall(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)

	cred, token, err := EnrolResellerCredential(ctx, db, uuid.New(), uuid.New(), "reseller-acme", "ACME Tickets")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AuthenticateResellerCredential(ctx, db, token); err != nil {
		t.Fatalf("precondition: the credential must work before revocation, else this test proves nothing: %v", err)
	}
	if err := RevokeResellerCredential(ctx, db, cred.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := AuthenticateResellerCredential(ctx, db, token); !errors.Is(err, ErrResellerCredentialUnknown) {
		t.Fatalf("a revoked credential authenticated, or failed with the wrong error: %v", err)
	}
	// Revocation is the operator's intent, and repeating it is not an error.
	if err := RevokeResellerCredential(ctx, db, cred.ID); err != nil {
		t.Fatalf("revoking an already-revoked credential must succeed: %v", err)
	}
}

// Every failing lookup reports the SAME error, so a partner integration cannot
// tell "revoked" from "never existed" and use the difference to enumerate.
func TestEveryFailingLookupIsIndistinguishable(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)

	cred, token, err := EnrolResellerCredential(ctx, db, uuid.New(), uuid.New(), "reseller-acme", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	if err := RevokeResellerCredential(ctx, db, cred.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, token string }{
		{"revoked", token},
		{"never issued", "00000000000000000000000000000000"},
		{"empty", ""},
		{"whitespace", "   "},
		{"not hex", "hello, world"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := AuthenticateResellerCredential(ctx, db, tc.token); !errors.Is(err, ErrResellerCredentialUnknown) {
				t.Fatalf("want ErrResellerCredentialUnknown, got %v", err)
			}
		})
	}
}

// Channel codes are exact and unnormalized (ADR-024): a credential issued for
// "reseller-acme" is not a credential for "Reseller-ACME".
//
// This is a REFUSAL test, and its fixture is built so the refusal can fail: the
// enrolled code and the probed codes differ only by case and whitespace, which is
// exactly what a normalizing implementation would collapse.
func TestChannelScopeIsExactAndNeverFolded(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)

	cred, token, err := EnrolResellerCredential(ctx, db, uuid.New(), uuid.New(), "reseller-acme", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	got, err := AuthenticateResellerCredential(ctx, db, token)
	if err != nil {
		t.Fatal(err)
	}
	if got.ChannelCode != "reseller-acme" {
		t.Fatalf("channel round-tripped as %q; ADR-024 requires the exact string", got.ChannelCode)
	}
	for _, other := range []string{"Reseller-ACME", "RESELLER-ACME", " reseller-acme", "reseller-acme "} {
		if got.ChannelCode == other {
			t.Fatalf("channel %q compared equal to %q: the code was folded somewhere", got.ChannelCode, other)
		}
	}
	_ = cred
}

// Rotation is enrol-then-revoke, and the live-uniqueness index must not block it.
//
// Written from the requirement ("a partner can be re-issued a credential after a
// leak") rather than from what the index happens to do: an index that also
// counted revoked rows would make a leaked credential permanently unreplaceable
// for that (organizer, channel, reseller).
func TestARevokedCredentialDoesNotBlockItsReplacement(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	org, reseller := uuid.New(), uuid.New()

	first, firstToken, err := EnrolResellerCredential(ctx, db, org, reseller, "reseller-acme", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	if err := RevokeResellerCredential(ctx, db, first.ID); err != nil {
		t.Fatal(err)
	}
	second, secondToken, err := EnrolResellerCredential(ctx, db, org, reseller, "reseller-acme", "ACME rotated")
	if err != nil {
		t.Fatalf("a revoked credential must not block re-issuing one for the same scope: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("rotation returned the same credential id")
	}
	if secondToken == firstToken {
		t.Fatal("rotation returned the same token; the replacement is not a new secret")
	}
	if _, err := AuthenticateResellerCredential(ctx, db, secondToken); err != nil {
		t.Fatalf("the replacement credential must authenticate: %v", err)
	}
	if _, err := AuthenticateResellerCredential(ctx, db, firstToken); !errors.Is(err, ErrResellerCredentialUnknown) {
		t.Fatal("the revoked credential still authenticates after rotation")
	}
}

// Two live credentials for the same (organizer, channel, reseller) are refused —
// the other half of the partial index, without which "one live credential" is a
// comment rather than a constraint.
func TestASecondLiveCredentialForTheSameScopeIsRefused(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	org, reseller := uuid.New(), uuid.New()

	if _, _, err := EnrolResellerCredential(ctx, db, org, reseller, "reseller-acme", "ACME"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnrolResellerCredential(ctx, db, org, reseller, "reseller-acme", "ACME again"); err == nil {
		t.Fatal("a second LIVE credential for the same organizer+channel+reseller was accepted")
	}
	// A different reseller on the same channel is a different partner and is allowed.
	if _, _, err := EnrolResellerCredential(ctx, db, org, uuid.New(), "reseller-acme", "Other partner"); err != nil {
		t.Fatalf("a different reseller on the same channel must be allowed: %v", err)
	}
}
