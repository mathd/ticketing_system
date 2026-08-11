package api

// Rate limiting for the back-office staff login (TKT-195, ADR-042).
//
// ADR-042 recorded the trade — POST /staff/authenticate is public because the
// login form in front of it is anonymous — and named THIS as the control that
// bounds submission volume. The package doc of shared/go/ratelimit says the same
// thing from the other side. Neither was true until ai-review S4: the limiter was
// documented in two places and installed in none, so credential stuffing against
// a staff account was bounded only by bcrypt's cost.
//
// Same dual-key shape as commerce's customer surface, for the same reasons:
//
//   - per SUBJECT (the identifier) — a low budget, so one account cannot be ground.
//     Nothing against a walk across many identifiers.
//   - per SOURCE (the client IP) — a generous budget, so one client cannot walk a
//     list. Evaded by rotation, and shared by everyone behind one proxy.
//
// Read shared/go/ratelimit's package doc before writing "rate limited" about this:
// it bounds a scripted client against one replica, and a restart empties it.

import (
	"net/http"
	"strconv"
	"time"

	"ticketing/services/catalog/internal/store"
	"ticketing/shared/httpx"
	"ticketing/shared/ratelimit"
)

const (
	// A staff member signing in mistypes once or twice, not ten times in a
	// quarter of an hour; a stuffer needs thousands. The budget sits well above
	// the first and four orders of magnitude below the second.
	staffAuthSubjectBurst  = 10
	staffAuthSubjectWindow = 15 * time.Minute

	// Per source, and sized for the whole back office rather than one person —
	// the same XFF collapse commerce documents. The back office calls catalog
	// server-side through the gateway, which replaces X-Forwarded-For with its own
	// peer (the BACK-OFFICE CONTAINER), so every staff member signing in through
	// the form shares ONE source key. What this budget really bounds is a caller
	// scripting the gateway directly, who gets their own. The subject budget is
	// what protects an individual account.
	staffAuthSourceBurst  = 300
	staffAuthSourceWindow = 15 * time.Minute

	// The maps are fed by unauthenticated input, so they are bounded. Reaching a
	// cap is a symptom to escalate, not a state to tune around (see Allow: at the
	// cap a NEW key is refused and tracked ones are still served, so key rotation
	// cannot flush the bucket holding an attacker back).
	staffAuthSubjectKeyCap = 50_000
	staffAuthSourceKeyCap  = 50_000
)

// staffTooManyRequests is the ONE refusal body, for the same reason
// invalidStaffCredentials is: two call sites eventually say slightly different
// things, and the difference is what an enumerator reads.
const staffTooManyRequests = "too many requests — wait a moment and try again"

// retryAfterSeconds advertises one token's worth of refill at the SUBJECT rate,
// the tighter of the two — advertising the looser one would send a staff member
// back too early.
var retryAfterSeconds = strconv.Itoa(int(staffAuthSubjectWindow.Seconds()) / staffAuthSubjectBurst)

type staffAuthLimiters struct {
	subject *ratelimit.Limiter
	source  *ratelimit.Limiter
}

func newStaffAuthLimiters(now func() time.Time) *staffAuthLimiters {
	return &staffAuthLimiters{
		subject: ratelimit.New(staffAuthSubjectBurst, staffAuthSubjectWindow, staffAuthSubjectKeyCap, now),
		source:  ratelimit.New(staffAuthSourceBurst, staffAuthSourceWindow, staffAuthSourceKeyCap, now),
	}
}

// lim never returns nil: a Server built as a literal (tests, and any future
// construction path that skips NewServer) must still be limited. "nil means
// allow" would make the control silently absent exactly where nobody looks.
func (s *Server) lim() *staffAuthLimiters {
	s.limOnce.Do(func() {
		if s.limiters == nil {
			s.limiters = newStaffAuthLimiters(nil)
		}
	})
	return s.limiters
}

// WithClock replaces the limiters' time source. Tests only — production gets
// time.Now. It rebuilds rather than mutates, so a test always starts from empty
// buckets, and it consumes limOnce so a later lim() cannot discard the clock.
func (s *Server) WithClock(now func() time.Time) *Server {
	s.limiters = newStaffAuthLimiters(now)
	s.limOnce.Do(func() {})
	return s
}

// allowStaffAuth spends one token from each bucket and writes the refusal itself
// when either is empty.
//
// Called BEFORE the store lookup, and that ordering is the property, not a
// detail: the bucket must fill for an identifier that does not exist exactly as
// it does for one that does. Refuse after the lookup and a 429 means "this
// account is real" — a sharper oracle than the shared 401 the login path went to
// some trouble to build.
//
// Source first, then subject: the source key needs no body and refusing there
// costs nothing, while the subject key is the one an attacker controls freely.
func (s *Server) allowStaffAuth(w http.ResponseWriter, r *http.Request, identifier string) bool {
	l := s.lim()
	if !l.source.Allow(httpx.ClientIP(r)) || !l.subject.Allow(store.NormalizeStaffIdentifier(identifier)) {
		w.Header().Set("Retry-After", retryAfterSeconds)
		writeJSON(w, http.StatusTooManyRequests, Error{Error: staffTooManyRequests})
		return false
	}
	return true
}
