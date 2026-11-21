//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Migration 0027 (TKT-525, ADR-087): the append-only refund_outcomes table and the
// evidence-linked completion rule on order_refunds.
//
// Each case names the EXACT constraint it expects to fire. A test that only asserts "the
// insert failed" stays green when the constraint under test is deleted and a different one
// happens to reject the same row.

// seedRefundAt inserts one order_refunds row directly, in whichever schema version the
// caller's database is at.
func seedRefundAt(t *testing.T, ctx context.Context, db *sql.DB, org, order uuid.UUID, status string, fact uuid.NullUUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	completedAt := "NULL"
	if status == "completed" {
		completedAt = "now()"
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(id,order_id,organizer_id,idempotency_key,request_fingerprint,
		                          quantity,unit_amount,amount,currency,actor,reason,status,
		                          completed_at,payment_fact_id)
		VALUES($1,$2,$3,$4,'fp',1,1000,1000,'EUR','ops@example.test','seed',$5,`+completedAt+`,$6)`,
		id, order, org, "k-"+id.String(), status, fact); err != nil {
		t.Fatal(err)
	}
	return id
}

// insertOutcome writes one outcome row as the command would, session_user included.
func insertOutcome(ctx context.Context, db execer, org uuid.UUID, subjectKind string, subject uuid.UUID, kind string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO refund_outcomes(organizer_id,subject_kind,subject_id,kind,amount,currency,
		                            external_ref,operator_name,session_user_name,reason)
		VALUES($1,$2,$3,$4,1000,'EUR','bank-transfer-1','Ada Operator',session_user,'paid by bank transfer')`,
		org, subjectKind, subject, kind)
	return err
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// violated returns the constraint name a PostgreSQL error reports, or "" for any other error.
func violated(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.ConstraintName
	}
	return ""
}

func completionChecks(t *testing.T, ctx context.Context, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT c.conname FROM pg_constraint c
		WHERE c.conrelid = 'order_refunds'::regclass AND c.contype = 'c'
		  AND (SELECT array_agg(a.attname::text ORDER BY a.attname::text) FROM pg_attribute a
		        WHERE a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey))
		      = ARRAY['completed_at','payment_fact_id','status']`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// Upgrading a database that holds pending and payments-completed refunds keeps both, gives
// them the constant subject kind and no outside evidence, and swaps 0007's completion CHECK for
// the named three-state rule while the status vocabulary CHECK stays.
func TestMigration0027PreservesLegacyRefundsAndReplacesTheCompletionCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.UpTo(ctx, 26); err != nil {
		t.Fatalf("apply migrations through 0026: %v", err)
	}
	if got := completionChecks(t, ctx, db); len(got) != 1 {
		t.Fatalf("at 0026 there must be exactly one 0007 completion CHECK, found %v", got)
	}
	org := uuid.New()
	order := seedV4OrderForOrganizer(t, ctx, db, org)
	pending := seedRefundAt(t, ctx, db, org, order, "pending", uuid.NullUUID{})
	fact := uuid.New()
	completed := seedRefundAt(t, ctx, db, org, order, "completed", uuid.NullUUID{UUID: fact, Valid: true})

	if _, err := provider.UpTo(ctx, 27); err != nil {
		t.Fatalf("apply 0027 over legacy refunds: %v", err)
	}

	for _, r := range []struct {
		id     uuid.UUID
		status string
		fact   uuid.NullUUID
	}{{pending, "pending", uuid.NullUUID{}}, {completed, "completed", uuid.NullUUID{UUID: fact, Valid: true}}} {
		var status, subjectKind string
		var gotFact uuid.NullUUID
		var outside sql.NullString
		if err := db.QueryRowContext(ctx, `
			SELECT status, payment_fact_id, outcome_subject_kind, outside_completion_kind
			FROM order_refunds WHERE organizer_id=$1 AND id=$2`, org, r.id).
			Scan(&status, &gotFact, &subjectKind, &outside); err != nil {
			t.Fatalf("legacy %s refund lost by 0027: %v", r.status, err)
		}
		if status != r.status || gotFact != r.fact || subjectKind != "order_refund" || outside.Valid {
			t.Errorf("legacy refund after 0027: status=%s fact=%v subject_kind=%s outside=%v, want %s %v order_refund NULL",
				status, gotFact, subjectKind, outside, r.status, r.fact)
		}
	}

	var named, vocabulary int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE conname = 'order_refunds_completion_evidence'),
		       count(*) FILTER (WHERE pg_get_constraintdef(oid) LIKE '%''pending''::text, ''completed''::text%')
		FROM pg_constraint WHERE conrelid = 'order_refunds'::regclass AND contype = 'c'`).
		Scan(&named, &vocabulary); err != nil {
		t.Fatal(err)
	}
	if named != 1 {
		t.Error("order_refunds_completion_evidence is missing after 0027")
	}
	if vocabulary != 1 {
		t.Error("the 0007 status vocabulary CHECK did not survive 0027; it was not the CHECK to drop")
	}
	if got := completionChecks(t, ctx, db); len(got) != 0 {
		t.Errorf("0007's completion CHECK over exactly (status, completed_at, payment_fact_id) survived 0027: %v. "+
			"It forbids a completed refund without a payments fact, so no outside completion can land", got)
	}
}

// The upgrade refuses, rather than guesses, when the 0007 completion CHECK cannot be found
// exactly once.
func TestMigration0027RefusesWhenTheCompletionCheckIsNotUnique(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.UpTo(ctx, 26); err != nil {
		t.Fatalf("apply migrations through 0026: %v", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE order_refunds ADD CONSTRAINT second_completion_rule
		CHECK (status = 'pending' OR (completed_at IS NOT NULL AND payment_fact_id IS NOT NULL))`); err != nil {
		t.Fatal(err)
	}
	_, err := provider.UpTo(ctx, 27)
	if err == nil || !strings.Contains(err.Error(), "expected exactly one completion CHECK") {
		t.Fatalf("0027 applied with two candidate completion CHECKs (err=%v). Dropping either one is a guess", err)
	}
	if got := completionChecks(t, ctx, db); len(got) != 2 {
		t.Fatalf("a refused 0027 changed the constraints: %v", got)
	}
}

// Each admissible and inadmissible completion shape, with the exact constraint that decides it.
func TestMigration0027CompletionEvidenceRule(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}
	org := uuid.New()
	order := seedV4OrderForOrganizer(t, ctx, db, org)

	// complete applies one completion shape to a fresh pending refund and reports the
	// violated constraint ("" on success). The refund and any outcome are created first.
	type shape struct {
		name         string
		outcomeOrg   func(uuid.UUID) uuid.UUID        // organizer the outcome is written under
		outcomeSubj  func(refund uuid.UUID) uuid.UUID // subject id the outcome names
		outcomeKind  string                           // "" = no outcome
		status       string
		fact         bool
		outsideKind  string // "" = NULL
		completedAt  bool
		wantViolated string
	}
	same := func(id uuid.UUID) uuid.UUID { return id }
	other := func(uuid.UUID) uuid.UUID { return uuid.New() }
	cases := []shape{
		{name: "outside completion with a matching refunded_outside outcome", outcomeOrg: same, outcomeSubj: same,
			outcomeKind: "refunded_outside", status: "completed", outsideKind: "refunded_outside", completedAt: true},
		{name: "ordinary completion with a payments fact", status: "completed", fact: true, completedAt: true},
		{name: "completed with no evidence of either kind", status: "completed", completedAt: true,
			wantViolated: "order_refunds_completion_evidence"},
		{name: "completed outside with no outcome row", status: "completed", outsideKind: "refunded_outside", completedAt: true,
			wantViolated: "order_refunds_outside_completion_outcome_fkey"},
		{name: "completed outside with an abandoned outcome", outcomeOrg: same, outcomeSubj: same,
			outcomeKind: "abandoned", status: "completed", outsideKind: "refunded_outside", completedAt: true,
			wantViolated: "order_refunds_outside_completion_outcome_fkey"},
		{name: "completed outside naming the abandoned kind", outcomeOrg: same, outcomeSubj: same,
			outcomeKind: "abandoned", status: "completed", outsideKind: "abandoned", completedAt: true,
			wantViolated: "order_refunds_completion_evidence"},
		{name: "completed outside with the outcome in another organizer", outcomeOrg: other, outcomeSubj: same,
			outcomeKind: "refunded_outside", status: "completed", outsideKind: "refunded_outside", completedAt: true,
			wantViolated: "order_refunds_outside_completion_outcome_fkey"},
		{name: "completed outside with the outcome on another subject", outcomeOrg: same, outcomeSubj: other,
			outcomeKind: "refunded_outside", status: "completed", outsideKind: "refunded_outside", completedAt: true,
			wantViolated: "order_refunds_outside_completion_outcome_fkey"},
		{name: "both completion sources", outcomeOrg: same, outcomeSubj: same,
			outcomeKind: "refunded_outside", status: "completed", fact: true, outsideKind: "refunded_outside", completedAt: true,
			wantViolated: "order_refunds_completion_evidence"},
		{name: "outside evidence on a pending row", outcomeOrg: same, outcomeSubj: same,
			outcomeKind: "refunded_outside", status: "pending", outsideKind: "refunded_outside",
			wantViolated: "order_refunds_completion_evidence"},
		{name: "outside completion with no completion time", outcomeOrg: same, outcomeSubj: same,
			outcomeKind: "refunded_outside", status: "completed", outsideKind: "refunded_outside",
			wantViolated: "order_refunds_completion_evidence"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			refund := seedRefundAt(t, ctx, db, org, order, "pending", uuid.NullUUID{})
			if c.outcomeKind != "" {
				if err := insertOutcome(ctx, db, c.outcomeOrg(org), "order_refund", c.outcomeSubj(refund), c.outcomeKind); err != nil {
					t.Fatalf("seed outcome: %v", err)
				}
			}
			fact := uuid.NullUUID{}
			if c.fact {
				fact = uuid.NullUUID{UUID: uuid.New(), Valid: true}
			}
			outside := sql.NullString{String: c.outsideKind, Valid: c.outsideKind != ""}
			completedAt := "NULL"
			if c.completedAt {
				completedAt = "now()"
			}
			_, err := db.ExecContext(ctx, `
				UPDATE order_refunds SET status=$3, payment_fact_id=$4, outside_completion_kind=$5, completed_at=`+completedAt+`
				WHERE organizer_id=$1 AND id=$2`, org, refund, c.status, fact, outside)
			if c.wantViolated == "" {
				if err != nil {
					t.Fatalf("admissible shape refused: %v", err)
				}
				return
			}
			if got := violated(err); got != c.wantViolated {
				t.Fatalf("violated %q (err=%v), want %q", got, err, c.wantViolated)
			}
		})
	}

	t.Run("a subject kind other than order_refund", func(t *testing.T) {
		refund := seedRefundAt(t, ctx, db, org, order, "pending", uuid.NullUUID{})
		_, err := db.ExecContext(ctx, `UPDATE order_refunds SET outcome_subject_kind='recovery' WHERE organizer_id=$1 AND id=$2`, org, refund)
		if got := violated(err); got != "order_refunds_outcome_subject_kind_check" {
			t.Fatalf("violated %q (err=%v), want order_refunds_outcome_subject_kind_check", got, err)
		}
	})
}

// The outcome row's own constraints, each with its exact name.
func TestMigration0027OutcomeRowConstraints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}
	valid := map[string]string{
		"subject_kind": "'order_refund'", "kind": "'refunded_outside'", "amount": "1000",
		"currency": "'EUR'", "external_ref": "'bank-1'", "operator_name": "'Ada'",
		"session_user_name": "session_user", "reason": "'paid outside'",
	}
	cols := []string{"subject_kind", "kind", "amount", "currency", "external_ref", "operator_name", "session_user_name", "reason"}
	insert := func(override map[string]string) error {
		var vals []string
		for _, c := range cols {
			v := valid[c]
			if o, ok := override[c]; ok {
				v = o
			}
			vals = append(vals, v)
		}
		_, err := db.ExecContext(ctx, `INSERT INTO refund_outcomes(organizer_id,subject_id,`+strings.Join(cols, ",")+`)
			VALUES($1,$2,`+strings.Join(vals, ",")+`)`, uuid.New(), uuid.New())
		return err
	}
	if err := insert(nil); err != nil {
		t.Fatalf("the valid outcome was refused: %v", err)
	}
	for _, c := range []struct {
		col, value, want string
	}{
		{"subject_kind", "'recovery'", "refund_outcomes_subject_kind_check"},
		{"kind", "'refunded'", "refund_outcomes_kind_check"},
		{"amount", "0", "refund_outcomes_amount_check"},
		{"currency", "'eur'", "refund_outcomes_currency_check"},
		{"external_ref", "'  '", "refund_outcomes_external_ref_check"},
		{"operator_name", "' '", "refund_outcomes_operator_name_check"},
		{"session_user_name", "''", "refund_outcomes_session_user_name_check"},
		{"reason", "''", "refund_outcomes_reason_check"},
	} {
		t.Run(c.col, func(t *testing.T) {
			if got := violated(insert(map[string]string{c.col: c.value})); got != c.want {
				t.Fatalf("%s=%s violated %q, want %q", c.col, c.value, got, c.want)
			}
		})
	}

	// The reference limit depends on the kind: 200 bytes for an outside reference typed by an
	// operator, 255 for a provider reference payments returned.
	for _, c := range []struct {
		kind   string
		length int
		want   string
	}{
		{"refunded_outside", 200, ""},
		{"refunded_outside", 201, "refund_outcomes_external_ref_check"},
		{"abandoned", 255, ""},
		{"abandoned", 256, "refund_outcomes_external_ref_check"},
	} {
		ref := "'re_" + strings.Repeat("x", c.length-3) + "'"
		if got := violated(insert(map[string]string{"kind": "'" + c.kind + "'", "external_ref": ref})); got != c.want {
			t.Fatalf("%s with a %d-byte reference violated %q, want %q", c.kind, c.length, got, c.want)
		}
	}

	// One outcome per subject, whatever its kind.
	org, subject := uuid.New(), uuid.New()
	if err := insertOutcome(ctx, db, org, "order_refund", subject, "abandoned"); err != nil {
		t.Fatal(err)
	}
	if got := violated(insertOutcome(ctx, db, org, "order_refund", subject, "refunded_outside")); got != "refund_outcomes_pkey" {
		t.Fatalf("a second outcome for one subject violated %q, want refund_outcomes_pkey", got)
	}
}

// UPDATE, DELETE and TRUNCATE are each refused; the row survives all three.
func TestMigration0027OutcomesAreAppendOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}
	org, subject := uuid.New(), uuid.New()
	if err := insertOutcome(ctx, db, org, "order_refund", subject, "refunded_outside"); err != nil {
		t.Fatal(err)
	}
	for name, stmt := range map[string]string{
		"update": `UPDATE refund_outcomes SET reason='rewritten'`,
		"delete": `DELETE FROM refund_outcomes`,
		// CASCADE, because a plain TRUNCATE is already refused by the foreign key from
		// order_refunds, which would hide a missing statement-level trigger. With CASCADE
		// only the trigger stands between the statement and both tables.
		"truncate": `TRUNCATE refund_outcomes CASCADE`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, stmt)
			if err == nil || !strings.Contains(err.Error(), "refund outcomes are append-only") {
				t.Fatalf("%s was not refused by the append-only trigger (err=%v)", name, err)
			}
		})
	}
	var reason string
	if err := db.QueryRowContext(ctx, `SELECT reason FROM refund_outcomes WHERE organizer_id=$1 AND subject_id=$2`, org, subject).Scan(&reason); err != nil || reason != "paid by bank transfer" {
		t.Fatalf("the outcome did not survive intact: reason=%q err=%v", reason, err)
	}
}

// insertDisposition writes one pending abandonment reservation, as ReserveAbandonment would.
func insertDisposition(ctx context.Context, db execer, org, refund uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO order_refund_abandonment_dispositions(organizer_id,refund_id,state,charge_key,amount,currency,operator_name,reason)
		VALUES($1,$2,'pending','checkout-key',1000,'EUR','Ada Operator','nobody will refund this attempt')`, org, refund)
	return err
}

// The reservation table: scoped to an existing refund of the same organizer, one row per refund,
// and the only admitted change is pending -> finalized with every other column kept. No delete
// and no truncate.
func TestMigration0027DispositionRules(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}
	org := uuid.New()
	order := seedV4OrderForOrganizer(t, ctx, db, org)
	refund := seedRefundAt(t, ctx, db, org, order, "pending", uuid.NullUUID{})

	if got := violated(insertDisposition(ctx, db, uuid.New(), refund)); got != "order_refund_abandonment_dispositions_refund_fkey" {
		t.Fatalf("a reservation under another organizer violated %q, want the refund foreign key", got)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO order_refund_abandonment_dispositions(organizer_id,refund_id,state,charge_key,amount,currency,operator_name,reason)
		VALUES($1,$2,'abandoned','k',1000,'EUR','Ada','r')`, org, refund)
	if got := violated(err); got != "order_refund_abandonment_dispositions_state_check" {
		t.Fatalf("an unknown state violated %q, want the state check", got)
	}
	if err := insertDisposition(ctx, db, org, refund); err != nil {
		t.Fatalf("the valid reservation was refused: %v", err)
	}
	if got := violated(insertDisposition(ctx, db, org, refund)); got != "order_refund_abandonment_dispositions_pkey" {
		t.Fatalf("a second reservation violated %q, want the primary key", got)
	}

	const guard = "abandonment dispositions move only from pending to finalized"
	for name, stmt := range map[string]string{
		"change the reason":    `UPDATE order_refund_abandonment_dispositions SET reason='rewritten' WHERE refund_id=$1`,
		"finalize and rewrite": `UPDATE order_refund_abandonment_dispositions SET state='finalized', operator_name='Grace' WHERE refund_id=$1`,
		"delete":               `DELETE FROM order_refund_abandonment_dispositions WHERE refund_id=$1`,
		"truncate":             `TRUNCATE order_refund_abandonment_dispositions`,
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			if strings.Contains(stmt, "$1") {
				_, err = db.ExecContext(ctx, stmt, refund)
			} else {
				_, err = db.ExecContext(ctx, stmt)
			}
			if err == nil || !strings.Contains(err.Error(), guard) {
				t.Fatalf("%s was not refused by the guard (err=%v)", name, err)
			}
		})
	}
	if _, err := db.ExecContext(ctx, `UPDATE order_refund_abandonment_dispositions SET state='finalized' WHERE refund_id=$1`, refund); err != nil {
		t.Fatalf("pending -> finalized was refused: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE order_refund_abandonment_dispositions SET state='pending' WHERE refund_id=$1`, refund); err == nil || !strings.Contains(err.Error(), guard) {
		t.Fatalf("finalized -> pending was not refused (err=%v)", err)
	}
}

func schemaVersion(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRowContext(ctx, `SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The Down refuses while ANY outcome exists, with only that history present, and leaves the
// version and the record intact. Once the history is gone it restores 0007's completion rule
// and removes every new object. Both directions: a refusal-only test is satisfied by a Down that
// always raises.
func TestMigration0027DownRefusesOverOutcomeHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}
	// An abandoned outcome only: no refund row references it, so the refusal is the history
	// check's alone and not a side effect of a foreign key or of a completed refund.
	org, subject := uuid.New(), uuid.New()
	if err := insertOutcome(ctx, db, org, "order_refund", subject, "abandoned"); err != nil {
		t.Fatal(err)
	}
	_, err := provider.DownTo(ctx, 26)
	if err == nil || !strings.Contains(err.Error(), "cannot roll back 0027") {
		t.Fatalf("0027 rolled back over refund outcome history (err=%v)", err)
	}
	if v := schemaVersion(t, ctx, db); v < 27 {
		t.Fatalf("a refused Down left the schema at %d", v)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM refund_outcomes WHERE organizer_id=$1 AND subject_id=$2`, org, subject).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a refused Down did not leave the outcome intact: n=%d err=%v", n, err)
	}

	// Clear the history the only way the triggers allow: with them disabled.
	if _, err := db.ExecContext(ctx, `ALTER TABLE refund_outcomes DISABLE TRIGGER USER; DELETE FROM refund_outcomes; ALTER TABLE refund_outcomes ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	refundOrder := seedV4OrderForOrganizer(t, ctx, db, org)
	seedRefundAt(t, ctx, db, org, refundOrder, "completed", uuid.NullUUID{UUID: uuid.New(), Valid: true})
	if _, err := provider.DownTo(ctx, 26); err != nil {
		t.Fatalf("0027 down with no outcome history: %v", err)
	}
	if v := schemaVersion(t, ctx, db); v != 26 {
		t.Fatalf("schema at %d after a clean Down, want 26", v)
	}
	var tableLeft, columnsLeft int
	if err := db.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='refund_outcomes'),
		       (SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='order_refunds'
		          AND column_name IN ('outcome_subject_kind','outside_completion_kind'))`).Scan(&tableLeft, &columnsLeft); err != nil {
		t.Fatal(err)
	}
	if tableLeft != 0 || columnsLeft != 0 {
		t.Fatalf("Down left refund_outcomes=%d new columns=%d", tableLeft, columnsLeft)
	}
	// 0007's rule is back: a completed refund without a payments fact is refused again.
	pending := seedRefundAt(t, ctx, db, org, refundOrder, "pending", uuid.NullUUID{})
	_, err = db.ExecContext(ctx, `UPDATE order_refunds SET status='completed', completed_at=now() WHERE id=$1`, pending)
	if got := violated(err); got != "order_refunds_completion_check" {
		t.Fatalf("after Down a completed refund with no fact violated %q (err=%v), want the restored order_refunds_completion_check", got, err)
	}
	// And Up applies again over the restored rule.
	if _, err := provider.UpTo(ctx, 27); err != nil {
		t.Fatalf("re-apply 0027 after Down: %v", err)
	}
}

// The Down takes its table locks BEFORE it checks for history. An outcome insert that is open
// when the Down starts makes the Down wait at its LOCK; when the insert commits, the history
// check must see it and refuse. A Down that checked first would see no committed outcome, then
// wait at the LOCK, then drop the table with the new outcome in it.
func TestMigration0027DownLocksBeforeItChecksHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}

	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	org, subject := uuid.New(), uuid.New()
	if err := insertOutcome(ctx, writer, org, "order_refund", subject, "abandoned"); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := provider.DownTo(ctx, 26)
		done <- err
	}()
	waitForLockWait(t, ctx, db, "LOCK TABLE order_refunds, refund_outcomes", done)
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "cannot roll back 0027") {
			t.Fatalf("0027 rolled back over an outcome that committed while it waited (err=%v). "+
				"The history check ran before the lock", err)
		}
	case <-ctx.Done():
		t.Fatal("the Down never finished")
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM refund_outcomes WHERE organizer_id=$1`, org).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the committed outcome is gone: n=%d err=%v", n, err)
	}
}

// The Down refuses with ONLY reservation history: a pending reservation can be the only local
// record that payments released its allowance. Without it the Down removes the table.
func TestMigration0027DownRefusesOverDispositionHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}
	org := uuid.New()
	refund := seedRefundAt(t, ctx, db, org, seedV4OrderForOrganizer(t, ctx, db, org), "pending", uuid.NullUUID{})
	if err := insertDisposition(ctx, db, org, refund); err != nil {
		t.Fatal(err)
	}
	_, err := provider.DownTo(ctx, 26)
	if err == nil || !strings.Contains(err.Error(), "abandonment disposition history exists") {
		t.Fatalf("0027 rolled back over a reservation (err=%v)", err)
	}
	if v := schemaVersion(t, ctx, db); v < 27 {
		t.Fatalf("a refused Down left the schema at %d", v)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE order_refund_abandonment_dispositions DISABLE TRIGGER USER; DELETE FROM order_refund_abandonment_dispositions; ALTER TABLE order_refund_abandonment_dispositions ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 26); err != nil {
		t.Fatalf("0027 down with no history: %v", err)
	}
	var left int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='order_refund_abandonment_dispositions'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("Down left the reservation table: %d err=%v", left, err)
	}
	if _, err := provider.UpTo(ctx, 27); err != nil {
		t.Fatalf("re-apply 0027 after Down: %v", err)
	}
}

// The reservation check runs after the Down's lock, like the outcome check. A reservation insert
// open when the Down starts holds a key-share lock on its refund row, so the Down waits at its
// LOCK; when the insert commits, the check must see it and refuse.
func TestMigration0027DownLocksBeforeItChecksDispositions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}
	org := uuid.New()
	refund := seedRefundAt(t, ctx, db, org, seedV4OrderForOrganizer(t, ctx, db, org), "pending", uuid.NullUUID{})
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	if err := insertDisposition(ctx, writer, org, refund); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := provider.DownTo(ctx, 26)
		done <- err
	}()
	waitForLockWait(t, ctx, db, "LOCK TABLE order_refunds, refund_outcomes", done)
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "abandonment disposition history exists") {
			t.Fatalf("0027 rolled back over a reservation that committed while it waited (err=%v)", err)
		}
	case <-ctx.Done():
		t.Fatal("the Down never finished")
	}
}

// waitForLockWait blocks until a backend in this database is waiting on a lock while running a
// statement that starts with prefix, and fails if the observed work finishes first.
func waitForLockWait(t *testing.T, ctx context.Context, db *sql.DB, prefix string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("the statement under observation finished before it waited on a lock (err=%v)", err)
		default:
		}
		var waiting bool
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			               WHERE datname = current_database() AND wait_event_type = 'Lock'
			                 AND ltrim(query, E' \t\n\r') LIKE ltrim($1, E' \t\n\r') || '%')`, prefix).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend ever waited on a lock in %q", prefix)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
