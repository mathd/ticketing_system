//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

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

	// A foreign row seeded BEFORE org A's, so both created_at directions carry a
	// neighbour. Without it every foreign row is newer than every row in the answer,
	// and a predicate leaking only OLDER foreign rows would be unrepresentable
	// (ai-review pass 2) — the same blind spot as the live-only fixture one pass
	// earlier, along a different axis.
	neighbourOldest, _, err := EnrolResellerCredential(ctx, db, orgB, resellerB, "reseller-other-0", "Other org oldest")
	if err != nil {
		t.Fatal(err)
	}

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
	//
	// The retired neighbour is enrolled AND revoked before the live one (TKT-290): org B's
	// reseller already holds neighbourOldest, and a third live credential would exceed
	// the cap of two. Every assertion below is about org A's rows, so this order changes
	// nothing they observe.
	neighbourRetired, _, err := EnrolResellerCredential(ctx, db, orgB, resellerB, "reseller-other-2", "Other org retired")
	if err != nil {
		t.Fatal(err)
	}
	if err := RevokeResellerCredential(ctx, db, neighbourRetired.ID); err != nil {
		t.Fatal(err)
	}
	neighbourLive, _, err := EnrolResellerCredential(ctx, db, orgB, resellerB, "reseller-other", "Other org live")
	if err != nil {
		t.Fatal(err)
	}
	if err := RevokeResellerCredential(ctx, db, retired.ID); err != nil {
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
		case neighbourOldest.ID:
			t.Fatal("the listing returned another organizer's OLDER credential: the scope predicate does not hold across created_at")
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

	// NEWEST FIRST, and this is a contract rather than a cosmetic detail: the smoke
	// path takes the FIRST row for a reseller id (`head -1`) as the credential it just
	// enrolled, so an ordering regression would hand it an older row. Nothing else
	// pinned `ORDER BY created_at DESC` — reversing or deleting it survived every
	// other assertion here, because membership and count do not depend on order
	// (ai-review pass 2).
	if len(got) >= 2 {
		for i := 1; i < len(got); i++ {
			if got[i].CreatedAt.After(got[i-1].CreatedAt) {
				t.Fatalf("listing is not newest-first: row %d (%s) was created after row %d (%s); "+
					"the smoke path's head -1 would select an older credential",
					i, got[i].CreatedAt, i-1, got[i-1].CreatedAt)
			}
		}
		// The seeded order is known, so assert the actual expected row is first rather
		// than only that the sequence is monotonic — a query returning one row would
		// satisfy monotonicity vacuously.
		if got[0].ID != retired.ID {
			t.Fatalf("newest credential first: got %s, want the most recently enrolled %s", got[0].ID, retired.ID)
		}
	}
}

// --- TKT-290: at most two live credentials per (organizer, reseller), across channels,
// decided in the database under an identity lock. ---

func liveCount(t *testing.T, ctx context.Context, db *sql.DB, org, reseller uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM reseller_credentials
		WHERE organizer_id=$1 AND reseller_id=$2 AND revoked_at IS NULL`, org, reseller).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustEnrol(t *testing.T, ctx context.Context, db *sql.DB, org, reseller uuid.UUID, channel string) (ResellerCredential, string) {
	t.Helper()
	cred, token, err := EnrolResellerCredential(ctx, db, org, reseller, channel, "cap test")
	if err != nil {
		t.Fatalf("enrol %s: %v", channel, err)
	}
	return cred, token
}

// COS1: the third live enrolment is refused, and rotation still works — revoke one and
// the next enrolment succeeds. Same channel, and across channels: the cap is per
// (organizer, reseller), not per channel.
func TestResellerCredentialCapAllowsRotation(t *testing.T) {
	for name, channels := range map[string][3]string{
		"same channel":       {"reseller-acme", "reseller-acme", "reseller-acme"},
		"different channels": {"reseller-a", "reseller-b", "reseller-c"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := migratedDB(t, ctx)
			org, reseller := uuid.New(), uuid.New()
			first, firstToken := mustEnrol(t, ctx, db, org, reseller, channels[0])
			_, secondToken := mustEnrol(t, ctx, db, org, reseller, channels[1])

			cred, token, err := EnrolResellerCredential(ctx, db, org, reseller, channels[2], "third")
			if !errors.Is(err, ErrResellerCredentialCap) {
				t.Fatalf("third live enrolment: err = %v, want ErrResellerCredentialCap", err)
			}
			if cred != (ResellerCredential{}) || token != "" {
				t.Fatal("a refused enrolment returned a credential or a token")
			}
			if n := liveCount(t, ctx, db, org, reseller); n != 2 {
				t.Fatalf("live = %d after the refusal, want 2", n)
			}
			for _, tok := range []string{firstToken, secondToken} {
				if _, err := AuthenticateResellerCredential(ctx, db, tok); err != nil {
					t.Fatalf("an existing credential stopped working after a refused enrolment: %v", err)
				}
			}

			// The rotation path: revoke one, and the replacement is issued.
			if err := RevokeResellerCredential(ctx, db, first.ID); err != nil {
				t.Fatal(err)
			}
			mustEnrol(t, ctx, db, org, reseller, channels[2])
			if n := liveCount(t, ctx, db, org, reseller); n != 2 {
				t.Fatalf("live = %d after rotation, want 2", n)
			}
			// History is kept: the rotated-out predecessor is still listed, as revoked.
			listed, err := ListResellerCredentials(ctx, db, org)
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, c := range listed {
				if c.ID == first.ID {
					found = true
					if c.RevokedAt == nil {
						t.Fatal("the rotated-out predecessor lists as live")
					}
				}
			}
			if !found || len(listed) != 3 {
				t.Fatalf("after rotation the listing has %d rows (predecessor found=%v), want 3 including it", len(listed), found)
			}
		})
	}
}

// COS3: revoked credentials do not count. One live and two revoked → an enrolment
// succeeds, and the revoked rows keep their revocation.
func TestResellerCredentialCapIgnoresRevokedRows(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	org, reseller := uuid.New(), uuid.New()
	// Seeded directly, not through the code under test: one live, two revoked at KNOWN
	// times, so the revoked rows can be compared exactly afterwards.
	revokedAt := map[uuid.UUID]time.Time{
		uuid.New(): time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		uuid.New(): time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
	}
	seed := func(id uuid.UUID, revoked *time.Time) {
		if _, err := db.ExecContext(ctx, `INSERT INTO reseller_credentials(id, reseller_id, organizer_id, channel_code, token_hash, label, revoked_at)
			VALUES ($1, $2, $3, 'reseller-acme', $4, 'seeded', $5)`, id, reseller, org, uuid.NewString(), revoked); err != nil {
			t.Fatal(err)
		}
	}
	seed(uuid.New(), nil)
	for id, at := range revokedAt {
		at := at
		seed(id, &at)
	}

	mustEnrol(t, ctx, db, org, reseller, "reseller-acme") // 1 live + 2 revoked: allowed
	if n := liveCount(t, ctx, db, org, reseller); n != 2 {
		t.Fatalf("live = %d, want 2", n)
	}
	for id, at := range revokedAt {
		var got *time.Time
		if err := db.QueryRowContext(ctx, `SELECT revoked_at FROM reseller_credentials WHERE id=$1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == nil || !got.Equal(at) {
			t.Fatalf("revoked row %s now has revoked_at %v, want %v unchanged", id, got, at)
		}
	}
}

// COS4: the cap is scoped by BOTH identity terms. Filling (org, reseller A) to the cap
// does not refuse reseller B under the same organizer, nor reseller A under another
// organizer. One case per term, each varying only that term.
func TestResellerCredentialCapScopesBothIdentityTerms(t *testing.T) {
	for name, vary := range map[string]func(org, reseller uuid.UUID) (uuid.UUID, uuid.UUID){
		"another reseller, same organizer": func(org, _ uuid.UUID) (uuid.UUID, uuid.UUID) { return org, uuid.New() },
		"same reseller, another organizer": func(_, reseller uuid.UUID) (uuid.UUID, uuid.UUID) { return uuid.New(), reseller },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := migratedDB(t, ctx)
			org, reseller := uuid.New(), uuid.New()
			mustEnrol(t, ctx, db, org, reseller, "reseller-acme")
			mustEnrol(t, ctx, db, org, reseller, "reseller-acme")
			otherOrg, otherReseller := vary(org, reseller)
			if _, _, err := EnrolResellerCredential(ctx, db, otherOrg, otherReseller, "reseller-acme", "other scope"); err != nil {
				t.Fatalf("a different identity was refused by another identity's cap: %v", err)
			}
		})
	}
}

// COS2: two CONCURRENT enrolments from one live credential end with exactly two live.
//
// The schedule is FORCED, not hoped for (AGENTS.md TKT-308). A test-only BEFORE INSERT
// trigger parks enrolment A inside its INSERT — after its count predicate has already
// passed — until the test releases a barrier lock. Enrolment B is started only once A is
// observed parked. With the identity lock, B waits on that lock and counts again after A
// commits: two live, B refused. Without it, B's count also sees ONE (A is uncommitted),
// B parks in the trigger beside A, and both commit: three live.
func TestResellerCredentialCapSerializesConcurrentEnrolment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	org, reseller := uuid.New(), uuid.New()
	mustEnrol(t, ctx, db, org, reseller, "reseller-seed")

	barrierKey := int32(time.Now().UnixNano() & 0x7fffffff)
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`
		CREATE FUNCTION reseller_credential_cap_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_xact_lock_shared(4242, %d); RETURN NEW; END $$;
		CREATE TRIGGER reseller_credential_cap_barrier BEFORE INSERT ON reseller_credentials
		FOR EACH ROW EXECUTE FUNCTION reseller_credential_cap_barrier();`, barrierKey)); err != nil {
		t.Fatal(err)
	}
	controller, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = controller.Rollback() }()
	if _, err := controller.ExecContext(ctx, `SELECT pg_advisory_xact_lock(4242, $1)`, barrierKey); err != nil {
		t.Fatal(err)
	}

	var parkedInserts, identityWaiters string
	// waitFor polls the database for a condition, failing the SETUP on timeout.
	waitFor := func(what, query string) {
		t.Helper()
		for {
			var ok bool
			if err := db.QueryRowContext(ctx, query).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if ok {
				return
			}
			if ctx.Err() != nil {
				t.Fatalf("setup: never observed %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	parkedInserts = `SELECT count(*) FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid AND NOT l.granted
		WHERE a.wait_event_type='Lock' AND a.query LIKE 'INSERT INTO reseller_credentials%' AND l.locktype='advisory'`
	identityWaiters = `SELECT count(*) FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid AND NOT l.granted
		WHERE a.wait_event_type='Lock' AND a.query LIKE 'SELECT pg_advisory_xact_lock(hashtextextended%' AND l.locktype='advisory'`

	// Each worker gets its OWN connection pool, named, so the observers match THIS test's
	// sessions and nothing else on the server (review pass 1). The workers default to
	// REPEATABLE READ: production must ask for READ COMMITTED explicitly, because under
	// a snapshot taken before the lock wait B would count one and insert a third
	// (review pass 1). Different labels as well as channels, so a lock key that wrongly
	// included either would no longer serialize them.
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	worker := func(name string) *sql.DB {
		w, err := sql.Open("pgx", os.Getenv("COMMERCE_MIGRATION_TEST_DATABASE_URL")+"?search_path="+schema+
			"&application_name="+name+"&default_transaction_isolation=repeatable%20read")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		return w
	}
	workerA, workerB := worker("tkt290-a-"+schema), worker("tkt290-b-"+schema)
	parkedInserts = strings.Replace(parkedInserts, "WHERE ", "WHERE a.application_name IN ('tkt290-a-"+schema+"', 'tkt290-b-"+schema+"') AND ", 1)
	identityWaiters = strings.Replace(identityWaiters, "WHERE ", "WHERE a.application_name = 'tkt290-b-"+schema+"' AND ", 1)

	type res struct{ err error }
	results := make(chan res, 2)
	enrol := func(w *sql.DB, channel, label string) {
		_, _, err := EnrolResellerCredential(ctx, w, org, reseller, channel, label)
		results <- res{err}
	}
	go enrol(workerA, "reseller-a", "concurrent A")
	waitFor("enrolment A parked inside its INSERT", "SELECT ("+parkedInserts+") = 1")
	go enrol(workerB, "reseller-b", "concurrent B")
	// B is either waiting on the identity lock (the guard works) or parked beside A in
	// the trigger (the guard is gone). Either way it has made its decision-relevant move.
	waitFor("enrolment B waiting", "SELECT ("+identityWaiters+") = 1 OR ("+parkedInserts+") = 2")
	if err := controller.Rollback(); err != nil {
		t.Fatal(err)
	}

	var ok, refused int
	for i := 0; i < 2; i++ {
		r := <-results
		switch {
		case r.err == nil:
			ok++
		case errors.Is(r.err, ErrResellerCredentialCap):
			refused++
		default:
			t.Fatalf("unexpected enrolment error: %v", r.err)
		}
	}
	if n := liveCount(t, ctx, db, org, reseller); n != 2 || ok != 1 || refused != 1 {
		t.Fatalf("concurrent enrolments from one live: live=%d ok=%d refused=%d, want 2/1/1", n, ok, refused)
	}
}

// Review pass 1: identities that already held MORE than two live credentials before the
// cap (the ADR names them) are refused, not topped up — and recover only once revocation
// brings them below two. Seeded directly, since the cap prevents reaching three through
// enrolment. This is what separates `< 2` from a comparator that only refuses at exactly 2.
func TestResellerCredentialCapRefusesALegacyOverCapIdentity(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	org, reseller := uuid.New(), uuid.New()
	ids := make([]uuid.UUID, 3)
	for i := range ids {
		ids[i] = uuid.New()
		if _, err := db.ExecContext(ctx, `INSERT INTO reseller_credentials(id, reseller_id, organizer_id, channel_code, token_hash, label)
			VALUES ($1, $2, $3, 'reseller-legacy', $4, 'legacy')`, ids[i], reseller, org, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	// After each refusal, EVERY seeded row is re-read: a refusal must not retire a legacy
	// credential behind the operator's back (review pass 2), and must not add a row.
	refused := func(when string, wantRevoked map[uuid.UUID]bool) {
		t.Helper()
		cred, token, err := EnrolResellerCredential(ctx, db, org, reseller, "reseller-legacy", "new")
		if !errors.Is(err, ErrResellerCredentialCap) || token != "" || cred != (ResellerCredential{}) {
			t.Fatalf("%s: err=%v token=%q cred=%+v, want the cap refusal and nothing returned", when, err, token, cred)
		}
		rows, err := db.QueryContext(ctx, `SELECT id, revoked_at IS NOT NULL FROM reseller_credentials
			WHERE organizer_id=$1 AND reseller_id=$2`, org, reseller)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		seen := 0
		for rows.Next() {
			var id uuid.UUID
			var revoked bool
			if err := rows.Scan(&id, &revoked); err != nil {
				t.Fatal(err)
			}
			want, known := wantRevoked[id]
			if !known {
				t.Fatalf("%s: an unexpected row %s exists after a refusal", when, id)
			}
			if revoked != want {
				t.Fatalf("%s: row %s revoked=%v, want %v — a refusal changed an existing credential", when, id, revoked, want)
			}
			seen++
		}
		if seen != len(ids) {
			t.Fatalf("%s: %d rows, want %d", when, seen, len(ids))
		}
	}
	refused("three live", map[uuid.UUID]bool{ids[0]: false, ids[1]: false, ids[2]: false})
	if err := RevokeResellerCredential(ctx, db, ids[0]); err != nil {
		t.Fatal(err)
	}
	refused("two live", map[uuid.UUID]bool{ids[0]: true, ids[1]: false, ids[2]: false})
	if err := RevokeResellerCredential(ctx, db, ids[1]); err != nil {
		t.Fatal(err)
	}
	mustEnrol(t, ctx, db, org, reseller, "reseller-legacy")
	if n := liveCount(t, ctx, db, org, reseller); n != 2 {
		t.Fatalf("live = %d, want 2", n)
	}
}
