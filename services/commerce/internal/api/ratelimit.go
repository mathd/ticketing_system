package api

// Rate limiting for the public, credential-free customer surface (TKT-224,
// ADR-051). ADR-049 §2 named this as the control for the membership oracle and
// ADR-050 added reset-mail volume to what it has to cover.
//
// Two limiters, because neither key is sufficient alone:
//
//   - per SUBJECT (the address, the order reference) — a low budget, so one
//     account cannot be ground. Does nothing against a walk across many subjects.
//   - per SOURCE (the client IP) — a generous budget, so one client cannot walk a
//     list. Trivially evaded by rotation, and shared NAT means a whole office
//     shares one budget, which is why it is generous rather than tight.
//
// Read the package doc of shared/go/ratelimit before writing "rate limited"
// anywhere: this bounds a single scripted client against one replica and nothing
// else.

import (
	"net/http"
	"strconv"
	"time"

	"ticketing/shared/httpx"
	"ticketing/shared/ratelimit"

	commercestore "ticketing/services/commerce/internal/store"
)

// customerTooManyRequests is the ONE refusal body, built once, for the same
// reason customerCredentialsInvalid is (see customer.go): two call sites
// constructing "the same" message are two call sites one of which eventually says
// something slightly different, and the difference is what an enumerator reads.
const customerTooManyRequests = "too many requests — wait a moment and try again"

// The budgets. Named constants because the tests assert against THEM rather than
// against copies: a test carrying its own literal keeps passing when the
// production value moves, which makes the limit untested exactly when it changed.
//
// The gap between the two is deliberate and load-bearing. A normal buyer session
// — register, sign in, mistype once, sign in again — is 4 subject-scoped calls,
// so the subject budget must sit clearly above that; an enumerator needs
// thousands, so it must sit clearly below that. 10 per 15 minutes leaves a real
// buyer more than double the headroom they need while costing a walker four
// orders of magnitude.
const (
	customerSubjectBurst  = 10
	customerSubjectWindow = 15 * time.Minute

	// Per source, and sized for AGGREGATE SITE TRAFFIC rather than for one person.
	//
	// The reason is structural and worth stating before someone "tightens" it. The
	// storefront calls commerce server-side through the gateway, and the gateway
	// replaces X-Forwarded-For with its own peer — which on that hop is the
	// STOREFRONT CONTAINER. So every buyer who uses the forms shares a single
	// source key, and this budget is a cap on the whole site's form traffic, not on
	// one client's.
	//
	// It cannot be fixed by having the storefront forward the buyer's address:
	// commerce would then be trusting a header that any caller reaching the gateway
	// can set, which is a total bypass, and the storefront deliberately holds no
	// credential that would let commerce tell its claim from an attacker's
	// (ADR-043, ADR-049 §1). That posture is worth more than this key.
	//
	// What this budget therefore bounds is a caller scripting the gateway
	// DIRECTLY — who does get their own key. For the form path the per-subject
	// limiter is what protects an account. ADR-051 §3 states the residual.
	customerSourceBurst  = 600
	customerSourceWindow = 15 * time.Minute

	// Key caps. The maps are fed by unauthenticated input, so they are bounded for
	// the same reason the storefront's session map is (ADR-049 §4). Reaching one is
	// a symptom to escalate, not a state to tune around.
	customerSubjectKeyCap = 50_000
	customerSourceKeyCap  = 50_000
)

// The on-sale write budget (ai-review S7). POST /reservations and POST /orders
// had no limit at all, so an unauthenticated caller scripting the gateway could
// churn reservations at line rate — each one holding stock for HOLD_TTL. That is
// inventory hoarding / denial of sale, the canonical on-sale bot, and for a
// platform whose domain is high-contention on-sales it is the attack that
// matters.
//
// Say what this is NOT before quoting it as the control. It is a token bucket,
// not a queue: it bounds a scripted client against one replica, gives every
// source its own budget by definition, and empties on restart. A real on-sale
// defence is a waiting room plus per-buyer concurrency, and this repo has not
// built one — ADR-054 records that, and the per-buyer hold cap below is the half
// of it that does not need a queue.
//
// A SEPARATE budget from the customer-credential one, not a shared bucket. The
// traffic shapes have nothing in common: a buyer signs in a handful of times and
// reserves a handful of times, but the two are independent, and a shared bucket
// would let checkout traffic lock people out of signing in — the same mistake
// the credential/recovery split (below) already had to correct once.
//
// Sized for AGGREGATE traffic for the same structural reason the customer source
// budget is: the storefront calls commerce server-side through the gateway, so
// every buyer using the forms shares ONE source key (the storefront container).
// What this really bounds is a caller scripting the gateway DIRECTLY, who gets
// their own key. It must sit well above what a busy on-sale sends through the
// storefront and well below what a bot needs to hoard an allocation.
const (
	checkoutSourceBurst  = 1200
	checkoutSourceWindow = 15 * time.Minute
	checkoutSourceKeyCap = 50_000
)

// The partner budget (TKT-240). Sized well above a working integration's steady
// rate and well below what a runaway retry loop or a compromised credential can
// send: a reseller sells continuously during an on-sale, so the bound is per
// minute rather than per quarter-hour, and it is per RESELLER, so one partner's
// burst cannot exhaust another's budget.
const (
	partnerSubjectBurst  = 600
	partnerSubjectWindow = time.Minute
	partnerSubjectKeyCap = 10_000
)

// customerLimiters is the enforcement state. Held on the Server so a test gets a
// fresh one per case, and so the clock seam is injectable.
type customerLimiters struct {
	subject  *ratelimit.Limiter
	source   *ratelimit.Limiter
	checkout *ratelimit.Limiter
	// The partner surface (TKT-240 / ADR-056), keyed on the RESELLER rather than
	// the source address.
	//
	// This surface is the one place ADR-051's per-subject half is fully available:
	// the credential is resolved by the validator BEFORE the body is decoded, so a
	// partner has a stable identity at limiting time -- unlike the checkout writes
	// above, whose comment records that they have no subject that early and are
	// source-keyed for that reason. Per-source would also be the weaker choice
	// here on its own: a partner is a server with a fixed address that legitimately
	// sends volume, so an address budget must be loose enough to be useless as a
	// per-partner bound.
	partner *ratelimit.Limiter
}

func newCustomerLimiters(now func() time.Time) *customerLimiters {
	return &customerLimiters{
		subject:  ratelimit.New(customerSubjectBurst, customerSubjectWindow, customerSubjectKeyCap, now),
		source:   ratelimit.New(customerSourceBurst, customerSourceWindow, customerSourceKeyCap, now),
		checkout: ratelimit.New(checkoutSourceBurst, checkoutSourceWindow, checkoutSourceKeyCap, now),
		partner:  ratelimit.New(partnerSubjectBurst, partnerSubjectWindow, partnerSubjectKeyCap, now),
	}
}

// limitCheckoutSource is middleware for the on-sale write path. Source-keyed
// only: neither operation has a subject that exists before the body is decoded,
// and the per-BUYER bound these two really need is a concurrency cap on live
// holds rather than a rate — see reserveConcurrencyCap.
func (s *Server) limitCheckoutSource(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.lim().checkout.Allow(clientIP(r)) {
			writeTooManyRequests(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitSource is middleware for the public customer routes. It keys on the client
// IP alone, which needs no request body — the per-subject half runs inside each
// handler, where the subject is already decoded and each operation knows what its
// own subject is (an address here, an order reference there).
func (s *Server) limitSource(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.lim().source.Allow(clientIP(r)) {
			writeTooManyRequests(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowSubject spends one token for a subject-scoped operation and writes the
// refusal itself when there is none left. Call it AFTER decoding and BEFORE the
// store call.
//
// Before the store call is not a style preference. The bucket must fill for a
// subject that does not exist exactly as it does for one that does — otherwise a
// 429 means "this address is real" and the limiter is a sharper oracle than the
// 409 it was added to blunt (ADR-049 §2). Refusing before any lookup makes that
// true by construction rather than by remembering it at four call sites.
func (s *Server) allowSubject(w http.ResponseWriter, subject string) bool {
	if s.lim().subject.Allow(subject) {
		return true
	}
	writeTooManyRequests(w)
	return false
}

// Subject scopes. Credential probing and account RECOVERY get separate budgets,
// and that separation is not tidiness — it is the difference between a limiter
// and a lockout.
//
// Sharing one bucket per address across both looked obviously right and is wrong:
// a buyer who mistypes their password until the budget is gone is exactly the
// buyer who then clicks "forgot password", and a shared bucket refuses them the
// way back in. The browser gate caught it — every reset request in
// test/browser/rate-limit.mjs was throttled and nothing was ever enqueued.
//
// It costs almost nothing defensively: an enumerator gets one budget per scope
// rather than one in total, a constant factor on a walk the per-source limiter
// and the arithmetic already make hopeless.
const (
	subjectScopeCredential = "cred:"  // register + sign in — probing an address
	subjectScopeRecovery   = "reset:" // asking for a link — the way back in
)

// customerEmailSubject is the subject key for the address-keyed operations. It
// delegates to the store's normalizer (see NormalizeCustomerEmail): a second idea
// of normalization here would give "Bob@x" and "bob@x" separate budgets.
func customerEmailSubject(scope, email string) string {
	return scope + commercestore.NormalizeCustomerEmail(email)
}

func writeTooManyRequests(w http.ResponseWriter) {
	// Retry-After in seconds. It tells a legitimate buyer what to do, and it tells
	// an enumerator nothing they could not measure with a clock.
	w.Header().Set("Retry-After", retryAfterSeconds)
	write(w, http.StatusTooManyRequests, map[string]string{"error": customerTooManyRequests})
}

// One token's worth of refill at the SUBJECT rate, which is the tighter of the
// two — advertising the looser one would send a buyer back too early.
var retryAfterSeconds = strconv.Itoa(int(customerSubjectWindow.Seconds()) / customerSubjectBurst)

// clientIP is the source key. The body moved to shared/go/httpx when catalog's
// staff login needed the same key (ai-review S4); everything that made it correct
// — the gateway strips inbound X-Forwarded-*, the LAST element is the one the
// nearest proxy wrote — is documented there, along with the residual.
func clientIP(r *http.Request) string { return httpx.ClientIP(r) }
