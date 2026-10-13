// TKT-380 probe driver. DISPOSABLE EVIDENCE, NOT PRODUCTION CODE.
//
// run.sh copies this file into a TEMPORARY copy of services/inventory as
// cmd/tkt380probe/main.go, next to prototype.go, and runs it against a
// disposable PostgreSQL. It is never compiled in the repository.
//
// Modes:
//
//	presale N    TODAY's code (CreateHold + Transition). Per run: control, S1,
//	             window control, W.
//	window N     Window control and W only (run.sh uses it on the window-fix build).
//	prototype N  The D13 prototype (TKT380PromotedTransition). Per run: S1, S2, S3.
//	r2 N         TODAY's CreateHold and Transition, built with r2-predicate.py
//	             (reading R2). Per run: control, S1, R2L.
//
// Schedules:
//
//	S1 (time base)  A finalize starts before its hold expires and queues on its pool
//	                lock (P2). The hold expires. An admission on P1 with the same code
//	                commits. Then the finalize runs.
//	S2 (code lock)  A finalize decides before expiry and is held open by a probe hook
//	                past expiry. An admission on P1 runs while it is open.
//	S3 (cap edit)   As S2, but the code starts UNCAPPED. While the finalize is open
//	                and after expiry, an operator sets cap 1, then an admission on P1
//	                runs.
//	W (code window) An admission waits on the code row lock, held by a transaction
//	                that does not update the row, across the code's closes_at.
//	R2L (cleanup)   A hold expires unconfirmed on P2 with no terminal write. An
//	                admission on P1 runs, then a hold on P2 (its lazy sweep of P2
//	                writes the expiry first).
//
// Every SUMMARY line ends in verdict=HELD (the rule held), verdict=BROKEN (the
// defect was observed) or verdict=UNEXPECTED (any other outcome). A reach
// condition that fails is a harness error.
//
// Exit codes: 0 = every schedule HELD; 1 = at least one BROKEN and none
// UNEXPECTED; 2 = UNEXPECTED, unmet reach condition, or harness error.
//
// Every wait is on an observed database state (pg_blocking_pids, DB clock),
// not on a sleep that hopes the other side got there.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/inventory/internal/store"
)

const (
	channel = "presale"
	code    = "PROMO-CAP-1"
	ttl     = 2 * time.Second
)

type verdict string

const (
	held       verdict = "HELD"
	broken     verdict = "BROKEN"
	unexpected verdict = "UNEXPECTED"
)

type fixture struct {
	ctx    context.Context
	admin  *sql.DB
	db     *sql.DB
	st     *store.Postgres
	org    uuid.UUID
	p1, p2 uuid.UUID
	label  map[uuid.UUID]string
	drop   func()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "HARNESS ERROR: "+format+"\n", args...)
	os.Exit(2)
}

func main() {
	if len(os.Args) != 3 {
		fatalf("usage: tkt380probe presale|window|prototype|r2 RUNS")
	}
	mode := os.Args[1]
	runs, err := strconv.Atoi(os.Args[2])
	if err != nil || runs < 1 {
		fatalf("bad RUNS %q", os.Args[2])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	base, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil {
		fatalf("parse DATABASE_URL: %v", err)
	}
	admin, err := sql.Open("pgx", base.String())
	if err != nil {
		fatalf("open admin: %v", err)
	}
	defer func() { _ = admin.Close() }()
	var version string
	if err = admin.QueryRowContext(ctx, `SHOW server_version`).Scan(&version); err != nil {
		fatalf("server_version: %v", err)
	}
	fmt.Printf("postgres server_version=%s\n", version)

	var all []verdict
	capOne := func() *int32 { n := int32(1); return &n }
	for n := 1; n <= runs; n++ {
		switch mode {
		case "presale":
			all = append(all,
				control(newFixture(ctx, admin, base, capOne(), nil), n),
				s1(newFixture(ctx, admin, base, capOne(), nil), n, "presale-S1", false),
				windowControl(ctx, admin, base, n),
				w(ctx, admin, base, n))
		case "window":
			all = append(all, windowControl(ctx, admin, base, n), w(ctx, admin, base, n))
		case "prototype":
			all = append(all,
				s1(newFixture(ctx, admin, base, capOne(), nil), n, "prototype-S1", true),
				s2(newFixture(ctx, admin, base, capOne(), nil), n),
				s3(newFixture(ctx, admin, base, nil, nil), n))
		case "r2":
			all = append(all,
				control(newFixture(ctx, admin, base, capOne(), nil), n),
				s1(newFixture(ctx, admin, base, capOne(), nil), n, "r2-S1", false),
				r2Liveness(newFixture(ctx, admin, base, capOne(), nil), n))
		default:
			fatalf("unknown mode %q", mode)
		}
	}
	exit := 0
	for _, v := range all {
		switch v {
		case unexpected:
			exit = 2
		case broken:
			if exit == 0 {
				exit = 1
			}
		}
	}
	fmt.Printf("RESULT mode=%s runs=%d exit=%d (0 all HELD, 1 BROKEN observed, 2 UNEXPECTED)\n", mode, runs, exit)
	os.Exit(exit)
}

// newFixture: one organizer, pools P1 and P2 (capacity 10), a gated presale
// allocation (cap 10) on each, and one code with the given cap and close time.
func newFixture(ctx context.Context, admin *sql.DB, base *url.URL, maxRedemptions *int32, closesAt *time.Time) *fixture {
	schema := "tkt380_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		fatalf("create schema: %v", err)
	}
	u := *base
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		fatalf("open schema db: %v", err)
	}
	if err = store.Migrate(ctx, db); err != nil {
		fatalf("migrate (real Inventory migrations): %v", err)
	}
	st := store.New(db, ttl)
	f := &fixture{ctx: ctx, admin: admin, db: db, st: st, org: uuid.New(), p1: uuid.New(), p2: uuid.New()}
	f.label = map[uuid.UUID]string{f.p1: "P1", f.p2: "P2"}
	f.drop = func() {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	}
	for _, p := range []uuid.UUID{f.p1, f.p2} {
		if err = st.Provision(ctx, uuid.New(), p, f.org, 10); err != nil {
			fatalf("provision: %v", err)
		}
		// Generous channel cap and pool capacity: nothing may refuse for
		// capacity, or a refusal would be for the wrong reason.
		if _, err = st.ReplaceChannelAllocations(ctx, f.org, p, []store.ChannelAllocation{{Channel: channel, Cap: 10, RequiresCode: true}}, nil); err != nil {
			fatalf("allocations: %v", err)
		}
	}
	if _, err = st.IssuePresaleCode(ctx, store.PresaleCode{OrganizerID: f.org, Channel: channel, Code: code, MaxRedemptions: maxRedemptions, ClosesAt: closesAt}); err != nil {
		fatalf("issue code: %v", err)
	}
	return f
}

func (f *fixture) hold(pool uuid.UUID, key string) (store.Claim, error) {
	c, _, err := f.st.CreateHold(f.ctx, f.org, pool, uuid.Nil, 1, 0, "", channel, key, store.WithPresaleCode(code))
	return c, err
}

func dbNow(ctx context.Context, admin *sql.DB) time.Time {
	var t time.Time
	if err := admin.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&t); err != nil {
		fatalf("clock: %v", err)
	}
	return t
}

func (f *fixture) dbNow() time.Time { return dbNow(f.ctx, f.admin) }

// waitDBPast blocks until the DATABASE clock is past t plus a margin.
func (f *fixture) waitDBPast(t time.Time) time.Time {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if now := f.dbNow(); now.After(t.Add(200 * time.Millisecond)) {
			return now
		}
		time.Sleep(20 * time.Millisecond)
	}
	fatalf("database clock never passed %s", t)
	return time.Time{}
}

type waiter struct {
	pid        int
	xactStart  time.Time
	queryStart time.Time
	waitType   string
	query      string
}

// blockedBy returns the backends whose lock wait is blocked by pid.
func (f *fixture) blockedBy(pid int) []waiter {
	rows, err := f.admin.QueryContext(f.ctx, `SELECT pid, xact_start, query_start, COALESCE(wait_event_type,''), query
		FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid)
	if err != nil {
		fatalf("pg_stat_activity: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []waiter
	for rows.Next() {
		var w waiter
		if err = rows.Scan(&w.pid, &w.xactStart, &w.queryStart, &w.waitType, &w.query); err != nil {
			fatalf("scan waiter: %v", err)
		}
		out = append(out, w)
	}
	return out
}

func (f *fixture) waitBlockedBy(pid int) waiter {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ws := f.blockedBy(pid); len(ws) == 1 {
			return ws[0]
		} else if len(ws) > 1 {
			fatalf("expected one waiter on pid %d, saw %d", pid, len(ws))
		}
		time.Sleep(10 * time.Millisecond)
	}
	fatalf("no backend became blocked by pid %d", pid)
	return waiter{}
}

// blocker opens a transaction on its own connection and runs lockSQL in it.
func (f *fixture) blocker(tag, lockSQL string, args ...any) (*sql.Conn, *sql.Tx, int) {
	bconn, err := f.db.Conn(f.ctx)
	if err != nil {
		fatalf("%s blocker conn: %v", tag, err)
	}
	btx, err := bconn.BeginTx(f.ctx, nil)
	if err != nil {
		fatalf("%s blocker begin: %v", tag, err)
	}
	var bpid int
	if err = btx.QueryRowContext(f.ctx, `SELECT pg_backend_pid()`).Scan(&bpid); err != nil {
		fatalf("%s blocker pid: %v", tag, err)
	}
	if _, err = btx.ExecContext(f.ctx, lockSQL, args...); err != nil {
		fatalf("%s blocker lock: %v", tag, err)
	}
	return bconn, btx, bpid
}

func (f *fixture) status() store.PresaleCodeStatus {
	ss, err := f.st.PresaleCodeStatuses(f.ctx, f.org, channel)
	if err != nil || len(ss) != 1 {
		fatalf("code status: %v (%d rows)", err, len(ss))
	}
	return ss[0]
}

func (f *fixture) redeemed() int64 { return f.status().Redeemed }

func capText(s store.PresaleCodeStatus) string {
	if s.MaxRedemptions == nil {
		return "none"
	}
	return strconv.Itoa(int(*s.MaxRedemptions))
}

func (f *fixture) printRows(prefix string, names map[uuid.UUID]string) {
	rows, err := f.db.QueryContext(f.ctx, `SELECT id, pool_id, status, quantity, presale_code, expires_at, expires_at <= clock_timestamp()
		FROM claims WHERE organizer_id=$1 ORDER BY created_at, id`, f.org)
	if err != nil {
		fatalf("rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	fmt.Printf("%s claims (name | pool | status | qty | code | expires_at | past_expiry_now):\n", prefix)
	for rows.Next() {
		var id, pool uuid.UUID
		var status, pc string
		var qty int32
		var exp time.Time
		var past bool
		if err = rows.Scan(&id, &pool, &status, &qty, &pc, &exp, &past); err != nil {
			fatalf("scan row: %v", err)
		}
		fmt.Printf("%s   %s | %s | %s | %d | %s | %s | %v\n", prefix, names[id], f.label[pool], status, qty, pc, exp.UTC().Format(time.RFC3339Nano), past)
	}
}

func errText(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, store.ErrPresaleCodeInvalid):
		return "ErrPresaleCodeInvalid"
	case errors.Is(err, store.ErrConflict):
		return "ErrConflict"
	default:
		return err.Error()
	}
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// control: the finalize commits BEFORE expiry, then an admission on P1 after
// expiry. The admission must be refused. This proves the fixture CAN refuse.
func control(f *fixture, n int) verdict {
	defer f.drop()
	tag := fmt.Sprintf("[run %d presale-control]", n)
	h, err := f.hold(f.p2, "h-"+uuid.NewString())
	if err != nil {
		fatalf("%s hold H: %v", tag, err)
	}
	fc, ferr := f.st.Transition(f.ctx, f.org, h.ID, "finalizing")
	f.waitDBPast(*h.ExpiresAt)
	a, aerr := f.hold(f.p1, "a-"+uuid.NewString())
	f.printRows(tag, map[uuid.UUID]string{h.ID: "H", a.ID: "A"})
	r := f.redeemed()
	v := unexpected
	if ferr == nil && fc.Status == "finalizing" && errors.Is(aerr, store.ErrPresaleCodeInvalid) && r == 1 {
		v = held
	}
	fmt.Printf("SUMMARY presale-control f_status=%s f_err=%s a_err=%s redeemed=%d cap=1 verdict=%s\n", fc.Status, errText(ferr), errText(aerr), r, v)
	return v
}

// s1 forces the time-base schedule. promoted=false drives today's Transition;
// promoted=true drives the D13 prototype.
func s1(f *fixture, n int, name string, promoted bool) verdict {
	defer f.drop()
	tag := fmt.Sprintf("[run %d %s]", n, name)
	h, err := f.hold(f.p2, "h-"+uuid.NewString())
	if err != nil {
		fatalf("%s hold H: %v", tag, err)
	}
	fmt.Printf("%s H created on P2: expires_at=%s\n", tag, ts(*h.ExpiresAt))

	// Blocker: an open transaction holding P2's pool row lock, as any other
	// claim transaction on P2 would.
	bconn, btx, bpid := f.blocker(tag, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, f.p2)
	defer func() { _ = bconn.Close() }()

	type result struct {
		c   store.Claim
		err error
	}
	done := make(chan result, 1)
	go func() {
		var c store.Claim
		var e error
		if promoted {
			c, e = f.st.TKT380PromotedTransition(f.ctx, f.org, h.ID, "finalizing")
		} else {
			c, e = f.st.Transition(f.ctx, f.org, h.ID, "finalizing")
		}
		done <- result{c, e}
	}()
	wt := f.waitBlockedBy(bpid)
	if !wt.xactStart.Before(*h.ExpiresAt) {
		fatalf("%s reach: finalize transaction started at %s, not before expiry %s", tag, ts(wt.xactStart), ts(*h.ExpiresAt))
	}
	fmt.Printf("%s finalize F observed blocked on P2: wait=%s xact_start=%s (before expiry: true) query=%q\n",
		tag, wt.waitType, ts(wt.xactStart), wt.query)

	past := f.waitDBPast(*h.ExpiresAt)
	fmt.Printf("%s DB clock now %s, past H expiry\n", tag, ts(past))
	a, aerr := f.hold(f.p1, "a-"+uuid.NewString())
	if len(f.blockedBy(bpid)) != 1 {
		fatalf("%s reach: finalize was not still blocked when the admission returned", tag)
	}
	fmt.Printf("%s admission A on P1 with the same code: err=%s; F still blocked after A returned: true\n", tag, errText(aerr))
	names := map[uuid.UUID]string{h.ID: "H", a.ID: "A"}

	if err = btx.Rollback(); err != nil {
		fatalf("%s blocker rollback: %v", tag, err)
	}
	r := <-done
	fmt.Printf("%s finalize F returned: status=%s err=%s\n", tag, r.c.Status, errText(r.err))
	f.printRows(tag+" after F", names)
	afterFinalize := f.redeemed()

	// Take every surviving claim to confirmed: sold units are permanent.
	if r.err == nil && r.c.Status == "finalizing" {
		if _, err = f.st.Transition(f.ctx, f.org, h.ID, "confirmed"); err != nil {
			fatalf("%s confirm H: %v", tag, err)
		}
	}
	if aerr == nil {
		if _, err = f.st.Transition(f.ctx, f.org, a.ID, "finalizing"); err != nil {
			fatalf("%s finalize A: %v", tag, err)
		}
		if _, err = f.st.Transition(f.ctx, f.org, a.ID, "confirmed"); err != nil {
			fatalf("%s confirm A: %v", tag, err)
		}
	}
	f.printRows(tag+" after confirm", names)
	afterConfirm := f.redeemed()

	v := unexpected
	switch {
	case aerr == nil && errors.Is(r.err, store.ErrConflict) && r.c.Status == "expired" && afterConfirm == 1:
		v = held
	case aerr == nil && r.err == nil && r.c.Status == "finalizing" && afterConfirm == 2:
		v = broken
	case errors.Is(aerr, store.ErrPresaleCodeInvalid) && r.err == nil && r.c.Status == "finalizing" && afterConfirm == 1:
		// Reading R2: the expired, unswept H still counts, so the admission is
		// refused and the finalize keeps the unit.
		v = held
	}
	fmt.Printf("SUMMARY %s a_err=%s f_status=%s f_err=%s redeemed_after_finalize=%d redeemed_after_confirm=%d cap=1 verdict=%s\n",
		name, errText(aerr), r.c.Status, errText(r.err), afterFinalize, afterConfirm, v)
	return v
}

// r2Liveness forces schedule R2L. Under R2 the admission on P1 is refused
// (H still counts: nothing wrote its expiry), and the hold on P2 is admitted
// (CreateHold sweeps P2 before it takes the code lock, so H is expired first).
// HELD needs both. BROKEN is the R1 outcome: the P1 admission is admitted on
// an expired H with no terminal write.
func r2Liveness(f *fixture, n int) verdict {
	defer f.drop()
	tag := fmt.Sprintf("[run %d r2-liveness]", n)
	h, err := f.hold(f.p2, "h-"+uuid.NewString())
	if err != nil {
		fatalf("%s hold H: %v", tag, err)
	}
	past := f.waitDBPast(*h.ExpiresAt)
	fmt.Printf("%s H created on P2: expires_at=%s; DB clock now %s\n", tag, ts(*h.ExpiresAt), ts(past))
	a, aerr := f.hold(f.p1, "a-"+uuid.NewString())
	fmt.Printf("%s admission A on P1: err=%s\n", tag, errText(aerr))
	var hBefore string
	if err = f.db.QueryRowContext(f.ctx, `SELECT status FROM claims WHERE id=$1`, h.ID).Scan(&hBefore); err != nil {
		fatalf("%s read H: %v", tag, err)
	}
	b, berr := f.hold(f.p2, "b-"+uuid.NewString())
	fmt.Printf("%s admission B on P2: err=%s\n", tag, errText(berr))
	var hAfter string
	if err = f.db.QueryRowContext(f.ctx, `SELECT status FROM claims WHERE id=$1`, h.ID).Scan(&hAfter); err != nil {
		fatalf("%s read H: %v", tag, err)
	}
	f.printRows(tag, map[uuid.UUID]string{h.ID: "H", a.ID: "A", b.ID: "B"})
	r := f.redeemed()
	v := unexpected
	switch {
	case errors.Is(aerr, store.ErrPresaleCodeInvalid) && hBefore == "held" && berr == nil && hAfter == "expired" && r == 1:
		v = held
	case aerr == nil:
		v = broken
	}
	fmt.Printf("SUMMARY r2-liveness a_err=%s h_before_b=%s b_err=%s h_after_b=%s redeemed=%d cap=1 verdict=%s\n",
		errText(aerr), hBefore, errText(berr), hAfter, r, v)
	return v
}

// waitDoneOrBlocked polls until done fires or a backend is blocked by pid.
// It returns true when the work finished first.
func (f *fixture) waitDoneOrBlocked(tag string, done <-chan struct{}, pid int) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return true
		default:
		}
		if len(f.blockedBy(pid)) == 1 {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	fatalf("%s reach: work neither finished nor blocked on pid %d", tag, pid)
	return false
}

// decide installs the prototype hook. It records F's pid and decision time,
// then waits until the database clock is past expiry before calling body.
func decide(f *fixture, tag string, expires time.Time, body func(fpid int)) (decidedAt *time.Time) {
	decidedAt = new(time.Time)
	store.TKT380AfterDecision = func(ctx context.Context, tx *sql.Tx) {
		var fpid int
		if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid(), clock_timestamp()`).Scan(&fpid, decidedAt); err != nil {
			fatalf("%s hook: %v", tag, err)
		}
		f.waitDBPast(expires)
		body(fpid)
	}
	return decidedAt
}

// s2 forces the code-lock schedule against the D13 prototype.
func s2(f *fixture, n int) verdict {
	defer f.drop()
	tag := fmt.Sprintf("[run %d prototype-S2]", n)
	h, err := f.hold(f.p2, "h-"+uuid.NewString())
	if err != nil {
		fatalf("%s hold H: %v", tag, err)
	}
	fmt.Printf("%s H created on P2: expires_at=%s\n", tag, ts(*h.ExpiresAt))

	var a store.Claim
	var aerr error
	aDone := make(chan struct{})
	var aFirst bool
	decidedAt := decide(f, tag, *h.ExpiresAt, func(fpid int) {
		go func() {
			a, aerr = f.hold(f.p1, "a-"+uuid.NewString())
			close(aDone)
		}()
		aFirst = f.waitDoneOrBlocked(tag, aDone, fpid)
	})
	defer func() { store.TKT380AfterDecision = nil }()

	fc, ferr := f.st.TKT380PromotedTransition(f.ctx, f.org, h.ID, "finalizing")
	<-aDone
	if ferr != nil || fc.Status != "finalizing" || !decidedAt.Before(*h.ExpiresAt) {
		fatalf("%s reach: finalize must decide alive before expiry; got status=%s err=%s decided=%s", tag, fc.Status, errText(ferr), ts(*decidedAt))
	}
	fmt.Printf("%s finalize F decided at %s (before expiry: true), status=%s err=%s\n", tag, ts(*decidedAt), fc.Status, errText(ferr))
	fmt.Printf("%s admission A on P1: finished while F was open=%v, err=%s\n", tag, aFirst, errText(aerr))
	f.printRows(tag, map[uuid.UUID]string{h.ID: "H", a.ID: "A"})
	r := f.redeemed()
	v := unexpected
	switch {
	case !aFirst && errors.Is(aerr, store.ErrPresaleCodeInvalid) && r == 1:
		v = held
	case aFirst && aerr == nil && r == 2:
		v = broken
	}
	fmt.Printf("SUMMARY prototype-S2 a_blocked_on_f=%v a_err=%s f_status=%s redeemed=%d cap=1 verdict=%s\n",
		!aFirst, errText(aerr), fc.Status, r, v)
	return v
}

// s3 forces the cap-edit schedule: the code starts UNCAPPED; while the
// finalize is open and after expiry, an operator sets cap 1 (IssuePresaleCode,
// an upsert that locks the code row); then an admission on P1 runs.
func s3(f *fixture, n int) verdict {
	defer f.drop()
	tag := fmt.Sprintf("[run %d prototype-S3]", n)
	h, err := f.hold(f.p2, "h-"+uuid.NewString())
	if err != nil {
		fatalf("%s hold H: %v", tag, err)
	}
	fmt.Printf("%s H created on P2 under an UNCAPPED code: expires_at=%s\n", tag, ts(*h.ExpiresAt))

	var eerr, aerr error
	var a store.Claim
	eDone, aDone := make(chan struct{}), make(chan struct{})
	var eFirst, aRanInHook bool
	one := int32(1)
	edit := func() {
		_, eerr = f.st.IssuePresaleCode(f.ctx, store.PresaleCode{OrganizerID: f.org, Channel: channel, Code: code, MaxRedemptions: &one})
		close(eDone)
	}
	admit := func() {
		a, aerr = f.hold(f.p1, "a-"+uuid.NewString())
		close(aDone)
	}
	decidedAt := decide(f, tag, *h.ExpiresAt, func(fpid int) {
		go edit()
		eFirst = f.waitDoneOrBlocked(tag, eDone, fpid)
		if eFirst {
			// The edit committed while F is open: admit now, before F commits.
			aRanInHook = true
			go admit()
			if !f.waitDoneOrBlocked(tag, aDone, fpid) {
				return
			}
		}
	})
	defer func() { store.TKT380AfterDecision = nil }()

	fc, ferr := f.st.TKT380PromotedTransition(f.ctx, f.org, h.ID, "finalizing")
	<-eDone
	if !aRanInHook {
		admit()
	}
	<-aDone
	if ferr != nil || fc.Status != "finalizing" || !decidedAt.Before(*h.ExpiresAt) || eerr != nil {
		fatalf("%s reach: finalize must decide alive before expiry and the edit must succeed; got status=%s err=%s edit_err=%v", tag, fc.Status, errText(ferr), eerr)
	}
	fmt.Printf("%s finalize F decided at %s (before expiry: true), status=%s\n", tag, ts(*decidedAt), fc.Status)
	fmt.Printf("%s cap edit (cap 1) finished while F was open=%v; admission A on P1 err=%s\n", tag, eFirst, errText(aerr))
	f.printRows(tag, map[uuid.UUID]string{h.ID: "H", a.ID: "A"})
	s := f.status()
	v := unexpected
	switch {
	case !eFirst && errors.Is(aerr, store.ErrPresaleCodeInvalid) && s.Redeemed == 1 && capText(s) == "1":
		v = held
	case eFirst && aerr == nil && s.Redeemed == 2 && capText(s) == "1":
		v = broken
	}
	fmt.Printf("SUMMARY prototype-S3 edit_blocked_on_f=%v a_err=%s f_status=%s redeemed=%d cap=%s verdict=%s\n",
		!eFirst, errText(aerr), fc.Status, s.Redeemed, capText(s), v)
	return v
}

// windowFixture issues an uncapped code whose window closes 2 s from now.
func windowFixture(ctx context.Context, admin *sql.DB, base *url.URL) (*fixture, time.Time) {
	closes := dbNow(ctx, admin).Add(2 * time.Second)
	return newFixture(ctx, admin, base, nil, &closes), closes
}

// windowControl: an admission that starts after closes_at must be refused.
func windowControl(ctx context.Context, admin *sql.DB, base *url.URL, n int) verdict {
	f, closes := windowFixture(ctx, admin, base)
	defer f.drop()
	f.waitDBPast(closes)
	_, aerr := f.hold(f.p1, "a-"+uuid.NewString())
	v := unexpected
	if errors.Is(aerr, store.ErrPresaleCodeInvalid) && f.redeemed() == 0 {
		v = held
	}
	fmt.Printf("SUMMARY window-control a_err=%s redeemed=%d verdict=%s\n", errText(aerr), f.redeemed(), v)
	return v
}

// w: a transaction locks the code row (FOR UPDATE, no update) before the
// window closes. Admission A starts before closes_at and waits on that lock.
// The database clock passes closes_at. The lock is released.
func w(ctx context.Context, admin *sql.DB, base *url.URL, n int) verdict {
	f, closes := windowFixture(ctx, admin, base)
	defer f.drop()
	tag := fmt.Sprintf("[run %d W]", n)
	fmt.Printf("%s code window closes_at=%s\n", tag, ts(closes))
	bconn, btx, bpid := f.blocker(tag, `SELECT 1 FROM presale_codes WHERE organizer_id=$1 AND channel_code=$2 AND code=$3 FOR UPDATE`, f.org, channel, code)
	defer func() { _ = bconn.Close() }()

	var a store.Claim
	var aerr error
	aDone := make(chan struct{})
	go func() {
		a, aerr = f.hold(f.p1, "a-"+uuid.NewString())
		close(aDone)
	}()
	wt := f.waitBlockedBy(bpid)
	if !wt.queryStart.Before(closes) || !strings.Contains(wt.query, "presale_codes") {
		fatalf("%s reach: admission must wait on the code row from before closes_at; query_start=%s query=%q", tag, ts(wt.queryStart), wt.query)
	}
	fmt.Printf("%s admission A observed blocked on the code row: query_start=%s (before closes_at: true) query=%q\n", tag, ts(wt.queryStart), wt.query)
	past := f.waitDBPast(closes)
	fmt.Printf("%s DB clock now %s, past closes_at; releasing the lock without an update\n", tag, ts(past))
	if err := btx.Rollback(); err != nil {
		fatalf("%s blocker rollback: %v", tag, err)
	}
	<-aDone
	f.printRows(tag, map[uuid.UUID]string{a.ID: "A"})
	r := f.redeemed()
	v := unexpected
	switch {
	case errors.Is(aerr, store.ErrPresaleCodeInvalid) && r == 0:
		v = held
	case aerr == nil && r == 1:
		v = broken
	}
	fmt.Printf("SUMMARY W a_err=%s redeemed=%d verdict=%s\n", errText(aerr), r, v)
	return v
}
