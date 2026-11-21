//go:build smoke

package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// Migration 0028 (TKT-498): the abandonment columns on order_exchanges, their named CHECKs,
// the write-once trigger, the replacement-reservation index and a Down that refuses over any
// started abandonment.
//
// Each refusal case names the EXACT constraint it expects, on a row whose other constraints are
// satisfied, so a case cannot stay green when its constraint is removed and another one happens
// to reject the same row.

// abandonmentChecks are the named CHECKs 0028 adds.
var abandonmentChecks = []string{
	"order_exchanges_abandonment_start_together",
	"order_exchanges_abandonment_start_on_unswitched_settlement",
	"order_exchanges_abandonment_operator_name_check",
	"order_exchanges_abandonment_reason_check",
	"order_exchanges_abandonment_session_user_check",
	"order_exchanges_abandonment_refund_evidence",
	"order_exchanges_abandonment_refund_failed_ref_check",
	"order_exchanges_abandonment_target_return_shape",
	"order_exchanges_abandonment_terminal_shape",
	"order_exchanges_abandonment_write_off_shape",
}

var abandonmentColumns = []string{
	"abandonment_started_at", "abandonment_operator_name", "abandonment_session_user", "abandonment_reason",
	"abandonment_refund_fact_id", "abandonment_refund_failed_ref", "abandonment_target_returned_at",
	"abandonment_written_off_amount", "abandoned_at",
}

func countCatalog(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func abandonmentObjects(t *testing.T, ctx context.Context, db *sql.DB) (columns, checks, triggers, indexes int) {
	t.Helper()
	columns = countCatalog(t, ctx, db, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name='order_exchanges' AND column_name = ANY($1)`, abandonmentColumns)
	checks = countCatalog(t, ctx, db, `SELECT count(*) FROM pg_constraint
		WHERE conrelid='order_exchanges'::regclass AND contype='c' AND conname = ANY($1)`, abandonmentChecks)
	triggers = countCatalog(t, ctx, db, `SELECT count(*) FROM pg_trigger
		WHERE tgrelid='order_exchanges'::regclass AND tgname='order_exchanges_abandonment_write_once'`)
	indexes = countCatalog(t, ctx, db, `SELECT count(*) FROM pg_indexes
		WHERE schemaname=current_schema() AND indexname='order_exchanges_replacement_reservation_idx'`)
	return
}

// startStmt reserves an abandonment the way the command does, session_user included.
const startStmt = `UPDATE order_exchanges SET abandonment_started_at=now(), abandonment_operator_name='Ada Operator',
	abandonment_session_user=session_user, abandonment_reason='source admitted before the switch'
	WHERE organizer_id=$1 AND id=$2`

// Upgrading a database that holds a settled and an unsettled exchange keeps both rows as they
// were, leaves every new column NULL, and adds every named object.
func TestMigration0028PreservesLegacyExchanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.UpTo(ctx, 27); err != nil {
		t.Fatalf("apply migrations through 0027: %v", err)
	}
	settled := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	unsettled := seedAbandonable(t, db, ctx, abandonSeedOpts{Unsettled: true})

	if _, err := provider.UpTo(ctx, 28); err != nil {
		t.Fatalf("apply 0028 over legacy exchanges: %v", err)
	}
	for _, f := range []abandonFixture{settled, unsettled} {
		var delta int64
		var settledAt sql.NullTime
		var nonNull int
		if err := db.QueryRowContext(ctx, `
			SELECT delta_amount, settled_at,
			       num_nonnulls(abandonment_started_at, abandonment_operator_name, abandonment_session_user,
			                    abandonment_reason, abandonment_refund_fact_id, abandonment_refund_failed_ref,
			                    abandonment_target_returned_at, abandonment_written_off_amount, abandoned_at)
			FROM order_exchanges WHERE organizer_id=$1 AND id=$2`, f.Org, f.ExchangeID).
			Scan(&delta, &settledAt, &nonNull); err != nil {
			t.Fatalf("legacy exchange lost by 0028: %v", err)
		}
		if delta != f.Delta || settledAt.Valid == (f == unsettled) || nonNull != 0 {
			t.Errorf("legacy exchange after 0028: delta=%d settled=%v non-null abandonment fields=%d, want %d %v 0",
				delta, settledAt.Valid, nonNull, f.Delta, f != unsettled)
		}
	}
	columns, checks, triggers, indexes := abandonmentObjects(t, ctx, db)
	if columns != len(abandonmentColumns) || checks != len(abandonmentChecks) || triggers != 1 || indexes != 1 {
		t.Fatalf("0028 objects: columns=%d checks=%d triggers=%d indexes=%d, want %d %d 1 1",
			columns, checks, triggers, indexes, len(abandonmentColumns), len(abandonmentChecks))
	}
}

// constraintCase brings a fresh candidate through `prep` (each statement must succeed), then
// runs `violate`, which must fail on exactly `want`. An empty `want` means the shape is
// admissible. `$1` and `$2` are the organizer and exchange.
type constraintCase struct {
	name      string
	unit      int64 // target unit: 2000 upgrade, 1000 downgrade, 1500 even
	unsettled bool
	prep      []string
	violate   string
	want      string
}

const (
	setRefund      = `UPDATE order_exchanges SET abandonment_refund_fact_id=gen_random_uuid() WHERE organizer_id=$1 AND id=$2`
	setFailure     = `UPDATE order_exchanges SET abandonment_refund_failed_ref='re_3Pq' WHERE organizer_id=$1 AND id=$2`
	setReturned    = `UPDATE order_exchanges SET abandonment_target_returned_at=now() WHERE organizer_id=$1 AND id=$2`
	setTerminal    = `UPDATE order_exchanges SET abandoned_at=now() WHERE organizer_id=$1 AND id=$2`
	setTerminalOff = `UPDATE order_exchanges SET abandoned_at=now(), abandonment_written_off_amount=-delta_amount WHERE organizer_id=$1 AND id=$2`
)

func startWith(operator, sessionUser, reason string) string {
	return `UPDATE order_exchanges SET abandonment_started_at=now(), abandonment_operator_name=` + operator +
		`, abandonment_session_user=` + sessionUser + `, abandonment_reason=` + reason + ` WHERE organizer_id=$1 AND id=$2`
}

var constraintCases = []constraintCase{
	// Admissible shapes, one per sign, through every stage.
	{name: "upgrade: start, refund, return, terminal", unit: 2000, prep: []string{startStmt, setRefund, setReturned}, violate: setTerminal},
	{name: "downgrade: start, return, terminal with the write-off", unit: 1000, prep: []string{startStmt, setReturned}, violate: setTerminalOff},
	{name: "even: start, return, terminal", unit: 1500, prep: []string{startStmt, setReturned}, violate: setTerminal},
	{name: "upgrade: recorded provider failure", unit: 2000, prep: []string{startStmt}, violate: setFailure},

	// order_exchanges_abandonment_start_together
	{name: "start without a reason", unit: 2000,
		violate: `UPDATE order_exchanges SET abandonment_started_at=now(), abandonment_operator_name='Ada',
			abandonment_session_user=session_user WHERE organizer_id=$1 AND id=$2`,
		want: "order_exchanges_abandonment_start_together"},
	{name: "operator without a start", unit: 2000,
		violate: `UPDATE order_exchanges SET abandonment_operator_name='Ada', abandonment_session_user=session_user,
			abandonment_reason='r' WHERE organizer_id=$1 AND id=$2`,
		want: "order_exchanges_abandonment_start_together"},
	// order_exchanges_abandonment_start_on_unswitched_settlement
	{name: "start on an unsettled exchange", unit: 2000, unsettled: true, violate: startStmt,
		want: "order_exchanges_abandonment_start_on_unswitched_settlement"},
	{name: "switch marker on a started exchange", unit: 2000, prep: []string{startStmt},
		violate: `UPDATE order_exchanges SET tickets_exchanged_at=now() WHERE organizer_id=$1 AND id=$2`,
		want:    "order_exchanges_abandonment_start_on_unswitched_settlement"},
	{name: "start on a switched exchange", unit: 2000,
		prep:    []string{`UPDATE order_exchanges SET tickets_exchanged_at=now() WHERE organizer_id=$1 AND id=$2`},
		violate: startStmt, want: "order_exchanges_abandonment_start_on_unswitched_settlement"},
	// order_exchanges_abandonment_operator_name_check
	{name: "untrimmed operator", unit: 2000, violate: startWith(`' Ada'`, `session_user`, `'r'`),
		want: "order_exchanges_abandonment_operator_name_check"},
	{name: "empty operator", unit: 2000, violate: startWith(`''`, `session_user`, `'r'`),
		want: "order_exchanges_abandonment_operator_name_check"},
	{name: "201-character operator", unit: 2000, violate: startWith(`repeat('a', 201)`, `session_user`, `'r'`),
		want: "order_exchanges_abandonment_operator_name_check"},
	{name: "200-character operator", unit: 2000, violate: startWith(`repeat('a', 200)`, `session_user`, `'r'`)},
	// order_exchanges_abandonment_reason_check
	{name: "untrimmed reason", unit: 2000, violate: startWith(`'Ada'`, `session_user`, `E'r\n'`),
		want: "order_exchanges_abandonment_reason_check"},
	{name: "empty reason", unit: 2000, violate: startWith(`'Ada'`, `session_user`, `''`),
		want: "order_exchanges_abandonment_reason_check"},
	{name: "501-character reason", unit: 2000, violate: startWith(`'Ada'`, `session_user`, `repeat('r', 501)`),
		want: "order_exchanges_abandonment_reason_check"},
	{name: "500-character reason", unit: 2000, violate: startWith(`'Ada'`, `session_user`, `repeat('r', 500)`)},
	// order_exchanges_abandonment_session_user_check
	{name: "empty session user", unit: 2000, violate: startWith(`'Ada'`, `''`, `'r'`),
		want: "order_exchanges_abandonment_session_user_check"},
	// order_exchanges_abandonment_refund_evidence
	{name: "refund fact before a start", unit: 2000, violate: setRefund,
		want: "order_exchanges_abandonment_refund_evidence"},
	{name: "refund fact on a downgrade", unit: 1000, prep: []string{startStmt}, violate: setRefund,
		want: "order_exchanges_abandonment_refund_evidence"},
	{name: "provider failure on an even exchange", unit: 1500, prep: []string{startStmt}, violate: setFailure,
		want: "order_exchanges_abandonment_refund_evidence"},
	{name: "refund fact and provider failure together", unit: 2000, prep: []string{startStmt, setRefund}, violate: setFailure,
		want: "order_exchanges_abandonment_refund_evidence"},
	// order_exchanges_abandonment_refund_failed_ref_check
	{name: "provider failure reference without re_", unit: 2000, prep: []string{startStmt},
		violate: `UPDATE order_exchanges SET abandonment_refund_failed_ref='pi_3Pq' WHERE organizer_id=$1 AND id=$2`,
		want:    "order_exchanges_abandonment_refund_failed_ref_check"},
	{name: "bare re_ reference", unit: 2000, prep: []string{startStmt},
		violate: `UPDATE order_exchanges SET abandonment_refund_failed_ref='re_' WHERE organizer_id=$1 AND id=$2`,
		want:    "order_exchanges_abandonment_refund_failed_ref_check"},
	{name: "256-byte reference", unit: 2000, prep: []string{startStmt},
		violate: `UPDATE order_exchanges SET abandonment_refund_failed_ref='re_' || repeat('x', 253) WHERE organizer_id=$1 AND id=$2`,
		want:    "order_exchanges_abandonment_refund_failed_ref_check"},
	{name: "255-byte reference", unit: 2000, prep: []string{startStmt},
		violate: `UPDATE order_exchanges SET abandonment_refund_failed_ref='re_' || repeat('x', 252) WHERE organizer_id=$1 AND id=$2`},
	// order_exchanges_abandonment_target_return_shape
	{name: "target return before a start", unit: 1500, violate: setReturned,
		want: "order_exchanges_abandonment_target_return_shape"},
	{name: "target return on an upgrade without its refund", unit: 2000, prep: []string{startStmt}, violate: setReturned,
		want: "order_exchanges_abandonment_target_return_shape"},
	{name: "target return after a provider failure", unit: 2000, prep: []string{startStmt, setFailure}, violate: setReturned,
		want: "order_exchanges_abandonment_target_return_shape"},
	// order_exchanges_abandonment_terminal_shape
	{name: "terminal before the target return", unit: 1500, prep: []string{startStmt}, violate: setTerminal,
		want: "order_exchanges_abandonment_terminal_shape"},
	// order_exchanges_abandonment_write_off_shape
	{name: "terminal downgrade without its write-off", unit: 1000, prep: []string{startStmt, setReturned}, violate: setTerminal,
		want: "order_exchanges_abandonment_write_off_shape"},
	{name: "write-off other than -delta", unit: 1000, prep: []string{startStmt, setReturned},
		violate: `UPDATE order_exchanges SET abandoned_at=now(), abandonment_written_off_amount=1 WHERE organizer_id=$1 AND id=$2`,
		want:    "order_exchanges_abandonment_write_off_shape"},
	{name: "write-off before terminal", unit: 1000, prep: []string{startStmt, setReturned},
		violate: `UPDATE order_exchanges SET abandonment_written_off_amount=-delta_amount WHERE organizer_id=$1 AND id=$2`,
		want:    "order_exchanges_abandonment_write_off_shape"},
	{name: "write-off on a terminal upgrade", unit: 2000, prep: []string{startStmt, setRefund, setReturned},
		violate: `UPDATE order_exchanges SET abandoned_at=now(), abandonment_written_off_amount=1000 WHERE organizer_id=$1 AND id=$2`,
		want:    "order_exchanges_abandonment_write_off_shape"},
}

func TestMigration0028NamedChecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}
	for _, c := range constraintCases {
		t.Run(c.name, func(t *testing.T) {
			f := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: c.unit, Unsettled: c.unsettled})
			for _, stmt := range c.prep {
				if _, err := db.ExecContext(ctx, stmt, f.Org, f.ExchangeID); err != nil {
					t.Fatalf("prep %q: %v", stmt, err)
				}
			}
			_, err := db.ExecContext(ctx, c.violate, f.Org, f.ExchangeID)
			if c.want == "" {
				if err != nil {
					t.Fatalf("admissible shape refused: %v", err)
				}
				return
			}
			if got := violated(err); got != c.want {
				t.Fatalf("violated %q (err=%v), want %q", got, err, c.want)
			}
		})
	}
}

// A populated abandonment field cannot change or be cleared, while an empty one can be filled,
// a field can be rewritten with its own value, and other columns of a started row stay
// writable. Each refused change satisfies every CHECK, so only the trigger can refuse it.
func TestMigration0028AbandonmentFieldsAreWriteOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}
	up := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: 2000})
	down := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: 1000})
	failed := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: 2000})
	for _, step := range []struct {
		f     abandonFixture
		stmts []string
	}{
		{up, []string{startStmt, setRefund, setReturned, setTerminal}},
		{down, []string{startStmt, setReturned, setTerminalOff}},
		{failed, []string{startStmt, setFailure}},
	} {
		for _, stmt := range step.stmts {
			if _, err := db.ExecContext(ctx, stmt, step.f.Org, step.f.ExchangeID); err != nil {
				t.Fatalf("an admissible fill was refused: %q: %v", stmt, err)
			}
		}
	}

	const guard = "exchange abandonment fields are write-once"
	for _, c := range []struct {
		name string
		f    abandonFixture
		set  string
	}{
		{"change the start time", up, `abandonment_started_at = abandonment_started_at - interval '1 minute'`},
		{"change the operator", up, `abandonment_operator_name = 'Grace Operator'`},
		{"change the session user", up, `abandonment_session_user = 'someone_else'`},
		{"change the reason", up, `abandonment_reason = 'rewritten'`},
		{"change the refund fact", up, `abandonment_refund_fact_id = gen_random_uuid()`},
		{"change the target return time", up, `abandonment_target_returned_at = abandonment_target_returned_at - interval '1 minute'`},
		{"clear the terminal marker", up, `abandoned_at = NULL`},
		{"change the provider failure", failed, `abandonment_refund_failed_ref = 're_other'`},
		{"clear the write-off with the terminal marker", down, `abandoned_at = NULL, abandonment_written_off_amount = NULL`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, `UPDATE order_exchanges SET `+c.set+` WHERE organizer_id=$1 AND id=$2`, c.f.Org, c.f.ExchangeID)
			if err == nil || !strings.Contains(err.Error(), guard) {
				t.Fatalf("%s was not refused by the write-once trigger (err=%v)", c.name, err)
			}
		})
	}

	if _, err := db.ExecContext(ctx, `UPDATE order_exchanges SET abandonment_operator_name = abandonment_operator_name,
		reversal_next_attempt_at = now() WHERE organizer_id=$1 AND id=$2`, up.Org, up.ExchangeID); err != nil {
		t.Fatalf("rewriting a field with its own value, or writing another column, was refused: %v", err)
	}
}

// The Down refuses while any abandonment has started, and leaves the version and the row as
// they were. Without started history it removes every new object, and Up applies again.
func TestMigration0028DownRefusesOverStartedHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	if _, err := db.ExecContext(ctx, startStmt, f.Org, f.ExchangeID); err != nil {
		t.Fatal(err)
	}
	_, err := provider.DownTo(ctx, 27)
	if err == nil || !strings.Contains(err.Error(), "cannot roll back 0028") {
		t.Fatalf("0028 rolled back over a started abandonment (err=%v)", err)
	}
	if v := schemaVersion(t, ctx, db); v < 28 {
		t.Fatalf("a refused Down left the schema at %d", v)
	}
	var started bool
	if err := db.QueryRowContext(ctx, `SELECT abandonment_started_at IS NOT NULL FROM order_exchanges
		WHERE organizer_id=$1 AND id=$2`, f.Org, f.ExchangeID).Scan(&started); err != nil || !started {
		t.Fatalf("a refused Down did not leave the started abandonment intact: started=%v err=%v", started, err)
	}

	// Clear the history the only way the trigger allows: with it disabled.
	if _, err := db.ExecContext(ctx, `ALTER TABLE order_exchanges DISABLE TRIGGER order_exchanges_abandonment_write_once;
		UPDATE order_exchanges SET abandonment_started_at=NULL, abandonment_operator_name=NULL,
		       abandonment_session_user=NULL, abandonment_reason=NULL;
		ALTER TABLE order_exchanges ENABLE TRIGGER order_exchanges_abandonment_write_once`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 27); err != nil {
		t.Fatalf("0028 down with no started abandonment: %v", err)
	}
	if v := schemaVersion(t, ctx, db); v != 27 {
		t.Fatalf("schema at %d after a clean Down, want 27", v)
	}
	columns, checks, triggers, indexes := abandonmentObjects(t, ctx, db)
	functions := countCatalog(t, ctx, db, `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname=current_schema() AND p.proname='order_exchanges_abandonment_write_once'`)
	if columns+checks+triggers+indexes+functions != 0 {
		t.Fatalf("Down left columns=%d checks=%d triggers=%d indexes=%d functions=%d", columns, checks, triggers, indexes, functions)
	}
	if n := countCatalog(t, ctx, db, `SELECT count(*) FROM order_exchanges WHERE organizer_id=$1 AND id=$2`, f.Org, f.ExchangeID); n != 1 {
		t.Fatalf("a clean Down lost the exchange row: %d", n)
	}
	if _, err := provider.UpTo(ctx, 28); err != nil {
		t.Fatalf("re-apply 0028 after Down: %v", err)
	}
}

// The Down takes its table lock BEFORE it checks for history. A first reservation that is open
// when the Down starts makes the Down wait at its LOCK; when the reservation commits, the check
// must see it and refuse. A Down that checked first would see nothing started, wait at the
// LOCK, and then drop the columns with the new reservation in them.
func TestMigration0028DownLocksBeforeItChecksHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	if _, err := writer.ExecContext(ctx, startStmt, f.Org, f.ExchangeID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := provider.DownTo(ctx, 27)
		done <- err
	}()
	waitForLockWait(t, ctx, db, "LOCK TABLE order_exchanges", done)
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "cannot roll back 0028") {
			t.Fatalf("0028 rolled back over a reservation that committed while it waited (err=%v). "+
				"The history check ran before the lock", err)
		}
	case <-ctx.Done():
		t.Fatal("the Down never finished")
	}
	var started bool
	if err := db.QueryRowContext(ctx, `SELECT abandonment_started_at IS NOT NULL FROM order_exchanges
		WHERE organizer_id=$1 AND id=$2`, f.Org, f.ExchangeID).Scan(&started); err != nil || !started {
		t.Fatalf("the committed reservation is gone: started=%v err=%v", started, err)
	}
}
