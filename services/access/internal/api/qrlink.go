package api

// Short-lived signed QR image links (ai-review S2).
//
// # What was wrong
//
// `GET /orders/{ref}/tickets/{ticket}/qr.png` checked nothing but the UUID
// syntax. The order ref has 122 bits of entropy so it is not enumerable — the
// exposure is that the system TREATS the ref as shareable: it is returned from
// checkout, from wallet reads and from claim redirects, it travels over plain
// HTTP today, and it lands in browser history, referrer headers and proxy logs.
// Anyone who ended up holding a QR image URL held a printable ticket,
// indefinitely, with no expiry and no second check.
//
// # What was decided, and what was not
//
// The order ref stays the bundle's credential. That is a product decision, not an
// oversight: forwarding "here are your tickets" to the person you bought them for
// is a feature every real ticketing product has, and an account-bound bundle
// would break guest delivery, which is this system's default.
//
// What changes is the IMAGE link. The bundle page mints a fresh signed URL for
// each ticket every time it is loaded, and the signature expires. A forwarded
// bundle link keeps working — the recipient loads it and gets fresh links — while
// a leaked *image* URL stops working, and image URLs are what get screenshotted,
// hot-linked, cached by an image proxy and written into an access log.
//
// # The adversary, named (ADR-021)
//
// This bounds someone who obtains an image URL — from a log, a referrer, a
// screenshot, a shoulder. It does NOT bound someone who obtains the order ref:
// they hold the bundle and can mint their own links, exactly as the buyer does.
// Closing that is a different decision (account-bound delivery) and TKT-222's
// authenticated read is the shape it would take.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// qrLinkTTL is how long a minted image link stays valid.
//
// Ten minutes: long enough to load a bundle page, let the images render, and
// print or screenshot them on a slow connection; short enough that a URL captured
// in a log or a referrer is dead before anyone reads the log. The bundle page is
// the renewal mechanism, so a buyer never meets this bound — only a link that
// outlived its page does.
const qrLinkTTL = 10 * time.Minute

// qrLinkSigner mints and verifies the links. A nil signer means the service has
// no link key configured, which is refused at startup — see main.go. It is a
// distinct value from every signing key in the platform: this proves "this URL
// was minted by us recently", which is not the claim the QR credential or the
// lifecycle trail makes, and one key making three claims is how a leak in the
// cheapest of them costs the most expensive.
type qrLinkSigner struct{ key []byte }

// sign produces the signature bytes for one (order, ticket, expiry) triple.
//
// The three fields are joined with a separator that cannot appear in any of them
// (UUIDs and decimal digits), so no two distinct triples share a signed message.
// Without that, "order A ticket B" and "order AB ticket ..." could canonicalise
// to the same bytes — the classic concatenation ambiguity, and the reason
// CanonicalHead in the lifecycle package is written the same way.
func (s qrLinkSigner) sign(ref, ticket uuid.UUID, expiry int64) []byte {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(ref.String() + "\n" + ticket.String() + "\n" + strconv.FormatInt(expiry, 10)))
	return mac.Sum(nil)
}

// mint returns the query string for a fresh link, including the leading "?".
func (s qrLinkSigner) mint(ref, ticket uuid.UUID, now time.Time) string {
	expiry := now.Add(qrLinkTTL).Unix()
	return "?exp=" + strconv.FormatInt(expiry, 10) +
		"&sig=" + base64.RawURLEncoding.EncodeToString(s.sign(ref, ticket, expiry))
}

// verify reports whether a request carries a live signature for this ticket.
//
// The signature is checked BEFORE the expiry, and with a constant-time compare.
// Checking expiry first would answer "that signature was fine, it is just old" to
// a forged request, which is a distinguishable answer an attacker can grind
// against; and `==` on a MAC returns on the first wrong byte, which answers how
// much of a guess was right. This is the same ordering the customer assertion
// already uses (services/commerce/internal/api/assertion.go).
func (s qrLinkSigner) verify(r *http.Request, ref, ticket uuid.UUID, now time.Time) bool {
	expiry, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	if err != nil {
		return false
	}
	presented, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("sig"))
	if err != nil {
		return false
	}
	if !hmac.Equal(presented, s.sign(ref, ticket, expiry)) {
		return false
	}
	return now.Unix() <= expiry
}
