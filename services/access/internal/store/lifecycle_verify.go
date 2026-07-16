package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"ticketing/services/access/internal/lifecycle"
)

// VerifyOptions tunes the offline verifier.
type VerifyOptions struct {
	// MaxPendingAge fails verification when a head change has waited longer than
	// this for a checkpoint. Zero disables the check. This lives in the audit
	// tool rather than in readiness on purpose: staleness is worth knowing about,
	// and it is not worth closing a gate over.
	MaxPendingAge time.Duration
	// RequireCoverage fails when a lifecycle event has no integrity row. It is
	// disabled only while the backfill is still adopting history — before the
	// server serves from the trail, never after.
	RequireCoverage bool
}

// VerifyLifecycle verifies the whole trail with public keys only: every ticket
// chain, coverage in both directions, every head signature, every extant epoch
// signature, and every organizer's checkpoint chain (ADR-021 §D7).
//
// What a clean result means, stated the way ADR-021 §The trust boundary demands:
// no modification, insertion or reordering has touched the covered history, IF
// the adversary could not re-sign it. A consistently rolled-back trail — a ticket
// head reverted together with the checkpoint suffix that committed it — verifies
// clean here and always will. No in-database check can see it; that is TKT-11's
// job. Do not read a green verify as "the trail is intact" without naming which
// adversary you mean.
func (p *Postgres) VerifyLifecycle(ctx context.Context, opts VerifyOptions) error {
	if p.cfg.Keyring == nil {
		return fmt.Errorf("verify-lifecycle needs a lifecycle keyring")
	}
	heads, err := p.verifyChains(ctx, opts)
	if err != nil {
		return err
	}
	if err := p.verifyHeads(ctx, heads); err != nil {
		return err
	}
	if err := p.verifyOrphanIntegrity(ctx); err != nil {
		return err
	}
	if err := p.verifyEpochSignatures(ctx); err != nil {
		return err
	}
	if err := p.verifyCheckpoints(ctx); err != nil {
		return err
	}
	return p.verifyPendingAge(ctx, opts)
}

type computedHead struct {
	sequence int64
	hash     []byte
}

// checkVersion rejects a stored canonical version this build does not write.
//
// The version travels inside the signed domain prefix ("…/v1"), so the hash and
// signature already bind it — but the stored COLUMN is a separate copy that
// nothing covered, and it is the discriminator future migrations dispatch on. An
// unverified discriminator can lie, which is the whole failure ADR-017 §5b′
// describes. Only version 1 exists today; a v2 verifier would branch here rather
// than reject.
func checkVersion(stored int64, what string) error {
	if stored != lifecycle.CanonicalVersion {
		return fmt.Errorf("%s declares canonical version %d, this build writes %d", what, stored, lifecycle.CanonicalVersion)
	}
	return nil
}

// verifyChains walks every ticket's chain in one ordered pass, the same shape
// the money journal's verifier uses (payments store.Verify), and returns the
// head each chain actually reaches.
func (p *Postgres) verifyChains(ctx context.Context, opts VerifyOptions) (map[uuid.UUID]computedHead, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT t.id, t.order_id, t.organizer_id, t.slot_id,
		       e.id, e.event_type, e.occurred_at, i.sequence, i.previous_hash, i.entry_hash, i.canonical_version
		FROM lifecycle_events e
		JOIN tickets t ON t.id = e.ticket_id
		LEFT JOIN lifecycle_event_integrity i ON i.event_id = e.id
		ORDER BY t.id, i.sequence`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	computed := map[uuid.UUID]computedHead{}
	prev := lifecycle.GenesisHash()
	var current uuid.UUID
	var count int64
	for rows.Next() {
		var ticketID uuid.UUID
		var id TicketIdentity
		var eventID uuid.UUID
		var eventType string
		var occurredAt time.Time
		var sequence sql.NullInt64
		var previousHash, entryHash []byte
		var version sql.NullInt64
		if err := rows.Scan(&ticketID, &id.OrderID, &id.OrganizerID, &id.SlotID,
			&eventID, &eventType, &occurredAt, &sequence, &previousHash, &entryHash, &version); err != nil {
			return nil, err
		}
		if ticketID != current {
			current, prev, count = ticketID, lifecycle.GenesisHash(), 0
		}
		// An event with no integrity row means something wrote to
		// lifecycle_events without going through the append path.
		if !sequence.Valid {
			if !opts.RequireCoverage {
				continue
			}
			return nil, fmt.Errorf("lifecycle event %s on ticket %s has no integrity row", eventID, ticketID)
		}
		// Dispatch on the stored version before trusting the bytes, the way
		// ADR-017 §5b′ requires of any versioned payload: a version exists
		// precisely because the rules can change, so verifying a future variant
		// with today's rules would judge it by the wrong ones. Only version 1
		// exists, so anything else is corruption rather than the future — and
		// silently recomputing with v1 anyway is how a discriminator rots into a
		// field nothing enforces.
		if err := checkVersion(version.Int64, "integrity row for event "+eventID.String()); err != nil {
			return nil, err
		}
		count++
		if sequence.Int64 != count {
			return nil, fmt.Errorf("sequence gap on ticket %s: found %d, expected %d", ticketID, sequence.Int64, count)
		}
		if !bytes.Equal(previousHash, prev) {
			return nil, fmt.Errorf("broken chain link on ticket %s at sequence %d", ticketID, sequence.Int64)
		}
		canonical := lifecycle.CanonicalEvent(lifecycle.Event{
			TicketID: ticketID, OrderID: id.OrderID, OrganizerID: id.OrganizerID, SlotID: id.SlotID,
			Sequence: sequence.Int64, EventID: eventID, Type: eventType, OccurredAt: lifecycle.Normalize(occurredAt),
		})
		want := lifecycle.HashEntry(prev, canonical)
		if !bytes.Equal(want, entryHash) {
			return nil, fmt.Errorf("entry hash mismatch on ticket %s at sequence %d", ticketID, sequence.Int64)
		}
		prev = want
		computed[ticketID] = computedHead{sequence: count, hash: want}
	}
	return computed, rows.Err()
}

func (p *Postgres) verifyHeads(ctx context.Context, computed map[uuid.UUID]computedHead) error {
	rows, err := p.db.QueryContext(ctx, `SELECT ticket_id,last_sequence,last_hash,key_id,signature,canonical_version FROM lifecycle_heads`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	seen := map[uuid.UUID]bool{}
	for rows.Next() {
		var ticketID uuid.UUID
		var sequence int64
		var hash, signature []byte
		var keyID string
		var version int64
		if err := rows.Scan(&ticketID, &sequence, &hash, &keyID, &signature, &version); err != nil {
			return err
		}
		if err := checkVersion(version, "head for ticket "+ticketID.String()); err != nil {
			return err
		}
		seen[ticketID] = true
		want, ok := computed[ticketID]
		if !ok {
			return fmt.Errorf("ticket %s has a head and no chained events", ticketID)
		}
		if want.sequence != sequence || !bytes.Equal(want.hash, hash) {
			return fmt.Errorf("head mismatch on ticket %s: head is sequence %d, chain reaches %d", ticketID, sequence, want.sequence)
		}
		if err := p.cfg.Keyring.VerifyHead(ticketID, sequence, keyID, hash, signature); err != nil {
			return fmt.Errorf("head signature on ticket %s: %w", ticketID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for ticketID := range computed {
		if !seen[ticketID] {
			return fmt.Errorf("ticket %s has chained events and no head", ticketID)
		}
	}
	return nil
}

// verifyOrphanIntegrity closes the other coverage direction: an integrity row
// whose event does not exist, or whose ticket disagrees with the event's, is a
// forged link rather than a missing one.
func (p *Postgres) verifyOrphanIntegrity(ctx context.Context) error {
	var orphan uuid.UUID
	err := p.db.QueryRowContext(ctx, `
		SELECT i.event_id FROM lifecycle_event_integrity i
		WHERE NOT EXISTS (SELECT 1 FROM lifecycle_events e WHERE e.id=i.event_id AND e.ticket_id=i.ticket_id)
		LIMIT 1`).Scan(&orphan)
	if err == nil {
		return fmt.Errorf("integrity row %s references no matching lifecycle event", orphan)
	}
	if err == sql.ErrNoRows {
		return nil
	}
	return err
}

// verifyEpochSignatures checks the epoch signatures that exist. It deliberately
// never checks that one SHOULD exist: ADR-021 §Consequences surrenders global
// ticket-set completeness, so a verifier cannot tell a deleted epoch signature
// from a ticket that legitimately has none. Asserting completeness here would
// invent a guarantee the scheme does not have.
func (p *Postgres) verifyEpochSignatures(ctx context.Context) error {
	rows, err := p.db.QueryContext(ctx, `SELECT ticket_id,key_id,ticket_sequence,head_hash,signature,canonical_version FROM lifecycle_head_epoch_signatures`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ticketID uuid.UUID
		var keyID string
		var sequence int64
		var hash, signature []byte
		var version int64
		if err := rows.Scan(&ticketID, &keyID, &sequence, &hash, &signature, &version); err != nil {
			return err
		}
		if err := checkVersion(version, "epoch signature for ticket "+ticketID.String()); err != nil {
			return err
		}
		if err := p.cfg.Keyring.VerifyHead(ticketID, sequence, keyID, hash, signature); err != nil {
			return fmt.Errorf("epoch signature on ticket %s under key %s: %w", ticketID, keyID, err)
		}
	}
	return rows.Err()
}

// verifyCheckpoints walks each organizer's checkpoint chain, recomputing every
// root from the head-change snapshots assigned to it.
//
// Recomputing is possible because the snapshots are archived: a checkpoint's
// leaf is the head as it stood then, which the live head cannot reconstruct once
// it has advanced. A separate leaves table would store that data twice.
func (p *Postgres) verifyCheckpoints(ctx context.Context) error {
	rows, err := p.db.QueryContext(ctx, `
		SELECT checkpoint_id,organizer_id,sequence,previous_root,root,leaf_count,key_id,signature,created_at,canonical_version
		FROM lifecycle_checkpoints ORDER BY organizer_id,sequence`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	type checkpointRow struct {
		id        int64
		cp        lifecycle.Checkpoint
		signature []byte
	}
	var all []checkpointRow
	for rows.Next() {
		var r checkpointRow
		var version int64
		if err := rows.Scan(&r.id, &r.cp.OrganizerID, &r.cp.Sequence, &r.cp.PreviousRoot, &r.cp.Root,
			&r.cp.LeafCount, &r.cp.KeyID, &r.signature, &r.cp.CreatedAt, &version); err != nil {
			return err
		}
		if err := checkVersion(version, fmt.Sprintf("checkpoint %d for organizer %s", r.cp.Sequence, r.cp.OrganizerID)); err != nil {
			return err
		}
		r.cp.CreatedAt = lifecycle.Normalize(r.cp.CreatedAt)
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	var current uuid.UUID
	prev := LastRoot{Sequence: 0, Root: lifecycle.GenesisHash()}
	for _, r := range all {
		if r.cp.OrganizerID != current {
			current = r.cp.OrganizerID
			prev = LastRoot{Sequence: 0, Root: lifecycle.GenesisHash()}
		}
		if r.cp.Sequence != prev.Sequence+1 {
			return fmt.Errorf("checkpoint gap for organizer %s: found %d, expected %d", r.cp.OrganizerID, r.cp.Sequence, prev.Sequence+1)
		}
		if !bytes.Equal(r.cp.PreviousRoot, prev.Root) {
			return fmt.Errorf("broken checkpoint link for organizer %s at sequence %d", r.cp.OrganizerID, r.cp.Sequence)
		}
		leaves, err := p.checkpointLeaves(ctx, r.id, r.cp.OrganizerID)
		if err != nil {
			return err
		}
		if len(leaves) != r.cp.LeafCount {
			return fmt.Errorf("checkpoint %d for organizer %s claims %d leaves, has %d", r.cp.Sequence, r.cp.OrganizerID, r.cp.LeafCount, len(leaves))
		}
		root, err := lifecycle.MerkleRoot(leaves)
		if err != nil {
			return fmt.Errorf("checkpoint %d for organizer %s: %w", r.cp.Sequence, r.cp.OrganizerID, err)
		}
		if !bytes.Equal(root, r.cp.Root) {
			return fmt.Errorf("checkpoint %d for organizer %s does not match its leaves", r.cp.Sequence, r.cp.OrganizerID)
		}
		if err := p.cfg.Keyring.VerifyCheckpoint(r.cp, r.signature); err != nil {
			return fmt.Errorf("checkpoint %d for organizer %s: %w", r.cp.Sequence, r.cp.OrganizerID, err)
		}
		prev = LastRoot{Sequence: r.cp.Sequence, Root: r.cp.Root}
	}
	return nil
}

// checkpointLeaves rebuilds a checkpoint's leaf set: the latest head change per
// ticket among the changes assigned to it. DISTINCT ON with a pinned ordering,
// backed by lifecycle_head_changes_checkpoint_idx.
//
// Every leaf is authenticated here, not merely re-hashed. Recomputing a root
// from the same rows the signer used proves only that the two agree — if the
// rows are attacker-supplied, they agree on the attacker's story. The head
// signature is what makes a leaf mean anything, and the ticket join is what
// binds it to an organizer, which no signature covers.
func (p *Postgres) checkpointLeaves(ctx context.Context, checkpointID int64, organizerID uuid.UUID) ([]lifecycle.Leaf, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT DISTINCT ON (c.ticket_id) c.ticket_id, c.sequence, c.head_hash, c.canonical_version, c.key_id, c.signature, t.organizer_id
		FROM lifecycle_head_changes c JOIN tickets t ON t.id = c.ticket_id
		WHERE c.checkpoint_id=$1
		ORDER BY c.ticket_id, c.change_id DESC`, checkpointID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []lifecycle.Leaf
	for rows.Next() {
		var l lifecycle.Leaf
		var version int
		var keyID string
		var signature []byte
		var owner uuid.UUID
		if err := rows.Scan(&l.TicketID, &l.Sequence, &l.HeadHash, &version, &keyID, &signature, &owner); err != nil {
			return nil, err
		}
		if version != lifecycle.CanonicalVersion {
			return nil, fmt.Errorf("checkpoint %d leaf for ticket %s declares canonical version %d, this build writes %d",
				checkpointID, l.TicketID, version, lifecycle.CanonicalVersion)
		}
		if owner != organizerID {
			return nil, fmt.Errorf("checkpoint %d covers ticket %s, which belongs to organizer %s", checkpointID, l.TicketID, owner)
		}
		if err := p.cfg.Keyring.VerifyHead(l.TicketID, l.Sequence, keyID, l.HeadHash, signature); err != nil {
			return nil, fmt.Errorf("checkpoint %d leaf for ticket %s is not a signed head: %w", checkpointID, l.TicketID, err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (p *Postgres) verifyPendingAge(ctx context.Context, opts VerifyOptions) error {
	if opts.MaxPendingAge <= 0 {
		return nil
	}
	oldest, ok, err := p.OldestPendingChange(ctx)
	if err != nil || !ok {
		return err
	}
	if age := p.now().Sub(oldest); age > opts.MaxPendingAge {
		return fmt.Errorf("a head change has waited %s for a checkpoint, over the %s bound: the checkpoint worker is not running", age.Truncate(time.Second), opts.MaxPendingAge)
	}
	return nil
}
