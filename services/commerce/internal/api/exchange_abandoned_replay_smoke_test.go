//go:build smoke

package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	commercestore "ticketing/services/commerce/internal/store"
)

// TKT-498 (D24). A replay of the original exchange request after the exchange's abandonment.
//
// A terminal abandonment answers HTTP 200 with status `abandoned`: the exchange will never
// switch, so `switch_pending` would report work that is no longer owed. The original exchange
// and money fields stay as history, and the replay does not repair the switch outbox. A started
// abandonment that is not terminal keeps today's answer, `switch_pending`, and its repair. The
// exchange is a downgrade, so the replayed money fields are not zero.
func TestAnExchangeReplayAfterTheAbandonmentReportsItsState(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "abandoned-replay", 2, 2500)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(2000)
	s := exchangeStackFor(t, db, f, policy)
	id := settleAnExchange(t, s, f, "abandoned-replay")

	e, err := commercestore.LoadSettledExchange(ctx, db, f.organizer, id)
	if err != nil {
		t.Fatal(err)
	}
	if e.DeltaAmount != -1000 {
		t.Fatalf("settled delta = %d, want the downgrade's -1000", e.DeltaAmount)
	}
	attribution, err := commercestore.NewAbandonmentAttribution("Ada Operator", "source admitted before the switch")
	if err != nil {
		t.Fatal(err)
	}
	if _, reserved, err := commercestore.ReserveExchangeAbandonment(ctx, db, commercestore.AbandonmentReservation{
		Expected: e, Attribution: attribution}); err != nil || !reserved {
		t.Fatalf("reserve: reserved=%v err=%v", reserved, err)
	}
	outbox := func() int {
		t.Helper()
		return countCommerceRows(t, ctx, db, `SELECT count(*) FROM completion_outbox WHERE order_id=$1`, e.ReplacementOrderID)
	}
	dropOutbox := func() {
		t.Helper()
		if _, err := db.ExecContext(ctx, `DELETE FROM completion_outbox WHERE order_id=$1`, e.ReplacementOrderID); err != nil {
			t.Fatal(err)
		}
	}

	// Started, not terminal: today's answer, and the replay still repairs the switch obligation.
	dropOutbox()
	code, body := s.exchange(t, f, "abandoned-replay")
	if code != http.StatusOK || body["status"] != "switch_pending" || body["replay"] != true {
		t.Fatalf("replay of a started abandonment = %d %v, want 200 switch_pending as a replay", code, body)
	}
	if n := outbox(); n != 1 {
		t.Fatalf("a started abandonment's replay left %d switch outbox row(s), want the repaired 1", n)
	}

	if _, err := commercestore.RecordExchangeAbandonmentTargetReturned(ctx, db, f.organizer, id); err != nil {
		t.Fatal(err)
	}
	cur, err := commercestore.LoadSettledExchange(ctx, db, f.organizer, id)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := commercestore.BuildExchangeAbandonedFact(cur)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := commercestore.FinishExchangeAbandonment(ctx, db, fact); err != nil {
		t.Fatal(err)
	}

	// Terminal: `abandoned`, the original fields as history, and no switch repair.
	dropOutbox()
	code, body = s.exchange(t, f, "abandoned-replay")
	if code != http.StatusOK || body["status"] != "abandoned" || body["replay"] != true {
		t.Fatalf("replay of a terminal abandonment = %d %v, want 200 abandoned as a replay", code, body)
	}
	if body["exchange_id"] != id.String() || body["replacement_order_id"] != e.ReplacementOrderID.String() ||
		body["source_order_id"] != f.order.String() || body["delta_amount"] != float64(e.DeltaAmount) ||
		body["source_total"] != float64(e.SourceTotal) || body["target_total"] != float64(e.TargetTotal) ||
		body["tickets_exchanged"] != false || body["capacity_returned"] != false {
		t.Fatalf("terminal replay = %v, want the original exchange and money fields", body)
	}
	if n := outbox(); n != 0 {
		t.Fatalf("a terminal abandonment's replay repaired %d switch outbox row(s), want 0", n)
	}
}

// RA2-2. The terminal commit wins the exchange row lock after the replay's first read. The
// replay read a started, unfinished abandonment, so its snapshot says `switch_pending`; under
// the settlement lock it must see abandoned_at, answer `abandoned` and write no outbox row.
//
// The schedule is fixed by PostgreSQL's FIFO tuple-lock queue: a holder transaction locks the
// row; FinishExchangeAbandonment queues first; the replay reads (no lock) and queues second;
// the holder ends; the terminal transaction commits before the replay's settlement lock.
func TestAReplayThatLosesTheLockToTheTerminalCommitAnswersAbandoned(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "abandon-race", 2, 2500)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(2500)
	s := exchangeStackFor(t, db, f, policy)
	id := settleAnExchange(t, s, f, "abandon-race")
	e, err := commercestore.LoadSettledExchange(ctx, db, f.organizer, id)
	if err != nil {
		t.Fatal(err)
	}
	attribution, err := commercestore.NewAbandonmentAttribution("Ada Operator", "source admitted before the switch")
	if err != nil {
		t.Fatal(err)
	}
	if _, reserved, err := commercestore.ReserveExchangeAbandonment(ctx, db, commercestore.AbandonmentReservation{
		Expected: e, Attribution: attribution}); err != nil || !reserved {
		t.Fatalf("reserve: reserved=%v err=%v", reserved, err)
	}
	if _, err := commercestore.RecordExchangeAbandonmentTargetReturned(ctx, db, f.organizer, id); err != nil {
		t.Fatal(err)
	}
	cur, err := commercestore.LoadSettledExchange(ctx, db, f.organizer, id)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := commercestore.BuildExchangeAbandonedFact(cur)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM completion_outbox WHERE order_id=$1`, e.ReplacementOrderID); err != nil {
		t.Fatal(err)
	}

	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `SELECT 1 FROM order_exchanges WHERE organizer_id=$1 AND id=$2 FOR UPDATE`, f.organizer, id); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, _, err := commercestore.FinishExchangeAbandonment(ctx, db, fact)
		finished <- err
	}()
	waitForExchangeLockWaiters(t, ctx, db, 1)
	type answer struct {
		code int
		body map[string]any
	}
	replayed := make(chan answer, 1)
	go func() {
		code, body := s.exchange(t, f, "abandon-race")
		replayed <- answer{code, body}
	}()
	waitForExchangeLockWaiters(t, ctx, db, 2)
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatalf("finish the abandonment: %v", err)
	}
	got := <-replayed
	if got.code != http.StatusOK || got.body["status"] != "abandoned" || got.body["replay"] != true {
		t.Fatalf("replay that lost the lock to the terminal commit = %d %v, want 200 abandoned as a replay", got.code, got.body)
	}
	if n := countCommerceRows(t, ctx, db, `SELECT count(*) FROM completion_outbox WHERE order_id=$1`, e.ReplacementOrderID); n != 0 {
		t.Fatalf("the replay wrote %d switch outbox row(s) after the terminal commit, want 0", n)
	}
}

// R3-5, RA3-3. A replay whose first read sees an UNSETTLED basis answers `abandoned` when the
// abandonment wins, on both settlement entry paths. The delta moved and the target did not
// confirm (202), so the replay resumes from the basis. It stops at one statement while another
// request settles the exchange and the abandonment finishes, then it continues:
//
//   - at the basis bind, which reloads a settled and abandoned row: the replay answers from
//     Commerce and calls no dependency;
//   - at the final settlement's locked read, after its forward calls: the read finds the
//     terminal row.
//
// Either way the answer is 200 `abandoned` as a replay, no dependency call follows the terminal
// state, and no switch outbox row is written.
func TestAReplayFromAnUnsettledBasisAnswersAbandonedWhenTheAbandonmentWins(t *testing.T) {
	for _, tc := range []struct{ name, key, statement string }{
		{"paused at the basis bind", "abandon-basis-bind", "r.id, r.buyer_id, r.hold_id, r.slot_id"},
		{"paused at the final settlement", "abandon-basis-settle", "SELECT settled_at, replacement_order_id, abandoned_at FROM order_exchanges"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := exchangeAPIDB(t)
			f := seedExchangeSource(t, db, ctx, tc.key, 2, 2500)
			policy := &stubPolicy{}
			policy.catalogUnit.Store(2000)
			policy.confirmFails.Store(true)
			s := exchangeStackFor(t, db, f, policy)
			if code, body := s.exchange(t, f, tc.key); code != http.StatusAccepted || body["status"] != "confirmation_pending" {
				t.Fatalf("first exchange = %d %v, want 202 confirmation_pending: the basis must be unsettled", code, body)
			}
			policy.confirmFails.Store(false)
			id := commercestore.ExchangeID(f.organizer, tc.key)

			gated, arrived, release := pausingDB(t, tc.statement)
			s.server.db = gated
			type answer struct {
				code int
				body map[string]any
			}
			replayed := make(chan answer, 1)
			go func() {
				code, body := s.exchange(t, f, tc.key)
				replayed <- answer{code, body}
			}()
			select {
			case <-arrived:
			case <-time.After(10 * time.Second):
				t.Fatal("the replay never reached the paused statement")
			}
			// Another request settles the exchange, and the abandonment finishes.
			if code, body := s.exchange(t, f, tc.key); code != http.StatusOK || body["status"] != "switch_pending" {
				t.Fatalf("the settling request = %d %v, want 200 switch_pending", code, body)
			}
			replacement := finishAbandonment(t, ctx, db, f.organizer, id)
			if _, err := db.ExecContext(ctx, `DELETE FROM completion_outbox WHERE order_id=$1`, replacement); err != nil {
				t.Fatal(err)
			}
			before := s.dependencyCalls()
			release()

			var got answer
			select {
			case got = <-replayed:
			case <-time.After(10 * time.Second):
				t.Fatal("the paused replay never answered")
			}
			if got.code != http.StatusOK || got.body["status"] != "abandoned" || got.body["replay"] != true {
				t.Fatalf("the paused replay = %d %v, want 200 abandoned as a replay", got.code, got.body)
			}
			if after := s.dependencyCalls(); after != before {
				t.Fatalf("the replay made %d dependency call(s) after the terminal state", after-before)
			}
			if n := countCommerceRows(t, ctx, db, `SELECT count(*) FROM completion_outbox WHERE order_id=$1`, replacement); n != 0 {
				t.Fatalf("the replay wrote %d switch outbox row(s) after the terminal state, want 0", n)
			}
		})
	}
}

// finishAbandonment runs the abandonment's Commerce steps on a settled exchange and returns its
// replacement order.
func finishAbandonment(t *testing.T, ctx context.Context, db *sql.DB, org, id uuid.UUID) uuid.UUID {
	t.Helper()
	e, err := commercestore.LoadSettledExchange(ctx, db, org, id)
	if err != nil {
		t.Fatal(err)
	}
	attribution, err := commercestore.NewAbandonmentAttribution("Ada Operator", "source admitted before the switch")
	if err != nil {
		t.Fatal(err)
	}
	if _, reserved, err := commercestore.ReserveExchangeAbandonment(ctx, db, commercestore.AbandonmentReservation{
		Expected: e, Attribution: attribution}); err != nil || !reserved {
		t.Fatalf("reserve: reserved=%v err=%v", reserved, err)
	}
	if _, err := commercestore.RecordExchangeAbandonmentTargetReturned(ctx, db, org, id); err != nil {
		t.Fatal(err)
	}
	cur, err := commercestore.LoadSettledExchange(ctx, db, org, id)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := commercestore.BuildExchangeAbandonedFact(cur)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := commercestore.FinishExchangeAbandonment(ctx, db, fact); err != nil {
		t.Fatal(err)
	}
	return e.ReplacementOrderID
}

// dependencyCalls is every call the exchange handler made to its stubs and its money-evidence
// reader so far.
func (s *exchangeStack) dependencyCalls() int {
	n := s.moneyEvidence.calls
	for _, c := range []*countingStub{s.catalog, s.inventory, s.payments, s.access} {
		c.mu.Lock()
		for _, v := range c.counts {
			n += v
		}
		c.mu.Unlock()
	}
	return n
}

// pausingDB opens a second pool on this package's smoke database. The first statement that
// contains marker waits, before it runs, until release is called; arrived closes when it is
// reached. Every other statement runs at once. A test uses it to stop one request at a chosen
// statement while other requests run on the same server.
func pausingDB(t *testing.T, marker string) (db *sql.DB, arrived <-chan struct{}, release func()) {
	t.Helper()
	cfg, err := pgx.ParseConfig(os.Getenv("COMMERCE_API_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	g := &statementGate{marker: marker, arrived: make(chan struct{}), release: make(chan struct{})}
	db = sql.OpenDB(gatedConnector{Connector: stdlib.GetConnector(*cfg), gate: g})
	t.Cleanup(func() { _ = db.Close() })
	var once sync.Once
	release = func() { once.Do(func() { close(g.release) }) }
	t.Cleanup(release)
	return db, g.arrived, release
}

type statementGate struct {
	marker           string
	taken            atomic.Bool
	arrived, release chan struct{}
}

func (g *statementGate) wait(query string) {
	if strings.Contains(query, g.marker) && g.taken.CompareAndSwap(false, true) {
		close(g.arrived)
		<-g.release
	}
}

type gatedConnector struct {
	driver.Connector
	gate *statementGate
}

func (c gatedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return gatedConn{Conn: conn.(*stdlib.Conn), gate: c.gate}, nil
}

type gatedConn struct {
	*stdlib.Conn
	gate *statementGate
}

func (c gatedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.gate.wait(query)
	return c.Conn.QueryContext(ctx, query, args)
}

func (c gatedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.gate.wait(query)
	return c.Conn.ExecContext(ctx, query, args)
}

// waitForExchangeLockWaiters waits until n sessions of this database wait on a lock.
func waitForExchangeLockWaiters(t *testing.T, ctx context.Context, db *sql.DB, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d session(s) wait on a lock, want %d", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
