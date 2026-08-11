package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ai-review S2. The QR image endpoint used to serve a printable ticket to anyone
// holding the URL, forever. It now needs a signature this service minted, and the
// signature expires.
//
// The tests below are written against the signer rather than the handler because
// the handler needs a database; the handler's use of it is one line
// (`if !s.qrLinks.verify(...)`) and its own test is the smoke suite's fetch of a
// real qr_url. What matters here is that the predicate cannot be satisfied by
// anything but a fresh, matching signature.
func TestQRLinkSignatureIsBoundToItsTicketAndExpires(t *testing.T) {
	signer := qrLinkSigner{key: []byte("link-key")}
	ref, ticket := uuid.New(), uuid.New()
	now := time.Unix(1_800_000_000, 0)

	live := httptest.NewRequest(http.MethodGet, "/x"+signer.mint(ref, ticket, now), nil)
	if !signer.verify(live, ref, ticket, now) {
		t.Fatal("a freshly minted link does not verify")
	}

	// Still valid a second before the bound, dead a second after. Asserted on
	// both sides: a check that only ever tested "much later" would pass with the
	// comparison inverted.
	if !signer.verify(live, ref, ticket, now.Add(qrLinkTTL-time.Second)) {
		t.Error("a link expired before its TTL")
	}
	if signer.verify(live, ref, ticket, now.Add(qrLinkTTL+time.Second)) {
		t.Error("an expired link still verifies — the whole point is that a leaked URL dies")
	}

	// Bound to THIS ticket in THIS order. A link for one ticket must not open
	// another, or a buyer with one legitimate link holds the whole event.
	if signer.verify(live, ref, uuid.New(), now) {
		t.Error("a link for one ticket opened another")
	}
	if signer.verify(live, uuid.New(), ticket, now) {
		t.Error("a link for one order opened another")
	}

	// A different key mints nothing this service accepts.
	other := qrLinkSigner{key: []byte("someone-elses-key")}
	forged := httptest.NewRequest(http.MethodGet, "/x"+other.mint(ref, ticket, now), nil)
	if signer.verify(forged, ref, ticket, now) {
		t.Error("a signature from a foreign key was accepted")
	}

	// Absent, malformed and empty parameters all fail closed. An unsigned request
	// is exactly the state this finding is about.
	for _, query := range []string{"", "?exp=&sig=", "?exp=abc&sig=abc", "?exp=9999999999", "?sig=AAAA", "?exp=9999999999&sig=!!!not-base64"} {
		if signer.verify(httptest.NewRequest(http.MethodGet, "/x"+query, nil), ref, ticket, now) {
			t.Errorf("query %q was accepted", query)
		}
	}
}

// The signed message must not be ambiguous across fields: two different triples
// that canonicalise to the same bytes would let one link verify for another
// ticket. UUIDs cannot contain the separator, so this is a guard against a future
// edit that swaps it for something that can.
func TestQRLinkSignedMessageSeparatorIsNotInTheFields(t *testing.T) {
	signer := qrLinkSigner{key: []byte("link-key")}
	ref, ticket := uuid.New(), uuid.New()
	if strings.ContainsAny(ref.String()+ticket.String(), "\n") {
		t.Fatal("a UUID rendered a newline: the signed form is ambiguous")
	}
	if len(signer.sign(ref, ticket, 1)) == 0 {
		t.Fatal("signature is empty")
	}
}
