// TKT-380 probe driver. DISPOSABLE EVIDENCE, NOT PRODUCTION CODE.
//
// run.sh copies this file into a TEMPORARY copy of services/inventory as
// cmd/tkt380probe/main.go, next to prototype.go, and runs it against a
// disposable PostgreSQL. It is never compiled in the repository.
//
// Modes:
//
//	presale N    Probe 1. TODAY's code: CreateHold + Transition. N repeats of a
//	             control schedule and of the forced race schedule S1.
//	prototype N  Probe 2. The D13 prototype (TKT380PromotedTransition) under
//	             schedules S1 and S2. Exit 1 if any run overshoots the cap.
//
// Schedule S1 (time base): a finalize transaction starts before the hold
// expires and queues on its pool lock (P2). The hold expires. An admission on
// another pool (P1) presents the same code and commits. Then the finalize runs.
//
// Schedule S2 (code lock): a finalize decides before the hold expires and is
// held, uncommitted, by a probe hook until after expiry. An admission on P1
// presents the same code while the finalize is still open.
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
	codeCap = 1
	ttl     = 2 * time.Second
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
		fatalf("usage: tkt380probe presale|prototype RUNS")
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

	overshoot := false
	for n := 1; n <= runs; n++ {
		switch mode {
		case "presale":
			control(newFixture(ctx, admin, base), n)
			s1(newFixture(ctx, admin, base), n, "presale-S1", false)
		case "prototype":
			if s1(newFixture(ctx, admin, base), n, "prototype-S1", true) {
				overshoot = true
			}
			if s2(newFixture(ctx, admin, base), n) {
				overshoot = true
			}
		default:
			fatalf("unknown mode %q", mode)
		}
	}
	if mode == "prototype" && overshoot {
		fmt.Println("RESULT prototype: CAP OVERSHOT in at least one run")
		os.Exit(1)
	}
	if mode == "prototype" {
		fmt.Println("RESULT prototype: cap held in every run")
	}
}

func newFixture(ctx context.Context, admin *sql.DB, base *url.URL) *fixture {
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
	one := int32(codeCap)
	if _, err = st.IssuePresaleCode(ctx, store.PresaleCode{OrganizerID: f.org, Channel: channel, Code: code, MaxRedemptions: &one}); err != nil {
		fatalf("issue code: %v", err)
	}
	return f
}

func (f *fixture) hold(pool uuid.UUID, key string) (store.Claim, error) {
	c, _, err := f.st.CreateHold(f.ctx, f.org, pool, uuid.Nil, 1, 0, "", channel, key, store.WithPresaleCode(code))
	return c, err
}

func (f *fixture) dbNow() time.Time {
	var t time.Time
	if err := f.admin.QueryRowContext(f.ctx, `SELECT clock_timestamp()`).Scan(&t); err != nil {
		fatalf("clock: %v", err)
	}
	return t
}

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
	pid       int
	xactStart time.Time
	waitType  string
	query     string
}

// blockedBy returns the backends whose lock wait is blocked by pid.
func (f *fixture) blockedBy(pid int) []waiter {
	rows, err := f.admin.QueryContext(f.ctx, `SELECT pid, xact_start, COALESCE(wait_event_type,''), query
		FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid)
	if err != nil {
		fatalf("pg_stat_activity: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []waiter
	for rows.Next() {
		var w waiter
		if err = rows.Scan(&w.pid, &w.xactStart, &w.waitType, &w.query); err != nil {
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

func (f *fixture) redeemed() int64 {
	ss, err := f.st.PresaleCodeStatuses(f.ctx, f.org, channel)
	if err != nil || len(ss) != 1 {
		fatalf("code status: %v (%d rows)", err, len(ss))
	}
	return ss[0].Redeemed
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

// control: the finalize commits BEFORE expiry, then an admission on P1 after
// expiry. The admission must be refused. This proves the fixture CAN refuse,
// so a later admission is not an artefact of a fixture that never refuses.
func control(f *fixture, n int) {
	defer f.drop()
	tag := fmt.Sprintf("[run %d presale-control]", n)
	h, err := f.hold(f.p2, "h-"+uuid.NewString())
	if err != nil {
		fatalf("%s hold H: %v", tag, err)
	}
	fc, ferr := f.st.Transition(f.ctx, f.org, h.ID, "finalizing")
	f.waitDBPast(*h.ExpiresAt)
	a, aerr := f.hold(f.p1, "a-"+uuid.NewString())
	names := map[uuid.UUID]string{h.ID: "H", a.ID: "A"}
	f.printRows(tag, names)
	r := f.redeemed()
	fmt.Printf("%s finalize H: status=%s err=%s; admission A on P1 after expiry: err=%s; redeemed=%d cap=%d\n",
		tag, fc.Status, errText(ferr), errText(aerr), r, codeCap)
	fmt.Printf("SUMMARY presale-control f_status=%s f_err=%s a_err=%s redeemed=%d cap=%d\n", fc.Status, errText(ferr), errText(aerr), r, codeCap)
}

// s1 forces the time-base schedule. promoted=false drives today's Transition;
// promoted=true drives the D13 prototype. Returns true when the cap overshoots.
func s1(f *fixture, n int, name string, promoted bool) bool {
	defer f.drop()
	tag := fmt.Sprintf("[run %d %s]", n, name)
	h, err := f.hold(f.p2, "h-"+uuid.NewString())
	if err != nil {
		fatalf("%s hold H: %v", tag, err)
	}
	fmt.Printf("%s H created on P2: expires_at=%s\n", tag, h.ExpiresAt.UTC().Format(time.RFC3339Nano))

	// Blocker: an open transaction holding P2's pool row lock, as any other
	// claim transaction on P2 would.
	bconn, err := f.db.Conn(f.ctx)
	if err != nil {
		fatalf("%s blocker conn: %v", tag, err)
	}
	defer func() { _ = bconn.Close() }()
	btx, err := bconn.BeginTx(f.ctx, nil)
	if err != nil {
		fatalf("%s blocker begin: %v", tag, err)
	}
	var bpid int
	if err = btx.QueryRowContext(f.ctx, `SELECT pg_backend_pid()`).Scan(&bpid); err != nil {
		fatalf("%s blocker pid: %v", tag, err)
	}
	if _, err = btx.ExecContext(f.ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, f.p2); err != nil {
		fatalf("%s blocker lock: %v", tag, err)
	}

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
	w := f.waitBlockedBy(bpid)
	startedBeforeExpiry := w.xactStart.Before(*h.ExpiresAt)
	fmt.Printf("%s finalize F observed blocked on P2: pid=%d wait=%s xact_start=%s (before expiry: %v) query=%q\n",
		tag, w.pid, w.waitType, w.xactStart.UTC().Format(time.RFC3339Nano), startedBeforeExpiry, w.query)

	past := f.waitDBPast(*h.ExpiresAt)
	fmt.Printf("%s DB clock now %s, past H expiry\n", tag, past.UTC().Format(time.RFC3339Nano))
	a, aerr := f.hold(f.p1, "a-"+uuid.NewString())
	stillBlocked := len(f.blockedBy(bpid)) == 1
	fmt.Printf("%s admission A on P1 with the same code: err=%s; F still blocked after A returned: %v\n", tag, errText(aerr), stillBlocked)
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

	reached := startedBeforeExpiry && stillBlocked
	over := afterConfirm > codeCap
	verdict := "NOT-CONFIRMED"
	switch {
	case !reached:
		verdict = "NOT-REACHABLE"
	case aerr == nil && r.err == nil && r.c.Status == "finalizing" && over:
		verdict = "CONFIRMED"
	}
	if promoted {
		verdict = "CAP-HELD"
		if over {
			verdict = "CAP-OVERSHOT"
		}
		if !reached {
			verdict = "NOT-REACHABLE"
		}
	}
	fmt.Printf("SUMMARY %s reach_f_started_before_expiry=%v reach_f_blocked_through_admission=%v a_err=%s f_status=%s f_err=%s redeemed_after_finalize=%d redeemed_after_confirm=%d cap=%d verdict=%s\n",
		name, startedBeforeExpiry, stillBlocked, errText(aerr), r.c.Status, errText(r.err), afterFinalize, afterConfirm, codeCap, verdict)
	return over
}

// s2 forces the code-lock schedule against the D13 prototype. The finalize
// decides before expiry; the probe hook holds it open, waits for expiry, then
// starts the admission and waits until the admission either finishes or is
// observed blocked BY THE FINALIZE's backend. Returns true on overshoot.
func s2(f *fixture, n int) bool {
	defer f.drop()
	tag := fmt.Sprintf("[run %d prototype-S2]", n)
	h, err := f.hold(f.p2, "h-"+uuid.NewString())
	if err != nil {
		fatalf("%s hold H: %v", tag, err)
	}
	fmt.Printf("%s H created on P2: expires_at=%s\n", tag, h.ExpiresAt.UTC().Format(time.RFC3339Nano))

	type result struct {
		c   store.Claim
		err error
	}
	aDone := make(chan result, 1)
	var a store.Claim
	var aerr error
	var aBlocked, aFinishedFirst bool
	var decidedAt time.Time
	store.TKT380AfterDecision = func(ctx context.Context, tx *sql.Tx) {
		var fpid int
		if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid(), clock_timestamp()`).Scan(&fpid, &decidedAt); err != nil {
			fatalf("%s hook: %v", tag, err)
		}
		f.waitDBPast(*h.ExpiresAt)
		go func() {
			c, e := f.hold(f.p1, "a-"+uuid.NewString())
			aDone <- result{c, e}
		}()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case r := <-aDone:
				a, aerr, aFinishedFirst = r.c, r.err, true
				return
			default:
			}
			if len(f.blockedBy(fpid)) == 1 {
				aBlocked = true
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		fatalf("%s admission neither finished nor blocked on the finalize", tag)
	}
	defer func() { store.TKT380AfterDecision = nil }()

	fc, ferr := f.st.TKT380PromotedTransition(f.ctx, f.org, h.ID, "finalizing")
	if !aFinishedFirst {
		r := <-aDone
		a, aerr = r.c, r.err
	}
	decidedBeforeExpiry := decidedAt.Before(*h.ExpiresAt)
	fmt.Printf("%s finalize F decided at %s (before expiry: %v), status=%s err=%s\n", tag, decidedAt.UTC().Format(time.RFC3339Nano), decidedBeforeExpiry, fc.Status, errText(ferr))
	fmt.Printf("%s admission A on P1: blocked behind F's open transaction=%v, finished while F was open=%v, err=%s\n", tag, aBlocked, aFinishedFirst, errText(aerr))
	names := map[uuid.UUID]string{h.ID: "H", a.ID: "A"}
	f.printRows(tag, names)
	r := f.redeemed()
	over := r > codeCap
	verdict := "CAP-HELD"
	if over {
		verdict = "CAP-OVERSHOT"
	}
	if fc.Status != "finalizing" || !decidedBeforeExpiry {
		verdict = "NOT-REACHABLE"
	}
	fmt.Printf("SUMMARY prototype-S2 reach_f_decided_before_expiry=%v a_blocked_on_f=%v a_finished_while_f_open=%v a_err=%s f_status=%s f_err=%s redeemed=%d cap=%d verdict=%s\n",
		decidedBeforeExpiry, aBlocked, aFinishedFirst, errText(aerr), fc.Status, errText(ferr), r, codeCap, verdict)
	return over
}
