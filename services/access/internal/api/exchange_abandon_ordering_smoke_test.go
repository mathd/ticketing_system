//go:build smoke

package api

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/access/internal/store"
	"ticketing/services/access/internal/ticket"
)

// TKT-498 (plan v2 Build order 3, D8). The abandonment's two ordered Access reads, against the
// REAL Access store, switch, reconciliation and admission handler.
//
// The proof is in the order: read the source's admission first, then the replacement's
// issuance. These two schedules tell a correct implementation from the two ways to get it wrong:
//
//   - Skipped replacement read. The switch commits (replacement tickets issued), its Commerce
//     callback is held, and a late offline admission then lands on a source ticket. The source
//     reads admitted and fully issued, so only the replacement read can refuse.
//   - Swapped order. The first Access answer is held after its snapshot, while the source is
//     unadmitted and the replacement has no ticket. The switch and the late admission commit,
//     then the answer is released. Source-first captured `admitted=false` and refuses. A
//     replacement-first command captured zero tickets and then reads the source admitted, and
//     would go on to reserve.
//
// The command under test is the real `commerce abandon-exchange` binary, built from this
// checkout and run against its own migrated Commerce database. Commerce's internal packages
// cannot be imported from this module, so the binary is the seam. Payments and inventory are
// counting stubs: a correct command must not reach them in either schedule.

const abandonAccessToken = "access-internal-token-for-abandonment-tests"

// pausingAccess serves the real router. When armed, it holds the FIRST answer after the
// handler has taken its snapshot, until released.
type pausingAccess struct {
	router  http.Handler
	mu      sync.Mutex
	reads   []string
	arm     bool
	paused  chan struct{}
	release chan struct{}
}

func (p *pausingAccess) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := httptest.NewRecorder()
	p.router.ServeHTTP(rec, r)
	p.mu.Lock()
	p.reads = append(p.reads, r.URL.Path)
	hold := p.arm
	p.arm = false
	p.mu.Unlock()
	if hold {
		close(p.paused)
		<-p.release
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

func (p *pausingAccess) readPaths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.reads...)
}

// countingDependency stands for payments and inventory. Payments answers the upgrade's capture
// evidence and a 404 for every leg, so a command that passed Access would reserve and go on to
// the refund; every write answers 503.
type countingDependency struct {
	mu    sync.Mutex
	calls []string
	delta int64
}

func (c *countingDependency) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.calls = append(c.calls, r.Method+" "+r.URL.Path)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/internal/operations"):
		_, _ = fmt.Fprintf(w, `{"resolved":true,"occurred_at":"2026-11-21T10:00:00Z","status":"succeeded","fact_id":"%s","captured_amount":%d,"currency":"EUR"}`,
			uuid.New(), c.delta)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/internal/refund-legs"):
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"refund leg not found"}`))
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"stub"}`))
	}
}

func (c *countingDependency) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// buildCommerce builds the commerce binary once, from the Commerce module directory, into the
// parent test's temporary directory, which the testing package removes after the test. The
// build has its own time bound.
func buildCommerce(t *testing.T) string {
	t.Helper()
	module, err := filepath.Abs(filepath.Join("..", "..", "..", "commerce"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "commerce")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/commerce")
	cmd.Dir = module
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build commerce: %v\n%s", err, out)
	}
	return bin
}

// commerceDatabase creates and migrates a Commerce database of its own, through the commerce
// binary's `migrate` command.
func commerceDatabase(t *testing.T, ctx context.Context, bin string) (*sql.DB, string) {
	t.Helper()
	admin, err := sql.Open("pgx", os.Getenv("ACCESS_MIGRATION_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := "access_abandon_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("ACCESS_MIGRATION_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn := u.String()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	cmd := exec.CommandContext(ctx, bin, "migrate")
	cmd.Env = append(os.Environ(), "DATABASE_URL="+dsn)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commerce migrate: %v\n%s", err, out)
	}
	return db, dsn
}

// abandonCase is one settled upgrade, seen from both services.
type abandonCase struct {
	org, exchange, source, replacement, slot, targetSlot uuid.UUID
	sourceTickets                                        []uuid.UUID
	delta                                                int64
}

// seedCommerceUpgrade writes the settled, unswitched upgrade into the Commerce database, with
// the source and replacement order ids Access knows, and its two gross facts.
func seedCommerceUpgrade(t *testing.T, ctx context.Context, db *sql.DB, c abandonCase) {
	t.Helper()
	const q, sourceUnit, targetUnit, fee = 2, int64(1500), int64(2000), int64(200)
	sourceTotal, targetTotal := sourceUnit*q, targetUnit*q
	buyer, sourceRes, replacementRes, targetHold, targetType := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	settled := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'EUR','completed')`,
		sourceRes, c.org, uuid.New(), c.slot, uuid.New(), buyer, q, sourceUnit, sourceTotal+fee, sourceTotal)
	exec(`INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref)
		VALUES($1,$2,'completed',$3,'fingerprint',$4)`, c.source, sourceRes, "src-"+c.source.String(), uuid.New())
	exec(`INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'EUR','completed')`,
		replacementRes, c.org, targetHold, c.targetSlot, targetType, buyer, q, targetUnit, targetTotal+fee, targetTotal)
	exec(`INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
		VALUES($1,$2,'completed',$3,'exchange')`, c.replacement, replacementRes, "exchange:"+c.exchange.String())
	exec(`INSERT INTO order_exchanges(organizer_id,id,source_order_id,replacement_order_id,target_ticket_type_id,
		                            idempotency_key,request_fingerprint,quantity,source_total,source_gross_total,
		                            target_total,delta_amount,target_unit_amount,target_hold_id,
		                            replacement_reservation_id,target_slot_id,basis_at,
		                            currency,actor,reason,created_at,settled_at)
		VALUES($1,$2,$3,$4,$5,$6,'fp',$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'EUR','ops','buyer asked',$16,$17)`,
		c.org, c.exchange, c.source, c.replacement, targetType, "xkey-"+c.exchange.String(), q,
		sourceTotal, sourceTotal+fee, targetTotal, c.delta, targetUnit, targetHold, replacementRes, c.targetSlot,
		settled.Add(-time.Minute), settled)
	// The fact ids are Commerce's derivations (store.ExchangeReversedFactID, ExchangeSoldFactID).
	for _, f := range []struct {
		suffix string
		amount int64
	}{{"order.exchange.reversed", sourceTotal + fee}, {"order.exchange.sold", targetTotal + fee}} {
		exec(`INSERT INTO order_facts(fact_id,order_id,organizer_id,buyer_id,fact_type,amount,currency,occurred_at)
			VALUES($1,$2,$3,$4,$5,$6,'EUR',$7)`,
			uuid.NewSHA1(uuid.NameSpaceOID, []byte(c.exchange.String()+":"+f.suffix)), c.source, c.org, buyer, f.suffix, f.amount, settled)
	}
}

func issueAccessOrder(t *testing.T, ctx context.Context, st *store.Postgres, qr *ticket.Signer, org, order, slot uuid.UUID, n int) []uuid.UUID {
	t.Helper()
	var tickets []store.Ticket
	var ids []uuid.UUID
	buyer, ticketType := uuid.New(), uuid.New()
	for i := 0; i < n; i++ {
		id := uuid.New()
		issuedAt := time.Now().UTC()
		payload, err := qr.Payload(id, order, org, slot, issuedAt)
		if err != nil {
			t.Fatal(err)
		}
		tickets = append(tickets, store.Ticket{ID: id, OrderID: order, GuestOrderRef: uuid.New(), OrganizerID: org,
			BuyerID: buyer, SlotID: slot, TicketTypeID: ticketType, Payload: payload, IssuedAt: issuedAt})
		ids = append(ids, id)
	}
	if err := st.Issue(ctx, store.IssueInput{EventID: uuid.New(), Tickets: tickets}); err != nil {
		t.Fatal(err)
	}
	return ids
}

// switchAndAdmitLate commits the real switch, holds its Commerce callback (Commerce is never
// told), then records a late offline admission on a source ticket through real reconciliation.
func switchAndAdmitLate(t *testing.T, ctx context.Context, st *store.Postgres, qr *ticket.Signer, c abandonCase) {
	t.Helper()
	var replacement []store.Ticket
	for i := 0; i < len(c.sourceTickets); i++ {
		id := uuid.New()
		issuedAt := time.Now().UTC()
		payload, err := qr.Payload(id, c.replacement, c.org, c.targetSlot, issuedAt)
		if err != nil {
			t.Fatal(err)
		}
		replacement = append(replacement, store.Ticket{ID: id, OrderID: c.replacement, GuestOrderRef: uuid.New(),
			OrganizerID: c.org, BuyerID: uuid.New(), SlotID: c.targetSlot, TicketTypeID: uuid.New(), Payload: payload, IssuedAt: issuedAt})
	}
	if err := st.SwitchExchange(ctx, store.SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: c.exchange, SourceOrderID: c.source, OrganizerID: c.org, Tickets: replacement,
	}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if _, err := st.ReconcileAdmission(ctx, store.ReconcileOccurrence{
		TicketID: c.sourceTickets[0], OrderID: c.source, OrganizerID: c.org, SlotID: c.slot,
		OccurrenceID: uuid.New(), OccurredAt: time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("late offline admission: %v", err)
	}
}

type abandonStack struct {
	access   *pausingAccess
	deps     *countingDependency
	st       *store.Postgres
	qr       *ticket.Signer
	commerce *sql.DB
	bin      string
	env      []string
	c        abandonCase
}

func newAbandonStack(t *testing.T, ctx context.Context, bin string) *abandonStack {
	t.Helper()
	_, st, qr := newSmokeServer(t, ctx)
	srv := New(ServerConfig{Store: st, InternalToken: abandonAccessToken})
	access := &pausingAccess{router: srv.Router(nil, true)}
	accessServer := httptest.NewServer(access)
	t.Cleanup(accessServer.Close)

	c := abandonCase{org: uuid.New(), exchange: uuid.New(), source: uuid.New(), replacement: uuid.New(),
		slot: uuid.New(), targetSlot: uuid.New(), delta: 1000}
	deps := &countingDependency{delta: c.delta}
	depServer := httptest.NewServer(deps)
	t.Cleanup(depServer.Close)

	commerce, dsn := commerceDatabase(t, ctx, bin)
	seedCommerceUpgrade(t, ctx, commerce, c)
	c.sourceTickets = issueAccessOrder(t, ctx, st, qr, c.org, c.source, c.slot, 2)
	return &abandonStack{access: access, deps: deps, st: st, qr: qr, commerce: commerce, bin: bin, c: c, env: append(os.Environ(),
		"DATABASE_URL="+dsn, "ACCESS_URL="+accessServer.URL, "PAYMENTS_URL="+depServer.URL+"/payments",
		"INVENTORY_URL="+depServer.URL+"/inventory", "INTERNAL_SERVICE_TOKEN="+abandonAccessToken,
		"PAYMENTS_INTERNAL_TOKEN=payments-token-for-abandonment-tests")}
}

func (s *abandonStack) command(ctx context.Context, t *testing.T) (*exec.Cmd, *bytes.Buffer) {
	cmd := exec.CommandContext(ctx, s.bin, "abandon-exchange", s.c.org.String(), s.c.exchange.String(),
		"Ada Operator", "source admitted before the switch")
	cmd.Env = s.env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	return cmd, &out
}

// assertRefusedWithoutEffects checks the outcome every correct schedule shares: a refusal that
// names Access, no reservation in Commerce, and no payments or inventory call.
func (s *abandonStack) assertRefusedWithoutEffects(t *testing.T, ctx context.Context, runErr error, out string) {
	t.Helper()
	var started sql.NullTime
	if err := s.commerce.QueryRowContext(ctx, `SELECT abandonment_started_at FROM order_exchanges WHERE organizer_id=$1 AND id=$2`,
		s.c.org, s.c.exchange).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if started.Valid {
		t.Fatalf("the command reserved the abandonment of a switched exchange; output:\n%s", out)
	}
	if n := s.deps.count(); n != 0 {
		t.Fatalf("the command made %d payments or inventory call(s): %v; output:\n%s", n, s.deps.calls, out)
	}
	if runErr == nil || !strings.Contains(out, "REFUSED: Access does not prove") {
		t.Fatalf("command err = %v, want the Access refusal; output:\n%s", runErr, out)
	}
}

func (s *abandonStack) admission(t *testing.T, ctx context.Context, order uuid.UUID) store.OrderAdmission {
	t.Helper()
	a, err := s.st.OrderAdmission(ctx, s.c.org, order)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Both schedules share one build of the commerce binary (D19's compile cost, paid once).
func TestAbandonmentReadOrderingAgainstRealAccess(t *testing.T) {
	bin := buildCommerce(t)
	t.Run("the replacement is read after a late admission on a switched source", func(t *testing.T) {
		readsTheReplacementAfterALateAdmissionOnASwitchedSource(t, bin)
	})
	t.Run("the source is read before the replacement", func(t *testing.T) {
		readsTheSourceBeforeTheReplacement(t, bin)
	})
}

// The skipped-read schedule. The source reads admitted and fully issued, exactly like a source
// whose switch Access refused; only the replacement read sees the switch.
func readsTheReplacementAfterALateAdmissionOnASwitchedSource(t *testing.T, bin string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := newAbandonStack(t, ctx, bin)
	switchAndAdmitLate(t, ctx, s.st, s.qr, s.c)
	// The schedule is real: the source alone passes, and the replacement carries the switch.
	if a := s.admission(t, ctx, s.c.source); !a.Admitted || a.IssuedCount != 2 {
		t.Fatalf("source admission after the switch and the late admission = %+v, want admitted with 2 issued", a)
	}
	if a := s.admission(t, ctx, s.c.replacement); a.IssuedCount != 2 {
		t.Fatalf("replacement issuance after the switch = %+v, want 2 tickets", a)
	}

	cmd, out := s.command(ctx, t)
	err := cmd.Run()
	s.assertRefusedWithoutEffects(t, ctx, err, out.String())
	reads := s.access.readPaths()
	want := []string{"/internal/orders/" + s.c.source.String() + "/admission", "/internal/orders/" + s.c.replacement.String() + "/admission"}
	if len(reads) != 2 || reads[0] != want[0] || reads[1] != want[1] {
		t.Fatalf("access reads = %v, want source then replacement %v", reads, want)
	}
}

// The swapped-order schedule. The first answer is held after its snapshot; the switch and the
// late admission commit in the gap. Source-first refuses on the captured `admitted=false`.
func readsTheSourceBeforeTheReplacement(t *testing.T, bin string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := newAbandonStack(t, ctx, bin)
	if a := s.admission(t, ctx, s.c.source); a.Admitted || a.IssuedCount != 2 {
		t.Fatalf("source before the schedule = %+v, want unadmitted with 2 issued", a)
	}
	if a := s.admission(t, ctx, s.c.replacement); a.IssuedCount != 0 {
		t.Fatalf("replacement before the schedule = %+v, want no ticket", a)
	}
	s.access.mu.Lock()
	s.access.arm, s.access.paused, s.access.release = true, make(chan struct{}), make(chan struct{})
	paused, release := s.access.paused, s.access.release
	s.access.mu.Unlock()
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	cmd, out := s.command(ctx, t)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-paused:
	case <-time.After(time.Minute):
		t.Fatalf("the command never read Access; output:\n%s", out.String())
	}
	switchAndAdmitLate(t, ctx, s.st, s.qr, s.c)
	releaseOnce.Do(func() { close(release) })
	err := cmd.Wait()

	// The schedule happened: after it, the source reads admitted and the replacement issued.
	if a := s.admission(t, ctx, s.c.source); !a.Admitted || a.IssuedCount != 2 {
		t.Fatalf("source after the schedule = %+v, want admitted with 2 issued", a)
	}
	if a := s.admission(t, ctx, s.c.replacement); a.IssuedCount != 2 {
		t.Fatalf("replacement after the schedule = %+v, want 2 tickets", a)
	}
	s.assertRefusedWithoutEffects(t, ctx, err, out.String())
	if reads := s.access.readPaths(); len(reads) != 1 || reads[0] != "/internal/orders/"+s.c.source.String()+"/admission" {
		t.Fatalf("access reads = %v, want the source alone: it refused on its captured answer", reads)
	}
}
