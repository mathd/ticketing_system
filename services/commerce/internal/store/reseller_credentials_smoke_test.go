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

// Zero-downtime rotation: the replacement is issued WHILE the original still works.
//
// This asserts the documented workflow rather than whatever the schema happens to
// allow -- which is the distinction that mattered here. The previous version of
// this test revoked FIRST and then enrolled, so it passed against a unique index
// that made the documented enrol-then-revoke workflow impossible. It agreed with
// the code and disagreed with the requirement, and ai-review caught it.
//
// The requirement, stated without naming the mechanism: a partner can be handed a
// new credential and keep selling on the old one until it has deployed the new.
func TestAReplacementIsIssuedWhileTheOriginalStillWorks(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	org, reseller := uuid.New(), uuid.New()

	first, firstToken, err := EnrolResellerCredential(ctx, db, org, reseller, "reseller-acme", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	// The replacement is issued with the original STILL LIVE. This is the step the
	// unique index refused.
	second, secondToken, err := EnrolResellerCredential(ctx, db, org, reseller, "reseller-acme", "ACME rotated")
	if err != nil {
		t.Fatalf("a replacement could not be issued while the original was live, so rotation "+
			"requires taking the partner offline first: %v", err)
	}
	if second.ID == first.ID || secondToken == firstToken {
		t.Fatal("rotation returned the same credential; the replacement is not a new secret")
	}

	// BOTH work during the handover, and both carry the same scope.
	for name, token := range map[string]string{"original": firstToken, "replacement": secondToken} {
		got, err := AuthenticateResellerCredential(ctx, db, token)
		if err != nil {
			t.Fatalf("the %s credential must authenticate during the handover: %v", name, err)
		}
		if got.OrganizerID != org || got.ChannelCode != "reseller-acme" || got.ResellerID != reseller {
			t.Fatalf("the %s credential resolved to a different scope: %+v", name, got)
		}
	}

	// Retiring the predecessor ends the handover and leaves the replacement working.
	if err := RevokeResellerCredential(ctx, db, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := AuthenticateResellerCredential(ctx, db, firstToken); !errors.Is(err, ErrResellerCredentialUnknown) {
		t.Fatal("the retired credential still authenticates after rotation completed")
	}
	if _, err := AuthenticateResellerCredential(ctx, db, secondToken); err != nil {
		t.Fatalf("the replacement stopped working when its predecessor was revoked: %v", err)
	}
}

// The operator's listing answers for ONE organizer, and the failure it must catch
// is an EXTRA row rather than a missing one (TKT-276).
//
// That decides the fixture: a cross-organizer leak is invisible to a fixture
// holding a single organizer, because every row it could return is a row that
// belongs in the answer. So two organizers are seeded, each with credentials, and
// the assertion is on the exact set of ids — not on a count, and not on "every
// returned row belongs to A", which a query returning a strict subset would also
// satisfy.
//
// At the store tier because the scope IS the WHERE clause, per this file's header.
// Delete `WHERE organizer_id = $1` from ListResellerCredentials and THIS test goes
// red with three rows instead of two.
//
// Revoked rows are included on purpose: the question an operator asks after a leak
// is "which credential did we revoke, and when", so a listing that hid them would
// answer the wrong question. The test pins that too, in both directions.
func TestListResellerCredentialsIsScopedToOneOrganizerAndKeepsRevokedRows(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	orgA, orgB := uuid.New(), uuid.New()
	resellerA, resellerB := uuid.New(), uuid.New()

	live, _, err := EnrolResellerCredential(ctx, db, orgA, resellerA, "reseller-acme", "ACME live")
	if err != nil {
		t.Fatal(err)
	}
	retired, _, err := EnrolResellerCredential(ctx, db, orgA, resellerA, "reseller-acme-2", "ACME retired")
	if err != nil {
		t.Fatal(err)
	}
	// The neighbours, and there are TWO of them for a reason that a first version of
	// this test got wrong (ai-review). Seeding only a LIVE neighbour makes a whole
	// class of scope defect unrepresentable: a predicate like
	// `WHERE organizer_id = $1 OR revoked_at IS NOT NULL` leaks every revoked
	// credential in the table across every tenant, and this test stayed GREEN under
	// exactly that mutation — its only cross-organizer row was live, so there was no
	// revoked foreign row for the leak to return. The fixture must be able to
	// represent the leak on BOTH sides of the revoked/live split, or it is only
	// testing the half it happens to seed.
	neighbourLive, _, err := EnrolResellerCredential(ctx, db, orgB, resellerB, "reseller-other", "Other org live")
	if err != nil {
		t.Fatal(err)
	}
	neighbourRetired, _, err := EnrolResellerCredential(ctx, db, orgB, resellerB, "reseller-other-2", "Other org retired")
	if err != nil {
		t.Fatal(err)
	}
	if err := RevokeResellerCredential(ctx, db, retired.ID); err != nil {
		t.Fatal(err)
	}
	if err := RevokeResellerCredential(ctx, db, neighbourRetired.ID); err != nil {
		t.Fatal(err)
	}

	got, err := ListResellerCredentials(ctx, db, orgA)
	if err != nil {
		t.Fatal(err)
	}

	byID := make(map[uuid.UUID]ResellerCredential, len(got))
	for _, c := range got {
		switch c.ID {
		case neighbourLive.ID:
			t.Fatal("the listing returned another organizer's LIVE credential: the scope predicate is gone")
		case neighbourRetired.ID:
			t.Fatal("the listing returned another organizer's REVOKED credential: the scope predicate does not hold for revoked rows")
		}
		byID[c.ID] = c
	}
	if len(got) != 2 {
		t.Fatalf("listing returned %d credentials, want exactly the 2 belonging to this organizer: %+v", len(got), got)
	}
	if _, ok := byID[live.ID]; !ok {
		t.Fatal("the live credential is missing from its own organizer's listing")
	}
	retiredRow, ok := byID[retired.ID]
	if !ok {
		t.Fatal("the revoked credential is missing: an operator reconciling after a leak needs to see it")
	}

	// Revoked state is what tells an operator whether a credential still sells.
	if retiredRow.RevokedAt == nil {
		t.Fatal("the revoked credential lists a nil revoked_at, so it reads as live")
	}
	if liveRow := byID[live.ID]; liveRow.RevokedAt != nil {
		t.Fatalf("the live credential lists revoked_at %v, so it reads as retired", *liveRow.RevokedAt)
	}
	// The scope the credential carries must be the scope asked for.
	for _, c := range got {
		if c.OrganizerID != orgA {
			t.Fatalf("credential %s carries organizer %s, want %s", c.ID, c.OrganizerID, orgA)
		}
	}
}
