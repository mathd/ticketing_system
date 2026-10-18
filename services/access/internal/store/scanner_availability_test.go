package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// Unit tests for the TKT-557 pre-decision helper and classifier (ADR-085 §8,
// ticket 0). They need no database. The real-driver producers are in
// scanner_availability_smoke_test.go. A hand-built error tree in this file can
// show a REJECTION or a walk rule. It cannot show that an outage produces a
// tree, so no acceptance here counts as outage evidence.

// fakeSteps is an injected step scheduler. The test decides when the timer
// callback runs and what time finish reads, so each order is forced, not hoped.
type fakeSteps struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeStepTimer
}

type fakeStepTimer struct {
	mu      sync.Mutex
	bound   time.Duration
	fire    func()
	stopped bool
}

func newFakeSteps() *fakeSteps {
	return &fakeSteps{now: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
}

func (f *fakeSteps) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeSteps) AfterFunc(d time.Duration, fn func()) stepTimer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeStepTimer{bound: d, fire: fn}
	f.timers = append(f.timers, t)
	return t
}

func (f *fakeSteps) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fakeSteps) last(t *testing.T) *fakeStepTimer {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.timers) == 0 {
		t.Fatal("no step timer was armed")
	}
	return f.timers[len(f.timers)-1]
}

func (f *fakeSteps) timer(i int) *fakeStepTimer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.timers[i]
}

func (f *fakeSteps) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

func (t *fakeStepTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

func (t *fakeStepTimer) isStopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped
}

func requireUnavailable(t *testing.T, err error, site AvailabilitySite, class AvailabilityClass) {
	t.Helper()
	avail := unavailableOf(t, err)
	if avail.Site != site || avail.Class != class {
		t.Fatalf("unavailable = %s/%s, want %s/%s (cause %v)", avail.Site, avail.Class, site, class, avail.Cause)
	}
}

// unavailableOf returns the named type inside err, or fails the test.
func unavailableOf(t *testing.T, err error) *PreDecisionUnavailable {
	t.Helper()
	var avail *PreDecisionUnavailable
	if !errors.As(err, &avail) {
		t.Fatalf("error = %v (%T), want *PreDecisionUnavailable", err, err)
	}
	return avail
}

func requireNotUnavailable(t *testing.T, err error) {
	t.Helper()
	var avail *PreDecisionUnavailable
	if errors.As(err, &avail) {
		t.Fatalf("error = %v, want no availability class (got %s/%s)", err, avail.Site, avail.Class)
	}
}

// Callback first, then finish: the timer fired while the phase was open, so
// the step expired and its child context is cancelled.
func TestStepCallbackBeforeFinishSelectsExpiry(t *testing.T) {
	steps := newFakeSteps()
	step := startBoundedStep(context.Background(), preDecisionStepBound, steps)
	timer := steps.last(t)
	if timer.bound != preDecisionStepBound {
		t.Fatalf("timer bound = %s, want %s", timer.bound, preDecisionStepBound)
	}
	timer.fire()
	if !errors.Is(step.ctx.Err(), context.Canceled) {
		t.Fatalf("child after callback = %v, want cancelled", step.ctx.Err())
	}
	outcome := step.finish()
	if !outcome.expired {
		t.Fatal("outcome.expired = false after the callback ran inside the phase")
	}
	err := outcome.unavailable(SiteDeviceLookup, context.Canceled)
	requireUnavailable(t, err, SiteDeviceLookup, ClassStepExpired)
	if avail := unavailableOf(t, err); !errors.Is(avail, context.Canceled) {
		t.Fatalf("cause = %v, want the original driver error kept", avail.Cause)
	}
}

// Finish first, then a late callback: the phase was already closed, so the
// callback must change nothing. This is the active-phase check.
func TestStepFinishBeforeCallbackKeepsTheChildAlive(t *testing.T) {
	steps := newFakeSteps()
	step := startBoundedStep(context.Background(), preDecisionStepBound, steps)
	outcome := step.finish()
	if outcome.expired {
		t.Fatal("outcome.expired = true with no callback and no elapsed bound")
	}
	if !steps.last(t).isStopped() {
		t.Fatal("finish did not disarm the step timer")
	}
	steps.last(t).fire()
	if err := step.ctx.Err(); err != nil {
		t.Fatalf("a callback after handoff cancelled the child: %v", err)
	}
	if err := outcome.unavailable(SiteSingleBegin, nil); err != nil {
		t.Fatalf("unavailable after a clean handoff = %v, want nil", err)
	}
	step.release()
	if !errors.Is(step.ctx.Err(), context.Canceled) {
		t.Fatal("release did not cancel the child")
	}
}

// The due time passed, but the callback has not run yet. Finish must do the
// same expiry transition itself, so a delayed callback cannot hand out an
// overdue result (D3's elapsed-time check).
func TestStepDelayedCallbackPastDueExpiresAtFinish(t *testing.T) {
	steps := newFakeSteps()
	step := startBoundedStep(context.Background(), preDecisionStepBound, steps)
	steps.advance(preDecisionStepBound)
	outcome := step.finish()
	if !outcome.expired {
		t.Fatal("finish after the due time did not select expiry")
	}
	if !errors.Is(step.ctx.Err(), context.Canceled) {
		t.Fatal("finish after the due time did not cancel the child")
	}
	requireUnavailable(t, outcome.unavailable(SitePassBegin, nil), SitePassBegin, ClassStepExpired)
	steps.last(t).fire() // the late callback is a no-op
}

// Just inside the bound: no expiry. Pins the boundary that the test above
// crosses, so a finish that always expires cannot pass both.
func TestStepFinishInsideTheBoundIsNotExpiry(t *testing.T) {
	steps := newFakeSteps()
	step := startBoundedStep(context.Background(), preDecisionStepBound, steps)
	steps.advance(preDecisionStepBound - time.Millisecond)
	if outcome := step.finish(); outcome.expired {
		t.Fatal("finish inside the bound selected expiry")
	}
	if err := step.ctx.Err(); err != nil {
		t.Fatalf("child cancelled inside the bound: %v", err)
	}
	step.release()
}

// Identical driver errors, opposite timer state: only the server-owned flag
// selects the class. The driver spelling never does (D27).
func TestStepSameErrorDependsOnlyOnTimerState(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.New("read tcp: i/o timeout")} {
		t.Run(cause.Error(), func(t *testing.T) {
			fired := startBoundedStep(context.Background(), preDecisionStepBound, newFakeSteps())
			fired.expire()
			requireUnavailable(t, fired.finish().unavailable(SitePolicyRead, cause), SitePolicyRead, ClassStepExpired)

			quiet := startBoundedStep(context.Background(), preDecisionStepBound, newFakeSteps())
			defer quiet.release()
			if err := quiet.finish().unavailable(SitePolicyRead, cause); err != nil {
				t.Fatalf("no timer, live parent, cause %v: got %v, want no availability class", cause, err)
			}
		})
	}
}

// D39 expiry wins over a late success, a late no-row (unknown or revoked
// device) read and a late unsupported error while the parent is alive.
func TestStepExpiryWinsOverLateResults(t *testing.T) {
	for name, cause := range map[string]error{
		"success":     nil,
		"no row":      sql.ErrNoRows,
		"unsupported": errors.New("unsupported driver failure"),
	} {
		t.Run(name, func(t *testing.T) {
			steps := newFakeSteps()
			step := startBoundedStep(context.Background(), preDecisionStepBound, steps)
			steps.last(t).fire()
			requireUnavailable(t, step.finish().unavailable(SiteDeviceLookup, cause), SiteDeviceLookup, ClassStepExpired)
		})
	}
}

// A parent deadline before or during the step is pre-decision 503, read from
// parent.Err(), never from the driver's spelling.
func TestStepParentDeadlineIsPreDecision(t *testing.T) {
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	step := startBoundedStep(parent, preDecisionStepBound, newFakeSteps())
	requireUnavailable(t, step.finish().unavailable(SiteSingleBegin, errors.New("driver text")), SiteSingleBegin, ClassParentDeadline)
}

// A cancelled parent is 500, and it takes precedence over a step-expiry flag.
func TestStepParentCancelBeatsExpiry(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	steps := newFakeSteps()
	step := startBoundedStep(parent, preDecisionStepBound, steps)
	steps.last(t).fire()
	cancel()
	if err := step.finish().unavailable(SitePolicyRead, context.Canceled); err != nil {
		t.Fatalf("parent cancelled with the step flag set: got %v, want no availability class", err)
	}
}

// After handoff the parent still reaches the child: a client that goes away
// must still cancel the transaction (no detached context).
func TestStepParentCancellationReachesTheChildAfterHandoff(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	step := startBoundedStep(parent, preDecisionStepBound, newFakeSteps())
	if outcome := step.finish(); outcome.expired {
		t.Fatal("unexpected expiry")
	}
	cancel()
	if !errors.Is(step.ctx.Err(), context.Canceled) {
		t.Fatal("parent cancellation after handoff did not reach the child context")
	}
}

// The real clock and real goroutines, with the callback and finish racing.
// Run with -race. Whatever wins, the reported state must match the child.
func TestStepConcurrentCallbackAndFinishAgree(t *testing.T) {
	for i := 0; i < 400; i++ {
		bound := time.Duration(i%7) * 50 * time.Microsecond
		step := startBoundedStep(context.Background(), bound, realSteps{})
		if i%2 == 0 {
			time.Sleep(time.Duration(i%5) * 20 * time.Microsecond)
		}
		outcome := step.finish()
		cancelled := step.ctx.Err() != nil
		if outcome.expired != cancelled {
			t.Fatalf("run %d: expired=%v but child cancelled=%v", i, outcome.expired, cancelled)
		}
		step.release()
	}
}

// --- classifier -------------------------------------------------------------

var errUnsupportedLeaf = errors.New("test-only unsupported leaf")

// closedPort returns an IPv4 loopback port that this test owned and closed.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// connectConfig is a real pgconn config. The ConnectError it produces is
// built by pgconn, never by hand.
func connectConfig(t *testing.T, host, port string) *pgconn.Config {
	t.Helper()
	cfg, err := pgconn.ParseConfig("postgres://access:unused@" + net.JoinHostPort(host, port) + "/access?sslmode=disable&connect_timeout=5")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func connect(t *testing.T, cfg *pgconn.Config) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err == nil {
		_ = conn.Close(ctx)
		t.Fatal("connection unexpectedly succeeded")
	}
	var ce *pgconn.ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("connect error %v (%T) carries no ConnectError", err, err)
	}
	return err
}

// fatalServer is the labelled pgproto3 UNIT SHAPE control: a local listener
// that answers every startup with one FATAL error. It is a wire simulation and
// is no outage evidence; the gate's real producers are in the smoke tests.
func fatalServer(t *testing.T, code string) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				backend := pgproto3.NewBackend(conn, conn)
				if _, err := backend.ReceiveStartupMessage(); err != nil {
					return
				}
				backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: code, Message: "unit shape control"})
				_ = backend.Flush()
			}(conn)
		}
	}()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// cutServer accepts and closes every connection during startup, with a FIN or,
// when reset is set, with an RST.
func cutServer(t *testing.T, reset bool) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 512)
			_, _ = conn.Read(buf)
			if reset {
				_ = conn.(*net.TCPConn).SetLinger(0)
			}
			_ = conn.Close()
		}
	}()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// withSibling makes pgconn try two endpoints and join both attempt errors
// under one real ConnectError. The second endpoint's dial returns
// errUnsupportedLeaf. Swap puts the unsupported attempt first.
func withSibling(t *testing.T, cfg *pgconn.Config, swap bool) {
	t.Helper()
	supportedHost, supportedPort := cfg.Host, cfg.Port
	unsupported := &pgconn.FallbackConfig{Host: "127.0.0.9", Port: 9}
	if swap {
		cfg.Fallbacks = []*pgconn.FallbackConfig{{Host: supportedHost, Port: supportedPort, TLSConfig: nil}}
		cfg.Host, cfg.Port = unsupported.Host, unsupported.Port
	} else {
		cfg.Fallbacks = []*pgconn.FallbackConfig{unsupported}
	}
	dial := cfg.DialFunc
	cfg.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == net.JoinHostPort("127.0.0.9", "9") {
			return nil, errUnsupportedLeaf
		}
		return dial(ctx, network, address)
	}
}

func TestConnectRefusalLeafIsAvailability(t *testing.T) {
	err := connect(t, connectConfig(t, "127.0.0.1", closedPort(t)))
	for _, site := range allSites {
		class, ok := classifyAvailability(site, err)
		if !ok || class != ClassConnectRefused {
			t.Fatalf("%s: class=%q ok=%v, want connect_refused for %v", site, class, ok, err)
		}
	}
}

func TestConnectSupportedLeafBesideAnUnsupportedLeafIsRejected(t *testing.T) {
	producers := map[string]func(t *testing.T) *pgconn.Config{
		"refused": func(t *testing.T) *pgconn.Config { return connectConfig(t, "127.0.0.1", closedPort(t)) },
		"57P03 (pgproto3 unit shape)": func(t *testing.T) *pgconn.Config {
			return connectConfig(t, "127.0.0.1", fatalServer(t, "57P03"))
		},
		"53300 (pgproto3 unit shape)": func(t *testing.T) *pgconn.Config {
			return connectConfig(t, "127.0.0.1", fatalServer(t, "53300"))
		},
	}
	for name, produce := range producers {
		for _, swap := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s swap=%v", name, swap), func(t *testing.T) {
				alone := connect(t, produce(t))
				if _, ok := classifyAvailability(SiteDeviceLookup, alone); !ok {
					t.Fatalf("the supported leaf alone was rejected: %v", alone)
				}
				cfg := produce(t)
				withSibling(t, cfg, swap)
				err := connect(t, cfg)
				if !errors.Is(err, errUnsupportedLeaf) {
					t.Fatalf("fixture did not join the unsupported attempt: %v", err)
				}
				for _, site := range allSites {
					if class, ok := classifyAvailability(site, err); ok {
						t.Fatalf("%s: mixed supported/unsupported join accepted as %s: %v", site, class, err)
					}
				}
			})
		}
	}
}

// The lookup form: a DNS error joined directly below ConnectError, with no
// per-attempt wrapper. Built here from a LookupFunc that returns a DNS error;
// the real resolver run is in the smoke matrix.
func TestConnectLookupFormDNSIsAvailabilityAndAnUnsupportedSiblingRejects(t *testing.T) {
	notFound := func(_ context.Context, host string) ([]string, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	cfg := connectConfig(t, "lookup-a.invalid", "5432")
	cfg.LookupFunc = notFound
	err := connect(t, cfg)
	if class, ok := classifyAvailability(SitePolicyRead, err); !ok || class != ClassConnectDNS {
		t.Fatalf("lookup-form DNS: class=%q ok=%v, want connect_dns: %v", class, ok, err)
	}
	for _, swap := range []bool{false, true} {
		cfg := connectConfig(t, "lookup-a.invalid", "5432")
		cfg.Fallbacks = []*pgconn.FallbackConfig{{Host: "lookup-b.invalid", Port: 5432}}
		cfg.LookupFunc = func(ctx context.Context, host string) ([]string, error) {
			if (host == "lookup-b.invalid") != swap {
				return nil, errUnsupportedLeaf
			}
			return notFound(ctx, host)
		}
		err := connect(t, cfg)
		if !errors.Is(err, errUnsupportedLeaf) {
			t.Fatalf("swap=%v: fixture did not join the unsupported lookup: %v", swap, err)
		}
		if class, ok := classifyAvailability(SitePolicyRead, err); ok {
			t.Fatalf("swap=%v: DNS beside an unsupported lookup accepted as %s", swap, class)
		}
	}
}

// A supported refusal attempt beside a supported DNS attempt is one mixed,
// fully supported set.
func TestConnectMixedSupportedLeavesAreConnectMixed(t *testing.T) {
	cfg := connectConfig(t, "127.0.0.1", closedPort(t))
	cfg.Fallbacks = []*pgconn.FallbackConfig{{Host: "127.0.0.9", Port: 9}}
	dial := cfg.DialFunc
	cfg.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "127.0.0.9:9" {
			return nil, &net.OpError{Op: "dial", Net: network, Err: &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}}
		}
		return dial(ctx, network, address)
	}
	err := connect(t, cfg)
	if class, ok := classifyAvailability(SiteSingleBegin, err); !ok || class != ClassConnectMixed {
		t.Fatalf("class=%q ok=%v, want connect_mixed: %v", class, ok, err)
	}
}

// Reset or EOF during startup is inside ConnectError, so the post-connect
// classes cannot rescue it, even at device/policy where they are proved.
func TestStartupResetOrEOFStaysUnsupported(t *testing.T) {
	for _, reset := range []bool{false, true} {
		err := connect(t, connectConfig(t, "127.0.0.1", cutServer(t, reset)))
		for _, site := range allSites {
			if class, ok := classifyAvailability(site, err); ok {
				t.Fatalf("reset=%v %s: startup cut accepted as %s: %v", reset, site, class, err)
			}
		}
	}
}

// An accepted node ends its branch even when it has Unwrap, and a supported
// node still cannot bless an unsupported sibling.
func TestConnectLeafWalkStopsAtAnAcceptedNode(t *testing.T) {
	dns := &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true, UnwrapErr: errUnsupportedLeaf}
	if errors.Unwrap(dns) == nil {
		t.Fatal("fixture: the DNS error must have a non-nil Unwrap")
	}
	if classes, ok := connectLeaves(dns); !ok || len(classes) != 1 || classes[0] != ClassConnectDNS {
		t.Fatalf("DNS node with UnwrapErr: classes=%v ok=%v, want one connect_dns", classes, ok)
	}
	if _, ok := connectLeaves(errors.Join(dns, errUnsupportedLeaf)); ok {
		t.Fatal("accepted DNS node joined with an unsupported sibling was accepted")
	}
	if _, ok := connectLeaves(errors.Join(errUnsupportedLeaf, dns)); ok {
		t.Fatal("unsupported sibling joined before an accepted DNS node was accepted")
	}
	if _, ok := connectLeaves(errors.Join()); ok {
		t.Fatal("an empty set was accepted")
	}
	if _, ok := connectLeaves(nil); ok {
		t.Fatal("nil was accepted")
	}
	nested := fmt.Errorf("hostname resolving error: %w", errors.Join(dns, fmt.Errorf("attempt: %w", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)})))
	if classes, ok := connectLeaves(nested); !ok || len(classes) != 2 {
		t.Fatalf("nested supported set: classes=%v ok=%v", classes, ok)
	}
}

var allSites = []AvailabilitySite{SiteDeviceLookup, SitePolicyRead, SiteSingleBegin, SitePassBegin}

// The proved post-connect pairs (D22) and nothing more: no cross product.
func TestPostConnectClassesOnlyAtTheirProvedSites(t *testing.T) {
	type pair struct {
		err   error
		class AvailabilityClass
		sites map[AvailabilitySite]bool
	}
	reads := map[AvailabilitySite]bool{SiteDeviceLookup: true, SitePolicyRead: true}
	begins := map[AvailabilitySite]bool{SiteSingleBegin: true, SitePassBegin: true}
	every := map[AvailabilitySite]bool{SiteDeviceLookup: true, SitePolicyRead: true, SiteSingleBegin: true, SitePassBegin: true}
	for name, p := range map[string]pair{
		"57P01":         {&pgconn.PgError{Severity: "FATAL", Code: "57P01"}, ClassBackendTerminated, every},
		"unexpectedEOF": {fmt.Errorf("authenticate scanner device: %w", io.ErrUnexpectedEOF), ClassUnexpectedEOF, reads},
		"connClosed":    {fmt.Errorf("lock: %w", pgconn.ErrConnClosed), ClassConnClosed, begins},
		"econnreset":    {&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, ClassConnReset, reads},
	} {
		for _, site := range allSites {
			class, ok := classifyAvailability(site, p.err)
			if p.sites[site] && (!ok || class != p.class) {
				t.Errorf("%s at %s: class=%q ok=%v, want %s", name, site, class, ok, p.class)
			}
			if !p.sites[site] && ok {
				t.Errorf("%s at %s: accepted as %s, but no real producer proved this pair", name, site, class)
			}
		}
	}
}

func TestUnsupportedErrorsStayUnclassified(t *testing.T) {
	closed := sql.OpenDB(fakeConnector{})
	_ = closed.Close()
	closedErr := closed.PingContext(context.Background())
	if closedErr == nil {
		t.Fatal("fixture: a closed pool answered a ping")
	}
	for name, err := range map[string]error{
		"bare 57P03 statement":          &pgconn.PgError{Code: "57P03"},
		"bare 53300 statement":          &pgconn.PgError{Code: "53300"},
		"io.EOF":                        io.EOF,
		"EPIPE":                         &net.OpError{Op: "write", Err: os.NewSyscallError("write", syscall.EPIPE)},
		"driver.ErrBadConn":             driver.ErrBadConn,
		"closed pool":                   closedErr,
		"bare Canceled":                 context.Canceled,
		"bare DeadlineExceeded":         context.DeadlineExceeded,
		"dial timeout":                  &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded},
		"no rows":                       sql.ErrNoRows,
		"unknown SQL":                   &pgconn.PgError{Code: "42P01"},
		"57P01 beside io.EOF":           errors.Join(&pgconn.PgError{Code: "57P01"}, io.EOF),
		"io.EOF beside 57P01":           errors.Join(io.EOF, &pgconn.PgError{Code: "57P01"}),
		"empty join":                    errors.Join(),
		"wrapper over nothing":          fmt.Errorf("plain text only"),
		"refused outside ConnectError":  &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
		"DNS outside ConnectError":      &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true},
		"wrapped bare 57P03 statement":  fmt.Errorf("policy: %w", &pgconn.PgError{Code: "57P03"}),
		"wrapped bare 53300 statement":  fmt.Errorf("begin: %w", &pgconn.PgError{Code: "53300"}),
		"unexpectedEOF beside unknown":  errors.Join(io.ErrUnexpectedEOF, errUnsupportedLeaf),
		"a post-connect and a nil leaf": errors.Join(io.ErrUnexpectedEOF, fmt.Errorf("%w", error(nil))),
	} {
		for _, site := range allSites {
			if class, ok := classifyAvailability(site, err); ok {
				t.Errorf("%s at %s: accepted as %s", name, site, class)
			}
		}
	}
	if class, ok := classifyAvailability(SitePassBegin, errors.Join(&pgconn.PgError{Code: "57P01"}, &pgconn.PgError{Code: "57P01"})); !ok || class != ClassBackendTerminated {
		t.Fatalf("a join whose branches all qualify: class=%q ok=%v", class, ok)
	}
}

// A rejected connect subtree beside a qualifying post-connect branch: the
// post-connect match must not rescue it.
func TestPostConnectBranchCannotRescueARejectedConnectSubtree(t *testing.T) {
	cfg := connectConfig(t, "127.0.0.1", closedPort(t))
	withSibling(t, cfg, false)
	rejected := connect(t, cfg)
	for _, site := range allSites {
		if class, ok := classifyAvailability(site, errors.Join(rejected, &pgconn.PgError{Code: "57P01"})); ok {
			t.Fatalf("%s: rejected connect subtree rescued as %s", site, class)
		}
	}
}

// --- device and policy reads through a fake driver --------------------------

// fakeConnector is a database/sql driver for unit tests. Each query runs
// onQuery, which can move the fake step clock and choose the result.
type fakeConnector struct {
	onQuery func(query string) error
	onBegin func() error
	log     *fakeLog
}

type fakeLog struct {
	mu        sync.Mutex
	queries   []string
	begins    int
	rollbacks int
	commits   int
	beginCtx  context.Context
}

func (l *fakeLog) add(q string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queries = append(l.queries, q)
}

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) { return &fakeConn{c: c}, nil }
func (c fakeConnector) Driver() driver.Driver                        { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use the connector") }

type fakeConn struct{ c fakeConnector }

func (f *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (f *fakeConn) Close() error                        { return nil }
func (f *fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("use BeginTx") }

func (f *fakeConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if f.c.log != nil {
		f.c.log.mu.Lock()
		f.c.log.begins++
		f.c.log.beginCtx = ctx
		f.c.log.mu.Unlock()
	}
	if f.c.onBegin != nil {
		if err := f.c.onBegin(); err != nil {
			return nil, err
		}
	}
	return &fakeTx{log: f.c.log}, nil
}

func (f *fakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	f.c.log.add(query)
	if f.c.onQuery != nil {
		if err := f.c.onQuery(query); err != nil {
			return nil, err
		}
	}
	return emptyRows{}, nil
}

func (f *fakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	f.c.log.add(query)
	return driver.RowsAffected(0), nil
}

type fakeTx struct{ log *fakeLog }

func (t *fakeTx) Commit() error {
	if t.log != nil {
		t.log.mu.Lock()
		t.log.commits++
		t.log.mu.Unlock()
	}
	return nil
}

func (t *fakeTx) Rollback() error {
	if t.log != nil {
		t.log.mu.Lock()
		t.log.rollbacks++
		t.log.mu.Unlock()
	}
	return nil
}

type emptyRows struct{}

func (emptyRows) Columns() []string              { return []string{"a", "b", "c", "d"} }
func (emptyRows) Close() error                   { return nil }
func (emptyRows) Next(dest []driver.Value) error { return io.EOF }

func fakeStore(t *testing.T, steps *fakeSteps, c fakeConnector) *Postgres {
	t.Helper()
	db := sql.OpenDB(c)
	t.Cleanup(func() { _ = db.Close() })
	p := New(db, Config{})
	p.steps = steps
	return p
}

// A late revoked or unknown device read (no row) with a live parent and a
// passed due time is the D39 expiry class, decided BEFORE the no-row read can
// become the 401 refusal.
func TestDeviceLookupLateNoRowReadIsExpiry(t *testing.T) {
	steps := newFakeSteps()
	st := fakeStore(t, steps, fakeConnector{onQuery: func(string) error {
		steps.advance(preDecisionStepBound + time.Millisecond)
		return nil
	}})
	_, err := st.AuthenticateScannerDevice(context.Background(), "revoked-device-token")
	requireUnavailable(t, err, SiteDeviceLookup, ClassStepExpired)
	if errors.Is(err, ErrScannerDeviceUnknown) {
		t.Fatal("expiry also reads as the unknown-device refusal")
	}
}

// A late unsupported error with a live parent and a passed due time is the
// D39 expiry class too, decided before it can become a 500.
func TestDeviceLookupLateUnsupportedErrorIsExpiry(t *testing.T) {
	steps := newFakeSteps()
	st := fakeStore(t, steps, fakeConnector{onQuery: func(string) error {
		steps.advance(preDecisionStepBound + time.Millisecond)
		return errUnsupportedLeaf
	}})
	_, err := st.AuthenticateScannerDevice(context.Background(), "device-token")
	requireUnavailable(t, err, SiteDeviceLookup, ClassStepExpired)
	if avail := unavailableOf(t, err); !errors.Is(avail, errUnsupportedLeaf) {
		t.Fatalf("cause = %v, want the original error kept for logs", avail.Cause)
	}
}

// The same reads inside the bound keep their ordinary meaning.
func TestDeviceLookupInsideTheBoundKeepsItsOrdinaryResults(t *testing.T) {
	st := fakeStore(t, newFakeSteps(), fakeConnector{})
	if _, err := st.AuthenticateScannerDevice(context.Background(), "unknown-token"); !errors.Is(err, ErrScannerDeviceUnknown) {
		t.Fatalf("no-row read inside the bound = %v, want ErrScannerDeviceUnknown", err)
	}
	st = fakeStore(t, newFakeSteps(), fakeConnector{onQuery: func(string) error { return errUnsupportedLeaf }})
	_, err := st.AuthenticateScannerDevice(context.Background(), "device-token")
	if !errors.Is(err, errUnsupportedLeaf) {
		t.Fatalf("unsupported error inside the bound = %v, want it wrapped", err)
	}
	requireNotUnavailable(t, err)
	// A blank token is refused before any statement or step.
	steps := newFakeSteps()
	st = fakeStore(t, steps, fakeConnector{})
	if _, err := st.AuthenticateScannerDevice(context.Background(), "  "); !errors.Is(err, ErrScannerDeviceUnknown) {
		t.Fatalf("blank token = %v, want ErrScannerDeviceUnknown", err)
	}
	if steps.count() != 0 {
		t.Fatal("a blank token armed a step timer")
	}
}

// The live policy read expires before slotPolicy's no-row single fallback is
// accepted, and before any acquisition.
func TestLivePolicyExpiryBeatsTheSingleFallback(t *testing.T) {
	steps := newFakeSteps()
	log := &fakeLog{}
	st := fakeStore(t, steps, fakeConnector{log: log, onQuery: func(string) error {
		steps.advance(preDecisionStepBound + time.Millisecond)
		return nil
	}})
	_, err := st.Scan(context.Background(), ScanInput{RedeemInput: RedeemInput{TicketID: uuid.New(), OrderID: uuid.New(), OrganizerID: uuid.New(), SlotID: uuid.New()}})
	requireUnavailable(t, err, SitePolicyRead, ClassStepExpired)
	if log.begins != 0 {
		t.Fatalf("an expired policy read still acquired a transaction (%d begins)", log.begins)
	}
}

// An acquisition whose step expired rolls back the returned transaction and
// starts no ticket query.
func TestBeginExpiryRollsBackAndStartsNoTicketQuery(t *testing.T) {
	steps := newFakeSteps()
	log := &fakeLog{}
	st := fakeStore(t, steps, fakeConnector{log: log, onBegin: func() error {
		steps.advance(preDecisionStepBound + time.Millisecond)
		return nil
	}})
	_, err := st.Scan(context.Background(), ScanInput{RedeemInput: RedeemInput{TicketID: uuid.New(), OrderID: uuid.New(), OrganizerID: uuid.New(), SlotID: uuid.New()}})
	requireUnavailable(t, err, SiteSingleBegin, ClassStepExpired)
	// database/sql's own context watcher can do the rollback on another
	// goroutine after the cancelled child, so wait for it with a bound.
	deadline := time.Now().Add(2 * time.Second)
	for {
		log.mu.Lock()
		rollbacks := log.rollbacks
		log.mu.Unlock()
		if rollbacks == 1 {
			break
		}
		if rollbacks > 1 || time.Now().After(deadline) {
			t.Fatalf("rollbacks = %d, want the returned transaction rolled back once", rollbacks)
		}
		time.Sleep(time.Millisecond)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, q := range log.queries {
		if q != `SELECT mode,max_entries,requires_exit FROM slot_re_entry_policies WHERE slot_id=$1` {
			t.Fatalf("query after an expired acquisition: %q", q)
		}
	}
}

// After a clean handoff the transaction context stays alive until release:
// the helper must not cancel the child on return.
func TestBeginHandoffKeepsTheTransactionContextUntilRelease(t *testing.T) {
	log := &fakeLog{}
	st := fakeStore(t, newFakeSteps(), fakeConnector{log: log})
	tx, release, err := st.beginLive(context.Background(), SitePassBegin)
	if err != nil {
		t.Fatal(err)
	}
	log.mu.Lock()
	txCtx := log.beginCtx
	log.mu.Unlock()
	if txCtx == nil || txCtx.Err() != nil {
		t.Fatalf("transaction context after handoff = %v, want alive", txCtx)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	release()
	if txCtx.Err() == nil {
		t.Fatal("release did not end the transaction context")
	}
}
