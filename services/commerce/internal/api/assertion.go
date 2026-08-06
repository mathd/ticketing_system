package api

// Customer checkout assertions (TKT-221 / US-A2). See ADR-049 § the TKT-221
// amendment.
//
// The problem this exists for: TKT-220 put customer accounts in commerce and the
// session in the STOREFRONT process, and the two never speak about a specific
// signed-in buyer. So when a checkout arrives, commerce has no way to know who is
// behind it. The two obvious answers are both wrong — a `customer_id` in the body
// is forgeable by anyone who can reach the public gateway, and a service
// credential in the storefront is the trust boundary ADR-049 §1 refused, because
// that process serves anonymous internet traffic.
//
// So commerce signs a statement it can verify later without storing anything:
// "this is customer X, until T". The buyer earns it by presenting their password;
// the storefront keeps it in its in-process session, server-side only, and
// forwards it on the checkout it proxies.
//
// What it is NOT: a session, a refresh token, or a general-purpose credential. It
// authorizes exactly one thing — naming a customer on a checkout — and it is a
// BEARER token until it expires. ADR-021's question, answered: this stops an
// internet caller from attributing an order to a stranger. It stops nothing at all
// against someone who has the signing key, the storefront's memory, or the
// database.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrCustomerAssertionInvalid is the ONLY verification failure reported. Expired,
// forged, malformed and truncated are one answer: a caller probing the difference
// learns which of their guesses was structurally right, and none of the four
// should ever reach a well-behaved client anyway.
var ErrCustomerAssertionInvalid = errors.New("invalid customer assertion")

// assertionVersion prefixes every token. It costs three bytes and it is what
// makes changing the format later a migration rather than a mystery: an old token
// presented after a format change is refused as invalid, not misparsed.
const assertionVersion = "v1"

// customerAssertionKey is the HMAC key. Commerce-only, and deliberately not
// INTERNAL_SERVICE_TOKEN (one shared value opening every service's internal
// surface) or COMMERCE_STAFF_WRITE_TOKEN (a money-moving refund). main.go refuses
// to start when it equals either.
type customerAssertionKey []byte

// mintCustomerAssertion produces `v1.<customer id>.<unix expiry>.<mac>`.
//
// The payload is signed, not encrypted: a holder can read which customer and when
// it dies. That is fine — they are the customer — and it keeps the token
// debuggable. What they cannot do is change either field, because the MAC covers
// the exact bytes that are compared on the way back in.
func mintCustomerAssertion(key customerAssertionKey, customerID uuid.UUID, expiresAt time.Time) string {
	payload := assertionVersion + "." + customerID.String() + "." + strconv.FormatInt(expiresAt.Unix(), 10)
	return payload + "." + assertionMAC(key, payload)
}

func assertionMAC(key customerAssertionKey, payload string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyCustomerAssertion returns the customer the token names, or
// ErrCustomerAssertionInvalid.
//
// Order matters: the MAC is checked BEFORE the expiry is trusted, because the
// expiry is attacker-controlled until the signature says otherwise. Checking
// expiry first would mean parsing and believing an unauthenticated number — and
// an attacker who could pick it would simply pick one far in the future.
func verifyCustomerAssertion(key customerAssertionKey, token string, now time.Time) (uuid.UUID, error) {
	if len(key) == 0 {
		// Fail closed on an unconfigured key. Startup already refuses that, so
		// this defends against a future construction path that forgets — without
		// it, an empty key would still produce a self-consistent MAC and every
		// forged token would verify.
		return uuid.Nil, ErrCustomerAssertionInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != assertionVersion {
		return uuid.Nil, ErrCustomerAssertionInvalid
	}
	payload := strings.Join(parts[:3], ".")
	// Constant-time: the comparison target is derived from a secret, and an
	// early-exit compare leaks how much of a forged MAC was right.
	if subtle.ConstantTimeCompare([]byte(parts[3]), []byte(assertionMAC(key, payload))) != 1 {
		return uuid.Nil, ErrCustomerAssertionInvalid
	}
	customerID, err := uuid.Parse(parts[1])
	if err != nil {
		return uuid.Nil, ErrCustomerAssertionInvalid
	}
	expiry, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return uuid.Nil, ErrCustomerAssertionInvalid
	}
	if !now.Before(time.Unix(expiry, 0)) {
		return uuid.Nil, ErrCustomerAssertionInvalid
	}
	return customerID, nil
}

// CustomerAssertionTTL is how long a minted assertion lives.
//
// It is EXACTLY the storefront's session lifetime, and the equality is the point
// (plan-review F1). If the assertion were shorter, a buyer who signed in, browsed,
// took a hold on a short TTL and reached the payment button would be refused
// there — at the single worst moment in the flow — with no way back except signing
// in again while their hold expires. The storefront cannot re-mint one: it holds
// the principal, not the password.
//
// Coupling them makes "the session is alive" and "the assertion is valid" one
// statement instead of two that can disagree. It costs nothing in exposure: the
// assertion lives only inside that in-process session, so it is already
// unreachable once the session is gone.
//
// Keep this equal to SESSION_TTL_MS in web/storefront/src/lib/session.ts. They are
// two constants in two languages; there is no mechanism that enforces the
// equality, which is why it is written down in both places and in the ADR.
const CustomerAssertionTTL = 8 * time.Hour

// assertionHeader carries the token. A header, not the body: `Checkout` is
// `additionalProperties: false` and adding a field there would change the guest
// request shape, which this ticket must not do.
const assertionHeader = "X-Customer-Assertion"

// customerFromRequest resolves the assertion header, if one is present.
//
// Absent means GUEST, and that is a first-class answer rather than a failure:
// checkout without an account is the default this system was built around.
// Present-but-invalid is NOT downgraded to guest — see the caller. Silently
// turning a failed attribution into a guest order would hide it from the buyer,
// who would then not find the purchase in their account and have no error to point
// at.
func customerFromRequest(key customerAssertionKey, header string, now time.Time) (uuid.NullUUID, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return uuid.NullUUID{}, nil
	}
	id, err := verifyCustomerAssertion(key, header, now)
	if err != nil {
		return uuid.NullUUID{}, fmt.Errorf("checkout assertion: %w", err)
	}
	return uuid.NullUUID{UUID: id, Valid: true}, nil
}

// WithCustomerAssertionKey supplies the signing key. A setter rather than another
// positional argument to New, for the same reason WithAccess is one: every
// existing caller keeps compiling, and a server constructed without it verifies
// nothing rather than verifying everything (see the empty-key check above).
func (s *Server) WithCustomerAssertionKey(key string) *Server {
	s.assertionKey = customerAssertionKey(key)
	return s
}

// mintForPrincipal is the one place an assertion is created, so the TTL cannot
// drift between the registration and sign-in paths.
func (s *Server) mintForPrincipal(customerID uuid.UUID) string {
	if len(s.assertionKey) == 0 {
		return ""
	}
	return mintCustomerAssertion(s.assertionKey, customerID, time.Now().Add(CustomerAssertionTTL))
}
