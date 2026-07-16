// Package lifecycle implements the ticket lifecycle trail's integrity scheme
// (ADR-021): the per-ticket hash chain, the per-organizer checkpoint Merkle
// root, and the Ed25519 signing that lets a third party verify both with public
// keys only.
//
// Scope, in the terms ADR-021 §The trust boundary insists on: this closes
// modification and insertion against an adversary who cannot re-sign the chain.
// It is silent against one who can. Every control here keeps its state in the
// Access database, and that database is what the adversary is defined as
// owning — so nothing in this package is rollback protection, and none of it
// may be described as such. The external attestation that would change this is
// TKT-11's.
package lifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// HashSize is the width of every hash in the trail.
const HashSize = sha256.Size

// Domain prefixes. Each canonical form carries its own, so bytes signed as one
// kind cannot verify as another, and each carries its version inline: a change
// to any format below is a canonical-version migration (ADR-021 §D8), which is
// why the version travels with the domain rather than in a separate field
// someone could forget to bump.
const (
	domainEvent      = "access-lifecycle/event/v1"
	domainHead       = "access-lifecycle/head/v1"
	domainLeaf       = "access-lifecycle/leaf/v1"
	domainNode       = "access-lifecycle/node/v1"
	domainCheckpoint = "access-lifecycle/checkpoint/v1"
)

// CanonicalVersion is the stored version of the forms above. It is persisted
// alongside every chained row so a future migration can tell which rules
// produced a given hash.
const CanonicalVersion = 1

// Event is the signed projection of a lifecycle event. It binds the ticket's
// identity (ADR-021 §D8) and deliberately has no field for buyer_id or
// guest_order_ref: ADR-003 §D3 keeps PII out of the trail, and ADR-012 makes the
// guest reference a no-store retrieval capability. Absent fields cannot be
// passed by accident.
type Event struct {
	TicketID    uuid.UUID
	OrderID     uuid.UUID
	OrganizerID uuid.UUID
	SlotID      uuid.UUID
	// Sequence is ticket-local: the chain shards per ticket, so there is no
	// organizer-wide ordering to bind (ADR-021 §Consequences names that trade).
	Sequence   int64
	EventID    uuid.UUID
	Type       string
	OccurredAt time.Time
}

// Normalize renders a timestamp the way it will come back out of PostgreSQL.
// timestamptz stores microseconds, so signing nanoseconds would canonicalize
// different bytes on reload than at write time and every verify would fail.
func Normalize(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func stamp(t time.Time) string { return Normalize(t).Format(time.RFC3339Nano) }

// CanonicalEvent renders the bytes an entry hash covers.
func CanonicalEvent(e Event) []byte {
	return fmt.Appendf(nil, "%s\n%s\n%s\n%s\n%s\n%d\n%s\n%s\n%s",
		domainEvent, e.TicketID, e.OrderID, e.OrganizerID, e.SlotID,
		e.Sequence, e.EventID, e.Type, stamp(e.OccurredAt))
}

// CanonicalHead renders the bytes a head signature covers. ADR-021 §D5 requires
// the signature to bind ticket identity, ticket-local sequence, canonical
// version and key id — "not the head hash alone", because a signature over a
// bare digest would verify against any ticket that ever reached that head.
func CanonicalHead(ticketID uuid.UUID, sequence int64, keyID string, headHash []byte) []byte {
	return fmt.Appendf(nil, "%s\n%s\n%d\n%s\n%s",
		domainHead, ticketID, sequence, keyID, hex.EncodeToString(headHash))
}

// CanonicalLeaf renders one Merkle leaf: a ticket head as it stood when a
// checkpoint committed it.
func CanonicalLeaf(ticketID uuid.UUID, sequence int64, headHash []byte) []byte {
	return fmt.Appendf(nil, "%s\n%s\n%d\n%s",
		domainLeaf, ticketID, sequence, hex.EncodeToString(headHash))
}

// Checkpoint is one link of an organizer's checkpoint chain: a signed Merkle
// root over the ticket heads that changed since the previous checkpoint.
//
// Read ADR-021 §D2 before attributing any security property to this. The
// checkpoint buys no rollback detection: an adversary truncates the chain rather
// than rewriting it, and a delta suffix drops cleanly. It exists because it is
// the one structure TKT-11 can afford to anchor — one root per interval instead
// of one attestation per ticket head. That is a build-order argument, not a
// security one, and it must not be dressed up as the latter.
type Checkpoint struct {
	OrganizerID  uuid.UUID
	Sequence     int64
	PreviousRoot []byte
	Root         []byte
	LeafCount    int
	KeyID        string
	CreatedAt    time.Time
}

// CanonicalCheckpoint renders the bytes a checkpoint signature covers.
// LeafCount is included so the tree's shape is committed and not merely its
// root — see the duplication note in MerkleRoot.
func CanonicalCheckpoint(c Checkpoint) []byte {
	return fmt.Appendf(nil, "%s\n%s\n%d\n%s\n%s\n%d\n%s\n%s",
		domainCheckpoint, c.OrganizerID, c.Sequence,
		hex.EncodeToString(c.PreviousRoot), hex.EncodeToString(c.Root),
		c.LeafCount, c.KeyID, stamp(c.CreatedAt))
}

// GenesisHash is the previous_hash of a ticket's first entry.
func GenesisHash() []byte { return make([]byte, HashSize) }

// HashEntry computes entry_hash_n = H(entry_hash_{n-1} ‖ canonical_n). Chaining
// through previous is what makes a modified entry k unreachable from the head:
// ADR-021 §D5 leans on exactly this to sign the head instead of every entry.
func HashEntry(previous, canonical []byte) []byte {
	h := sha256.New()
	h.Write(previous)
	h.Write(canonical)
	return h.Sum(nil)
}

func hashNode(left, right []byte) []byte {
	return HashEntry(nil, fmt.Appendf(nil, "%s\n%s\n%s",
		domainNode, hex.EncodeToString(left), hex.EncodeToString(right)))
}

// Leaf is one ticket's head as committed by a checkpoint.
type Leaf struct {
	TicketID uuid.UUID
	Sequence int64
	HeadHash []byte
}

// MerkleRoot builds the delta checkpoint's root over the changed heads.
//
// Leaves are sorted by raw ticket UUID so the root does not depend on the order
// the delta query returned, and a ticket may appear at most once. That
// uniqueness is load-bearing rather than tidy: an odd level duplicates its last
// node, and duplicate-last lets two distinct leaf multisets reach the same root
// (CVE-2012-2459). It is unreachable here only because a ticket cannot appear
// twice — so anyone relaxing the check below reintroduces root ambiguity.
// Signing LeafCount (see CanonicalCheckpoint) commits the shape as a second
// guard.
func MerkleRoot(leaves []Leaf) ([]byte, error) {
	if len(leaves) == 0 {
		return nil, errors.New("checkpoint over zero leaves")
	}
	sorted := slices.Clone(leaves)
	slices.SortFunc(sorted, func(a, b Leaf) int { return bytes.Compare(a.TicketID[:], b.TicketID[:]) })

	level := make([][]byte, 0, len(sorted))
	for i, l := range sorted {
		if len(l.HeadHash) != HashSize {
			return nil, fmt.Errorf("leaf for ticket %s has a %d-byte head hash", l.TicketID, len(l.HeadHash))
		}
		if i > 0 && sorted[i-1].TicketID == l.TicketID {
			return nil, fmt.Errorf("duplicate leaf for ticket %s", l.TicketID)
		}
		level = append(level, HashEntry(nil, CanonicalLeaf(l.TicketID, l.Sequence, l.HeadHash)))
	}
	for len(level) > 1 {
		next := make([][]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			right := level[i]
			if i+1 < len(level) {
				right = level[i+1]
			}
			next = append(next, hashNode(level[i], right))
		}
		level = next
	}
	return level[0], nil
}
