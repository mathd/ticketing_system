//go:build smoke

package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// The journal fault-injection matrix (TKT-56 Slice 4, ADR-032 §Delivery slices/4).
//
// These run against a REAL PostgreSQL because every one of them depends on something
// no in-memory double has: the per-organizer row lock, the append-only triggers,
// transaction visibility, or backend termination. They deliberately do NOT run against
// the live payments database — the packaged-binary checks in scripts/smoke.sh use that
// one as their fixture and corrupt it on purpose, so sharing would race them. smoke.sh
// creates payments_store_smoke for this file.
//
// Verify() scans the WHOLE table, so these tests are sequential by construction (no
// t.Parallel) and anything that corrupts a row restores it before returning. Two tests
// deliberately leave state behind: TestJournalRotationKeepsHistoryVerifiable (its mixed-key
// chain is smoke.sh's fixture for the packaged verify-journal) and
// TestJournalVerificationFailsWhenHistoricalKeyIsRetired (it appends a fresh v1-era entry).

const (
	// Fixed literal keys. The rotation test leaves a genuinely mixed-kid journal
	// behind, and scripts/smoke.sh then runs the PACKAGED binary's verify-journal
	// against it with exactly these values — that is what makes the "verify-journal
	// verifies a multi-key journal" claim evidence rather than an argument.
	smokeKIDv1 = "smoke-v1"
	smokeKIDv2 = "smoke-v2"
	smokeKeyv1 = "smoke-journal-key-v1-0123456789"
	smokeKeyv2 = "smoke-journal-key-v2-0123456789"
)

var (
	migrateOnce sync.Once
	migrateErr  error
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("PAYMENTS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PAYMENTS_TEST_DATABASE_URL is not set")
	}
	return dsn
}

// journalDB opens the store-test database and ensures the schema exists. Migrating
// here (rather than relying on the smoke stack) keeps this file runnable against any
// empty database, matching the commerce precedent.
func journalDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := sql.Open("pgx", testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	migrateOnce.Do(func() { migrateErr = Migrate(ctx, db) })
	if migrateErr != nil {
		t.Fatalf("migrate store-test database: %v", migrateErr)
	}
	return db, ctx
}

func mustRing(t *testing.T, activeKID, activeKey, historical string) *Keyring {
	t.Helper()
	ring, err := NewKeyring(activeKID, []byte(activeKey), historical)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

// onlyV1 / onlyV2 are single-key rings — the configuration before a rotation, and the
// configuration after v1 has been retired. Neither can verify the other's era, which is
// the whole point of the two tests that use them.
func onlyV1(t *testing.T) *Keyring { return mustRing(t, smokeKIDv1, smokeKeyv1, "") }
func onlyV2(t *testing.T) *Keyring { return mustRing(t, smokeKIDv2, smokeKeyv2, "") }

// fullRing is v2 active with v1 retained: the post-rotation production shape, and the
// only ring that can verify this database once the rotation test has run.
//
// Every test that calls Verify uses it, and that is not incidental. Verify scans the
// WHOLE table across all organizers, so a test holding a narrower ring fails on some
// OTHER test's entries — which is exactly how the first run of this file broke, with
// the restart and crash tests reporting `unknown key id "smoke-v2"` about rows they
// never wrote. Total verification is what makes these tests independent of each other's
// order; only the two tests that are ABOUT a narrow ring hold one.
func fullRing(t *testing.T) *Keyring {
	t.Helper()
	return mustRing(t, smokeKIDv2, smokeKeyv2, smokeKIDv1+"="+base64.RawStdEncoding.EncodeToString([]byte(smokeKeyv1)))
}

func fact(org uuid.UUID) Fact {
	return Fact{
		ID: uuid.New(), OrganizerID: org, BuyerID: uuid.New(),
		Type: "order.created", Amount: 1250, Currency: "EUR",
		OccurredAt: time.Now().UTC(),
		Payload:    map[string]string{"order_id": uuid.NewString()},
	}
}

// --- concurrency ------------------------------------------------------------

// Distinct facts for ONE organizer appended concurrently must each commit exactly
// once and receive contiguous, correctly linked sequence numbers. This is not what
// the packaged verify-concurrent-append proves: that one appends the SAME fact from
// 8 workers and asserts 1 new + 7 replays (main.go), i.e. the replay short-circuit.
// This one exercises the per-organizer row lock under genuine contention.
func TestJournalConcurrentDistinctAppendsSerializePerOrganizer(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org := uuid.New()

	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	seqs := make([]int64, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, replay, err := j.Append(ctx, fact(org))
			errs[i], seqs[i] = err, e.Sequence
			if replay {
				errs[i] = errors.New("distinct fact reported as a replay")
			}
		}()
	}
	wg.Wait()

	seen := map[int64]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if seen[seqs[i]] {
			t.Fatalf("sequence %d handed out twice — the organizer lock did not serialize", seqs[i])
		}
		seen[seqs[i]] = true
	}
	for s := int64(1); s <= workers; s++ {
		if !seen[s] {
			t.Fatalf("sequence %d missing; got %v", s, seen)
		}
	}
	if err := j.Verify(ctx); err != nil {
		t.Fatalf("chain broken after concurrent appends: %v", err)
	}
}

// --- conflicting replay -----------------------------------------------------

// Reusing one fact id with changed content is a conflict, not a replay: it must be
// refused and must advance nothing.
func TestJournalRejectsConflictingReplay(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org := uuid.New()

	first := fact(org)
	if _, _, err := j.Append(ctx, first); err != nil {
		t.Fatal(err)
	}
	seqBefore, headBefore := headOf(t, db, ctx, org)

	conflicting := first
	conflicting.Amount = first.Amount + 1
	if _, _, err := j.Append(ctx, conflicting); err == nil {
		t.Fatal("a fact id reused with different content must be refused")
	} else if !strings.Contains(err.Error(), "reused with different content") {
		t.Fatalf("unexpected error: %v", err)
	}

	seqAfter, headAfter := headOf(t, db, ctx, org)
	if seqAfter != seqBefore || headAfter != headBefore {
		t.Fatal("a refused conflicting append advanced the head")
	}
	if err := j.Verify(ctx); err != nil {
		t.Fatalf("verify after refused conflict: %v", err)
	}
}

func headOf(t *testing.T, db *sql.DB, ctx context.Context, org uuid.UUID) (int64, string) {
	t.Helper()
	var seq int64
	var hash []byte
	if err := db.QueryRowContext(ctx, `SELECT last_sequence,last_hash FROM journal_heads WHERE organizer_id=$1`, org).Scan(&seq, &hash); err != nil {
		t.Fatal(err)
	}
	return seq, fmt.Sprintf("%x", hash)
}

// --- the production append-only guard ---------------------------------------

// Before modelling an adversary who removes the triggers, prove the triggers are
// really there and really fire. Without this, the tamper test below could pass
// against a database whose append-only guarantee had silently been dropped.
func TestJournalAppendOnlyTriggersRejectMutation(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org := uuid.New()
	f := fact(org)
	if _, _, err := j.Append(ctx, f); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, stmt string }{
		{"update", `UPDATE journal_entries SET amount=1 WHERE fact_id=$1`},
		{"delete", `DELETE FROM journal_entries WHERE fact_id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, tc.stmt, f.ID)
			if err == nil {
				t.Fatalf("%s on journal_entries was accepted; the append-only trigger is not in force", tc.name)
			}
			if !strings.Contains(err.Error(), "append-only") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	t.Run("truncate", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `TRUNCATE journal_entries`); err == nil {
			t.Fatal("TRUNCATE journal_entries was accepted")
		}
	})
}

// --- tampering --------------------------------------------------------------

// Name the adversary (ADR-021). Disabling the triggers is not a cheat that grants an
// imaginary superpower: triggers are DDL and the service role owns these tables in the
// deployed stack, so a database writer really can remove them — the same premise
// scripts/smoke.sh states for the lifecycle trail.
//
// What this proves: modification is EVIDENT against a writer who holds no HMAC key.
// What it does not prove: anything against a holder of any key in the ring, who
// re-signs freely; and nothing about rollback, which stays undetectable.
func TestJournalVerifyRejectsRowTampering(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org := uuid.New()
	f := fact(org)
	if _, _, err := j.Append(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := j.Verify(ctx); err != nil {
		t.Fatalf("control: a clean journal must verify first, got: %v", err)
	}

	for _, tc := range []struct {
		name, column string
		value        any
		wantErr      string
	}{
		{"signed money field", "amount", int64(999999), "invalid hash"},
		{"entry hash", "entry_hash", make([]byte, 32), "invalid hash"},
		{"signature", "signature", make([]byte, 32), "invalid signature"},
		// key_id is not inside canonical v1, so relabelling it does not change the
		// entry hash — it fails because the ring resolves a different (or no) key.
		// BOTH halves of that matter, and only the first was pinned at first: a kid the
		// ring does not hold, and a kid it DOES hold under a different secret. The
		// second is the case the whole widening turns on, so it is not optional.
		{"key id relabelled to a key not in the ring", "key_id", "smoke-vX", "unknown key id"},
		{"key id relabelled to another ring member", "key_id", smokeKIDv1, "invalid signature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := tamper(t, db, ctx, f.ID, tc.column, tc.value)
			defer restore()
			err := j.Verify(ctx)
			if err == nil {
				t.Fatalf("tampering with %s was not detected", tc.column)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}

	if err := j.Verify(ctx); err != nil {
		t.Fatalf("journal not restored after tampering: %v", err)
	}
}

// tamper mutates one column of one entry with the append-only triggers lifted, and
// returns the restore func. Restoring matters: Verify() is global, so a row left
// corrupted would fail every later test in this file for the wrong reason.
func tamper(t *testing.T, db *sql.DB, ctx context.Context, factID uuid.UUID, column string, value any) func() {
	t.Helper()
	var original any
	switch column {
	case "amount":
		var v int64
		mustScan(t, db, ctx, `SELECT amount FROM journal_entries WHERE fact_id=$1`, factID, &v)
		original = v
	case "key_id":
		var v string
		mustScan(t, db, ctx, `SELECT key_id FROM journal_entries WHERE fact_id=$1`, factID, &v)
		original = v
	default:
		var v []byte
		mustScan(t, db, ctx, `SELECT `+column+` FROM journal_entries WHERE fact_id=$1`, factID, &v)
		original = v
	}
	exec(t, db, ctx, `ALTER TABLE journal_entries DISABLE TRIGGER USER`)
	exec(t, db, ctx, `UPDATE journal_entries SET `+column+`=$1 WHERE fact_id=$2`, value, factID)
	return func() {
		// The triggers must come back even if the restoring UPDATE fails. Leaving
		// production triggers disabled would make every later test in this file — and
		// smoke.sh's packaged verify-journal — fail for a reason that has nothing to do
		// with what they assert. The UPDATE has to run first: the trigger being restored
		// is precisely the one that would reject it. t.Errorf, not t.Fatalf, so a failed
		// restore reports itself without skipping the re-enable.
		defer func() {
			if _, err := db.ExecContext(ctx, `ALTER TABLE journal_entries ENABLE TRIGGER USER`); err != nil {
				t.Errorf("re-enable append-only triggers: %v", err)
			}
		}()
		if _, err := db.ExecContext(ctx, `UPDATE journal_entries SET `+column+`=$1 WHERE fact_id=$2`, original, factID); err != nil {
			t.Errorf("restore %s: %v", column, err)
		}
	}
}

func mustScan(t *testing.T, db *sql.DB, ctx context.Context, q string, arg any, dest any) {
	t.Helper()
	if err := db.QueryRowContext(ctx, q, arg).Scan(dest); err != nil {
		t.Fatal(err)
	}
}

func exec(t *testing.T, db *sql.DB, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// --- head / entry consistency -----------------------------------------------

// journal_heads carries no append-only trigger (only journal_entries does), so these
// mutations need no trigger juggling — which is itself worth pinning: the head is the
// softest target in the schema and the only thing standing behind it is Verify.
func TestJournalVerifyRejectsHeadEntryDesync(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org := uuid.New()
	if _, _, err := j.Append(ctx, fact(org)); err != nil {
		t.Fatal(err)
	}

	var seq int64
	var hash []byte
	if err := db.QueryRowContext(ctx, `SELECT last_sequence,last_hash FROM journal_heads WHERE organizer_id=$1`, org).Scan(&seq, &hash); err != nil {
		t.Fatal(err)
	}

	t.Run("head hash rewritten", func(t *testing.T) {
		exec(t, db, ctx, `UPDATE journal_heads SET last_hash=$1 WHERE organizer_id=$2`, make([]byte, 32), org)
		defer exec(t, db, ctx, `UPDATE journal_heads SET last_hash=$1 WHERE organizer_id=$2`, hash, org)
		assertVerifyFails(t, j, ctx, "journal head mismatch")
	})

	t.Run("head sequence advanced past the entries", func(t *testing.T) {
		exec(t, db, ctx, `UPDATE journal_heads SET last_sequence=$1 WHERE organizer_id=$2`, seq+5, org)
		defer exec(t, db, ctx, `UPDATE journal_heads SET last_sequence=$1 WHERE organizer_id=$2`, seq, org)
		assertVerifyFails(t, j, ctx, "journal head mismatch")
	})

	t.Run("entries with no head at all", func(t *testing.T) {
		exec(t, db, ctx, `DELETE FROM journal_heads WHERE organizer_id=$1`, org)
		defer exec(t, db, ctx, `INSERT INTO journal_heads(organizer_id,last_sequence,last_hash) VALUES($1,$2,$3)`, org, seq, hash)
		assertVerifyFails(t, j, ctx, "missing head")
	})

	if err := j.Verify(ctx); err != nil {
		t.Fatalf("heads not restored: %v", err)
	}
}

func assertVerifyFails(t *testing.T, j *Journal, ctx context.Context, want string) {
	t.Helper()
	err := j.Verify(ctx)
	if err == nil {
		t.Fatalf("expected verification to fail with %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not mention %q", err, want)
	}
}

// --- rotation: the decisive test --------------------------------------------

// C1 + C3. Before this slice, Verify rejected any entry whose key_id differed from
// the single configured key, so rotating the signing key invalidated all history.
//
// This test deliberately LEAVES its mixed-kid entries in the database: scripts/smoke.sh
// then runs the packaged binary's verify-journal over them with v2 active and v1
// historical, which is what turns "verify-journal verifies a multi-key journal" into
// evidence produced by the real CLI rather than a claim about a library call.
func TestJournalRotationKeepsHistoryVerifiable(t *testing.T) {
	db, ctx := journalDB(t)
	org := uuid.New()

	before := New(db, onlyV1(t))
	oldEntry, _, err := before.Append(ctx, fact(org))
	if err != nil {
		t.Fatal(err)
	}
	if oldEntry.KeyID != smokeKIDv1 {
		t.Fatalf("pre-rotation entry signed under %q", oldEntry.KeyID)
	}

	// Rotate: v2 becomes active, v1 is retained.
	after := New(db, fullRing(t))
	newEntry, _, err := after.Append(ctx, fact(org))
	if err != nil {
		t.Fatal(err)
	}
	if newEntry.KeyID != smokeKIDv2 {
		t.Fatalf("post-rotation entry signed under %q, want %q", newEntry.KeyID, smokeKIDv2)
	}

	var kids int
	if err := db.QueryRowContext(ctx, `SELECT count(DISTINCT key_id) FROM journal_entries WHERE organizer_id=$1`, org).Scan(&kids); err != nil {
		t.Fatal(err)
	}
	if kids != 2 {
		t.Fatalf("expected a genuinely mixed-key chain, got %d distinct key ids", kids)
	}

	if err := after.Verify(ctx); err != nil {
		t.Fatalf("a journal spanning a rotation must verify end to end: %v", err)
	}
}

// C4 + C5. Retiring a verification key makes that era unverifiable — that is the
// stated, accepted consequence of the retirement policy, and it must surface as an
// explicit failure naming the key, never as a skipped entry.
func TestJournalVerificationFailsWhenHistoricalKeyIsRetired(t *testing.T) {
	db, ctx := journalDB(t)

	// Write the v1-era entry this test is about, rather than relying on the rotation
	// test having run first: an order dependency between tests is a test that passes
	// for a reason its name does not state, and it silently becomes vacuous the day
	// someone runs this one with -run.
	if _, _, err := New(db, onlyV1(t)).Append(ctx, fact(uuid.New())); err != nil {
		t.Fatal(err)
	}

	// A ring holding ONLY the post-rotation key: v1's era is now unverifiable, and that
	// must surface as an explicit failure naming the missing key — never a skipped entry.
	//
	// Assert the KEY, not the organizer. Verify returns on the FIRST unrecognized row in
	// an organizer_id-ordered scan, and the organizers here are random UUIDs, so which v1
	// row trips it is a coin flip between this test's entry and the rotation test's. An
	// earlier revision asserted the organizer as well and was nondeterministic for exactly
	// that reason. Nothing is lost: the claim under test is "a v1-era entry is unverifiable
	// under a ring without v1", which is about the key. Self-containment comes from the
	// append above — under -run this test's entry is the only one that can trip it.
	assertVerifyFails(t, New(db, onlyV2(t)), ctx, `unknown key id "`+smokeKIDv1+`"`)
}

// --- restart ----------------------------------------------------------------

// C6 "restarts". A new process over the same database must continue the committed chain
// rather than restart it: sequence continues, the link is intact, and the chain verifies.
//
// Be honest about the strength of this one. Journal holds no in-memory sequence or head
// state, so a fresh pool is today indistinguishable from the original by construction and
// this cannot fail for the reason its name suggests. Its value is as a REGRESSION PIN: it
// goes red the day Append starts caching a head or a sequence in process memory, which is
// the change that would make a real restart lose or duplicate a sequence number.
func TestJournalRestartContinuesCommittedChain(t *testing.T) {
	dsn := testDSN(t)
	db, ctx := journalDB(t)
	org := uuid.New()

	first, _, err := New(db, fullRing(t)).Append(ctx, fact(org))
	if err != nil {
		t.Fatal(err)
	}

	// A genuinely new pool — no shared connection, no cached state.
	restarted, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()

	second, _, err := New(restarted, fullRing(t)).Append(ctx, fact(org))
	if err != nil {
		t.Fatalf("append after restart: %v", err)
	}
	if second.Sequence != first.Sequence+1 {
		t.Fatalf("restart did not continue the chain: %d then %d", first.Sequence, second.Sequence)
	}
	if fmt.Sprintf("%x", second.PreviousHash) != fmt.Sprintf("%x", first.EntryHash) {
		t.Fatal("post-restart entry does not link to the pre-restart one")
	}
	if err := New(restarted, fullRing(t)).Verify(ctx); err != nil {
		t.Fatalf("verify after restart: %v", err)
	}
}

// --- crash boundary ---------------------------------------------------------

// C6 "crash boundaries". Append INSERTs the entry and UPDATEs the head in ONE
// transaction; if that ever stopped being true, a crash in between would leave an
// entry with no head — a permanently unverifiable journal.
//
// Proving it requires killing the REAL Append mid-flight. A hand-written
// BEGIN/INSERT/ROLLBACK test would be simpler and deterministic, but it would only
// prove the test's own transaction is atomic: it would stay green if Append were
// refactored to commit the entry separately, which is precisely the regression this
// exists to catch.
//
// The mechanism: a test-only BEFORE UPDATE trigger on journal_heads sleeps. Reaching
// it proves the entry INSERT has already returned and the transaction is in the
// insert-to-commit window. A second connection finds that exact backend and kills it.
//
// What this proves: PostgreSQL aborts the whole real Append transaction when the
// connection dies after the entry insert, and a reconstructed process resumes from
// committed state. What it does NOT prove, and must not be read as proving:
// postmaster failure, host power loss, WAL/storage corruption, or container restart.
func TestJournalBackendTerminationRollsBackPartialAppend(t *testing.T) {
	dsn := testDSN(t)
	observer, ctx := journalDB(t)
	org := uuid.New()

	// F3: seed a COMMITTED entry first, and before the delay trigger exists — both
	// because the seed's own head UPDATE would otherwise sleep, and because on a
	// first-ever append the head row is created inside the same transaction, so an
	// abort would leave no head at all and "the head is unchanged" would be vacuous.
	seed, _, err := New(observer, fullRing(t)).Append(ctx, fact(org))
	if err != nil {
		t.Fatal(err)
	}
	seedSeq, seedHead := headOf(t, observer, ctx, org)

	const appName = "tkt56_crash_boundary"
	victim, err := sql.Open("pgx", withAppName(dsn, appName))
	if err != nil {
		t.Fatal(err)
	}
	victim.SetMaxOpenConns(1)
	defer func() { _ = victim.Close() }()

	installDelayTrigger(t, observer, ctx)
	defer dropDelayTrigger(t, observer, ctx)

	doomed := fact(org)
	// Build the ring on THIS goroutine: mustRing calls t.Fatal, which is only legal from
	// the test goroutine. Called inside the goroutine below, a construction failure would
	// Goexit the wrong goroutine, never write to appendErr, and surface as a timeout with
	// an unrelated message.
	victimJournal := New(victim, fullRing(t))
	appendErr := make(chan error, 1)
	go func() {
		_, _, err := victimJournal.Append(context.Background(), doomed)
		appendErr <- err
	}()

	pid := awaitBackendInHeadUpdate(t, observer, ctx, appName)
	if _, err := observer.ExecContext(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("terminate backend %d: %v", pid, err)
	}

	select {
	case err := <-appendErr:
		if err == nil {
			t.Fatal("Append returned success after its backend was terminated")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Append did not return after its backend was terminated")
	}

	// A budget of its own for the post-kill assertions. The shared journalDB context is
	// 120s and this test can already have spent 30s waiting for the backend plus 30s
	// waiting for Append to unwind; on a slow machine the assertions below would then
	// fail with "context deadline exceeded" and report a deadline as if it were a
	// journal invariant.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The entry must be gone WITH the head advance — both or neither.
	var rows int
	if err := observer.QueryRowContext(ctx, `SELECT count(*) FROM journal_entries WHERE fact_id=$1`, doomed.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("the entry survived a transaction that never committed")
	}
	seq, head := headOf(t, observer, ctx, org)
	if seq != seedSeq || head != seedHead {
		t.Fatalf("head moved despite the abort: %d/%s, want %d/%s", seq, head, seedSeq, seedHead)
	}

	// A reconstructed process continues cleanly from committed state.
	dropDelayTrigger(t, observer, ctx)
	retried, _, err := New(observer, fullRing(t)).Append(ctx, doomed)
	if err != nil {
		t.Fatalf("retry after crash: %v", err)
	}
	if retried.Sequence != seed.Sequence+1 {
		t.Fatalf("retry sequence %d, want %d", retried.Sequence, seed.Sequence+1)
	}
	if err := New(observer, fullRing(t)).Verify(ctx); err != nil {
		t.Fatalf("verify after crash and retry: %v", err)
	}
}

func withAppName(dsn, name string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "application_name=" + name
}

func installDelayTrigger(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	exec(t, db, ctx, `CREATE OR REPLACE FUNCTION tkt56_delay_head() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN PERFORM pg_sleep(60); RETURN NEW; END $$`)
	exec(t, db, ctx, `DROP TRIGGER IF EXISTS tkt56_delay_head_trigger ON journal_heads`)
	exec(t, db, ctx, `CREATE TRIGGER tkt56_delay_head_trigger BEFORE UPDATE ON journal_heads FOR EACH ROW EXECUTE FUNCTION tkt56_delay_head()`)
}

func dropDelayTrigger(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	exec(t, db, ctx, `DROP TRIGGER IF EXISTS tkt56_delay_head_trigger ON journal_heads`)
	// Drop the function too: a trigger function left behind in the database is what makes
	// a later CREATE TRIGGER silently reuse a sleep nobody remembers installing.
	exec(t, db, ctx, `DROP FUNCTION IF EXISTS tkt56_delay_head()`)
}

// awaitBackendInHeadUpdate finds the append's backend while it is blocked INSIDE the
// head update, and nothing else.
//
// The predicate is load-bearing, not incidental. Matching merely on application_name
// could terminate an idle pool connection before the entry INSERT ever ran — and every
// later assertion would still pass, leaving a test that proves only "killing a
// connection rolls back whatever it did" while claiming to pin the insert-to-commit
// boundary. Requiring the PgSleep wait AND the UPDATE journal_heads statement is what
// makes reaching this point mean what the test says it means; it also makes a
// mis-installed trigger self-detecting, because then Append simply completes and the
// entry-absent assertion fails loudly.
func awaitBackendInHeadUpdate(t *testing.T, db *sql.DB, ctx context.Context, appName string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var pid int
		err := db.QueryRowContext(ctx, `
			SELECT pid FROM pg_stat_activity
			WHERE application_name = $1
			  AND wait_event_type = 'Timeout' AND wait_event = 'PgSleep'
			  AND query LIKE '%UPDATE journal_heads%'`, appName).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no backend reached the head update within 30s: the delay trigger did not fire, or pg_sleep is not reported as wait_event='PgSleep' on this PostgreSQL")
	return 0
}
