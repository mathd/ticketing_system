//go:build smoke

// Package testdb is shared smoke-test plumbing for the Access store and API
// tests (TKT-557, ADR-085 §8 ticket 0).
//
// It opens a REAL pgx pool on an isolated schema and adds real connector and
// transport fault controls: the real resolver on a .invalid name, real dials
// to a closed or a backlog-full port, a role with CONNECTION LIMIT 0, the
// recovery-held PostgreSQL that the smoke runner starts, and an owned TCP
// proxy that can cut or withhold an established socket. No control builds a
// driver error by hand.
//
// A separate, healthy admin handle does setup, readback and backend control.
// It never goes through the fault controls, so a readback never uses the
// faulted connection.
//
// The package imports neither store nor api, so both can use it without an
// import cycle. Each caller supplies its own migration step.
package testdb

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"ticketing/shared/runtimecfg"
)

const (
	// AdminDSNEnv is the existing Access smoke database (a superuser URL).
	AdminDSNEnv = "ACCESS_MIGRATION_TEST_DATABASE_URL"
	// RecoveryDSNEnv is the recovery-held PostgreSQL that scripts/smoke.sh
	// starts for the Access suite (TKT-557 D28/D31). It has explicit
	// sslmode=disable and answers every startup with SQLSTATE 57P03.
	RecoveryDSNEnv = "ACCESS_RECOVERY_TEST_DATABASE_URL"
)

// Fault selects what the NEXT connector attempt meets. While a fault is
// armed, every reused pooled connection is discarded, so the operation under
// test must dial: a cold fault reaches exactly the attempt the test aims at.
type Fault string

const (
	FaultNone Fault = ""
	// FaultDNSLookup: LookupFunc runs the real resolver on a unique .invalid
	// name (the lookup form: a DNS error joined directly below ConnectError).
	FaultDNSLookup Fault = "dns_lookup"
	// FaultDNSAttempt: DialFunc runs a real net.Dialer on a unique .invalid
	// name (the per-attempt form, through net.OpError).
	FaultDNSAttempt Fault = "dns_attempt"
	// FaultRefused: DialFunc dials a closed IPv4 loopback port that this
	// fixture owned.
	FaultRefused Fault = "refused"
	// FaultBlockedDial: DialFunc dials a listener whose accept queue is full,
	// so the dial cannot complete before its context ends.
	FaultBlockedDial Fault = "blocked_dial"
	// FaultDialTimeout: the same blocked listener, with a 100 ms dialer
	// timeout. A negative control: an independent socket timeout before the
	// server step timer.
	FaultDialTimeout Fault = "dial_timeout"
	// FaultRecovery: the attempt goes to the recovery-held PostgreSQL on its
	// IPv4 loopback address.
	FaultRecovery Fault = "recovery"
	// FaultRecoveryLocalhost: the same server through host "localhost", which
	// can add an IPv6 refusal attempt beside the IPv4 57P03 attempt.
	FaultRecoveryLocalhost Fault = "recovery_localhost"
	// FaultRoleLimit: the attempt authenticates as a role with CONNECTION
	// LIMIT 0 (SQLSTATE 53300). Call LimitedRole first.
	FaultRoleLimit Fault = "role_limit"
	// FaultMixedSupported: one connect attempt set with three real supported
	// leaves: the recovery-held 57P03, a lookup-form DNS failure for a
	// .invalid fallback host, and a refusal from a closed owned port.
	FaultMixedSupported Fault = "mixed_supported"
	// FaultMixedUnsupported: the recovery-held 57P03 and a lookup-form DNS
	// failure, beside an independent 100 ms dial timeout (unsupported).
	FaultMixedUnsupported Fault = "mixed_unsupported"
	// FaultMixedUnsupportedFirst: the same set with the unsupported attempt
	// first.
	FaultMixedUnsupportedFirst Fault = "mixed_unsupported_first"
)

// CutMode is what the proxy does when it sees the armed pattern in bytes
// from the client.
type CutMode string

const (
	// CutFIN closes the client side with a FIN before it forwards the bytes
	// that complete the pattern.
	CutFIN CutMode = "fin"
	// CutRST closes the client side with an RST (SO_LINGER=0) before it
	// forwards the bytes that complete the pattern.
	CutRST CutMode = "rst"
	// Withhold forwards the bytes and then holds every server reply until the
	// client closes.
	Withhold CutMode = "withhold"
)

// Phase says when a query hook runs.
type Phase int

const (
	AtStart Phase = iota
	AtEnd
)

// Options configures a fixture.
type Options struct {
	// Migrate prepares the schema on the admin handle.
	Migrate func(ctx context.Context, db *sql.DB) error
	// Proxy routes every healthy application connection through the owned
	// TCP proxy, so a test can cut or withhold an established socket.
	Proxy bool
	// MaxOpen replaces the runtimecfg pool size when it is not zero. Only a
	// fixture that is labelled as a pool-wait fixture sets it.
	MaxOpen int
}

// QueryEvent is one traced statement on the application pool.
type QueryEvent struct {
	SQL        string
	PID        uint32
	Start, End time.Time
	Rows       int64
	Err        error
	Done       bool
}

// Fixture is one isolated schema with a faultable application pool.
type Fixture struct {
	t       testing.TB
	Admin   *sql.DB
	DB      *sql.DB
	Schema  string
	AppName string

	target  string // host:port of the real server on the healthy path
	invalid string // unique .invalid name for both DNS faults

	mu            sync.Mutex
	fault         Fault
	attempts      int
	faulted       int
	dials         int
	discards      int
	queries       []*QueryEvent
	hooks         []*hook
	refusedAddr   string
	blocked       *blockedListener
	limitUser     string
	limitPassword string
	proxy         *proxy
}

type hook struct {
	phase Phase
	match string
	fn    func(QueryEvent)
	done  bool
}

type eventKey struct{}

// New creates a fixture, or skips when the Access smoke database is not set.
func New(t testing.TB, ctx context.Context, opts Options) *Fixture {
	t.Helper()
	adminDSN := os.Getenv(AdminDSNEnv)
	if adminDSN == "" {
		t.Skip(AdminDSNEnv + " is not set")
	}
	suffix := randomHex(t, 4)
	f := &Fixture{t: t, Schema: "access_scanner_" + suffix, AppName: "tkt557_" + suffix, invalid: "tkt557-" + suffix + ".invalid"}

	adminCfg := disabledConfig(t, adminDSN)
	adminCfg.RuntimeParams["search_path"] = f.Schema
	adminCfg.RuntimeParams["application_name"] = f.AppName + "_admin"
	f.Admin = stdlib.OpenDB(*adminCfg)
	t.Cleanup(func() { _ = f.Admin.Close() })
	if _, err := f.Admin.ExecContext(ctx, "CREATE SCHEMA "+f.Schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = f.Admin.ExecContext(ctx, "DROP SCHEMA "+f.Schema+" CASCADE")
	})
	if opts.Migrate != nil {
		if err := opts.Migrate(ctx, f.Admin); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}

	cfg := disabledConfig(t, adminDSN)
	cfg.RuntimeParams["search_path"] = f.Schema
	cfg.RuntimeParams["application_name"] = f.AppName
	cfg.Tracer = f
	if cfg.Host == "localhost" {
		// One IPv4 attempt per connection, so a fault has one subject.
		cfg.Host = "127.0.0.1"
	}
	cfg.Fallbacks = nil
	f.target = net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port)))
	if opts.Proxy {
		f.proxy = newProxy(t, f.target)
	}
	f.DB = stdlib.OpenDB(*cfg, stdlib.OptionBeforeConnect(f.beforeConnect), stdlib.OptionResetSession(f.resetSession))
	t.Cleanup(func() { _ = f.DB.Close() })
	pool, err := runtimecfg.DatabaseFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	pool.Apply(f.DB)
	if opts.MaxOpen > 0 {
		f.DB.SetMaxOpenConns(opts.MaxOpen)
		f.DB.SetMaxIdleConns(min(pool.MaxIdleConns, opts.MaxOpen))
	}
	return f
}

// disabledConfig parses dsn with an explicit sslmode=disable, the only form
// Access accepts (D21).
func disabledConfig(t testing.TB, dsn string) *pgx.ConnConfig {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: invalid URL", AdminDSNEnv)
	}
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		t.Fatalf("parse database config: %v", err)
	}
	if cfg.TLSConfig != nil {
		t.Fatal("explicit sslmode=disable still produced a TLS config")
	}
	return cfg
}

func randomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// Arm sets the fault for every later connector attempt and makes every
// reused pooled connection be discarded until Disarm.
func (f *Fixture) Arm(fault Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepareLocked(fault)
	f.fault = fault
}

// Disarm restores healthy connector attempts and pooled reuse.
func (f *Fixture) Disarm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fault = FaultNone
}

// ArmAfter arms fault when a statement whose text contains match ends: a
// barrier after a successful earlier statement in the same request.
func (f *Fixture) ArmAfter(match string, fault Fault) {
	f.mu.Lock()
	f.prepareLocked(fault)
	f.mu.Unlock()
	f.Once(AtEnd, match, func(QueryEvent) { f.Arm(fault) })
}

func (f *Fixture) prepareLocked(fault Fault) {
	switch fault {
	case FaultRefused:
		if f.refusedAddr == "" {
			f.refusedAddr = closedLoopback(f.t)
		}
	case FaultBlockedDial, FaultDialTimeout:
		if f.blocked == nil {
			f.blocked = newBlockedListener(f.t)
		}
	case FaultMixedSupported:
		if f.refusedAddr == "" {
			f.refusedAddr = closedLoopback(f.t)
		}
	case FaultMixedUnsupported, FaultMixedUnsupportedFirst:
		if f.blocked == nil {
			f.blocked = newBlockedListener(f.t)
		}
	case FaultRoleLimit:
		if f.limitUser == "" {
			f.t.Fatal("FaultRoleLimit needs LimitedRole first")
		}
	}
}

// Once runs fn one time, when a statement whose text contains match (case
// insensitive) starts or ends. fn runs on the statement's goroutine.
func (f *Fixture) Once(phase Phase, match string, fn func(QueryEvent)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks = append(f.hooks, &hook{phase: phase, match: strings.ToLower(match), fn: fn})
}

func (f *Fixture) takeLocked(phase Phase, sql string) []func(QueryEvent) {
	lower := strings.ToLower(sql)
	var due []func(QueryEvent)
	for _, h := range f.hooks {
		if !h.done && h.phase == phase && strings.Contains(lower, h.match) {
			h.done = true
			due = append(due, h.fn)
		}
	}
	return due
}

// TraceQueryStart is the pgx tracer seam. It records every statement.
func (f *Fixture) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	event := &QueryEvent{SQL: data.SQL, PID: conn.PgConn().PID(), Start: time.Now()}
	f.mu.Lock()
	f.queries = append(f.queries, event)
	due := f.takeLocked(AtStart, data.SQL)
	snapshot := *event
	f.mu.Unlock()
	for _, fn := range due {
		fn(snapshot)
	}
	return context.WithValue(ctx, eventKey{}, event)
}

// TraceQueryEnd records the end of a statement, its row count and its error.
func (f *Fixture) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	event, ok := ctx.Value(eventKey{}).(*QueryEvent)
	if !ok {
		return
	}
	f.mu.Lock()
	event.End, event.Rows, event.Err, event.Done = time.Now(), data.CommandTag.RowsAffected(), data.Err, true
	due := f.takeLocked(AtEnd, event.SQL)
	snapshot := *event
	f.mu.Unlock()
	for _, fn := range due {
		fn(snapshot)
	}
}

// Queries returns a copy of every traced statement so far.
func (f *Fixture) Queries() []QueryEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]QueryEvent, 0, len(f.queries))
	for _, q := range f.queries {
		out = append(out, *q)
	}
	return out
}

// CountQueries counts traced statements whose text contains match.
func (f *Fixture) CountQueries(match string) int {
	n := 0
	for _, q := range f.Queries() {
		if strings.Contains(strings.ToLower(q.SQL), strings.ToLower(match)) {
			n++
		}
	}
	return n
}

// Stats is the connector evidence for one fixture.
type Stats struct {
	Attempts, Faulted, Dials, Discards int
}

// Stats returns connector attempts (every one, retries included), attempts
// that met a fault, dials on the healthy and faulted paths, and reused
// connections discarded while a fault was armed.
func (f *Fixture) Stats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Stats{Attempts: f.attempts, Faulted: f.faulted, Dials: f.dials, Discards: f.discards}
}

func (f *Fixture) noteDial() {
	f.mu.Lock()
	f.dials++
	f.mu.Unlock()
}

func (f *Fixture) resetSession(context.Context, *pgx.Conn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fault != FaultNone {
		f.discards++
		return driver.ErrBadConn
	}
	return nil
}

// beforeConnect picks this attempt's fault once and sets the attempt's own
// lookup and dial functions, so one attempt never mixes two faults.
func (f *Fixture) beforeConnect(_ context.Context, cc *pgx.ConnConfig) error {
	f.mu.Lock()
	fault := f.fault
	f.attempts++
	if fault != FaultNone {
		f.faulted++
	}
	refused, blocked, invalid := f.refusedAddr, f.blocked, f.invalid
	limitUser, limitPassword := f.limitUser, f.limitPassword
	f.mu.Unlock()

	dialer := &net.Dialer{}
	direct := func(ctx context.Context, network, address string) (net.Conn, error) {
		f.noteDial()
		return dialer.DialContext(ctx, network, address)
	}
	switch fault {
	case FaultNone:
		cc.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
			f.noteDial()
			if f.proxy != nil {
				address = f.proxy.addr
			}
			return dialer.DialContext(ctx, network, address)
		}
	case FaultDNSLookup:
		cc.LookupFunc = func(ctx context.Context, _ string) ([]string, error) {
			return net.DefaultResolver.LookupHost(ctx, invalid)
		}
		cc.DialFunc = direct
	case FaultDNSAttempt:
		cc.DialFunc = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return direct(ctx, network, net.JoinHostPort(invalid, "5432"))
		}
	case FaultRefused:
		cc.DialFunc = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return direct(ctx, network, refused)
		}
	case FaultBlockedDial:
		cc.DialFunc = func(ctx context.Context, network, _ string) (net.Conn, error) {
			blocked.entered.Add(1)
			return direct(ctx, network, blocked.addr)
		}
	case FaultDialTimeout:
		cc.DialFunc = func(ctx context.Context, network, _ string) (net.Conn, error) {
			blocked.entered.Add(1)
			f.noteDial()
			return (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, network, blocked.addr)
		}
	case FaultRecovery, FaultRecoveryLocalhost:
		recovery := RecoveryConfig(f.t)
		cc.Host, cc.Port = recovery.Host, recovery.Port
		cc.User, cc.Password, cc.Database = recovery.User, recovery.Password, recovery.Database
		cc.TLSConfig, cc.Fallbacks = nil, nil
		if fault == FaultRecoveryLocalhost {
			cc.Host = "localhost"
		}
		cc.LookupFunc = net.DefaultResolver.LookupHost
		cc.DialFunc = direct
	case FaultRoleLimit:
		cc.User, cc.Password = limitUser, limitPassword
		cc.DialFunc = direct
	case FaultMixedSupported, FaultMixedUnsupported, FaultMixedUnsupportedFirst:
		recovery := RecoveryConfig(f.t)
		cc.User, cc.Password, cc.Database = recovery.User, recovery.Password, recovery.Database
		cc.TLSConfig = nil
		cc.LookupFunc = net.DefaultResolver.LookupHost
		recoveryHop := &pgconn.FallbackConfig{Host: recovery.Host, Port: recovery.Port}
		lookupHop := &pgconn.FallbackConfig{Host: invalid, Port: 5432}
		other := refused
		if fault != FaultMixedSupported {
			other = blocked.addr
		}
		host, port, _ := net.SplitHostPort(other)
		otherPort, _ := strconv.Atoi(port)
		otherHop := &pgconn.FallbackConfig{Host: host, Port: uint16(otherPort)}
		hops := []*pgconn.FallbackConfig{recoveryHop, lookupHop, otherHop}
		if fault == FaultMixedUnsupportedFirst {
			hops = []*pgconn.FallbackConfig{otherHop, lookupHop, recoveryHop}
		}
		cc.Host, cc.Port, cc.Fallbacks = hops[0].Host, hops[0].Port, hops[1:]
		cc.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
			if fault != FaultMixedSupported && address == other {
				blocked.entered.Add(1)
				f.noteDial()
				return (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, network, address)
			}
			return direct(ctx, network, address)
		}
	}
	return nil
}

// RecoveryConfig returns the recovery-held PostgreSQL that the smoke runner
// started. In the gate the variable is required: a missing URL, a URL without
// explicit disable, or a server that does not answer a real pgx startup with
// SQLSTATE 57P03 fails the test. It never skips and never falls back.
func RecoveryConfig(t testing.TB) *pgx.ConnConfig {
	t.Helper()
	dsn := os.Getenv(RecoveryDSNEnv)
	if dsn == "" {
		t.Fatalf("%s is not set: scripts/smoke.sh must start the recovery-held PostgreSQL for the Access suite (TKT-557 D28)", RecoveryDSNEnv)
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Query().Get("sslmode") != "disable" {
		t.Fatalf("%s must be a URL with explicit sslmode=disable", RecoveryDSNEnv)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", RecoveryDSNEnv, err)
	}
	return cfg
}

// RequireRecoveryRejects proves that the recovery producer is live: one real
// pgx startup returns SQLSTATE 57P03 below a ConnectError.
func RequireRecoveryRejects(t testing.TB, ctx context.Context) {
	t.Helper()
	cfg := RecoveryConfig(t)
	conn, err := pgconn.ConnectConfig(ctx, &cfg.Config)
	if err == nil {
		_ = conn.Close(ctx)
		t.Fatal("the recovery-held PostgreSQL accepted a connection: it is not in recovery")
	}
	var connectErr *pgconn.ConnectError
	var pgErr *pgconn.PgError
	if !errors.As(err, &connectErr) || !errors.As(err, &pgErr) || pgErr.Code != "57P03" {
		t.Fatalf("recovery producer returned %v, want SQLSTATE 57P03 below ConnectError", err)
	}
}

// LimitedRole creates a unique nonsuperuser role with CONNECTION LIMIT 0 for
// FaultRoleLimit, and removes only that role when the test ends.
func (f *Fixture) LimitedRole(ctx context.Context) {
	f.t.Helper()
	name := "tkt557_limit_" + randomHex(f.t, 6)
	password := randomHex(f.t, 16)
	if _, err := f.Admin.ExecContext(ctx, "CREATE ROLE "+name+" LOGIN NOSUPERUSER CONNECTION LIMIT 0 PASSWORD '"+password+"'"); err != nil {
		f.t.Fatalf("create limited role: %v", err)
	}
	f.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = f.Admin.ExecContext(ctx, "SELECT pg_terminate_backend(pid, 5000) FROM pg_stat_activity WHERE usename = $1", name)
		if _, err := f.Admin.ExecContext(ctx, "DROP ROLE "+name); err != nil {
			f.t.Errorf("drop limited role %s: %v", name, err)
		}
	})
	f.mu.Lock()
	f.limitUser, f.limitPassword = name, password
	f.mu.Unlock()
}

// AppBackends returns the server pids of the application pool's sessions.
func (f *Fixture) AppBackends(ctx context.Context) []int {
	f.t.Helper()
	rows, err := f.Admin.QueryContext(ctx, `SELECT pid FROM pg_stat_activity WHERE application_name = $1 ORDER BY pid`, f.AppName)
	if err != nil {
		f.t.Fatalf("list backends: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var pids []int
	for rows.Next() {
		var pid int
		if err := rows.Scan(&pid); err != nil {
			f.t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return pids
}

// Terminate ends one server backend from the admin session and waits until
// it has exited.
func (f *Fixture) Terminate(ctx context.Context, pid int) {
	f.t.Helper()
	var ok bool
	if err := f.Admin.QueryRowContext(ctx, `SELECT pg_terminate_backend($1, 5000)`, pid).Scan(&ok); err != nil || !ok {
		f.t.Fatalf("terminate backend %d: ok=%v err=%v", pid, ok, err)
	}
}

// TerminateAppBackends ends every application session and returns the pids.
func (f *Fixture) TerminateAppBackends(ctx context.Context) []int {
	f.t.Helper()
	pids := f.AppBackends(ctx)
	for _, pid := range pids {
		f.Terminate(ctx, pid)
	}
	return pids
}

// WaitForLockWait polls until an application session waits on a lock whose
// query contains match, or until timeout. It reports what it saw.
func (f *Fixture) WaitForLockWait(ctx context.Context, match string, timeout time.Duration) bool {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var n int
		err := f.Admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE application_name = $1 AND wait_event_type = 'Lock' AND position(lower($2) IN lower(query)) > 0`, f.AppName, match).Scan(&n)
		if err != nil {
			f.t.Fatalf("watch lock waits: %v", err)
		}
		if n > 0 {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// OpenTransactions counts application sessions that are idle in a
// transaction: a leaked BEGIN shows up here.
func (f *Fixture) OpenTransactions(ctx context.Context) int {
	f.t.Helper()
	var n int
	if err := f.Admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
		WHERE application_name = $1 AND state LIKE 'idle in transaction%'`, f.AppName).Scan(&n); err != nil {
		f.t.Fatalf("count open transactions: %v", err)
	}
	return n
}

// WaitNoOpenTransactions waits a bounded time for every application
// transaction to end.
func (f *Fixture) WaitNoOpenTransactions(ctx context.Context, timeout time.Duration) int {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		n := f.OpenTransactions(ctx)
		if n == 0 || time.Now().After(deadline) {
			return n
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// BlockedDials reports how many attempts entered the blocked listener's
// dial function.
func (f *Fixture) BlockedDials() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blocked == nil {
		return 0
	}
	return f.blocked.entered.Load()
}

// CutOn arms the proxy once: when the client bytes that one connection sends
// after this call complete pattern (case insensitive), that connection meets
// mode. The match crosses socket reads. It returns a channel that closes when
// the cut happens.
func (f *Fixture) CutOn(pattern string, mode CutMode) <-chan struct{} {
	f.t.Helper()
	if f.proxy == nil {
		f.t.Fatal("CutOn needs Options.Proxy")
	}
	return f.proxy.arm(pattern, mode)
}

// closedLoopback returns an IPv4 loopback address that this fixture owned
// and then closed.
func closedLoopback(t testing.TB) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// blockedListener is a real Linux listener with backlog 1 and no accept
// loop. Two owned connections fill its accept queue, so a later dial cannot
// complete before its context ends.
type blockedListener struct {
	addr    string
	entered atomic.Int64
}

func newBlockedListener(t testing.TB) *blockedListener {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 1); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	inet, ok := sa.(*syscall.SockaddrInet4)
	if !ok {
		t.Fatal("blocked listener is not IPv4")
	}
	b := &blockedListener{addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(inet.Port))}
	held := 0
	for i := 0; i < 8; i++ {
		conn, err := net.DialTimeout("tcp4", b.addr, 150*time.Millisecond)
		if err != nil {
			if held < 2 {
				t.Fatalf("blocked listener: queue not filled after %d connections: %v", held, err)
			}
			return b
		}
		held++
		t.Cleanup(func() { _ = conn.Close() })
	}
	t.Fatal("blocked listener: the accept queue never filled")
	return nil
}

// proxy is an owned TCP forwarder between the application pool and the real
// server. Its cuts act on a real established socket.
type proxy struct {
	addr   string
	target string
	mu     sync.Mutex
	rule   *cutRule
}

type cutRule struct {
	pattern []byte
	mode    CutMode
	fired   chan struct{}
}

func newProxy(t testing.TB, target string) *proxy {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{addr: ln.Addr().String(), target: target}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go p.handle(client)
		}
	}()
	return p
}

func (p *proxy) arm(pattern string, mode CutMode) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rule = &cutRule{pattern: bytes.ToLower([]byte(pattern)), mode: mode, fired: make(chan struct{})}
	return p.rule.fired
}

// clientStream is the match state of one client connection: the rule it
// last saw and at most len(pattern)-1 bytes that it read under that rule.
type clientStream struct {
	rule *cutRule
	tail []byte
}

// match reports the armed rule when chunk completes its pattern in the
// connection's byte stream. A TCP read boundary is not a statement boundary
// (ai-review R1-1), so the match crosses reads. Only bytes read after the rule
// was armed count, and the buffer holds at most len(pattern)-1 bytes. The
// caller acts before it forwards chunk, so the server never holds a complete
// pattern that a cut was armed for.
func (p *proxy) match(s *clientStream, chunk []byte) *cutRule {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rule == nil {
		return nil
	}
	if s.rule != p.rule {
		// Bytes read under no rule are never kept, and bytes read under an
		// earlier rule never complete this one.
		s.rule, s.tail = p.rule, nil
	}
	window := append(s.tail, bytes.ToLower(chunk)...)
	if bytes.Contains(window, p.rule.pattern) {
		rule := p.rule
		p.rule = nil
		s.rule, s.tail = nil, nil
		return rule
	}
	if keep := len(p.rule.pattern) - 1; len(window) > keep {
		window = window[len(window)-keep:]
	}
	s.tail = append([]byte(nil), window...)
	return nil
}

func (p *proxy) handle(client net.Conn) {
	server, err := net.Dial("tcp4", p.target)
	if err != nil {
		_ = client.Close()
		return
	}
	var withhold atomic.Bool
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := server.Read(buf)
			if n > 0 && !withhold.Load() {
				if _, werr := client.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		if !withhold.Load() {
			_ = client.Close()
		}
	}()
	stream := &clientStream{}
	buf := make([]byte, 32<<10)
	for {
		n, err := client.Read(buf)
		if n > 0 {
			if rule := p.match(stream, buf[:n]); rule != nil {
				switch rule.mode {
				case CutFIN:
					withhold.Store(true)
					_ = server.Close()
					if tcp, ok := client.(*net.TCPConn); ok {
						_ = tcp.CloseWrite()
					}
					close(rule.fired)
					drain(client)
					return
				case CutRST:
					withhold.Store(true)
					_ = server.Close()
					if tcp, ok := client.(*net.TCPConn); ok {
						_ = tcp.SetLinger(0)
					}
					_ = client.Close()
					close(rule.fired)
					return
				case Withhold:
					withhold.Store(true)
					close(rule.fired)
				}
			}
			if _, werr := server.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	_ = server.Close()
	_ = client.Close()
}

// drain reads until the peer closes, then closes: after a FIN the client
// sees EOF, and its close ends the pair.
func drain(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 4096)
	for {
		if _, err := conn.Read(buf); err != nil {
			break
		}
	}
	_ = conn.Close()
}

// ErrorTree renders a cause tree with every node's type, for test logs.
func ErrorTree(err error) string {
	var b strings.Builder
	var walk func(error, string)
	walk = func(e error, indent string) {
		if e == nil {
			return
		}
		fmt.Fprintf(&b, "%s%T: %s\n", indent, e, firstLine(e.Error()))
		switch node := e.(type) { //nolint:errorlint // renders each node of the tree, one by one
		case interface{ Unwrap() []error }:
			for _, child := range node.Unwrap() {
				walk(child, indent+"  ")
			}
		case interface{ Unwrap() error }:
			walk(node.Unwrap(), indent+"  ")
		}
	}
	walk(err, "")
	return b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
