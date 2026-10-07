package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

var (
	fpOrg  = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	fpID   = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	fpSlot = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	fpTT   = uuid.MustParse("44444444-4444-4444-8444-444444444444")
)

// TKT-313 COS2. The fixed-format kinds keep their EXACT bytes. Each row calls the wrapper the
// production operation calls, so a changed kind tag or argument order goes red here; the
// smoke test TestStaffReplaysStoreTheWrapperBytes pins that the operations call them. op-convert and grp-draw are
// the ones that matter most: ADR-023 repairs a crashed staff sale by REPLAYING them with the
// same key, so a rehash makes a repair that spans a deploy answer 409 and strands a committed
// carve. These literals were captured from the algorithm before TKT-313; a change to any of
// them is a stored-fingerprint migration, not a test update.
func TestOpFingerprintIsByteIdenticalForFixedFormatKinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"op-convert", opConvertFingerprint(fpOrg, fpID, fpTT, fpSlot, 3, 2500, "EUR"), "96075b04d05bdadc9ff63ac9eeeb8c55bed1512d1a7bf2794f54309eb883e1ae"},
		{"grp-draw", groupDrawFingerprint(fpOrg, fpID, fpTT, fpSlot, 3, 2500, "EUR"), "045a1dad8b1beb2d0d2572b128c16820c6ced89c61127a3631df4707ed365c21"},
		{"op-place", opPlaceFingerprint(fpOrg, fpSlot, 3, "house", "front of house"), "fc841570567b6cae2612e6080b998e1b94cfd582d6460058c582607d3a63ddf5"},
		{"op-release", opReleaseFingerprint(fpOrg, fpID, 3), "eae704c679e3e6b267bcca92dfdd800544023b05d7b342a8285162295bf1af08"},
		{"refund-return", refundReturnFingerprint(fpOrg, fpID, 3), "47e46677c028810975fdce7dff39acd2cb339a7d668f1fd02e6e2fefc0086896"},
		{"adjust-capacity", adjustCapacityFingerprint(fpOrg, fpSlot, 80), "82c8c72fc1895426d5cceedc619f7dd74b6767726e89acd59060e92b32424870"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s fingerprint = %s, want %s (byte-identical to the pre-TKT-313 algorithm)", tc.name, tc.got, tc.want)
		}
	}
}

// TKT-313. grp-place carries two free-text parts (counterparty, channel) between fixed ones,
// and the space-joined `%v` form let a shifted boundary produce identical bytes. The framed
// form must separate the constructed pair, and must never equal ANY legacy grp-place hash.
func TestGroupPlaceFingerprintFramesItsFreeTextParts(t *testing.T) {
	early := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	a := groupPlaceFingerprint(fpOrg, fpSlot, 5, "Acme 2027-01-01T00:00:00Z", late, "", "")
	b := groupPlaceFingerprint(fpOrg, fpSlot, 5, "Acme", early, "2027-06-01T00:00:00Z ", "")
	if a == b {
		t.Fatal("two different group placements (a space boundary shifted between counterparty, expiry and channel) hash identically")
	}
	// The counterparty needs its own frame, not just the channel's. Unframed, the counterparty
	// can swallow the expiry and the channel's length prefix: A puts a fake "<expiry> 0:" inside
	// its channel, B folds A's expiry and channel prefix into its counterparty.
	chA := "z " + early.Format(time.RFC3339Nano) + " 0:"
	cpB := fmt.Sprintf("Acme %s %d:z", late.Format(time.RFC3339Nano), len(chA))
	if groupPlaceFingerprint(fpOrg, fpSlot, 5, "Acme", late, chA, "") == groupPlaceFingerprint(fpOrg, fpSlot, 5, cpB, early, "", "") {
		t.Fatal("a counterparty that swallows the expiry and the channel's length prefix hashes like a different request")
	}
	// The channel is framed too, although it is the last free-text part: the presale suffix
	// follows it, so an unframed channel spelled like a suffix would match a coded request.
	coded := groupPlaceFingerprint(fpOrg, fpSlot, 5, "Acme", late, "agency", "CODE")
	if coded == groupPlaceFingerprint(fpOrg, fpSlot, 5, "Acme", late, "agency c6:agency:p4:CODE", "") {
		t.Fatal("a code-less placement whose channel is spelled like the presale suffix hashes like a coded one")
	}
	// Without the version tag, a framed request with counterparty "Acme" and no channel is
	// the same byte string as a LEGACY row whose counterparty was literally "4:Acme" and
	// channel "0:" — and a legacy row is what a retry spanning the deploy meets.
	legacy := opFingerprint("grp-place", fpOrg, fpSlot, int32(5), "4:Acme", late.Format(time.RFC3339Nano), "0:")
	if groupPlaceFingerprint(fpOrg, fpSlot, 5, "Acme", late, "", "") == legacy {
		t.Fatal("a framed fingerprint equals a legacy one: the version tag is not separating them")
	}
	// Identical requests still hash identically (the replay must keep working).
	if coded != groupPlaceFingerprint(fpOrg, fpSlot, 5, "Acme", late, "agency", "CODE") {
		t.Fatal("the same request hashed differently")
	}
}
