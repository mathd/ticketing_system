//go:build smoke

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// pass admission tests — TKT-87: ADR-005's re_entry_policy enforced over
// ADR-025's repeatable entry/exit events and occurrence protocol. Passes never
// gain `redeemed` (§D1); live denials append nothing; reconciliation records
// facts and derives revisable conflicts (§D2).

func seedPolicy(t *testing.T, ctx context.Context, st *Postgres, s seeded, policy ReEntryPolicy) {
	t.Helper()
	if err := st.UpsertSlotPolicy(ctx, uuid.New(), SlotPolicy{SlotID: s.id.SlotID, OrganizerID: s.id.OrganizerID, Policy: policy}); err != nil {
		t.Fatal(err)
	}
}

func scanInput(s seeded, occ uuid.UUID, direction AdmissionEventType, at time.Time) ScanInput {
	return ScanInput{
		RedeemInput: RedeemInput{
			TicketID: s.ticketID, OrderID: s.id.OrderID, OrganizerID: s.id.OrganizerID, SlotID: s.id.SlotID,
			OccurrenceID: occ, OccurredAt: at,
		},
		Direction: direction,
	}
}

func countEvents(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID, eventType string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type=$2`, ticketID, eventType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestScanMultiEntryAdmitsRepeatedly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	seedPolicy(t, ctx, st, s, ReEntryPolicy{Mode: "multi"})

	for i := 0; i < 2; i++ {
		result, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionEntry, deviceTime().Add(time.Duration(i)*time.Minute)))
		if err != nil {
			t.Fatal(err)
		}
		if !result.Accepted || result.Decision != DecisionAccepted || result.Replayed {
			t.Fatalf("entry %d = %+v, want first-time acceptance", i, result)
		}
	}
	// Any caller of Redeem must be policy-correct too (plan risk 1): a direct
	// Redeem on a pass ticket records an entry, never a redemption.
	result, err := st.Redeem(ctx, occurrenceRedeemInput(s, uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted {
		t.Fatalf("Redeem on a pass ticket = %+v, want accepted-as-entry", result)
	}
	if got := countEvents(t, ctx, db, s.ticketID, "entry"); got != 3 {
		t.Fatalf("entry events = %d, want 3", got)
	}
	if got := countEvents(t, ctx, db, s.ticketID, "redeemed"); got != 0 {
		t.Fatalf("redeemed events = %d, want 0 — passes never redeem (ADR-025 §D1)", got)
	}
}

func TestScanPassRequiresOccurrence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	seedPolicy(t, ctx, st, s, ReEntryPolicy{Mode: "multi"})

	result, err := st.Scan(ctx, ScanInput{RedeemInput: s.redeemInput(), Direction: AdmissionEntry})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.Decision != DecisionOccurrenceRequired {
		t.Fatalf("occurrence-less pass scan = %+v, want occurrence_required denial", result)
	}
	if got := countEvents(t, ctx, db, s.ticketID, "entry") + countEvents(t, ctx, db, s.ticketID, "redeemed"); got != 0 {
		t.Fatalf("events appended on a denial = %d, want 0", got)
	}
}

func TestScanCountLimitedDeniesBeyondMax(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	seedPolicy(t, ctx, st, s, ReEntryPolicy{Mode: "count_limited", MaxEntries: i32(2)})

	for i := 0; i < 2; i++ {
		if _, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionEntry, deviceTime().Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	result, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionEntry, deviceTime().Add(10*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.Decision != DecisionEntryLimitReached {
		t.Fatalf("entry beyond max = %+v, want entry_limit_reached", result)
	}
	if got := countEvents(t, ctx, db, s.ticketID, "entry"); got != 2 {
		t.Fatalf("entry events = %d, want 2 — the denial appended nothing and the count is derived from the trace", got)
	}
}

func TestScanRequiresExitFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())
	seedPolicy(t, ctx, st, s, ReEntryPolicy{Mode: "multi", RequiresExit: true})

	if _, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionEntry, deviceTime())); err != nil {
		t.Fatal(err)
	}
	result, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionEntry, deviceTime().Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.Decision != DecisionExitRequired {
		t.Fatalf("re-entry while inside = %+v, want exit_required", result)
	}
	result, err = st.Scan(ctx, scanInput(s, uuid.New(), AdmissionExit, deviceTime().Add(2*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted {
		t.Fatalf("exit while inside = %+v, want accepted", result)
	}
	result, err = st.Scan(ctx, scanInput(s, uuid.New(), AdmissionEntry, deviceTime().Add(3*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted {
		t.Fatalf("re-entry after exit = %+v, want accepted", result)
	}
	// An exit with no open entry is denied and appends nothing.
	result, err = st.Scan(ctx, scanInput(issueAndSeed(t, ctx, st, ReEntryPolicy{Mode: "multi", RequiresExit: true}), uuid.New(), AdmissionExit, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.Decision != DecisionNotInside {
		t.Fatalf("exit while outside = %+v, want not_inside", result)
	}
	if got := countEvents(t, ctx, db, s.ticketID, "entry"); got != 2 {
		t.Fatalf("entry events = %d, want 2", got)
	}
	if got := countEvents(t, ctx, db, s.ticketID, "exit"); got != 1 {
		t.Fatalf("exit events = %d, want 1", got)
	}
}

func issueAndSeed(t *testing.T, ctx context.Context, st *Postgres, policy ReEntryPolicy) seeded {
	t.Helper()
	s := issueTicket(t, ctx, st, uuid.New())
	seedPolicy(t, ctx, st, s, policy)
	return s
}

func TestScanEntryOccurrenceReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueAndSeed(t, ctx, st, ReEntryPolicy{Mode: "multi"})
	occ := uuid.New()

	first, err := st.Scan(ctx, scanInput(s, occ, AdmissionEntry, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := st.Scan(ctx, scanInput(s, occ, AdmissionEntry, deviceTime().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || !replay.Accepted || !replay.OccurredAt.Equal(first.OccurredAt) {
		t.Fatalf("replay = %+v, want distinguishable replay of the original result at %v", replay, first.OccurredAt)
	}
	if got := countEvents(t, ctx, db, s.ticketID, "entry"); got != 1 {
		t.Fatalf("entry events = %d, want 1 — a replay never re-appends", got)
	}
	// The same occurrence retried as the OTHER direction is never a replay.
	if _, err = st.Scan(ctx, scanInput(s, occ, AdmissionExit, deviceTime())); !errors.Is(err, ErrOccurrenceCollision) {
		t.Fatalf("entry occurrence replayed as exit: err = %v, want ErrOccurrenceCollision", err)
	}
	// Cross-ticket reuse is a collision, not a replay.
	other := issueAndSeed(t, ctx, st, ReEntryPolicy{Mode: "multi"})
	if _, err = st.Scan(ctx, scanInput(other, occ, AdmissionEntry, deviceTime())); !errors.Is(err, ErrOccurrenceCollision) {
		t.Fatalf("cross-ticket occurrence reuse: err = %v, want ErrOccurrenceCollision", err)
	}
	// Non-v4 occurrence ids are refused before anything is decided.
	if _, err = st.Scan(ctx, scanInput(s, uuid.NewSHA1(uuid.NameSpaceOID, []byte("v5")), AdmissionEntry, deviceTime())); err == nil {
		t.Fatal("non-UUIDv4 occurrence accepted")
	}
}

func TestScanSingleAndUnknownPolicyKeepTodaySemantics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	// No policy row at all: Scan IS Redeem (COS 4/7 — fail to today's
	// semantics, never fail-open).
	unknown := issueTicket(t, ctx, st, uuid.New())
	result, err := st.Scan(ctx, scanInput(unknown, uuid.New(), AdmissionEntry, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.Decision != DecisionAccepted {
		t.Fatalf("unknown-policy scan = %+v, want redeemed acceptance", result)
	}
	if got := countEvents(t, ctx, db, unknown.ticketID, "redeemed"); got != 1 {
		t.Fatalf("redeemed events = %d, want 1", got)
	}
	dup, err := st.Scan(ctx, scanInput(unknown, uuid.New(), AdmissionEntry, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	if dup.Accepted || dup.Decision != DecisionAlreadyRedeemed {
		t.Fatalf("second unknown-policy scan = %+v, want already_redeemed", dup)
	}

	// Explicit single policy behaves identically, and an exit scan against it
	// is a distinguishable denial that appends nothing (ADR-025 §D1: single
	// tickets have no entry/exit vocabulary).
	single := issueAndSeed(t, ctx, st, ReEntryPolicy{Mode: "single"})
	result, err = st.Scan(ctx, scanInput(single, uuid.New(), AdmissionExit, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.Decision != DecisionExitNotApplicable {
		t.Fatalf("exit on single slot = %+v, want exit_not_applicable", result)
	}
	if got := countEvents(t, ctx, db, single.ticketID, "exit"); got != 0 {
		t.Fatalf("exit events on single slot = %d, want 0", got)
	}
}

func TestUpsertSlotPolicyIdempotentByEnvelope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	slotID, organizerID, envelopeID := uuid.New(), uuid.New(), uuid.New()

	if err := st.UpsertSlotPolicy(ctx, envelopeID, SlotPolicy{SlotID: slotID, OrganizerID: organizerID, Policy: ReEntryPolicy{Mode: "multi"}}); err != nil {
		t.Fatal(err)
	}
	// Redelivery of the SAME envelope must be a no-op even when its decoded
	// payload differs (it cannot, but the dedup is by envelope id, not bytes).
	if err := st.UpsertSlotPolicy(ctx, envelopeID, SlotPolicy{SlotID: slotID, OrganizerID: organizerID, Policy: ReEntryPolicy{Mode: "single"}}); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := db.QueryRowContext(ctx, `SELECT mode FROM slot_re_entry_policies WHERE slot_id=$1`, slotID).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "multi" {
		t.Fatalf("mode = %s after redelivery, want the first envelope's multi", mode)
	}
	// A NEW envelope for the same slot converges the row (a future catalog
	// re-publication path).
	if err := st.UpsertSlotPolicy(ctx, uuid.New(), SlotPolicy{SlotID: slotID, OrganizerID: organizerID, Policy: ReEntryPolicy{Mode: "count_limited", MaxEntries: i32(4)}}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT mode FROM slot_re_entry_policies WHERE slot_id=$1`, slotID).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "count_limited" {
		t.Fatalf("mode = %s, want count_limited after a new envelope", mode)
	}
}

func conflictStates(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) map[PolicyConflict]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT rule,occurrence_id,status FROM pass_policy_conflicts WHERE ticket_id=$1`, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[PolicyConflict]string{}
	for rows.Next() {
		var rule, status string
		var occ uuid.UUID
		if err := rows.Scan(&rule, &occ, &status); err != nil {
			t.Fatal(err)
		}
		out[PolicyConflict{Rule: PolicyConflictRule(rule), OccurrenceID: occ}] = status
	}
	// A truncated read here returns FEWER conflicts, which is what a passing assertion
	// looks like. Err() is the only thing separating "none" from "the read died".
	if err := rows.Err(); err != nil {
		t.Fatalf("conflict states: %v", err)
	}
	return out
}

func policyAlarms(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) []map[string]any {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT envelope FROM lifecycle_integrity_alarm_outbox WHERE subject=$1 ORDER BY created_at,event_id`, SubjectAdmissionPolicyConflictAlarm)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Schema int            `json:"schema"`
			Data   map[string]any `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Schema != 1 {
			t.Fatalf("policy conflict alarm schema = %d, want 1", envelope.Schema)
		}
		if envelope.Data["ticket_id"] == ticketID.String() {
			out = append(out, envelope.Data)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("policy alarms: %v", err)
	}
	return out
}

func TestPassReconcileRecordsFactualEntryExitOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueAndSeed(t, ctx, st, ReEntryPolicy{Mode: "multi", RequiresExit: true})
	occA, occB, occC := uuid.New(), uuid.New(), uuid.New()

	// Two entries sync first — gate B's re-entry looks like a violation.
	for i, occ := range []uuid.UUID{occA, occB} {
		in := s.reconcileInput(occ, deviceTime().Add(time.Duration(i*10)*time.Minute))
		in.Type = AdmissionEntry
		result, err := st.ReconcileAdmission(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if result.Outcome != ReconcileRecorded {
			t.Fatalf("pass occurrence %d outcome = %s, want recorded — recording is not deciding (ADR-025 §D2)", i, result.Outcome)
		}
	}
	if got := countEvents(t, ctx, db, s.ticketID, "entry"); got != 2 {
		t.Fatalf("entry events = %d, want 2", got)
	}
	for _, banned := range []string{"redeemed", "duplicate_admit"} {
		if got := countEvents(t, ctx, db, s.ticketID, banned); got != 0 {
			t.Fatalf("%s events = %d, want 0 — pass reconciliation never mints them (§D2)", banned, got)
		}
	}
	states := conflictStates(t, ctx, db, s.ticketID)
	if states[PolicyConflict{Rule: ConflictExitRequired, OccurrenceID: occB}] != "raised" {
		t.Fatalf("conflict states = %+v, want exit_required raised on %s", states, occB)
	}

	// The missing exit syncs late, claiming a physical time between the two
	// entries: the derived conflict is withdrawn, nothing on the trail moves.
	in := s.reconcileInput(occC, deviceTime().Add(5*time.Minute))
	in.Type = AdmissionExit
	result, err := st.ReconcileAdmission(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ReconcileRecorded {
		t.Fatalf("late exit outcome = %s, want recorded", result.Outcome)
	}
	states = conflictStates(t, ctx, db, s.ticketID)
	if states[PolicyConflict{Rule: ConflictExitRequired, OccurrenceID: occB}] != "withdrawn" {
		t.Fatalf("conflict states = %+v, want exit_required withdrawn after the late exit", states)
	}
	if got := countEvents(t, ctx, db, s.ticketID, "exit"); got != 1 {
		t.Fatalf("exit events = %d, want 1", got)
	}

	alarms := policyAlarms(t, ctx, db, s.ticketID)
	if len(alarms) != 2 {
		t.Fatalf("policy conflict alarms = %d (%+v), want raise then withdrawal", len(alarms), alarms)
	}
	if alarms[0]["status"] != "raised" || alarms[1]["status"] != "withdrawn" {
		t.Fatalf("alarm statuses = %v,%v want raised,withdrawn", alarms[0]["status"], alarms[1]["status"])
	}
	if alarms[0]["conflict_id"] != alarms[1]["conflict_id"] {
		t.Fatalf("conflict ids differ across the pair: %v vs %v — consumers cannot upsert", alarms[0]["conflict_id"], alarms[1]["conflict_id"])
	}
	if alarms[0]["revisable"] != true {
		t.Fatalf("alarm revisable = %v, want true — this class is a projection, not evidence", alarms[0]["revisable"])
	}
}

func TestCountLimitedReconcileAlarmsSurplus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueAndSeed(t, ctx, st, ReEntryPolicy{Mode: "count_limited", MaxEntries: i32(1)})
	surplus := uuid.New()

	in := s.reconcileInput(uuid.New(), deviceTime())
	in.Type = AdmissionEntry
	if _, err := st.ReconcileAdmission(ctx, in); err != nil {
		t.Fatal(err)
	}
	in = s.reconcileInput(surplus, deviceTime().Add(time.Minute))
	in.Type = AdmissionEntry
	result, err := st.ReconcileAdmission(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ReconcileRecorded {
		t.Fatalf("surplus outcome = %s, want recorded — the fact happened", result.Outcome)
	}
	if got := countEvents(t, ctx, db, s.ticketID, "entry"); got != 2 {
		t.Fatalf("entry events = %d, want 2", got)
	}
	states := conflictStates(t, ctx, db, s.ticketID)
	if states[PolicyConflict{Rule: ConflictEntryLimitReached, OccurrenceID: surplus}] != "raised" {
		t.Fatalf("conflict states = %+v, want entry_limit_reached raised on the surplus entry", states)
	}
}

func TestReconcileReplayBindsType(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueAndSeed(t, ctx, st, ReEntryPolicy{Mode: "multi"})
	occ := uuid.New()

	in := s.reconcileInput(occ, deviceTime())
	in.Type = AdmissionEntry
	if _, err := st.ReconcileAdmission(ctx, in); err != nil {
		t.Fatal(err)
	}
	replay := s.reconcileInput(occ, deviceTime())
	replay.Type = AdmissionEntry
	result, err := st.ReconcileAdmission(ctx, replay)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ReconcileSynced {
		t.Fatalf("same-type replay outcome = %s, want synced", result.Outcome)
	}
	asExit := s.reconcileInput(occ, deviceTime())
	asExit.Type = AdmissionExit
	if _, err := st.ReconcileAdmission(ctx, asExit); !errors.Is(err, ErrOccurrenceCollision) {
		t.Fatalf("entry occurrence re-synced as exit: err = %v, want ErrOccurrenceCollision", err)
	}
}

func TestReconcileExitOnSingleSlotIsRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())

	in := s.reconcileInput(uuid.New(), deviceTime())
	in.Type = AdmissionExit
	if _, err := st.ReconcileAdmission(ctx, in); !errors.Is(err, ErrExitNotApplicable) {
		t.Fatalf("exit occurrence on a single slot: err = %v, want ErrExitNotApplicable — single tickets have no entry/exit vocabulary (§D1)", err)
	}
	if got := countEvents(t, ctx, db, s.ticketID, "exit"); got != 0 {
		t.Fatalf("exit events = %d, want 0", got)
	}
}

func TestScanDegradedPassAdmitOnceThenDenies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueAndSeed(t, ctx, st, ReEntryPolicy{Mode: "multi"})
	corruptChain(t, ctx, db, s.ticketID)
	occ := uuid.New()

	result, err := st.Scan(ctx, scanInput(s, occ, AdmissionEntry, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.Decision != DecisionAdmittedDegraded {
		t.Fatalf("pass scan on corrupt chain = %+v, want the §D6 degraded admission unchanged", result)
	}
	second, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionEntry, deviceTime().Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if second.Accepted || second.Decision != DecisionIntegrityQuarantined {
		t.Fatalf("second degraded pass scan = %+v, want integrity_quarantined", second)
	}
	replay, err := st.Scan(ctx, scanInput(s, occ, AdmissionEntry, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Accepted || !replay.Replayed || replay.Decision != DecisionAdmittedDegraded {
		t.Fatalf("degraded occurrence retry = %+v, want its original result as a replay", replay)
	}
}

func TestPassReconcileBrokenChainRecordsTypedQuarantine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueAndSeed(t, ctx, st, ReEntryPolicy{Mode: "multi", RequiresExit: true})
	corruptChain(t, ctx, db, s.ticketID)
	occ := uuid.New()

	in := s.reconcileInput(occ, deviceTime())
	in.Type = AdmissionExit
	result, err := st.ReconcileAdmission(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ReconcileRecorded {
		t.Fatalf("broken-chain pass fact outcome = %s, want recorded on the quarantine side", result.Outcome)
	}
	var eventType string
	if err := db.QueryRowContext(ctx, `SELECT event_type FROM lifecycle_integrity_quarantine WHERE occurrence_id=$1`, occ).Scan(&eventType); err != nil {
		t.Fatal(err)
	}
	if eventType != "exit" {
		t.Fatalf("quarantine fact typed %q, want exit — the factual action survives the broken chain", eventType)
	}
	if got := countEvents(t, ctx, db, s.ticketID, "exit"); got != 0 {
		t.Fatalf("exit appended to an unverified chain: %d rows", got)
	}
}

// ai-review findings, pinned red-first before their fixes.

// K1: a live exit against a broken-chain pass ticket must never take the §D6
// degraded ADMISSION — an exit admits nobody. It is denied distinguishably,
// records nothing, and owes the integrity alarm.
func TestScanBrokenChainExitIsDeniedNotAdmitted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueAndSeed(t, ctx, st, ReEntryPolicy{Mode: "multi", RequiresExit: true})
	corruptChain(t, ctx, db, s.ticketID)

	result, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionExit, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.Decision != DecisionExitUnverified {
		t.Fatalf("broken-chain exit = %+v, want a non-admitting exit_unverified denial", result)
	}
	var quarantined int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE ticket_id=$1 AND admitted_at IS NOT NULL`, s.ticketID).Scan(&quarantined); err != nil {
		t.Fatal(err)
	}
	if quarantined != 0 {
		t.Fatalf("a denied exit consumed the ticket's one degraded admission (%d rows)", quarantined)
	}
	// The §D6 entry posture is untouched: the NEXT entry still takes it.
	entry, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionEntry, deviceTime()))
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Accepted || entry.Decision != DecisionAdmittedDegraded {
		t.Fatalf("entry after denied exit = %+v, want the degraded admission", entry)
	}
}

// G3: a redemption recorded before the policy projection landed is a physical
// admission — it must consume pass allowance once the policy arrives, or the
// holder gets max_entries + 1.
func TestScanCountsPrePolicyRedemptionTowardAllowance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())

	// Projection skew: redeemed lands under single semantics first.
	if _, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionEntry, deviceTime())); err != nil {
		t.Fatal(err)
	}
	if got := countEvents(t, ctx, db, s.ticketID, "redeemed"); got != 1 {
		t.Fatalf("redeemed events = %d, want 1 (pre-policy single path)", got)
	}
	seedPolicy(t, ctx, st, s, ReEntryPolicy{Mode: "count_limited", MaxEntries: i32(1)})

	result, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionEntry, deviceTime().Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.Decision != DecisionEntryLimitReached {
		t.Fatalf("post-policy entry = %+v, want entry_limit_reached — the redemption consumed the allowance", result)
	}
}

// G4: ADR-025 §D3's identity-before-denial order holds on the degraded path
// for pass events too — a lost-response entry retry against a now-broken
// chain replays its original result, never a collision.
func TestScanDegradedReplayHonorsRecordedEntry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueAndSeed(t, ctx, st, ReEntryPolicy{Mode: "multi"})
	occ := uuid.New()

	first, err := st.Scan(ctx, scanInput(s, occ, AdmissionEntry, deviceTime()))
	if err != nil || !first.Accepted {
		t.Fatalf("entry = %+v err=%v", first, err)
	}
	corruptChain(t, ctx, db, s.ticketID)
	replay, err := st.Scan(ctx, scanInput(s, occ, AdmissionEntry, deviceTime()))
	if err != nil {
		t.Fatalf("entry retry on broken chain: %v, want its original result as a replay", err)
	}
	if !replay.Replayed || !replay.Accepted || !replay.OccurredAt.Equal(first.OccurredAt) {
		t.Fatalf("replay = %+v, want the original accepted result", replay)
	}
}

// G5: broken-chain pass reconciliation still re-evaluates the derived
// projection — the union includes quarantine facts, and a conflict on a
// never-live-scanned ticket must not stay silent.
func TestPassReconcileBrokenChainStillEvaluatesConflicts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueAndSeed(t, ctx, st, ReEntryPolicy{Mode: "multi", RequiresExit: true})
	corruptChain(t, ctx, db, s.ticketID)
	occA, occB := uuid.New(), uuid.New()

	for i, occ := range []uuid.UUID{occA, occB} {
		in := s.reconcileInput(occ, deviceTime().Add(time.Duration(i*10)*time.Minute))
		in.Type = AdmissionEntry
		result, err := st.ReconcileAdmission(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if result.Outcome != ReconcileRecorded {
			t.Fatalf("occurrence %d = %+v, want recorded on the quarantine side", i, result)
		}
	}
	states := conflictStates(t, ctx, db, s.ticketID)
	if states[PolicyConflict{Rule: ConflictExitRequired, OccurrenceID: occB}] != "raised" {
		t.Fatalf("conflict states = %+v, want exit_required raised from quarantine-side facts", states)
	}
}

// Second-pass S5: a duplicate_admit row is a recorded physical admission —
// once the policy is a pass, it must consume allowance like any other
// un-directioned admission, or the facts basis drifts from the replay path.
func TestScanCountsDuplicateAdmitTowardAllowance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueTicket(t, ctx, st, uuid.New())

	// Single-vocabulary history: a redemption and a reconciled offline
	// double-admit.
	if _, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionEntry, deviceTime())); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileAdmission(ctx, s.reconcileInput(uuid.New(), deviceTime().Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if got := countEvents(t, ctx, db, s.ticketID, "duplicate_admit"); got != 1 {
		t.Fatalf("duplicate_admit events = %d, want 1", got)
	}
	seedPolicy(t, ctx, st, s, ReEntryPolicy{Mode: "count_limited", MaxEntries: i32(2)})

	result, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionEntry, deviceTime().Add(2*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.Decision != DecisionEntryLimitReached {
		t.Fatalf("entry after redeemed+duplicate_admit = %+v, want entry_limit_reached — both physical admissions count", result)
	}
}

// C (TKT-269) — branch (c): a REFUNDED pass ticket. Pass reconciliation records
// factual entry/exit only and never mints conflict events (ADR-025 §D2), and
// that holds whether or not the ticket is refunded.
//
// The refund seed is load-bearing, not decoration — the question the plan
// critique forced (A4). Without it these assertions are satisfied by any pass
// reconcile and the test duplicates TestPassReconcileRecordsFactualEntryExitOnly.
// With it, the test is red against a refunded guard placed above the pass split,
// which is the mutation it exists to catch: policy conflicts on a pass are
// DERIVED and revisable projections, so a refunded pass admission must not mint
// an immutable conflict alarm that no later event could retract.
func TestPassReconcileRefundedTicketRecordsFactWithoutConflictAlarm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	s := issueAndSeed(t, ctx, st, ReEntryPolicy{Mode: "multi"})
	if _, err := st.RefundOrderTickets(ctx, s.id.OrganizerID, s.id.OrderID, uuid.New(), 1); err != nil {
		t.Fatal(err)
	}

	in := s.reconcileInput(uuid.New(), deviceTime())
	in.Type = AdmissionEntry
	result, err := st.ReconcileAdmission(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ReconcileRecorded {
		t.Fatalf("refunded pass reconcile outcome = %s, want recorded — recording is not deciding (ADR-025 §D2)", result.Outcome)
	}
	if got := countEvents(t, ctx, db, s.ticketID, "entry"); got != 1 {
		t.Fatalf("entry events = %d, want 1 — the factual admission is recorded", got)
	}
	for _, banned := range []string{"redeemed", "duplicate_admit"} {
		if got := countEvents(t, ctx, db, s.ticketID, banned); got != 0 {
			t.Fatalf("%s events = %d, want 0 — pass reconciliation never mints them (§D2)", banned, got)
		}
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_integrity_alarm_outbox WHERE subject=$1`, SubjectAdmissionConflictAlarm); n != 0 {
		t.Fatalf("%d admission-conflict alarms on a refunded PASS admission, want 0 — pass conflicts are derived and revisable (§D2); an alarm minted above the pass split would be neither", n)
	}
}
