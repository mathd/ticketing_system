package api

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var testAssertionKey = customerAssertionKey("tkt-221 test signing key, not a production value")

func TestCustomerAssertionRoundTrips(t *testing.T) {
	id := uuid.New()
	now := time.Unix(1_800_000_000, 0)

	token := mintCustomerAssertion(testAssertionKey, id, now.Add(CustomerAssertionTTL))

	got, err := verifyCustomerAssertion(testAssertionKey, token, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got != id {
		t.Fatalf("customer = %s, want %s", got, id)
	}
}

// The whole point of the token. Every one of these is a caller trying to name
// somebody else, and every one must be the SAME refusal — a caller who can tell
// "expired" from "forged" learns which half of their guess was right.
func TestCustomerAssertionRefusesEveryTamperedForm(t *testing.T) {
	alice, mallory := uuid.New(), uuid.New()
	now := time.Unix(1_800_000_000, 0)
	valid := mintCustomerAssertion(testAssertionKey, alice, now.Add(CustomerAssertionTTL))
	parts := strings.Split(valid, ".")

	for _, tc := range []struct {
		name  string
		token string
	}{
		// The headline attack: take a token you legitimately hold and point it
		// at someone else's account.
		{"another customer's id, original signature", strings.Join([]string{parts[0], mallory.String(), parts[2], parts[3]}, ".")},
		// The other direction: extend your own expiry.
		{"expiry pushed out, original signature", strings.Join([]string{parts[0], parts[1], strconv.FormatInt(now.Add(1000*time.Hour).Unix(), 10), parts[3]}, ".")},
		{"signature from a different key", mintCustomerAssertion(customerAssertionKey("a different key entirely"), alice, now.Add(CustomerAssertionTTL))},
		{"signature truncated", strings.Join([]string{parts[0], parts[1], parts[2], parts[3][:len(parts[3])-2]}, ".")},
		{"version bumped", strings.Join([]string{"v2", parts[1], parts[2], parts[3]}, ".")},
		{"a field removed", strings.Join(parts[:3], ".")},
		{"empty", ""},
		{"not a token at all", "hello"},
		{"unparseable customer id", strings.Join([]string{parts[0], "not-a-uuid", parts[2], parts[3]}, ".")},
		// The nil uuid is what a zero value looks like, not an identity. A
		// correctly SIGNED one is the interesting case: it can only come from a
		// construction bug on this side, and it must not arrive as a principal.
		{"the nil uuid, correctly signed", mintCustomerAssertion(testAssertionKey, uuid.Nil, now.Add(CustomerAssertionTTL))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := verifyCustomerAssertion(testAssertionKey, tc.token, now); !errors.Is(err, ErrCustomerAssertionInvalid) {
				t.Fatalf("want ErrCustomerAssertionInvalid, got %v", err)
			}
		})
	}
}

func TestCustomerAssertionExpires(t *testing.T) {
	id := uuid.New()
	now := time.Unix(1_800_000_000, 0)
	expiry := now.Add(CustomerAssertionTTL)
	token := mintCustomerAssertion(testAssertionKey, id, expiry)

	if _, err := verifyCustomerAssertion(testAssertionKey, token, expiry.Add(-time.Second)); err != nil {
		t.Fatalf("one second before expiry must still verify: %v", err)
	}
	// Exactly at the deadline is expired: the window is half-open, so "valid
	// until T" cannot be read two ways.
	if _, err := verifyCustomerAssertion(testAssertionKey, token, expiry); !errors.Is(err, ErrCustomerAssertionInvalid) {
		t.Fatalf("at expiry: want ErrCustomerAssertionInvalid, got %v", err)
	}
}

// An unconfigured key must not produce a self-consistent MAC that every forged
// token then satisfies. Startup refuses an empty key, so this is defence against a
// future construction path that forgets — the failure it prevents is total.
func TestCustomerAssertionRefusesEverythingWhenTheKeyIsUnset(t *testing.T) {
	id := uuid.New()
	now := time.Unix(1_800_000_000, 0)
	forged := mintCustomerAssertion(customerAssertionKey(""), id, now.Add(CustomerAssertionTTL))

	if _, err := verifyCustomerAssertion(customerAssertionKey(""), forged, now); !errors.Is(err, ErrCustomerAssertionInvalid) {
		t.Fatalf("an unset key must verify nothing, got %v", err)
	}
}

// Absent is GUEST — a first-class answer, not a failure. Present-but-invalid is
// an error, and deliberately NOT a silent downgrade to guest: hiding a failed
// attribution would leave the buyer with a purchase missing from their account and
// nothing to point at.
func TestCustomerFromRequestSeparatesGuestFromForged(t *testing.T) {
	id := uuid.New()
	now := time.Unix(1_800_000_000, 0)

	for _, tc := range []struct {
		name   string
		header string
	}{{"absent", ""}, {"whitespace only", "   "}} {
		t.Run("guest: "+tc.name, func(t *testing.T) {
			got, err := customerFromRequest(testAssertionKey, tc.header, now)
			if err != nil {
				t.Fatalf("a guest checkout must not error: %v", err)
			}
			if got.Valid {
				t.Fatalf("a guest checkout must not be attributed, got %s", got.UUID)
			}
		})
	}

	got, err := customerFromRequest(testAssertionKey, mintCustomerAssertion(testAssertionKey, id, now.Add(CustomerAssertionTTL)), now)
	if err != nil || !got.Valid || got.UUID != id {
		t.Fatalf("a valid assertion must attribute: %v %v", got, err)
	}

	if _, err := customerFromRequest(testAssertionKey, "v1.garbage.0.x", now); !errors.Is(err, ErrCustomerAssertionInvalid) {
		t.Fatalf("a forged assertion must NOT be downgraded to guest, got %v", err)
	}
}

// The assertion's lifetime is the storefront session's, and nothing enforces the
// equality across two languages — so it is pinned here and stated in the ADR.
// A change on one side without the other reintroduces the 401-at-the-payment-button
// failure this coupling exists to remove (plan-review F1).
func TestCustomerAssertionTTLMatchesTheStorefrontSessionTTL(t *testing.T) {
	const storefrontSessionTTLMillis = 8 * 60 * 60 * 1000 // web/storefront/src/lib/session.ts

	if CustomerAssertionTTL.Milliseconds() != storefrontSessionTTLMillis {
		t.Fatalf("assertion TTL %s != storefront SESSION_TTL_MS %dms — a buyer would be refused at "+
			"the payment button when the shorter of the two ran out",
			CustomerAssertionTTL, storefrontSessionTTLMillis)
	}
}
