package store

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Fixed inputs for the pure fingerprint tests. The expected pre-hash strings below
// are written by hand from these values. They are never produced by fingerprint().
var (
	fpOrg      = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	fpSlot     = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	fpTT       = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	fpReseller = uuid.MustParse("44444444-4444-4444-4444-444444444444")
)

// fpOf calls fingerprint() with the fixed organizer, slot, ticket type, quantity and
// unit amount. Only the fields that the tests vary are parameters.
func fpOf(currency, channel, code string, reseller uuid.UUID) string {
	return fingerprint(fpOrg, fpSlot, fpTT, 2, 1500, currency, channel, code, reseller)
}

// sha256Hex hashes a hand-written pre-hash string. It is the expected value.
func sha256Hex(s string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

// KNOWN COLLISIONS. Each pair below is two DIFFERENT requests with the SAME fingerprint.
//
// The channel is appended without a frame when there is no presale code, so the
// channel can spell the frame that a code would add. The currency is not framed
// either, so the currency can spell it too. TKT-510 measured the first pair.
//
// The encoding stays as it is. A change rehashes the stored claims, and every
// in-flight retry stops replaying. Replay is safe because CreateHold also compares
// the stored channel and currency with the request (store.go, the existing-row branch).
// The smoke tests in fingerprint_replay_smoke_test.go prove that compare.
//
// If this test fails, the encoding changed. Update ADR-064 §9 and the comment on
// fingerprint(). Do not delete this test.
func TestFingerprintCollisionsAcrossDifferentChannelsArePinned(t *testing.T) {
	type req struct{ currency, channel, code string }
	pairs := []struct {
		name string
		a, b req
	}{
		{
			// The TKT-510 pair: a code-less channel that spells A's code frame.
			name: "code-less channel spells the code frame",
			a:    req{"EUR", "agency", "CODE"},
			b:    req{"EUR", "agency:c6:agency:p4:CODE", ""},
		},
		{
			// Both sides carry a code. The code-bearing channel is framed, but the
			// bare ":"+channel before the frame is not.
			name: "both sides carry a code",
			a:    req{"EUR", "a", ":c11:a:c1:a:p21::p1:z"},
			b:    req{"EUR", "a:c1:a:p21:", "z"},
		},
		{
			// An empty channel adds no ":"+channel, so a code-less channel can spell
			// the empty channel's code frame.
			name: "empty channel with a code",
			a:    req{"EUR", "", "K"},
			b:    req{"EUR", "c0::p1:K", ""},
		},
		{
			// The currency is the field before the channel, and it is not framed.
			// The handler limits it to EUR or "", but CreateHold does not.
			name: "currency spells the code frame",
			a:    req{"EUR", "", "K"},
			b:    req{"EUR:c0::p1:K", "", ""},
		},
	}
	for _, p := range pairs {
		for _, reseller := range []uuid.UUID{uuid.Nil, fpReseller} {
			t.Run(fmt.Sprintf("%s/reseller=%v", p.name, reseller != uuid.Nil), func(t *testing.T) {
				if p.a == p.b {
					t.Fatal("the pair is one request; it cannot show a collision")
				}
				x := fpOf(p.a.currency, p.a.channel, p.a.code, reseller)
				y := fpOf(p.b.currency, p.b.channel, p.b.code, reseller)
				if x != y {
					t.Fatalf("%+v and %+v no longer collide. The fingerprint encoding changed: "+
						"stored claims are rehashed and their retries stop replaying. Update "+
						"ADR-064 §9 and the fingerprint() comment; do not delete this test.", p.a, p.b)
				}
			})
		}
	}
}

// With the SAME channel and currency, two different requests must not collide.
//
// This is the property that CreateHold's replay depends on: it compares the channel
// and currency itself, and the fingerprint must tell apart everything else.
func TestFingerprintSeparatesRequestsOnTheSameChannel(t *testing.T) {
	type req struct {
		channel, code string
		reseller      uuid.UUID
	}
	pairs := []struct {
		name string
		a, b req
	}{
		{"empty channel, with and without a code",
			req{"", "", uuid.Nil}, req{"", "K", uuid.Nil}},
		{"code containing a channel frame",
			req{"presale", ":c7:presale:p1:V", uuid.Nil}, req{"presale", "V", uuid.Nil}},
		{"code containing a code frame",
			req{"presale", "V:p1:W", uuid.Nil}, req{"presale", "V", uuid.Nil}},
		{"code containing a reseller frame, without vs with a reseller",
			req{"presale", "V:r36:" + fpReseller.String(), uuid.Nil}, req{"presale", "V", fpReseller}},
		{"code lengths 9 and 10",
			req{"presale", strings.Repeat("x", 9), uuid.Nil}, req{"presale", strings.Repeat("x", 10), uuid.Nil}},
		{"code lengths 99 and 100",
			req{"presale", strings.Repeat("x", 99), uuid.Nil}, req{"presale", strings.Repeat("x", 100), uuid.Nil}},
		{"code length 10 that starts like a length 9 frame",
			req{"presale", "123456789", uuid.Nil}, req{"presale", "9:12345678", uuid.Nil}},
		{"without and with a reseller, code-less",
			req{"presale", "", uuid.Nil}, req{"presale", "", fpReseller}},
		{"without and with a reseller, with a code",
			req{"presale", "V", uuid.Nil}, req{"presale", "V", fpReseller}},
		{"empty channel, without and with a reseller",
			req{"", "", uuid.Nil}, req{"", "", fpReseller}},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			if p.a.channel != p.b.channel {
				t.Fatal("this test is about ONE channel; the pair has two")
			}
			if fpOf("EUR", p.a.channel, p.a.code, p.a.reseller) == fpOf("EUR", p.b.channel, p.b.code, p.b.reseller) {
				t.Fatalf("%+v and %+v share a channel and collide: a retry of one would replay the other", p.a, p.b)
			}
		})
	}
	// The channel c0 without a code against the empty channel with a code. The two
	// channels differ, but the pair is close to the third collision above, so it is
	// pinned here as a pair that must stay apart.
	if fpOf("EUR", "c0", "", uuid.Nil) == fpOf("EUR", "", "K", uuid.Nil) {
		t.Fatal(`(channel "c0", no code) and (channel "", code "K") collide`)
	}
}

// GOLDEN PRE-HASH STRINGS for the code-bearing and reseller-bearing classes.
//
// Each expected string is written out by hand from the format, then hashed here
// with crypto/sha256. A fixture built from fingerprint() could not detect a change to
// fingerprint(), so no expected value comes from it.
//
// TestFingerprintStaysByteIdenticalWithoutAPresaleCode pins the code-less classes.
// If one of these literals needs a change, that is a wire-compatibility break: the
// stored claims of that class stop replaying.
func TestFingerprintGoldenPreHashForFramedClasses(t *testing.T) {
	const base = "11111111-1111-1111-1111-111111111111:22222222-2222-2222-2222-222222222222:" +
		"33333333-3333-3333-3333-333333333333:2:1500:EUR"
	cases := []struct {
		name          string
		channel, code string
		reseller      uuid.UUID
		preHash       string
	}{
		{"colon-free channel and a code", "presale", "VIP", uuid.Nil,
			base + ":presale:c7:presale:p3:VIP"},
		{"channel with colons and a code", "a:b", "VIP", uuid.Nil,
			base + ":a:b:c3:a:b:p3:VIP"},
		{"channel and a reseller", "presale", "", fpReseller,
			base + ":presale:r36:44444444-4444-4444-4444-444444444444"},
		{"channel, a code and a reseller", "presale", "VIP", fpReseller,
			base + ":presale:c7:presale:p3:VIP:r36:44444444-4444-4444-4444-444444444444"},
		{"empty channel and a code", "", "K", uuid.Nil,
			base + ":c0::p1:K"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, want := fpOf("EUR", c.channel, c.code, c.reseller), sha256Hex(c.preHash); got != want {
				t.Fatalf("fingerprint changed for this class:\n got %s\nwant %s (sha256 of %q)\n"+
					"Stored claims of this class stop replaying.", got, want, c.preHash)
			}
		})
	}
}
