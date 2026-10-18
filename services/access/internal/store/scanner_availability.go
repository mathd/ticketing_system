package store

// Pre-decision availability for the scanner routes (TKT-557, ADR-085 §8
// ticket 0).
//
// Four statements run before any ticket decision: the device lookup SELECT,
// the initial live policy SELECT, and the single and pass transaction
// acquisitions. Only these four can make a PreDecisionUnavailable. The API
// answers 503 for it, and a scanner can then admit offline. Every later error
// stays an ordinary error and a 500, because a later statement can follow a
// decision or a commit.
//
// Two things select the class. One is server-owned phase state: the step
// timer fired while the step was still in its pre-decision phase and the
// parent was alive (D39 expiry), or the parent deadline passed. The other is
// the real-driver ledger below. The driver's error spelling never selects the
// expiry class (D27).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// preDecisionStepBound limits each pre-decision step: the device SELECT, the
// initial policy SELECT and each live BeginTx.
//
// PROVISIONAL. Ticket 0 ships this value without load proof. TKT-560 measures
// it, and venue use after TKT-558 requires TKT-560 Done and the owner's
// selected bound and acceptable offline-spill rate. A changed value needs an
// ADR-085 amendment.
const preDecisionStepBound = 500 * time.Millisecond

// touchStepBound limits the advisory Touch statement (D27, D30 as amended by
// D34). Touch expiry never changes an authentication result.
//
// PROVISIONAL, for the same reason and with the same TKT-560 precondition as
// preDecisionStepBound.
const touchStepBound = 500 * time.Millisecond

// AvailabilitySite names one pre-decision step.
type AvailabilitySite string

const (
	SiteDeviceLookup AvailabilitySite = "device_lookup"
	SitePolicyRead   AvailabilitySite = "policy_read"
	SiteSingleBegin  AvailabilitySite = "single_begin"
	SitePassBegin    AvailabilitySite = "pass_begin"
)

// AvailabilityClass names why a pre-decision step failed.
type AvailabilityClass string

const (
	// ClassStepExpired: the step timer fired in the pre-decision phase and the
	// parent was alive (D39 expiry). It does not prove that the database was
	// down, only that the step passed its bound.
	ClassStepExpired AvailabilityClass = "step_expired"
	// ClassParentDeadline: the parent deadline passed before or during the step.
	ClassParentDeadline AvailabilityClass = "parent_deadline"

	// Connection failures. Every branch below the outer ConnectError ends at
	// one of these leaves.
	ClassConnectDNS        AvailabilityClass = "connect_dns"
	ClassConnectRefused    AvailabilityClass = "connect_refused"
	ClassConnectStartup    AvailabilityClass = "connect_starting_up" // SQLSTATE 57P03
	ClassConnectTooMany    AvailabilityClass = "connect_too_many"    // SQLSTATE 53300
	ClassConnectMixed      AvailabilityClass = "connect_mixed"
	ClassUnsupportedMix    AvailabilityClass = "mixed"
	ClassBackendTerminated AvailabilityClass = "backend_terminated" // SQLSTATE 57P01

	// Post-connect failures, each only at the sites where a real producer
	// proved it (D22).
	ClassUnexpectedEOF AvailabilityClass = "unexpected_eof"
	ClassConnClosed    AvailabilityClass = "conn_closed"
	ClassConnReset     AvailabilityClass = "conn_reset"
)

// PreDecisionUnavailable is a failure at one of the four pre-decision sites.
// It proves that no ticket decision ran in THIS request. It proves nothing
// about an earlier attempt for the same occurrence (occurrence uncertainty U).
type PreDecisionUnavailable struct {
	Site  AvailabilitySite
	Class AvailabilityClass
	// Cause is the original error, kept for logs. It can be nil when the step
	// expired or the parent deadline passed after a successful statement.
	Cause error
}

func (e *PreDecisionUnavailable) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("scanner store unavailable before decision (site=%s class=%s)", e.Site, e.Class)
	}
	return fmt.Sprintf("scanner store unavailable before decision (site=%s class=%s): %v", e.Site, e.Class, e.Cause)
}

func (e *PreDecisionUnavailable) Unwrap() error { return e.Cause }

// postConnectSites is the proved class/site ledger outside ConnectError
// (D22). A pair absent here stays a 500 until a real producer proves it.
var postConnectSites = map[AvailabilityClass]map[AvailabilitySite]bool{
	ClassBackendTerminated: {SiteDeviceLookup: true, SitePolicyRead: true, SiteSingleBegin: true, SitePassBegin: true},
	ClassUnexpectedEOF:     {SiteDeviceLookup: true, SitePolicyRead: true},
	ClassConnReset:         {SiteDeviceLookup: true, SitePolicyRead: true},
	ClassConnClosed:        {SiteSingleBegin: true, SitePassBegin: true},
}

// classifyAvailability returns the class of a non-expiry failure at site.
//
// It walks the whole cause tree. A ConnectError subtree is checked against
// the connect leaf set only, so a post-connect match cannot rescue a rejected
// connect subtree. For a join, every branch must qualify. errors.Is and
// errors.As are not used to select a class: they search every branch at once,
// so one supported branch would bless an unsupported sibling.
func classifyAvailability(site AvailabilitySite, err error) (AvailabilityClass, bool) {
	classes, ok := availabilityBranches(site, err)
	if !ok || len(classes) == 0 {
		return "", false
	}
	return summarizeClasses(classes), true
}

//nolint:errorlint // A node-by-node walk: see classifyAvailability for why errors.Is/As cannot select the class.
func availabilityBranches(site AvailabilitySite, err error) ([]AvailabilityClass, bool) {
	if err == nil {
		return nil, false
	}
	if connectErr, ok := err.(*pgconn.ConnectError); ok {
		return connectLeaves(connectErr.Unwrap())
	}
	if class, ok := postConnectNode(err); ok {
		return []AvailabilityClass{class}, postConnectSites[class][site]
	}
	switch node := err.(type) {
	case interface{ Unwrap() []error }:
		return everyBranch(node.Unwrap(), func(child error) ([]AvailabilityClass, bool) {
			return availabilityBranches(site, child)
		})
	case interface{ Unwrap() error }:
		return availabilityBranches(site, node.Unwrap())
	}
	return nil, false
}

// connectLeaves walks the subtree below an outer ConnectError. It checks each
// node before it unwraps it: an accepted node ends its branch, even if the
// node has Unwrap (a DNS error does). Every branch must end at an accepted
// node.
//
//nolint:errorlint // A node-by-node walk: see classifyAvailability.
func connectLeaves(err error) ([]AvailabilityClass, bool) {
	if err == nil {
		return nil, false
	}
	switch node := err.(type) {
	case *net.DNSError:
		return []AvailabilityClass{ClassConnectDNS}, true
	case *pgconn.PgError:
		switch node.Code {
		case "57P03":
			return []AvailabilityClass{ClassConnectStartup}, true
		case "53300":
			return []AvailabilityClass{ClassConnectTooMany}, true
		}
		return nil, false
	case syscall.Errno:
		if node == syscall.ECONNREFUSED {
			return []AvailabilityClass{ClassConnectRefused}, true
		}
		return nil, false
	case interface{ Unwrap() []error }:
		return everyBranch(node.Unwrap(), connectLeaves)
	case interface{ Unwrap() error }:
		return connectLeaves(node.Unwrap())
	}
	return nil, false
}

// postConnectNode recognises one post-connect node by identity or by exact
// SQLSTATE. It says nothing about the site; postConnectSites does.
//
//nolint:errorlint // A node-by-node walk: see classifyAvailability.
func postConnectNode(err error) (AvailabilityClass, bool) {
	switch err {
	case io.ErrUnexpectedEOF:
		return ClassUnexpectedEOF, true
	case pgconn.ErrConnClosed:
		return ClassConnClosed, true
	}
	switch node := err.(type) {
	case *pgconn.PgError:
		if node.Code == "57P01" {
			return ClassBackendTerminated, true
		}
	case syscall.Errno:
		if node == syscall.ECONNRESET {
			return ClassConnReset, true
		}
	}
	return "", false
}

func everyBranch(children []error, walk func(error) ([]AvailabilityClass, bool)) ([]AvailabilityClass, bool) {
	if len(children) == 0 {
		return nil, false
	}
	var classes []AvailabilityClass
	for _, child := range children {
		found, ok := walk(child)
		if !ok {
			return nil, false
		}
		classes = append(classes, found...)
	}
	return classes, true
}

func summarizeClasses(classes []AvailabilityClass) AvailabilityClass {
	first := classes[0]
	same, connect := true, true
	for _, class := range classes {
		same = same && class == first
		switch class {
		case ClassConnectDNS, ClassConnectRefused, ClassConnectStartup, ClassConnectTooMany:
		default:
			connect = false
		}
	}
	switch {
	case same:
		return first
	case connect:
		return ClassConnectMixed
	default:
		return ClassUnsupportedMix
	}
}

// stepTimer and stepScheduler are the clock the step helper reads.
// Production leaves Postgres.steps nil and gets the real clock. Unit tests
// inject one to force the order of timer callback and phase handoff.
type stepTimer interface{ Stop() bool }

type stepScheduler interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) stepTimer
}

type realSteps struct{}

func (realSteps) Now() time.Time { return time.Now() }

func (realSteps) AfterFunc(d time.Duration, f func()) stepTimer { return time.AfterFunc(d, f) }

func (p *Postgres) scheduler() stepScheduler {
	if p.steps != nil {
		return p.steps
	}
	return realSteps{}
}

// boundedStep is one bounded step. Its child context is a child of the
// request context, so parent cancellation always reaches it. The timer can
// cancel the child only while the pre-decision phase is open. finish closes
// the phase under the same mutex, so a late callback cannot cancel a child
// that has been handed to later work.
type boundedStep struct {
	mu     sync.Mutex
	active bool
	fired  bool
	due    time.Time
	parent context.Context
	ctx    context.Context
	cancel context.CancelFunc
	timer  stepTimer
	now    func() time.Time
}

// stepOutcome is the snapshot that finish takes before any result is read.
type stepOutcome struct {
	expired   bool
	parentErr error
}

func startBoundedStep(parent context.Context, bound time.Duration, steps stepScheduler) *boundedStep {
	ctx, cancel := context.WithCancel(parent)
	s := &boundedStep{active: true, parent: parent, ctx: ctx, cancel: cancel, now: steps.Now}
	s.due = steps.Now().Add(bound)
	// Held while the timer is armed, so the callback cannot run before s.timer
	// is set.
	s.mu.Lock()
	s.timer = steps.AfterFunc(bound, s.expire)
	s.mu.Unlock()
	return s
}

// expire is the timer callback. It records the flag before it cancels.
func (s *boundedStep) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}
	s.fired = true
	s.cancel()
}

// finish closes the pre-decision phase and disarms the timer. When the due
// time passed before the callback got the mutex, finish makes the same expiry
// transition itself, so an overdue result is never handed out.
func (s *boundedStep) finish() stepOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active && !s.fired && !s.now().Before(s.due) {
		s.fired = true
		s.cancel()
	}
	s.active = false
	s.timer.Stop()
	return stepOutcome{expired: s.fired, parentErr: s.parent.Err()}
}

// release cancels the child context. A read releases it after the complete
// row read; a transaction releases it after commit or rollback.
func (s *boundedStep) release() { s.cancel() }

// unavailable interprets one step result. A cancelled parent is never a
// pre-decision class, and it takes precedence over the step flag. A parent
// deadline and D39 expiry win over any late result, including a success, a
// no-row read and an unsupported error.
func (o stepOutcome) unavailable(site AvailabilitySite, err error) error {
	switch {
	case errors.Is(o.parentErr, context.Canceled):
		return nil
	case errors.Is(o.parentErr, context.DeadlineExceeded):
		return &PreDecisionUnavailable{Site: site, Class: ClassParentDeadline, Cause: err}
	case o.expired:
		return &PreDecisionUnavailable{Site: site, Class: ClassStepExpired, Cause: err}
	case err == nil:
		return nil
	}
	if class, ok := classifyAvailability(site, err); ok {
		return &PreDecisionUnavailable{Site: site, Class: class, Cause: err}
	}
	return nil
}

// livePolicy is Scan's initial policy read as a pre-decision step. The step
// completes before slotPolicy's no-row and missing-table fallback is
// accepted, so D39 expiry wins over that fallback too. The availability rule
// is here and not in slotPolicy, because reconciliation also calls slotPolicy
// inside its own transaction, after its ticket lock.
func (p *Postgres) livePolicy(ctx context.Context, slotID uuid.UUID) (ReEntryPolicy, error) {
	step := startBoundedStep(ctx, preDecisionStepBound, p.scheduler())
	defer step.release()
	policy, err := p.slotPolicy(step.ctx, p.db, slotID)
	if avail := step.finish().unavailable(SitePolicyRead, err); avail != nil {
		return ReEntryPolicy{}, avail
	}
	return policy, err
}

// beginLive acquires a live scan transaction as a pre-decision step. Only the
// acquisition phase is bounded and classified. When expiry or a parent
// failure wins, any returned transaction is rolled back before the error is
// returned, so no ticket query can start. On success the still-live child
// context goes to the caller with the transaction; the caller calls release
// after commit or rollback. The step timer is already disarmed, and parent
// cancellation stays connected.
func (p *Postgres) beginLive(ctx context.Context, site AvailabilitySite) (*sql.Tx, func(), error) {
	step := startBoundedStep(ctx, preDecisionStepBound, p.scheduler())
	tx, err := p.db.BeginTx(step.ctx, nil)
	if avail := step.finish().unavailable(site, err); avail != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		step.release()
		return nil, nil, avail
	}
	if err != nil {
		step.release()
		return nil, nil, err
	}
	return tx, step.release, nil
}
