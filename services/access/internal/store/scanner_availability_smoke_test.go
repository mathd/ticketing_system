//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"ticketing/services/access/internal/testdb"
)

// The TKT-557 real-driver ledger (ADR-085 §8 ticket 0, D22/D27). Every case
// uses real pgx through the shared testdb fixture, and a real producer: the
// real resolver, real sockets, a real role limit, real backend termination
// and the recovery-held PostgreSQL that scripts/smoke.sh starts. These tests
// prove the store's application type, site, class and original cause. The
// HTTP status and body are proved separately at the Router tier.

const (
	policySQL     = "FROM slot_re_entry_policies"
	ticketLockSQL = "FROM tickets WHERE id=$1 FOR UPDATE"
	deviceSQL     = "FROM scanner_devices"
)

// latency lines are collected for the TKT-557 verification report.
func logLatency(t *testing.T, site AvailabilitySite, class string, elapsed time.Duration) {
	t.Helper()
	t.Logf("TKT557-LATENCY site=%s class=%s ms=%.1f", site, class, float64(elapsed.Microseconds())/1000)
}

type scannerHarness struct {
	f      *testdb.Fixture
	st     *Postgres // the faultable application pool
	seed   *Postgres // the healthy admin handle
	token  string
	device ScannerDevice
	single seeded
	pass   seeded
}

func newScannerHarness(t *testing.T, ctx context.Context, opts testdb.Options) *scannerHarness {
	t.Helper()
	opts.Migrate = Migrate
	f := testdb.New(t, ctx, opts)
	cfg := testConfig(t)
	h := &scannerHarness{f: f, st: New(f.DB, cfg), seed: New(f.Admin, cfg)}
	device, token, err := h.seed.EnrolScannerDevice(ctx, uuid.New(), "gate")
	if err != nil {
		t.Fatal(err)
	}
	h.device, h.token = device, token
	h.single = issueTicket(t, ctx, h.seed, uuid.New())
	h.pass = issueAndSeed(t, ctx, h.seed, ReEntryPolicy{Mode: "multi"})
	return h
}

// call runs the store operation whose first statement is the site's own.
func (h *scannerHarness) call(ctx context.Context, site AvailabilitySite) error {
	switch site {
	case SiteDeviceLookup:
		_, err := h.st.AuthenticateScannerDevice(ctx, h.token)
		return err
	case SitePolicyRead, SiteSingleBegin:
		_, err := h.st.Scan(ctx, ScanInput{RedeemInput: h.single.redeemInput()})
		return err
	case SitePassBegin:
		_, err := h.st.Scan(ctx, scanInput(h.pass, uuid.New(), AdmissionEntry, time.Now().UTC()))
		return err
	}
	panic("unknown site " + string(site))
}

// armAt arms a cold fault at the site: before the call for the reads, and at
// the post-policy barrier for the acquisitions, so the policy SELECT has
// already succeeded on a healthy connection.
func (h *scannerHarness) armAt(site AvailabilitySite, fault testdb.Fault) {
	switch site {
	case SiteDeviceLookup, SitePolicyRead:
		h.f.Arm(fault)
	default:
		h.f.ArmAfter(policySQL, fault)
	}
}

// faultAt runs one cold fault at one site under an independent outer bound.
func (h *scannerHarness) faultAt(t *testing.T, site AvailabilitySite, fault testdb.Fault) (time.Duration, error) {
	t.Helper()
	h.armAt(site, fault)
	defer h.f.Disarm()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := h.call(ctx, site)
	return time.Since(start), err
}

// requireNoDecision proves nothing past the site ran: for an acquisition the
// policy SELECT succeeded first, and no ticket lock started at any site. The
// healthy admin handle reads back that no admission was written.
func (h *scannerHarness) requireNoDecision(t *testing.T, ctx context.Context, site AvailabilitySite) {
	t.Helper()
	if n := h.f.CountQueries(ticketLockSQL); n != 0 {
		t.Fatalf("%s: %d ticket decision statements ran", site, n)
	}
	if site == SiteSingleBegin || site == SitePassBegin {
		ok := false
		for _, q := range h.f.Queries() {
			if strings.Contains(q.SQL, policySQL) && q.Done && q.Err == nil {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("%s: the policy SELECT did not succeed before the acquisition fault", site)
		}
	}
	for _, s := range []seeded{h.single, h.pass} {
		if n := countRows(t, ctx, h.f.Admin, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type IN ('redeemed','entry','exit')`, s.ticketID); n != 0 {
			t.Fatalf("%s: %d admission events were written", site, n)
		}
	}
}

// requireClass fails unless err carries the named type with this site and
// class.
func requireClass(t *testing.T, err error, site AvailabilitySite, class AvailabilityClass) {
	t.Helper()
	var avail *PreDecisionUnavailable
	if !errors.As(err, &avail) {
		t.Fatalf("%s: error is not *PreDecisionUnavailable:\n%s", site, testdb.ErrorTree(err))
	}
	if avail.Site != site || avail.Class != class {
		t.Fatalf("%s: got %s/%s, want %s/%s:\n%s", site, avail.Site, avail.Class, site, class, testdb.ErrorTree(err))
	}
}

func requireNoClass(t *testing.T, err error, site AvailabilitySite) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: the call succeeded, want a failure", site)
	}
	var avail *PreDecisionUnavailable
	if errors.As(err, &avail) {
		t.Fatalf("%s: unsupported failure classified as %s/%s:\n%s", site, avail.Site, avail.Class, testdb.ErrorTree(err))
	}
}

// connectCauseOf returns the real ConnectError inside err, or fails.
func connectCauseOf(t *testing.T, err error) *pgconn.ConnectError {
	t.Helper()
	var ce *pgconn.ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("no real ConnectError in the cause:\n%s", testdb.ErrorTree(err))
	}
	return ce
}

func requireConnectCause(t *testing.T, err error) {
	t.Helper()
	_ = connectCauseOf(t, err)
}

var everySite = []AvailabilitySite{SiteDeviceLookup, SitePolicyRead, SiteSingleBegin, SitePassBegin}

// typeName is the dynamic type of one node. The shape tests compare it to
// name the exact real producer tree, not to classify.
func typeName(err error) string { return fmt.Sprintf("%T", err) }

// The real resolver on a unique .invalid name, from LookupFunc: the lookup
// tree has a DNS join directly below the ConnectError and no per-attempt
// wrapper. A classifier that accepts only per-attempt DNS fails here.
func TestRealConnectLookupDNSAtEverySite(t *testing.T) {
	for _, site := range everySite {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			elapsed, err := h.faultAt(t, site, testdb.FaultDNSLookup)
			requireClass(t, err, site, ClassConnectDNS)
			avail := err
			ce := connectCauseOf(t, avail)
			tree := testdb.ErrorTree(ce)
			if strings.Contains(tree, "perDialConnectError") {
				t.Fatalf("lookup form carries a per-attempt wrapper:\n%s", tree)
			}
			var join interface{ Unwrap() []error }
			if !errors.As(ce.Unwrap(), &join) || len(join.Unwrap()) == 0 {
				t.Fatalf("lookup form has no DNS join:\n%s", tree)
			}
			for _, child := range join.Unwrap() {
				if typeName(child) != "*net.DNSError" {
					t.Fatalf("lookup join child is %s, want *net.DNSError directly:\n%s", typeName(child), tree)
				}
			}
			h.requireNoDecision(t, ctx, site)
			if s := h.f.Stats(); s.Faulted != 1 {
				t.Fatalf("faulted connector attempts = %d, want 1 (%+v)", s.Faulted, s)
			}
			logLatency(t, site, string(ClassConnectDNS)+"/lookup", elapsed)
		})
	}
}

// The real dialer on the same kind of name, from DialFunc: the distinct
// per-attempt tree through perDialConnectError and net.OpError.
func TestRealConnectAttemptDNSAtEverySite(t *testing.T) {
	for _, site := range everySite {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			elapsed, err := h.faultAt(t, site, testdb.FaultDNSAttempt)
			requireClass(t, err, site, ClassConnectDNS)
			avail := err
			tree := testdb.ErrorTree(connectCauseOf(t, avail))
			var op *net.OpError
			var dns *net.DNSError
			if !strings.Contains(tree, "perDialConnectError") || !errors.As(avail, &op) || !errors.As(avail, &dns) {
				t.Fatalf("not the per-attempt OpError/DNSError tree:\n%s", tree)
			}
			h.requireNoDecision(t, ctx, site)
			logLatency(t, site, string(ClassConnectDNS)+"/attempt", elapsed)
		})
	}
}

func TestRealConnectRefusedAtEverySite(t *testing.T) {
	for _, site := range everySite {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			elapsed, err := h.faultAt(t, site, testdb.FaultRefused)
			requireClass(t, err, site, ClassConnectRefused)
			avail := err
			requireConnectCause(t, avail)
			if !errors.Is(avail, syscall.ECONNREFUSED) {
				t.Fatalf("the syscall cause was not kept:\n%s", testdb.ErrorTree(avail))
			}
			h.requireNoDecision(t, ctx, site)
			logLatency(t, site, string(ClassConnectRefused), elapsed)
		})
	}
}

// The recovery-held PostgreSQL (D28): a real server rejects the startup with
// SQLSTATE 57P03 before the step timer fires. No wire simulation can stand in
// for this case.
func TestRealRecoveryStartupAtEverySite(t *testing.T) {
	for _, site := range everySite {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			testdb.RequireRecoveryRejects(t, ctx)
			elapsed, err := h.faultAt(t, site, testdb.FaultRecovery)
			requireClass(t, err, site, ClassConnectStartup)
			avail := err
			requireConnectCause(t, avail)
			var pgErr *pgconn.PgError
			if !errors.As(avail, &pgErr) || pgErr.Code != "57P03" {
				t.Fatalf("no real SQLSTATE 57P03 in the cause:\n%s", testdb.ErrorTree(avail))
			}
			if elapsed >= preDecisionStepBound {
				t.Fatalf("57P03 took %s: the step timer, not the server, may have decided", elapsed)
			}
			h.requireNoDecision(t, ctx, site)
			logLatency(t, site, string(ClassConnectStartup), elapsed)
		})
	}
}

// Through "localhost", an IPv6 refusal attempt can sit beside the IPv4 57P03
// attempt. Both leaves are supported, so the join is one supported set.
func TestRealRecoveryThroughLocalhostIsOneSupportedSet(t *testing.T) {
	for _, site := range []AvailabilitySite{SiteDeviceLookup, SiteSingleBegin} {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			testdb.RequireRecoveryRejects(t, ctx)
			addrs, err := net.DefaultResolver.LookupHost(ctx, "localhost")
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.faultAt(t, site, testdb.FaultRecoveryLocalhost)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "57P03" {
				t.Fatalf("no real 57P03 attempt:\n%s", testdb.ErrorTree(err))
			}
			want := ClassConnectStartup
			if len(addrs) > 1 {
				// The extra attempt is the IPv6 loopback, where nothing listens.
				if !errors.Is(err, syscall.ECONNREFUSED) {
					t.Fatalf("localhost resolves to %v, but the second attempt is not a refusal:\n%s", addrs, testdb.ErrorTree(err))
				}
				want = ClassConnectMixed
			}
			requireClass(t, err, site, want)
			t.Logf("localhost resolves to %v; class %s", addrs, want)
		})
	}
}

// Real producers in ONE connect attempt set, under the actual outer
// ConnectError: a supported set is one mixed class; a supported lookup-form
// branch and a real 57P03 beside an unsupported independent dial timeout stay
// unsupported, in either order. A supported child cannot bless a sibling.
func TestRealMixedConnectLeavesUnderOneConnectError(t *testing.T) {
	for _, site := range everySite {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			testdb.RequireRecoveryRejects(t, ctx)

			_, err := h.faultAt(t, site, testdb.FaultMixedSupported)
			requireClass(t, err, site, ClassConnectMixed)
			avail := err
			var pgErr *pgconn.PgError
			var dns *net.DNSError
			if !errors.As(avail, &pgErr) || pgErr.Code != "57P03" || !errors.As(avail, &dns) || !errors.Is(avail, syscall.ECONNREFUSED) {
				t.Fatalf("supported set lacks a real leaf:\n%s", testdb.ErrorTree(avail))
			}

			for _, fault := range []testdb.Fault{testdb.FaultMixedUnsupported, testdb.FaultMixedUnsupportedFirst} {
				elapsed, err := h.faultAt(t, site, fault)
				requireNoClass(t, err, site)
				requireConnectCause(t, err)
				if !errors.As(err, &pgErr) || pgErr.Code != "57P03" || !errors.As(err, &dns) {
					t.Fatalf("%s: the supported leaves are missing, so the rejection proves nothing:\n%s", fault, testdb.ErrorTree(err))
				}
				if elapsed >= preDecisionStepBound {
					t.Fatalf("%s: took %s, so the step timer decided, not the leaf rule", fault, elapsed)
				}
			}
			h.requireNoDecision(t, ctx, site)
		})
	}
}

// A dedicated nonsuperuser role with CONNECTION LIMIT 0: a real connect-time
// SQLSTATE 53300.
func TestRealRoleLimitAtEverySite(t *testing.T) {
	for _, site := range everySite {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			h.f.LimitedRole(ctx)
			elapsed, err := h.faultAt(t, site, testdb.FaultRoleLimit)
			requireClass(t, err, site, ClassConnectTooMany)
			avail := err
			requireConnectCause(t, avail)
			var pgErr *pgconn.PgError
			if !errors.As(avail, &pgErr) || pgErr.Code != "53300" {
				t.Fatalf("no real SQLSTATE 53300 in the cause:\n%s", testdb.ErrorTree(avail))
			}
			h.requireNoDecision(t, ctx, site)
			logLatency(t, site, string(ClassConnectTooMany), elapsed)
		})
	}
}

// A real backlog-full listener: the dial cannot finish, the production 500 ms
// step timer fires in the pre-decision phase, and the class is expiry
// whatever the returned error chain says. Remove the phase-expiry class and
// this test fails.
func TestRealBlockedDialExpiresAtEverySite(t *testing.T) {
	for _, site := range everySite {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			elapsed, err := h.faultAt(t, site, testdb.FaultBlockedDial)
			requireClass(t, err, site, ClassStepExpired)
			if h.f.BlockedDials() < 1 {
				t.Fatal("the blocked listener's dial function was never entered")
			}
			if elapsed < preDecisionStepBound || elapsed >= 3*time.Second {
				t.Fatalf("returned after %s, want within [%s, 3s)", elapsed, preDecisionStepBound)
			}
			h.requireNoDecision(t, ctx, site)
			logLatency(t, site, string(ClassStepExpired)+"/blocked-dial", elapsed)
		})
	}
}

// Negative control: an independent 100 ms socket timeout fires before the
// still-live 500 ms step. The timeout spelling is not an admitted class.
func TestIndependentDialTimeoutIsUnsupported(t *testing.T) {
	for _, site := range everySite {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			elapsed, err := h.faultAt(t, site, testdb.FaultDialTimeout)
			requireNoClass(t, err, site)
			if elapsed >= preDecisionStepBound {
				t.Fatalf("the dial timeout took %s: the step timer fired first, so this control proves nothing", elapsed)
			}
			h.requireNoDecision(t, ctx, site)
		})
	}
}

// A pooled backend terminated from the healthy admin session, then reused:
// the real returned code is 57P01 at each site.
func TestRealBackendTerminationAtEverySite(t *testing.T) {
	for _, site := range everySite {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			// Warm one pooled connection with TWO statements. Pinned pgx stdlib
			// pings a connection on its first reuse and after one idle second;
			// a ping on a dead backend makes database/sql dial again, so the
			// code reaches the caller only on a recently reused connection.
			for i := 0; i < 2; i++ {
				var one int
				if err := h.f.DB.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
					t.Fatal(err)
				}
			}
			var killed []int
			switch site {
			case SiteDeviceLookup, SitePolicyRead:
				killed = h.f.TerminateAppBackends(ctx)
			default:
				// The policy SELECT succeeds; its connection's backend ends
				// before the acquisition reuses it.
				h.f.Once(testdb.AtEnd, policySQL, func(e testdb.QueryEvent) {
					h.f.Terminate(ctx, int(e.PID))
					killed = append(killed, int(e.PID))
				})
			}
			opCtx, opCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer opCancel()
			start := time.Now()
			err := h.call(opCtx, site)
			elapsed := time.Since(start)
			if len(killed) != 1 {
				t.Fatalf("terminated %d backends, want exactly the one pooled connection", len(killed))
			}
			requireClass(t, err, site, ClassBackendTerminated)
			avail := err
			var pgErr *pgconn.PgError
			if !errors.As(avail, &pgErr) || pgErr.Code != "57P01" {
				t.Fatalf("no real SQLSTATE 57P01:\n%s", testdb.ErrorTree(avail))
			}
			h.requireNoDecision(t, ctx, site)
			logLatency(t, site, string(ClassBackendTerminated), elapsed)
		})
	}
}

// The owned proxy receives the read's request bytes and closes the client
// side with a FIN before it forwards them.
func TestRealFINCutAtReadSites(t *testing.T) {
	for site, pattern := range map[AvailabilitySite]string{SiteDeviceLookup: deviceSQL, SitePolicyRead: policySQL} {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{Proxy: true})
			fired := h.f.CutOn(pattern, testdb.CutFIN)
			opCtx, opCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer opCancel()
			start := time.Now()
			err := h.call(opCtx, site)
			elapsed := time.Since(start)
			requireFired(t, fired)
			requireClass(t, err, site, ClassUnexpectedEOF)
			avail := err
			if !errors.Is(avail, io.ErrUnexpectedEOF) {
				t.Fatalf("cause lost:\n%s", testdb.ErrorTree(avail))
			}
			h.requireNoDecision(t, ctx, site)
			logLatency(t, site, string(ClassUnexpectedEOF), elapsed)
		})
	}
}

// The same cut with SO_LINGER=0: an RST.
func TestRealRSTCutAtReadSites(t *testing.T) {
	for site, pattern := range map[AvailabilitySite]string{SiteDeviceLookup: deviceSQL, SitePolicyRead: policySQL} {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{Proxy: true})
			fired := h.f.CutOn(pattern, testdb.CutRST)
			opCtx, opCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer opCancel()
			start := time.Now()
			err := h.call(opCtx, site)
			elapsed := time.Since(start)
			requireFired(t, fired)
			requireClass(t, err, site, ClassConnReset)
			avail := err
			if !errors.Is(avail, syscall.ECONNRESET) {
				t.Fatalf("cause lost:\n%s", testdb.ErrorTree(avail))
			}
			h.requireNoDecision(t, ctx, site)
			logLatency(t, site, string(ClassConnReset), elapsed)
		})
	}
}

// The established socket is cut at BEGIN, after the successful policy SELECT.
func TestRealBeginSocketCutAtBeginSites(t *testing.T) {
	for _, site := range []AvailabilitySite{SiteSingleBegin, SitePassBegin} {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{Proxy: true})
			var fired <-chan struct{}
			h.f.Once(testdb.AtEnd, policySQL, func(testdb.QueryEvent) { fired = h.f.CutOn("begin", testdb.CutFIN) })
			opCtx, opCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer opCancel()
			start := time.Now()
			err := h.call(opCtx, site)
			elapsed := time.Since(start)
			if fired == nil {
				t.Fatal("the policy barrier never ran")
			}
			requireFired(t, fired)
			requireClass(t, err, site, ClassConnClosed)
			avail := err
			if !errors.Is(avail, pgconn.ErrConnClosed) {
				t.Fatalf("cause lost:\n%s", testdb.ErrorTree(avail))
			}
			h.requireNoDecision(t, ctx, site)
			logLatency(t, site, string(ClassConnClosed), elapsed)
		})
	}
}

func requireFired(t *testing.T, fired <-chan struct{}) {
	t.Helper()
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy never saw the target bytes")
	}
}

// The proxy forwards BEGIN and holds the server's reply. The step expires,
// and the transaction the server opened does not leak.
func TestWithheldBeginResponseExpires(t *testing.T) {
	for _, site := range []AvailabilitySite{SiteSingleBegin, SitePassBegin} {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{Proxy: true})
			var fired <-chan struct{}
			h.f.Once(testdb.AtEnd, policySQL, func(testdb.QueryEvent) { fired = h.f.CutOn("begin", testdb.Withhold) })
			opCtx, opCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer opCancel()
			start := time.Now()
			err := h.call(opCtx, site)
			elapsed := time.Since(start)
			requireFired(t, fired)
			requireClass(t, err, site, ClassStepExpired)
			if elapsed < preDecisionStepBound || elapsed >= 3*time.Second {
				t.Fatalf("returned after %s, want within [%s, 3s)", elapsed, preDecisionStepBound)
			}
			if n := h.f.WaitNoOpenTransactions(ctx, 3*time.Second); n != 0 {
				t.Fatalf("%d transactions left open after the withheld BEGIN", n)
			}
			if inUse := h.f.DB.Stats().InUse; inUse != 0 {
				t.Fatalf("%d connections still in use", inUse)
			}
			h.requireNoDecision(t, ctx, site)
			logLatency(t, site, string(ClassStepExpired)+"/withheld-begin", elapsed)
		})
	}
}

// ACCESS EXCLUSIVE on the read's table from an admin transaction: the read
// blocks, the backend is seen waiting before the timer fires, and the step
// expires.
func TestTableLockedReadExpires(t *testing.T) {
	for site, table := range map[AvailabilitySite]string{SiteDeviceLookup: "scanner_devices", SitePolicyRead: "slot_re_entry_policies"} {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			lock, err := h.f.Admin.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lock.Rollback() }()
			if _, err := lock.ExecContext(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var callErr error
			var elapsed time.Duration
			go func() {
				defer close(done)
				opCtx, opCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer opCancel()
				start := time.Now()
				callErr = h.call(opCtx, site)
				elapsed = time.Since(start)
			}()
			if !h.f.WaitForLockWait(ctx, table, preDecisionStepBound-50*time.Millisecond) {
				t.Fatal("the read was never seen waiting on the table lock before the timer")
			}
			<-done
			requireClass(t, callErr, site, ClassStepExpired)
			if elapsed < preDecisionStepBound || elapsed >= 3*time.Second {
				t.Fatalf("returned after %s, want within [%s, 3s)", elapsed, preDecisionStepBound)
			}
			_ = lock.Rollback()
			h.requireNoDecision(t, ctx, site)
			logLatency(t, site, string(ClassStepExpired)+"/table-lock", elapsed)
		})
	}
}

// POOL-WAIT FIXTURE (MaxOpen=1). A control session holds the only
// connection, so acquisition waits until the step expires. At the
// acquisition sites the hold starts at the post-policy barrier.
func TestPoolWaitExpiresAtEverySite(t *testing.T) {
	for _, site := range everySite {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{MaxOpen: 1})
			release := make(chan struct{})
			var held sync.WaitGroup
			holdQueued := func() {
				// Ask for the only connection on another goroutine, and wait
				// until the request is queued or holds it: the order is forced.
				before := h.f.DB.Stats().WaitCount
				held.Add(1)
				go func() {
					defer held.Done()
					conn, err := h.f.DB.Conn(context.Background())
					if err != nil {
						t.Errorf("hold the pool: %v", err)
						return
					}
					<-release
					_ = conn.Close()
				}()
				deadline := time.Now().Add(2 * time.Second)
				for h.f.DB.Stats().WaitCount == before && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if h.f.DB.Stats().WaitCount == before {
					t.Error("the hold never queued behind the in-use connection")
				}
			}
			switch site {
			case SiteDeviceLookup, SitePolicyRead:
				conn, err := h.f.DB.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				held.Add(1)
				go func() {
					defer held.Done()
					<-release
					_ = conn.Close()
				}()
			default:
				// The policy SELECT holds the only connection while this runs.
				h.f.Once(testdb.AtEnd, policySQL, func(testdb.QueryEvent) { holdQueued() })
			}
			opCtx, opCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer opCancel()
			start := time.Now()
			err := h.call(opCtx, site)
			elapsed := time.Since(start)
			close(release)
			held.Wait()
			requireClass(t, err, site, ClassStepExpired)
			if elapsed < preDecisionStepBound || elapsed >= 3*time.Second {
				t.Fatalf("returned after %s, want within [%s, 3s)", elapsed, preDecisionStepBound)
			}
			h.requireNoDecision(t, ctx, site)
			logLatency(t, site, string(ClassStepExpired)+"/pool-wait", elapsed)
		})
	}
}

// A stale idle connection: past pgx's one-second ping threshold, reuse pings
// first, and the proxy holds the ping's reply. The same step bounds that
// phase. The discard and dial effects are recorded, not assumed.
func TestStaleIdlePingIsBoundedByTheStep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := newScannerHarness(t, ctx, testdb.Options{Proxy: true})
	var one int
	if err := h.f.DB.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	fired := h.f.CutOn("-- ping", testdb.Withhold)
	before := h.f.Stats()
	opCtx, opCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer opCancel()
	start := time.Now()
	err := h.call(opCtx, SiteDeviceLookup)
	elapsed := time.Since(start)
	requireFired(t, fired)
	requireClass(t, err, SiteDeviceLookup, ClassStepExpired)
	if elapsed < preDecisionStepBound || elapsed >= 3*time.Second {
		t.Fatalf("returned after %s, want within [%s, 3s)", elapsed, preDecisionStepBound)
	}
	after := h.f.Stats()
	t.Logf("stale ping: dials %d -> %d, attempts %d -> %d, open %d", before.Dials, after.Dials, before.Attempts, after.Attempts, h.f.DB.Stats().OpenConnections)
	logLatency(t, SiteDeviceLookup, string(ClassStepExpired)+"/stale-ping", elapsed)
}

// --- later failures stay ordinary errors (D14) ------------------------------

// A ticket row lock holds the decision's FOR UPDATE AFTER handoff, then the
// parent deadline passes. That deadline is after the decision began, so it is
// no availability class.
func TestLaterTicketLockDeadlineIsNotAvailability(t *testing.T) {
	for _, site := range []AvailabilitySite{SiteSingleBegin, SitePassBegin} {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			ticket := h.single
			if site == SitePassBegin {
				ticket = h.pass
			}
			lock := holdTicketLock(t, ctx, h.f.Admin, ticket.ticketID)
			defer func() { _ = lock.Rollback() }()
			done := make(chan error, 1)
			go func() {
				opCtx, opCancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
				defer opCancel()
				done <- h.call(opCtx, site)
			}()
			if !h.f.WaitForLockWait(ctx, "for update", time.Second) {
				t.Fatal("the decision's FOR UPDATE was never seen waiting")
			}
			err := <-done
			requireNoClass(t, err, site)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Logf("later deadline returned:\n%s", testdb.ErrorTree(err))
			}
			_ = lock.Rollback()
			h.requireNoAdmission(t, ctx, ticket)
		})
	}
}

// The transaction's backend is terminated while it waits at the ticket read.
// The real cause is 57P01, the same code that is availability at a
// pre-decision site, and here it must stay an ordinary error.
func TestLaterBackendLossIsNotAvailability(t *testing.T) {
	for _, site := range []AvailabilitySite{SiteSingleBegin, SitePassBegin} {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			ticket := h.single
			if site == SitePassBegin {
				ticket = h.pass
			}
			lock := holdTicketLock(t, ctx, h.f.Admin, ticket.ticketID)
			defer func() { _ = lock.Rollback() }()
			// The hook runs on the call's goroutine: the pid crosses to this
			// one through a channel, not through a shared variable.
			pids := make(chan uint32, 1)
			h.f.Once(testdb.AtStart, ticketLockSQL, func(e testdb.QueryEvent) { pids <- e.PID })
			done := make(chan error, 1)
			go func() {
				opCtx, opCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer opCancel()
				done <- h.call(opCtx, site)
			}()
			if !h.f.WaitForLockWait(ctx, "for update", 2*time.Second) {
				t.Fatal("the decision's FOR UPDATE was never seen waiting")
			}
			var pid uint32
			select {
			case pid = <-pids:
			case <-time.After(time.Second):
				t.Fatal("the ticket read never reported its backend")
			}
			h.f.Terminate(ctx, int(pid))
			err := <-done
			requireNoClass(t, err, site)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "57P01" {
				t.Fatalf("want the real 57P01 cause kept:\n%s", testdb.ErrorTree(err))
			}
			_ = lock.Rollback()
			h.requireNoAdmission(t, ctx, ticket)
		})
	}
}

// The established socket is cut at the decision's first statement, after
// handoff. A post-connect class that is availability at a pre-decision site
// is an ordinary error here.
func TestPostHandoffSocketCutIsNotAvailability(t *testing.T) {
	for _, site := range []AvailabilitySite{SiteSingleBegin, SitePassBegin} {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{Proxy: true})
			var fired <-chan struct{}
			h.f.Once(testdb.AtEnd, "begin", func(testdb.QueryEvent) { fired = h.f.CutOn("for update", testdb.CutFIN) })
			opCtx, opCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer opCancel()
			err := h.call(opCtx, site)
			if fired == nil {
				t.Fatal("the begin barrier never ran")
			}
			requireFired(t, fired)
			requireNoClass(t, err, site)
			if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, pgconn.ErrConnClosed) {
				t.Fatalf("not a real socket-cut cause:\n%s", testdb.ErrorTree(err))
			}
			ticket := h.single
			if site == SitePassBegin {
				ticket = h.pass
			}
			h.requireNoAdmission(t, ctx, ticket)
		})
	}
}

func holdTicketLock(t *testing.T, ctx context.Context, admin *sql.DB, ticketID uuid.UUID) *sql.Tx {
	t.Helper()
	tx, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM tickets WHERE id=$1 FOR UPDATE`, ticketID); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	return tx
}

func (h *scannerHarness) requireNoAdmission(t *testing.T, ctx context.Context, s seeded) {
	t.Helper()
	if n := countRows(t, ctx, h.f.Admin, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type IN ('redeemed','entry','exit')`, s.ticketID); n != 0 {
		t.Fatalf("%d admission events were written", n)
	}
	if n := countRows(t, ctx, h.f.Admin, `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE ticket_id=$1`, s.ticketID); n != 0 {
		t.Fatalf("%d quarantine rows were written", n)
	}
}

func (h *scannerHarness) admissions(t *testing.T, ctx context.Context, s seeded) int {
	t.Helper()
	return countRows(t, ctx, h.f.Admin, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type IN ('redeemed','entry')`, s.ticketID)
}

// --- the handoff keeps the transaction alive ---------------------------------

// The real 500 ms timer: a decision that takes longer than the acquisition
// bound after handoff still commits.
func TestSuccessfulTransactionOutlivesTheAcquisitionBound(t *testing.T) {
	for _, site := range []AvailabilitySite{SiteSingleBegin, SitePassBegin} {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			h.f.Once(testdb.AtStart, ticketLockSQL, func(testdb.QueryEvent) { time.Sleep(preDecisionStepBound + 150*time.Millisecond) })
			opCtx, opCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer opCancel()
			if err := h.call(opCtx, site); err != nil {
				t.Fatalf("a valid scan failed after outliving the acquisition bound:\n%s", testdb.ErrorTree(err))
			}
			ticket := h.single
			if site == SitePassBegin {
				ticket = h.pass
			}
			if n := h.admissions(t, ctx, ticket); n != 1 {
				t.Fatalf("admissions = %d, want the committed one", n)
			}
		})
	}
}

// An injected scheduler fires the acquisition step's timer callback AFTER
// handoff and before commit. The child is still alive and the real
// transaction commits. Without the callback's active-phase check, the late
// callback cancels the child and the transaction rolls back.
func TestLateCallbackAfterHandoffDoesNotCancelTheTransaction(t *testing.T) {
	for _, site := range []AvailabilitySite{SiteSingleBegin, SitePassBegin} {
		t.Run(string(site), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newScannerHarness(t, ctx, testdb.Options{})
			steps := newFakeSteps()
			h.st.steps = steps
			fired := false
			h.f.Once(testdb.AtStart, ticketLockSQL, func(testdb.QueryEvent) {
				// timers[0] is the policy step; timers[1] the acquisition.
				if steps.count() != 2 {
					t.Errorf("armed %d step timers before the decision, want 2", steps.count())
					return
				}
				steps.timer(1).fire()
				fired = true
			})
			if err := h.call(ctx, site); err != nil {
				t.Fatalf("scan after a late callback:\n%s", testdb.ErrorTree(err))
			}
			if !fired {
				t.Fatal("the late callback never ran")
			}
			ticket := h.single
			if site == SitePassBegin {
				ticket = h.pass
			}
			if n := h.admissions(t, ctx, ticket); n != 1 {
				t.Fatalf("admissions = %d, want the committed one", n)
			}
		})
	}
}

// A generic %w wrapper around the raw driver cause would lose the
// application type: the store must construct PreDecisionUnavailable itself.
func TestDeviceLookupAvailabilityIsTheNamedApplicationType(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := newScannerHarness(t, ctx, testdb.Options{})
	_, err := h.faultAt(t, SiteDeviceLookup, testdb.FaultRefused)
	requireClass(t, err, SiteDeviceLookup, ClassConnectRefused)
	if errors.Is(err, ErrScannerDeviceUnknown) {
		t.Fatal("availability also reads as the unknown-device refusal")
	}
	var avail *PreDecisionUnavailable
	if !errors.As(err, &avail) || avail.Cause == nil {
		t.Fatal("the original cause was not kept")
	}
}

// Real unknown and revoked devices stay the one refusal, by SQL.
func TestUnknownAndRevokedDevicesStayRefusals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := newScannerHarness(t, ctx, testdb.Options{})
	if _, err := h.st.AuthenticateScannerDevice(ctx, "never-enrolled"); !errors.Is(err, ErrScannerDeviceUnknown) {
		t.Fatalf("unknown token = %v", err)
	}
	if err := h.seed.RevokeScannerDevice(ctx, h.device.ID); err != nil {
		t.Fatal(err)
	}
	_, err := h.st.AuthenticateScannerDevice(ctx, h.token)
	if !errors.Is(err, ErrScannerDeviceUnknown) {
		t.Fatalf("revoked token = %v", err)
	}
	var avail *PreDecisionUnavailable
	if errors.As(err, &avail) {
		t.Fatal("a revoked device read as availability")
	}
}

// --- advisory Touch ----------------------------------------------------------

func readLastSeen(t *testing.T, ctx context.Context, db *sql.DB, id uuid.UUID) *time.Time {
	t.Helper()
	var seen *time.Time
	if err := db.QueryRowContext(ctx, `SELECT last_seen_at FROM scanner_devices WHERE id=$1`, id).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	return seen
}

// Another session holds only the device row lock. Touch skips the row
// instead of waiting: zero rows, the exact seeded timestamp, no extra dial,
// and well inside an independent 350 ms bound (a test discriminant, not a
// venue SLA). Without SKIP LOCKED it waits until the 500 ms step cancels it.
func TestTouchSkipsAHeldRowLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := newScannerHarness(t, ctx, testdb.Options{})
	seeded := time.Date(2026, 10, 17, 3, 4, 5, 123456000, time.UTC)
	if _, err := h.f.Admin.ExecContext(ctx, `UPDATE scanner_devices SET last_seen_at=$1 WHERE id=$2`, seeded, h.device.ID); err != nil {
		t.Fatal(err)
	}
	// Warm the pool, so a dial during Touch would show.
	if _, err := h.st.AuthenticateScannerDevice(ctx, h.token); err != nil {
		t.Fatal(err)
	}
	lock, err := h.f.Admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback() }()
	if _, err := lock.ExecContext(ctx, `SELECT id FROM scanner_devices WHERE id=$1 FOR UPDATE`, h.device.ID); err != nil {
		t.Fatal(err)
	}
	before := h.f.Stats()
	start := time.Now()
	h.st.TouchScannerDevice(ctx, h.device.ID)
	elapsed := time.Since(start)
	var touch *testdb.QueryEvent
	for _, q := range h.f.Queries() {
		// Matched on the SET clause, which survives the SKIP LOCKED mutation.
		if strings.Contains(q.SQL, "SET last_seen_at = now()") {
			q := q
			touch = &q
		}
	}
	if touch == nil || !touch.Done {
		t.Fatal("the Touch statement was not seen to start and end")
	}
	if elapsed >= 350*time.Millisecond {
		t.Fatalf("Touch took %s under a held row lock, want < 350ms (err %v)", elapsed, touch.Err)
	}
	if touch.Err != nil || touch.Rows != 0 {
		t.Fatalf("Touch: rows=%d err=%v, want zero rows and no error", touch.Rows, touch.Err)
	}
	if after := h.f.Stats(); after.Dials != before.Dials {
		t.Fatalf("Touch dialled %d times on a healthy locked-row path", after.Dials-before.Dials)
	}
	_ = lock.Rollback()
	if got := readLastSeen(t, ctx, h.f.Admin, h.device.ID); got == nil || !got.Equal(seeded) {
		t.Fatalf("last_seen_at = %v, want the exact seeded %v", got, seeded)
	}
	t.Logf("TKT557-TOUCH held-row-lock elapsed_us=%d rows=%d", elapsed.Microseconds(), touch.Rows)
}

// The control: unlocked, Touch updates the timestamp.
func TestTouchUpdatesAnUnlockedRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := newScannerHarness(t, ctx, testdb.Options{})
	if got := readLastSeen(t, ctx, h.f.Admin, h.device.ID); got != nil {
		t.Fatalf("fixture: last_seen_at already set: %v", got)
	}
	h.st.TouchScannerDevice(ctx, h.device.ID)
	if got := readLastSeen(t, ctx, h.f.Admin, h.device.ID); got == nil {
		t.Fatal("an unlocked Touch did not update last_seen_at")
	}
}

// POOL-WAIT FIXTURE (MaxOpen=1). Real authentication first; then a control
// session holds the only connection until Touch returns. Touch returns inside
// an independent 750 ms bound (the 500 ms step bound plus scheduling), and a
// 1500 ms Touch bound fails it.
func TestTouchPoolWaitIsBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := newScannerHarness(t, ctx, testdb.Options{MaxOpen: 1})
	if _, err := h.st.AuthenticateScannerDevice(ctx, h.token); err != nil {
		t.Fatal(err)
	}
	held, err := h.f.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	start := time.Now()
	h.st.TouchScannerDevice(ctx, h.device.ID)
	elapsed := time.Since(start)
	_ = held.Close()
	if elapsed >= 750*time.Millisecond {
		t.Fatalf("Touch waited %s for the pool, want < 750ms", elapsed)
	}
	if elapsed < touchStepBound {
		t.Fatalf("Touch returned after %s: it did not wait for the pool at all, so this fixture proves nothing", elapsed)
	}
	if _, err := h.st.AuthenticateScannerDevice(ctx, h.token); err != nil {
		t.Fatalf("the pool did not recover after Touch: %v", err)
	}
	t.Logf("TKT557-TOUCH pool-wait elapsed_ms=%d", elapsed.Milliseconds())
}

// A table lock reaches the Touch statement itself: Touch waits, the step
// ends it, and it stays advisory. A later healthy read works.
func TestTouchTableLockIsAdvisory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := newScannerHarness(t, ctx, testdb.Options{})
	lock, err := h.f.Admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback() }()
	if _, err := lock.ExecContext(ctx, "LOCK TABLE scanner_devices IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		h.st.TouchScannerDevice(ctx, h.device.ID)
		done <- time.Since(start)
	}()
	if !h.f.WaitForLockWait(ctx, "scanner_devices", touchStepBound-50*time.Millisecond) {
		t.Fatal("Touch was never seen waiting on the table lock")
	}
	elapsed := <-done
	if elapsed < touchStepBound || elapsed >= 3*time.Second {
		t.Fatalf("Touch returned after %s, want within [%s, 3s)", elapsed, touchStepBound)
	}
	_ = lock.Rollback()
	if _, err := h.st.AuthenticateScannerDevice(ctx, h.token); err != nil {
		t.Fatalf("healthy read after the advisory Touch expiry: %v", err)
	}
	if got := readLastSeen(t, ctx, h.f.Admin, h.device.ID); got != nil {
		t.Fatalf("an expired Touch still updated last_seen_at: %v", got)
	}
	t.Logf("TKT557-TOUCH table-lock elapsed_ms=%d stats=%+v", elapsed.Milliseconds(), h.f.Stats())
}

// A stale idle connection reaches Touch: past pgx's one-second ping threshold
// reuse pings first, and the proxy holds the ping's reply. Touch's own step
// bounds that phase too, Touch stays advisory, and a later read works. The
// discard and dial effects are recorded, not assumed.
func TestTouchStaleIdlePingIsBoundedByItsStep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := newScannerHarness(t, ctx, testdb.Options{Proxy: true})
	if _, err := h.st.AuthenticateScannerDevice(ctx, h.token); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	fired := h.f.CutOn("-- ping", testdb.Withhold)
	before := h.f.Stats()
	start := time.Now()
	h.st.TouchScannerDevice(ctx, h.device.ID)
	elapsed := time.Since(start)
	requireFired(t, fired)
	if elapsed < touchStepBound || elapsed >= 750*time.Millisecond {
		t.Fatalf("Touch returned after %s, want within [%s, 750ms)", elapsed, touchStepBound)
	}
	if got := readLastSeen(t, ctx, h.f.Admin, h.device.ID); got != nil {
		t.Fatalf("a Touch held at the ping still updated last_seen_at: %v", got)
	}
	if _, err := h.st.AuthenticateScannerDevice(ctx, h.token); err != nil {
		t.Fatalf("healthy read after the held ping: %v", err)
	}
	after := h.f.Stats()
	t.Logf("TKT557-TOUCH stale-ping elapsed_ms=%d dials %d->%d attempts %d->%d", elapsed.Milliseconds(), before.Dials, after.Dials, before.Attempts, after.Attempts)
}
