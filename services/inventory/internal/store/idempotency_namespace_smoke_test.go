//go:build smoke

package store

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// A public caller must not be able to occupy a STAFF idempotency key (TKT-296 D2).
//
// Four staff writers decorated the key in Go -- "op-place:", "convert:<id>:",
// "grp-place:", "grp-draw:<id>:" -- and wrote it with reseller_scope NULL, so the
// row landed in claims_public_idempotency beside arbitrary public keys. Public keys
// are caller-supplied strings up to 200 chars and claims rows are never deleted, so
// a public caller who sent "op-place:X" permanently occupied that row: a later staff
// PlaceOperationalHold with key X passed the claim_history registry (empty for it),
// hit the unique index at INSERT, and answered an UNMAPPED 500 -- forever, for that
// (organizer, key). Migration 0016 had already named this exact shape: "a prefix
// inside a shared namespace is not a namespace; it is a naming convention that an
// attacker also gets to use."
//
// WHAT A FAILURE LOOKS LIKE IN THE RESULT SET, which is what the assertions test:
// the staff row is MISSING and an error escapes. Not a duplicated row, not a
// substituted one. So the assertions are "the staff operation succeeded" and "both
// rows exist", never "the write was refused".
//
// MUTATION that proves this test can fail: drop `AND staff_scope IS NULL` from
// claims_public_idempotency in migration 0019 (or re-add any key prefix to the
// writer). The staff insert then collides with the seeded public row and returns
// ErrIdempotency instead of a hold, and the first assertion goes red. Verified
// against the pre-fix schema directly: the same two INSERTs raise
// `duplicate key value violates unique constraint "claims_public_idempotency"`.
func TestAPublicCallerCannotOccupyAStaffIdempotencyKey(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 20)

	const key = "SHARED-KEY"

	// A public buyer takes the bare key first. This is the attack: no credential is
	// needed, and the key is whatever string the caller chooses.
	publicHold, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", key)
	if err != nil {
		t.Fatalf("the public hold was refused: %v", err)
	}

	// And the OLD prefixed spelling too, which is what an attacker who had read the
	// source would send to deny the staff path specifically.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", "op-place:"+key); err != nil {
		t.Fatalf("the public hold on the prefixed spelling was refused: %v", err)
	}

	// The staff operation with the SAME bare key must still succeed.
	staffHold, replayed, err := st.PlaceOperationalHold(ctx, org, slot, 2,
		"house", "row A", "ops@example.test", "holdback", key)
	if err != nil {
		t.Fatalf("a staff hold was refused because a PUBLIC caller had taken the key %q: %v\n"+
			"This is TKT-296 D2: staff claims share the public idempotency namespace, so any "+
			"caller can deny a staff key permanently. Migration 0019 gives staff its own.", key, err)
	}
	if replayed {
		t.Fatal("the staff hold reported itself a replay of the public row")
	}
	if staffHold.ID == publicHold.ID {
		t.Fatal("the staff operation returned the PUBLIC caller's claim")
	}

	// Both namespaces hold the same key string, independently.
	var public, staff int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM claims WHERE organizer_id=$1 AND idempotency_key=$2 AND staff_scope IS NULL`,
		org, key).Scan(&public); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM claims WHERE organizer_id=$1 AND idempotency_key=$2 AND staff_scope IS TRUE`,
		org, key).Scan(&staff); err != nil {
		t.Fatal(err)
	}
	if public != 1 || staff != 1 {
		t.Fatalf("expected exactly one public and one staff claim for %q, got public=%d staff=%d",
			key, public, staff)
	}

	// The staff key is stored BARE. If a prefix ever comes back, the namespace is
	// doing nothing and this ticket has regressed to a naming convention.
	var stored string
	if err := db.QueryRowContext(ctx,
		`SELECT idempotency_key FROM claims WHERE id=$1`, staffHold.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != key {
		t.Fatalf("the staff claim stored a decorated key %q, want the bare %q", stored, key)
	}
}

// A staff claim must not be replayed to a PUBLIC caller presenting the same key.
//
// The write-side namespace is only half the fix: the public replay lookup must also
// refuse to SEE staff rows, or a public request with a staff key would either replay
// someone else's hold or be refused as key reuse against a row it may not read.
//
// MUTATION: drop `AND staff_scope IS NULL` from the lookup in CreateHold
// (store.go). The public request then finds the staff row, its fingerprint differs,
// and it is refused with ErrIdempotency -- the "public claim created" assertion goes
// red. This mutation is caught ONLY here: the write-side test above still passes
// with the read predicate removed, because its inserts never collide.
func TestAPublicRequestDoesNotReplayAStaffClaim(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 20)

	const key = "STAFF-FIRST"

	staffHold, _, err := st.PlaceOperationalHold(ctx, org, slot, 2,
		"house", "row B", "ops@example.test", "holdback", key)
	if err != nil {
		t.Fatalf("the staff hold was refused: %v", err)
	}

	// A public buyer now sends the same key with entirely different terms.
	publicHold, replayed, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", key)
	if err != nil {
		t.Fatalf("a public hold was refused because a STAFF claim held the key %q: %v", key, err)
	}
	if replayed {
		t.Fatal("the public request REPLAYED the staff claim — it read across the namespace boundary")
	}
	if publicHold.ID == staffHold.ID {
		t.Fatal("the public request was handed the staff claim")
	}

	var kinds int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM claims WHERE organizer_id=$1 AND idempotency_key=$2`, org, key).Scan(&kinds); err != nil {
		t.Fatal(err)
	}
	if kinds != 2 {
		t.Fatalf("expected the staff row and the public row to coexist, found %d claim(s)", kinds)
	}
}
