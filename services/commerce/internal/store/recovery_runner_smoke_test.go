//go:build smoke

package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/recovery"
	"ticketing/services/commerce/internal/store"
)

const terminalRefundReason = "provider_refund_failed: provider refused the refund (re_connected); manual reconciliation required"

type connectedPayments struct {
	t           *testing.T
	organizerID uuid.UUID
	key         string
	refundCalls int
}

func (p *connectedPayments) LookupOperation(_ context.Context, organizerID uuid.UUID, key string) (recovery.Operation, bool, error) {
	if organizerID != p.organizerID || key != p.key {
		p.t.Errorf("unexpected LookupOperation organizer=%s key=%q", organizerID, key)
		return recovery.Operation{}, false, recovery.ErrProviderUnresolved
	}
	return recovery.Operation{Resolved: true, Status: "captured"}, true, nil
}

func (p *connectedPayments) Status(_ context.Context, organizerID uuid.UUID, key string) (recovery.PSPStatus, error) {
	if organizerID != p.organizerID || key != p.key {
		p.t.Errorf("unexpected Status organizer=%s key=%q", organizerID, key)
		return recovery.PSPStatus{}, recovery.ErrProviderUnresolved
	}
	return recovery.PSPStatus{
		Outcome: "captured", Captured: true, Authorized: true,
		AuthorizedAmount: 1250, CapturedAmount: 1250, Currency: "EUR",
	}, nil
}

func (p *connectedPayments) Void(_ context.Context, organizerID uuid.UUID, key string) (recovery.CompensationResult, error) {
	p.t.Errorf("unexpected Void organizer=%s key=%q", organizerID, key)
	return recovery.CompensationResult{}, errors.New("unexpected void for seeded order")
}

func (p *connectedPayments) Refund(_ context.Context, organizerID uuid.UUID, key string) (recovery.CompensationResult, error) {
	if organizerID != p.organizerID || key != p.key {
		p.t.Errorf("unexpected Refund organizer=%s key=%q", organizerID, key)
		return recovery.CompensationResult{}, recovery.ErrProviderUnresolved
	}
	p.refundCalls++
	return recovery.CompensationResult{}, &recovery.ProviderRefundFailedError{ProviderRef: "re_connected"}
}

type connectedInventory struct {
	t *testing.T
}

func (i *connectedInventory) Confirm(_ context.Context, reservationID, holdID uuid.UUID) error {
	i.t.Errorf("unexpected inventory Confirm reservation=%s hold=%s", reservationID, holdID)
	return errors.New("unexpected inventory confirm")
}

func (i *connectedInventory) Release(_ context.Context, reservationID, holdID uuid.UUID) error {
	i.t.Errorf("unexpected inventory Release reservation=%s hold=%s", reservationID, holdID)
	return errors.New("unexpected inventory release")
}

type connectedJournal struct {
	t *testing.T
}

func (j *connectedJournal) OrderCreated(_ context.Context, s store.StuckOrder) error {
	j.t.Errorf("unexpected journal OrderCreated for order=%s", s.OrderID)
	return nil
}

func (j *connectedJournal) OrderCompleted(_ context.Context, s store.StuckOrder) error {
	j.t.Errorf("unexpected journal OrderCompleted for order=%s", s.OrderID)
	return nil
}

func (j *connectedJournal) OrderFailed(_ context.Context, s store.StuckOrder) error {
	j.t.Errorf("unexpected journal OrderFailed for order=%s", s.OrderID)
	return errors.New("unexpected journal call")
}

type connectedCompleter struct {
	t *testing.T
}

func (c *connectedCompleter) Complete(_ context.Context, s store.StuckOrder) error {
	c.t.Errorf("unexpected completer call for order=%s", s.OrderID)
	return errors.New("unexpected completer call")
}

// isolatedStore keeps the runner on this test's order. The claim is still the real SQL
// claim; foreign rows it picks up are handed back at once, uncharged. The brief lease this
// puts on those rows is safe only because nothing else uses this database while the test
// runs: the store package has no parallel tests, and TestCommerceSmokeDatabasesAreIsolated
// keeps every other package off commerce_store_smoke. Adding t.Parallel here breaks that.
type isolatedStore struct {
	recovery.DBStore
	t       *testing.T
	orderID uuid.UUID
}

func (s isolatedStore) ClaimStuckOrders(ctx context.Context, _ int, lease time.Duration) ([]store.StuckOrder, error) {
	rows, err := s.DBStore.ClaimStuckOrders(ctx, 10000, lease)
	if err != nil {
		return nil, err
	}
	var isolated []store.StuckOrder
	for _, row := range rows {
		if row.OrderID == s.orderID {
			isolated = append(isolated, row)
			continue
		}
		if err := s.AbandonRecoveryClaim(ctx, row.OrderID, row.ClaimID); err != nil {
			s.t.Errorf("abandon unrelated recovery claim for order %s: %v", row.OrderID, err)
			return nil, err
		}
	}
	return isolated, nil
}

// unreachablePayments fails the operation lookup, so the drive reaches the release without a PSP
// answer. Every other PSP call is a test error: no status, void or refund is read for an order
// whose evidence never arrived.
type unreachablePayments struct {
	t *testing.T
}

func (p *unreachablePayments) LookupOperation(context.Context, uuid.UUID, string) (recovery.Operation, bool, error) {
	return recovery.Operation{}, false, errors.New("psp unreachable")
}

func (p *unreachablePayments) Status(context.Context, uuid.UUID, string) (recovery.PSPStatus, error) {
	p.t.Errorf("unexpected PSP status call: the lookup failed, so no status is read")
	return recovery.PSPStatus{}, errors.New("unexpected status call")
}

func (p *unreachablePayments) Void(context.Context, uuid.UUID, string) (recovery.CompensationResult, error) {
	p.t.Errorf("unexpected PSP void call: the lookup failed, so no compensation is driven")
	return recovery.CompensationResult{}, errors.New("unexpected void call")
}

func (p *unreachablePayments) Refund(context.Context, uuid.UUID, string) (recovery.CompensationResult, error) {
	p.t.Errorf("unexpected PSP refund call: the lookup failed, so no compensation is driven")
	return recovery.CompensationResult{}, errors.New("unexpected refund call")
}

// supersededStore hands the runner the claim the real claim SQL took, then writes a live
// successor claim onto the same row before the drive starts. The release therefore carries a
// token the row no longer holds, which is the state a lease lapsing mid-drive leaves behind.
// The row is read back right after that write, so the test can show later that nothing else
// wrote to it.
type supersededStore struct {
	isolatedStore
	claimA    uuid.UUID // the claim the runner was given
	successor uuid.UUID // the live claim written over it
	atHandoff string    // the row immediately after the successor claim was written
}

func (s *supersededStore) ClaimStuckOrders(ctx context.Context, limit int, lease time.Duration) ([]store.StuckOrder, error) {
	claimed, err := s.isolatedStore.ClaimStuckOrders(ctx, limit, lease)
	if err != nil || len(claimed) != 1 {
		return claimed, err
	}
	s.claimA = claimed[0].ClaimID
	s.successor = uuid.New()
	if _, err := s.DB.ExecContext(ctx, `
		UPDATE orders SET recovery_claim_id=$2, recovery_lease_until=now()+interval '5 minutes'
		WHERE id=$1 AND recovery_claim_id=$3`, s.orderID, s.successor, s.claimA); err != nil {
		return nil, err
	}
	s.atHandoff = rowState(s.t, s.DB, ctx, s.orderID)
	return claimed, nil
}

// rowState reads an order's recovery columns as one text value, so two reads compare exactly,
// microseconds included.
func rowState(t *testing.T, db *sql.DB, ctx context.Context, orderID uuid.UUID) string {
	t.Helper()
	var state string
	if err := db.QueryRowContext(ctx, `
		SELECT concat_ws('|',
		       coalesce(recovery_claim_id::text,'-'), coalesce(recovery_lease_until::text,'-'),
		       recovery_attempts::text, recovery_next_attempt_at::text,
		       coalesce(recovery_parked_at::text,'-'), coalesce(recovery_last_error,'-'),
		       status, updated_at::text)
		FROM orders WHERE id=$1`, orderID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// runnerLogLine is one record of the runner's text log.
type runnerLogLine struct {
	level, msg, raw string
}

var runnerLogPattern = regexp.MustCompile(`level=(\S+) msg="([^"]*)"`)

// runnerLogLines parses a text-handler capture, one record per line. A line that does not parse
// fails the test, so an absence assertion cannot pass by skipping lines.
func runnerLogLines(t *testing.T, out string) []runnerLogLine {
	t.Helper()
	var lines []runnerLogLine
	for _, raw := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if raw == "" {
			continue
		}
		m := runnerLogPattern.FindStringSubmatch(raw)
		if m == nil {
			t.Fatalf("log line does not parse as a text record: %q", raw)
		}
		lines = append(lines, runnerLogLine{level: m[1], msg: m[2], raw: raw})
	}
	return lines
}

// runnerLogged returns the records with this level and message.
func runnerLogged(lines []runnerLogLine, level, msg string) []runnerLogLine {
	var out []runnerLogLine
	for _, l := range lines {
		if l.level == level && l.msg == msg {
			out = append(out, l)
		}
	}
	return out
}

// TKT-322, connected. A release that finds its claim superseded is logged as a supersession and
// nothing more. The successor claim is written by a plain UPDATE in supersededStore, after the
// real claim has run. The release is the real DBStore release, and only the payments port is
// faked, to fail the drive. The precondition is checked before the log is read.
func TestASupersededReleaseLogsOnceAndLeavesTheSuccessorsClaim(t *testing.T) {
	db, ctx := store.OutboxDBForTest(t)
	seeded := store.SeedStuckForTest(t, "created")
	if _, err := db.ExecContext(ctx, `UPDATE orders SET recovery_attempts=$2 WHERE id=$1`,
		seeded.OrderID, store.MaxRecoveryAttempts-1); err != nil {
		t.Fatal(err)
	}
	st := &supersededStore{isolatedStore: isolatedStore{DBStore: recovery.DBStore{DB: db}, t: t, orderID: seeded.OrderID}}
	var logs strings.Builder
	runner, err := recovery.New(st, &unreachablePayments{t: t}, &connectedInventory{t: t}, &connectedJournal{t: t},
		&connectedCompleter{t: t}, time.Minute, 16, time.Minute,
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}

	runner.RunOnce(ctx)

	// The precondition the log assertions depend on: the row holds a live successor claim that is
	// distinct from the claim the runner released with. Without it, a log line proves nothing
	// about the fence.
	if st.successor == uuid.Nil || st.successor == st.claimA {
		t.Fatalf("precondition: successor claim %s must differ from the runner's claim %s", st.successor, st.claimA)
	}
	var claim uuid.NullUUID
	var leaseLive bool
	if err := db.QueryRowContext(ctx, `SELECT recovery_claim_id, recovery_lease_until > now() FROM orders WHERE id=$1`,
		seeded.OrderID).Scan(&claim, &leaseLive); err != nil {
		t.Fatal(err)
	}
	if !claim.Valid || claim.UUID != st.successor || !leaseLive {
		t.Fatalf("precondition: row claim = %v live=%t, want the live successor %s", claim, leaseLive, st.successor)
	}

	lines := runnerLogLines(t, logs.String())
	superseded := runnerLogged(lines, "INFO", "recovery claim superseded")
	if len(lines) != 1 || len(superseded) != 1 || !strings.Contains(superseded[0].raw, seeded.OrderID.String()) {
		t.Fatalf("want exactly one INFO superseded record for order %s; logged %v", seeded.OrderID, lines)
	}
	if got := rowState(t, db, ctx, seeded.OrderID); got != st.atHandoff {
		t.Fatalf("the successor's row changed after the handoff:\n handoff %s\n now     %s", st.atHandoff, got)
	}
}

// TKT-322, connected. The parking the runner logs must be the parking the SQL performed. The order
// starts one failure short of the budget, so the real release reaches it: the row must carry the
// marker at the full attempt count, and the log must say parked and nothing else.
func TestTheRunnerLogsParkingOnlyWhenTheRealReleaseParksTheRow(t *testing.T) {
	db, ctx := store.OutboxDBForTest(t)
	seeded := store.SeedStuckForTest(t, "created")
	if _, err := db.ExecContext(ctx, `UPDATE orders SET recovery_attempts=$2 WHERE id=$1`,
		seeded.OrderID, store.MaxRecoveryAttempts-1); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	runner, err := recovery.New(isolatedStore{DBStore: recovery.DBStore{DB: db}, t: t, orderID: seeded.OrderID},
		&unreachablePayments{t: t}, &connectedInventory{t: t}, &connectedJournal{t: t}, &connectedCompleter{t: t},
		time.Minute, 16, time.Minute, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}

	runner.RunOnce(ctx)

	var parked bool
	var attempts int
	if err := db.QueryRowContext(ctx, `SELECT recovery_parked_at IS NOT NULL, recovery_attempts FROM orders WHERE id=$1`,
		seeded.OrderID).Scan(&parked, &attempts); err != nil {
		t.Fatal(err)
	}
	if !parked || attempts != store.MaxRecoveryAttempts {
		t.Fatalf("row parked=%t attempts=%d, want parked at %d attempts", parked, attempts, store.MaxRecoveryAttempts)
	}
	lines := runnerLogLines(t, logs.String())
	if len(lines) != 1 {
		t.Fatalf("logged %d records, want exactly one parking record: %v", len(lines), lines)
	}
	if got := lines[0]; got.level != "ERROR" || got.msg != "stuck order parked after exhausting recovery attempts" ||
		!strings.Contains(got.raw, seeded.OrderID.String()) {
		t.Fatalf("logged %s %q, want the parked ERROR for order %s", got.level, got.msg, seeded.OrderID)
	}
}

func TestRunnerReparksAnUnparkedTerminalRefundInOnePass(t *testing.T) {
	db, ctx := store.OutboxDBForTest(t)
	seeded := store.SeedStuckForTest(t, "reconciliation_required")
	t.Cleanup(func() {
		if _, err := db.ExecContext(ctx, `DELETE FROM order_recovery_unparks WHERE order_id=$1`, seeded.OrderID); err != nil {
			t.Errorf("delete unpark records: %v", err)
		}
	})
	if err := db.QueryRowContext(ctx, `SELECT idempotency_key FROM orders WHERE id=$1`, seeded.OrderID).Scan(&seeded.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	// The smoke database is shared, so isolate the claim batch before the runner can drive other orders.
	// Abandoning other claims releases their leases without charging attempts; all later store calls use the real DBStore.
	dbStore := recovery.DBStore{DB: db}
	payments := &connectedPayments{t: t, organizerID: seeded.OrganizerID, key: seeded.IdempotencyKey}
	inventory := &connectedInventory{t: t}
	journal := &connectedJournal{t: t}
	completer := &connectedCompleter{t: t}
	runner, err := recovery.New(isolatedStore{DBStore: dbStore, t: t, orderID: seeded.OrderID}, payments, inventory, journal, completer,
		time.Minute, 16, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}

	assertParked := func(pass string) {
		t.Helper()
		var parked bool
		var lastError string
		var attempts int
		var claimID sql.NullString
		var status string
		if err := db.QueryRowContext(ctx, `
			SELECT recovery_parked_at IS NOT NULL, recovery_last_error,
			       recovery_attempts, recovery_claim_id::text, status
			FROM orders WHERE id=$1`, seeded.OrderID).
			Scan(&parked, &lastError, &attempts, &claimID, &status); err != nil {
			t.Fatalf("%s: read order: %v", pass, err)
		}
		if !parked || lastError != terminalRefundReason || attempts != 0 || claimID.Valid || status != "reconciliation_required" {
			t.Fatalf("%s: parked=%t last_error=%q attempts=%d claim_id=%v status=%q",
				pass, parked, lastError, attempts, claimID, status)
		}
	}

	runner.RunOnce(ctx)
	assertParked("first pass")
	if payments.refundCalls != 1 {
		t.Fatalf("refund calls after first pass = %d, want 1", payments.refundCalls)
	}

	if err := store.UnparkOrder(ctx, db, seeded.OrderID, "operator reviewed"); err != nil {
		t.Fatal(err)
	}
	// The earlier park refreshed updated_at. Backdate it past ClaimStuckOrders' two-minute
	// grace period so the next pass tests the re-drive, not the grace window.
	if _, err := db.ExecContext(ctx, `UPDATE orders SET updated_at=now()-interval '10 minutes' WHERE id=$1`, seeded.OrderID); err != nil {
		t.Fatal(err)
	}

	runner.RunOnce(ctx)
	assertParked("second pass")
	if payments.refundCalls != 2 {
		t.Fatalf("refund calls after second pass = %d, want 2", payments.refundCalls)
	}

	// Re-parking also refreshes updated_at, so age the row before checking parked-row exclusion.
	if _, err := db.ExecContext(ctx, `UPDATE orders SET updated_at=now()-interval '10 minutes' WHERE id=$1`, seeded.OrderID); err != nil {
		t.Fatal(err)
	}
	runner.RunOnce(ctx)
	if payments.refundCalls != 2 {
		t.Fatalf("refund calls after third pass = %d, want 2 for a parked order", payments.refundCalls)
	}
	assertParked("third pass")
}

// --- TKT-285: a recovered ZERO-TOTAL order, through the real claim SQL and the real completion
// transaction (D12, D17b).
//
// What the runner-tier tests cannot show is that the zero branch is reachable from what the
// database actually holds. They hand the runner a StuckOrder with Amount 0; this hands it a
// reservation whose total_amount is 0 and lets ClaimStuckOrders read it. Replace the SQL total
// with a positive constant and the order is a PAID order with no operation: the runner would
// release it, and connectedInventory.Release fails this test directly.

// zeroPayments permits EXACTLY ONE operation lookup, answered not-found, and fails the test on
// anything else: a PSP-skipped order has no charge to resolve, so status, void and refund are
// all calls about a charge that was never made. A second lookup is refused too — the runner
// reads the evidence once.
type zeroPayments struct {
	t           *testing.T
	organizerID uuid.UUID
	key         string
	lookups     int
}

func (p *zeroPayments) LookupOperation(_ context.Context, organizerID uuid.UUID, key string) (recovery.Operation, bool, error) {
	p.lookups++
	if organizerID != p.organizerID || key != p.key {
		p.t.Errorf("unexpected LookupOperation organizer=%s key=%q", organizerID, key)
	}
	return recovery.Operation{}, false, nil
}

func (p *zeroPayments) Status(_ context.Context, organizerID uuid.UUID, key string) (recovery.PSPStatus, error) {
	p.t.Errorf("unexpected Status for a PSP-skipped order organizer=%s key=%q", organizerID, key)
	return recovery.PSPStatus{}, errors.New("unexpected status call")
}

func (p *zeroPayments) Void(_ context.Context, organizerID uuid.UUID, key string) (recovery.CompensationResult, error) {
	p.t.Errorf("unexpected Void for a PSP-skipped order organizer=%s key=%q", organizerID, key)
	return recovery.CompensationResult{}, errors.New("unexpected void call")
}

func (p *zeroPayments) Refund(_ context.Context, organizerID uuid.UUID, key string) (recovery.CompensationResult, error) {
	p.t.Errorf("unexpected Refund for a PSP-skipped order organizer=%s key=%q", organizerID, key)
	return recovery.CompensationResult{}, errors.New("unexpected refund call")
}

// confirmingInventory records the two inventory calls a zero order can legitimately make.
type confirmingInventory struct {
	confirms, releases int
}

func (i *confirmingInventory) Confirm(context.Context, uuid.UUID, uuid.UUID) error {
	i.confirms++
	return nil
}

func (i *confirmingInventory) Release(context.Context, uuid.UUID, uuid.UUID) error {
	i.releases++
	return nil
}

// submittedFact is one request payments' /internal/facts received.
type submittedFact struct {
	FactID     uuid.UUID `json:"fact_id"`
	FactType   string    `json:"fact_type"`
	Amount     int64     `json:"amount"`
	Currency   string    `json:"currency"`
	OccurredAt time.Time `json:"occurred_at"`
}

// zeroRecoveryRun seeds a coherent zero-total order in `status`, optionally with its
// order.created fact, and runs the REAL runner (real claim SQL, real completer, real fact
// table and fact submission) against a payments stub. It returns what the stub saw.
func zeroRecoveryRun(t *testing.T, status string, withIntent bool) (seeded store.StuckOrder, payments *zeroPayments, inventory *confirmingInventory, facts []submittedFact, db *sql.DB, ctx context.Context) {
	t.Helper()
	db, ctx = store.OutboxDBForTest(t)
	seeded = store.SeedZeroStuckForTest(t, status)
	if err := db.QueryRowContext(ctx, `SELECT idempotency_key FROM orders WHERE id=$1`, seeded.OrderID).Scan(&seeded.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if withIntent {
		// Written through the same store function checkout's twin uses; the amount comes from
		// the order read back through the claim, not from the fixture.
		claimed := claimedZeroForRunner(t, db, ctx, seeded)
		if _, _, err := store.RecordOrderFact(ctx, db, claimed, "order.created"); err != nil {
			t.Fatal(err)
		}
		// The probe claim above leased the row; hand it back so the runner can claim it.
		if err := store.AbandonRecoveryClaim(ctx, db, claimed.OrderID, claimed.ClaimID); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	payments = &zeroPayments{t: t, organizerID: seeded.OrganizerID, key: seeded.IdempotencyKey}
	inventory = &confirmingInventory{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var f submittedFact
		if r.URL.Path != "/internal/facts" || json.NewDecoder(r.Body).Decode(&f) != nil {
			t.Errorf("unexpected payments request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		facts = append(facts, f)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	journal := recovery.JournalFact{Client: server.Client(), PaymentsURL: server.URL, Token: "t", DB: recovery.StoreFactDB{DB: db}}
	runner, err := recovery.New(isolatedStore{DBStore: recovery.DBStore{DB: db}, t: t, orderID: seeded.OrderID},
		payments, inventory, journal, recovery.StoreCompleter{DB: db}, time.Minute, 16, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner.RunOnce(ctx)
	mu.Lock()
	defer mu.Unlock()
	return seeded, payments, inventory, append([]submittedFact(nil), facts...), db, ctx
}

// claimedZeroForRunner reads the order through the real claim SQL (see claimedZero in the
// store package's own tests; this package cannot reach its unexported helpers).
func claimedZeroForRunner(t *testing.T, db *sql.DB, ctx context.Context, s store.StuckOrder) store.StuckOrder {
	t.Helper()
	claimed, err := store.ClaimStuckOrders(ctx, db, 10000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var mine store.StuckOrder
	for _, c := range claimed {
		if c.OrderID == s.OrderID {
			mine = c
			continue
		}
		if err := store.AbandonRecoveryClaim(ctx, db, c.OrderID, c.ClaimID); err != nil {
			t.Fatal(err)
		}
	}
	if mine.OrderID == uuid.Nil {
		t.Fatalf("seeded order %s was not claimable", s.OrderID)
	}
	return mine
}

func TestRunnerCompletesAZeroTotalOrderWithOnlyALookupAndTheJournalFacts(t *testing.T) {
	for _, status := range []string{"created", "confirmation_pending"} {
		t.Run(status, func(t *testing.T) {
			seeded, payments, inventory, facts, db, ctx := zeroRecoveryRun(t, status, status == "created")

			var gotStatus string
			var guestRef sql.NullString
			var leaseClaim sql.NullString
			if err := db.QueryRowContext(ctx, `SELECT status, guest_order_ref::text, recovery_claim_id::text FROM orders WHERE id=$1`,
				seeded.OrderID).Scan(&gotStatus, &guestRef, &leaseClaim); err != nil {
				t.Fatal(err)
			}
			if gotStatus != "completed" || !guestRef.Valid || leaseClaim.Valid {
				t.Fatalf("order status=%q guest_ref=%v claim=%v, want completed, a guest reference and a cleared claim", gotStatus, guestRef, leaseClaim)
			}
			var owed int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM completion_outbox WHERE order_id=$1`, seeded.OrderID).Scan(&owed); err != nil {
				t.Fatal(err)
			}
			if owed != 1 {
				t.Fatalf("completion outbox rows = %d, want 1: issuance depends on the event the completion owes", owed)
			}
			// Exactly ONE lookup, answered not-found, for both statuses: a zero order is classified by
			// operation evidence whatever its status (D21).
			if payments.lookups != 1 {
				t.Errorf("payments lookups = %d, want 1", payments.lookups)
			}
			if inventory.confirms != 1 || inventory.releases != 0 {
				t.Errorf("inventory confirm/release = %d/%d, want 1/0", inventory.confirms, inventory.releases)
			}
			// What payments received is what commerce stored: identity, amount, currency and
			// the first-written timestamp, for exactly the two order facts.
			if len(facts) != 2 || facts[0].FactType != "order.created" || facts[1].FactType != "order.completed" {
				t.Fatalf("payments received facts %+v, want exactly [order.created order.completed]", facts)
			}
			for _, f := range facts {
				var amount int64
				var currency string
				var stored time.Time
				if err := db.QueryRowContext(ctx, `SELECT amount, currency, occurred_at FROM order_facts WHERE fact_id=$1 AND order_id=$2`,
					f.FactID, seeded.OrderID).Scan(&amount, &currency, &stored); err != nil {
					t.Fatalf("%s was submitted but is not in order_facts: %v", f.FactType, err)
				}
				if amount != 0 || currency != "EUR" || f.Amount != 0 || f.Currency != "EUR" || !f.OccurredAt.Equal(stored) {
					t.Errorf("%s: stored %d %s at %s, submitted %d %s at %s; want 0 EUR and equal timestamps",
						f.FactType, amount, currency, stored, f.Amount, f.Currency, f.OccurredAt)
				}
			}
		})
	}
}

// A zero-total `created` order whose checkout never wrote its intent fact is released as
// `not_attempted`, exactly as a paid order with no operation is — through the real SQL.
func TestRunnerReleasesAZeroTotalOrderThatNeverRecordedItsIntent(t *testing.T) {
	seeded, payments, inventory, facts, db, ctx := zeroRecoveryRun(t, "created", false)

	var status, outcome, reservationStatus string
	var claim, lease sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT o.status, coalesce(o.terminal_outcome,''), r.status, o.recovery_claim_id::text, o.recovery_lease_until::text
		FROM orders o JOIN reservations r ON r.id=o.reservation_id WHERE o.id=$1`, seeded.OrderID).
		Scan(&status, &outcome, &reservationStatus, &claim, &lease); err != nil {
		t.Fatal(err)
	}
	// Exactly the terminal state a paid order with no operation reaches: `not_attempted`
	// surfaces as `timeout`, the reservation is failed, and the recovery claim is released.
	if status != "timeout" || outcome != "not_attempted" || reservationStatus != "failed" || claim.Valid || lease.Valid {
		t.Fatalf("order status=%q outcome=%q reservation=%q claim=%v lease=%v, want timeout / not_attempted / failed with the claim and lease cleared",
			status, outcome, reservationStatus, claim, lease)
	}
	if payments.lookups != 1 || inventory.releases != 1 || inventory.confirms != 0 {
		t.Errorf("lookups=%d releases=%d confirms=%d, want 1/1/0", payments.lookups, inventory.releases, inventory.confirms)
	}
	if len(facts) != 1 || facts[0].FactType != "order.failed" {
		t.Fatalf("payments received %+v, want exactly one order.failed and nothing else", facts)
	}
}
