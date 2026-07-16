package lifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The canonical forms below are pinned as literal strings, not built from the
// functions under test. A fixture assembled from the type it claims to prove
// encodes the behaviour it was meant to check and cannot fail (ADR-017 §5b′,
// TKT-61). Everything ADR-021 §D8 names — timestamp precision, ordering ties,
// UUID formatting, domain and version bytes — is readable in these literals.
//
// Changing any byte here is a canonical-version migration, not a test update.

var (
	goldTicket    = uuid.MustParse("10000000-0000-0000-0000-000000000001")
	goldOrder     = uuid.MustParse("10000000-0000-0000-0000-000000000002")
	goldOrganizer = uuid.MustParse("10000000-0000-0000-0000-000000000003")
	goldSlot      = uuid.MustParse("10000000-0000-0000-0000-000000000004")
	goldEvent     = uuid.MustParse("10000000-0000-0000-0000-000000000005")
)

const goldEventCanonical = "access-lifecycle/event/v1\n" +
	"10000000-0000-0000-0000-000000000001\n" +
	"10000000-0000-0000-0000-000000000002\n" +
	"10000000-0000-0000-0000-000000000003\n" +
	"10000000-0000-0000-0000-000000000004\n" +
	"1\n" +
	"10000000-0000-0000-0000-000000000005\n" +
	"issued\n" +
	"2026-07-12T14:30:00Z"

func goldenEvent() Event {
	return Event{
		TicketID: goldTicket, OrderID: goldOrder, OrganizerID: goldOrganizer, SlotID: goldSlot,
		Sequence: 1, EventID: goldEvent, Type: "issued",
		OccurredAt: time.Date(2026, time.July, 12, 14, 30, 0, 0, time.UTC),
	}
}

func TestCanonicalEventGolden(t *testing.T) {
	if got := string(CanonicalEvent(goldenEvent())); got != goldEventCanonical {
		t.Fatalf("canonical event drifted.\n got: %q\nwant: %q", got, goldEventCanonical)
	}
}

// PostgreSQL timestamptz stores microseconds. Signing a nanosecond value would
// canonicalize different bytes after a reload than it did at write time, so the
// truncation has to happen before signing, not at the database boundary.
func TestCanonicalEventTruncatesNanosecondsToMicroseconds(t *testing.T) {
	e := goldenEvent()
	e.OccurredAt = time.Date(2026, time.July, 12, 14, 30, 0, 123456789, time.UTC)
	if got := string(CanonicalEvent(e)); !strings.HasSuffix(got, "\n2026-07-12T14:30:00.123456Z") {
		t.Fatalf("nanoseconds survived canonicalization: %q", got)
	}
}

func TestCanonicalEventNormalizesToUTC(t *testing.T) {
	e := goldenEvent()
	e.OccurredAt = time.Date(2026, time.July, 12, 16, 30, 0, 0, time.FixedZone("CEST", 2*60*60))
	if got := string(CanonicalEvent(e)); got != goldEventCanonical {
		t.Fatalf("zone leaked into canonical form.\n got: %q\nwant: %q", got, goldEventCanonical)
	}
}

// Equal occurred_at is real: Issue writes the issued event with the ticket's own
// issued_at, and a bulk backfill reads rows ordered by (occurred_at, id). The tie
// break is part of the signed order, so it is pinned here.
func TestCanonicalEventBindsSequenceNotTimestampOrder(t *testing.T) {
	a, b := goldenEvent(), goldenEvent()
	b.Sequence = 2
	if bytes.Equal(CanonicalEvent(a), CanonicalEvent(b)) {
		t.Fatal("sequence is not bound: two sequences produced identical canonical bytes")
	}
}

func TestCanonicalEventBindsEveryIdentifier(t *testing.T) {
	base := CanonicalEvent(goldenEvent())
	other := uuid.MustParse("20000000-0000-0000-0000-00000000000f")
	for name, mutate := range map[string]func(*Event){
		"ticket":    func(e *Event) { e.TicketID = other },
		"order":     func(e *Event) { e.OrderID = other },
		"organizer": func(e *Event) { e.OrganizerID = other },
		"slot":      func(e *Event) { e.SlotID = other },
		"event id":  func(e *Event) { e.EventID = other },
		"type":      func(e *Event) { e.Type = "redeemed" },
		"time":      func(e *Event) { e.OccurredAt = e.OccurredAt.Add(time.Microsecond) },
	} {
		e := goldenEvent()
		mutate(&e)
		if bytes.Equal(base, CanonicalEvent(e)) {
			t.Fatalf("%s is not bound by the canonical form", name)
		}
	}
}

// ADR-003 §D3 keeps PII and the guest reference out of the trail, and ADR-012
// makes guest_order_ref a no-store retrieval capability. Enforcing that on the
// input type means a future caller cannot pass one in by accident.
func TestEventTypeCannotCarryPIIOrGuestReference(t *testing.T) {
	rt := reflect.TypeOf(Event{})
	for _, forbidden := range []string{"BuyerID", "Buyer", "GuestOrderRef", "GuestRef", "Email", "Name"} {
		if _, found := rt.FieldByName(forbidden); found {
			t.Fatalf("Event exposes %s — ADR-003 §D3 forbids PII and the guest reference in the canonical form", forbidden)
		}
	}
}

// Independent check: recompute with the standard library rather than asserting a
// hex literal this package produced. A pasted digest would only prove the code
// still does what it did, which is the circularity the golden literals avoid.
func TestHashEntryIsSHA256OfPreviousThenCanonical(t *testing.T) {
	prev := GenesisHash()
	h := sha256.New()
	h.Write(prev)
	h.Write([]byte(goldEventCanonical))
	want := h.Sum(nil)
	if got := HashEntry(prev, []byte(goldEventCanonical)); !bytes.Equal(got, want) {
		t.Fatalf("entry hash = %x, want %x", got, want)
	}
}

func TestHashEntryChainsOnPrevious(t *testing.T) {
	canon := []byte(goldEventCanonical)
	first := HashEntry(GenesisHash(), canon)
	second := HashEntry(first, canon)
	if bytes.Equal(first, second) {
		t.Fatal("previous_hash does not feed the entry hash: identical content chained to the same digest")
	}
}

func TestGenesisHashIsThirtyTwoZeroBytes(t *testing.T) {
	if got := GenesisHash(); len(got) != HashSize || !bytes.Equal(got, make([]byte, HashSize)) {
		t.Fatalf("genesis hash = %x", got)
	}
}

const goldHeadCanonical = "access-lifecycle/head/v1\n" +
	"10000000-0000-0000-0000-000000000001\n" +
	"7\n" +
	"access-lifecycle/2026-07\n" +
	"00000000000000000000000000000000000000000000000000000000000000ff"

// ADR-021 §D5: the head signature binds ticket identity, ticket-local sequence,
// canonical version and key id — "not the head hash alone". A signature over the
// bare digest would verify against any ticket that ever reached the same head.
func TestCanonicalHeadGolden(t *testing.T) {
	head := make([]byte, HashSize)
	head[HashSize-1] = 0xff
	got := string(CanonicalHead(goldTicket, 7, "access-lifecycle/2026-07", head))
	if got != goldHeadCanonical {
		t.Fatalf("canonical head drifted.\n got: %q\nwant: %q", got, goldHeadCanonical)
	}
}

func TestCanonicalHeadBindsEveryField(t *testing.T) {
	head := make([]byte, HashSize)
	base := CanonicalHead(goldTicket, 7, "access-lifecycle/2026-07", head)
	other := make([]byte, HashSize)
	other[0] = 0x01
	cases := [][]byte{
		CanonicalHead(uuid.MustParse("20000000-0000-0000-0000-00000000000f"), 7, "access-lifecycle/2026-07", head),
		CanonicalHead(goldTicket, 8, "access-lifecycle/2026-07", head),
		CanonicalHead(goldTicket, 7, "access-lifecycle/2026-08", head),
		CanonicalHead(goldTicket, 7, "access-lifecycle/2026-07", other),
	}
	for i, c := range cases {
		if bytes.Equal(base, c) {
			t.Fatalf("case %d: field not bound by the canonical head", i)
		}
	}
}

const goldLeafCanonical = "access-lifecycle/leaf/v1\n" +
	"10000000-0000-0000-0000-000000000001\n" +
	"3\n" +
	"0000000000000000000000000000000000000000000000000000000000000000"

func TestCanonicalLeafGolden(t *testing.T) {
	if got := string(CanonicalLeaf(goldTicket, 3, make([]byte, HashSize))); got != goldLeafCanonical {
		t.Fatalf("canonical leaf drifted.\n got: %q\nwant: %q", got, goldLeafCanonical)
	}
}

func leaf(id string, seq int64, b byte) Leaf {
	h := make([]byte, HashSize)
	h[0] = b
	return Leaf{TicketID: uuid.MustParse(id), Sequence: seq, HeadHash: h}
}

func TestMerkleRootSingleLeafIsTheHashedLeaf(t *testing.T) {
	l := leaf("10000000-0000-0000-0000-000000000001", 3, 0x00)
	sum := sha256.Sum256(CanonicalLeaf(l.TicketID, l.Sequence, l.HeadHash))
	got, err := MerkleRoot([]Leaf{l})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sum[:]) {
		t.Fatalf("single-leaf root = %x, want %x", got, sum[:])
	}
}

// Recomputed with the standard library rather than with this package's own tree,
// so the test can actually disagree with the implementation.
func TestMerkleRootTwoLeavesGolden(t *testing.T) {
	a := leaf("10000000-0000-0000-0000-000000000001", 1, 0xaa)
	b := leaf("20000000-0000-0000-0000-000000000002", 2, 0xbb)
	la := sha256.Sum256(CanonicalLeaf(a.TicketID, a.Sequence, a.HeadHash))
	lb := sha256.Sum256(CanonicalLeaf(b.TicketID, b.Sequence, b.HeadHash))
	want := sha256.Sum256([]byte("access-lifecycle/node/v1\n" + hex.EncodeToString(la[:]) + "\n" + hex.EncodeToString(lb[:])))
	got, err := MerkleRoot([]Leaf{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want[:]) {
		t.Fatalf("two-leaf root = %x, want %x", got, want[:])
	}
}

// Leaves arrive in whatever order the delta query returned them. The root must
// not depend on that, or two verifiers reading the same checkpoint disagree.
func TestMerkleRootIsIndependentOfInputOrder(t *testing.T) {
	a := leaf("30000000-0000-0000-0000-000000000003", 1, 0xaa)
	b := leaf("10000000-0000-0000-0000-000000000001", 2, 0xbb)
	c := leaf("20000000-0000-0000-0000-000000000002", 3, 0xcc)
	first, err := MerkleRoot([]Leaf{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	second, err := MerkleRoot([]Leaf{c, a, b})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("root depends on input order: %x vs %x", first, second)
	}
}

func TestMerkleRootDoesNotMutateCallerSlice(t *testing.T) {
	in := []Leaf{
		leaf("30000000-0000-0000-0000-000000000003", 1, 0xaa),
		leaf("10000000-0000-0000-0000-000000000001", 2, 0xbb),
	}
	if _, err := MerkleRoot(in); err != nil {
		t.Fatal(err)
	}
	if in[0].TicketID.String() != "30000000-0000-0000-0000-000000000003" {
		t.Fatal("MerkleRoot sorted the caller's slice in place")
	}
}

// This is the guard that keeps the odd-level duplication in MerkleRoot safe.
// With duplicate-last, two distinct leaf multisets can reach one root
// (CVE-2012-2459). That is unreachable only because a ticket appears at most
// once, so the rejection below is load-bearing, not defensive tidiness.
func TestMerkleRootRejectsDuplicateTicket(t *testing.T) {
	a := leaf("10000000-0000-0000-0000-000000000001", 1, 0xaa)
	b := leaf("10000000-0000-0000-0000-000000000001", 2, 0xbb)
	if _, err := MerkleRoot([]Leaf{a, b}); err == nil {
		t.Fatal("duplicate ticket accepted: odd-level duplication makes the root ambiguous without this check")
	}
}

func TestMerkleRootRejectsEmptyLeafSet(t *testing.T) {
	if _, err := MerkleRoot(nil); err == nil {
		t.Fatal("empty checkpoint accepted; a checkpoint over nothing commits to nothing")
	}
}

func TestMerkleRootOddLevelIsStable(t *testing.T) {
	leaves := []Leaf{
		leaf("10000000-0000-0000-0000-000000000001", 1, 0xaa),
		leaf("20000000-0000-0000-0000-000000000002", 2, 0xbb),
		leaf("30000000-0000-0000-0000-000000000003", 3, 0xcc),
	}
	first, err := MerkleRoot(leaves)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MerkleRoot(leaves)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("odd-level root is not deterministic")
	}
}

const goldCheckpointCanonical = "access-lifecycle/checkpoint/v1\n" +
	"10000000-0000-0000-0000-000000000003\n" +
	"4\n" +
	"0000000000000000000000000000000000000000000000000000000000000000\n" +
	"00000000000000000000000000000000000000000000000000000000000000ab\n" +
	"2\n" +
	"access-lifecycle/2026-07\n" +
	"2026-07-12T14:30:00Z"

func goldenCheckpoint() Checkpoint {
	root := make([]byte, HashSize)
	root[HashSize-1] = 0xab
	return Checkpoint{
		OrganizerID: goldOrganizer, Sequence: 4,
		PreviousRoot: make([]byte, HashSize), Root: root, LeafCount: 2,
		KeyID:     "access-lifecycle/2026-07",
		CreatedAt: time.Date(2026, time.July, 12, 14, 30, 0, 0, time.UTC),
	}
}

func TestCanonicalCheckpointGolden(t *testing.T) {
	if got := string(CanonicalCheckpoint(goldenCheckpoint())); got != goldCheckpointCanonical {
		t.Fatalf("canonical checkpoint drifted.\n got: %q\nwant: %q", got, goldCheckpointCanonical)
	}
}

// leaf_count is signed so the tree's shape is committed, not just its root.
// Without it, the odd-level duplication would leave the shape unattested.
func TestCanonicalCheckpointBindsEveryField(t *testing.T) {
	base := CanonicalCheckpoint(goldenCheckpoint())
	for name, mutate := range map[string]func(*Checkpoint){
		"organizer":     func(c *Checkpoint) { c.OrganizerID = goldTicket },
		"sequence":      func(c *Checkpoint) { c.Sequence = 5 },
		"previous root": func(c *Checkpoint) { c.PreviousRoot = bytes.Repeat([]byte{1}, HashSize) },
		"root":          func(c *Checkpoint) { c.Root = bytes.Repeat([]byte{2}, HashSize) },
		"leaf count":    func(c *Checkpoint) { c.LeafCount = 3 },
		"key id":        func(c *Checkpoint) { c.KeyID = "access-lifecycle/2026-08" },
		"created at":    func(c *Checkpoint) { c.CreatedAt = c.CreatedAt.Add(time.Second) },
	} {
		c := goldenCheckpoint()
		mutate(&c)
		if bytes.Equal(base, CanonicalCheckpoint(c)) {
			t.Fatalf("%s is not bound by the canonical checkpoint", name)
		}
	}
}

// Each form carries its own domain prefix so bytes signed as one kind can never
// verify as another.
func TestDomainsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range []string{domainEvent, domainHead, domainLeaf, domainNode, domainCheckpoint} {
		if seen[d] {
			t.Fatalf("duplicate domain prefix %q: two canonical forms are interchangeable", d)
		}
		seen[d] = true
		if !strings.HasPrefix(d, "access-lifecycle/") || !strings.HasSuffix(d, "/v1") {
			t.Fatalf("domain %q must be namespaced and versioned", d)
		}
	}
}
