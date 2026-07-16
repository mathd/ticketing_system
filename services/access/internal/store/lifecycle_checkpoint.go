package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"ticketing/services/access/internal/lifecycle"
)

// MaxAlarmAttempts bounds outbox retries before a row is dead-lettered, so one
// poison alarm cannot block the queue behind it.
const MaxAlarmAttempts = 10

// ErrCheckpointRegression means the stored checkpoint chain went backwards
// relative to what this process last observed.
//
// It is NOT a rollback tripwire, and must never be described as one. The memory
// it compares against is this process's own, so it dies with the process: an
// adversary who truncates the chain while nothing is watching, or simply waits
// for a restart, is met by a fresh worker that adopts the surviving prefix as
// truth and resumes. ADR-021 §D2 required TKT-67 to either put that memory
// somewhere the database adversary cannot reach, or say plainly that no tripwire
// exists — this repo has no such place until TKT-11, so: no tripwire exists.
// What this does buy is that the worker refuses to LAUNDER a regression it can
// actually see, rather than extending it under a fresh valid signature.
var ErrCheckpointRegression = errors.New("checkpoint chain regressed against this process's last observation")

// LastRoot is a checkpoint chain position. The zero value means "never observed".
type LastRoot struct {
	Sequence int64
	Root     []byte
}

// CheckpointResult describes a checkpoint that was just written.
type CheckpointResult struct {
	OrganizerID uuid.UUID
	Sequence    int64
	Root        []byte
	LeafCount   int
}

// PendingCheckpointOrganizers lists organizers with uncheckpointed head changes.
// Backed by lifecycle_head_changes_pending_idx: the scan is over the pending
// partial index, not over every change ever made (ADR-019's discipline — a
// scoped read is only scoped if an index backs the filter).
func (p *Postgres) PendingCheckpointOrganizers(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT DISTINCT organizer_id FROM lifecycle_head_changes WHERE checkpoint_id IS NULL`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CheckpointOrganizer commits one delta checkpoint for an organizer: a signed
// Merkle root over the heads that changed since the previous checkpoint, chained
// to it (ADR-021 §D2).
//
// Nothing here touches a ticket row. The worker locks only its own queue rows,
// so a doors-open turnstile never waits on checkpointing — that contention
// profile is the entire reason ADR-021 rejected the money journal's
// per-organizer chain (§Option 1, ADR-010).
//
// observed is this process's last seen position for the organizer; see
// ErrCheckpointRegression for what it is and is not worth.
func (p *Postgres) CheckpointOrganizer(ctx context.Context, organizerID uuid.UUID, observed LastRoot) (CheckpointResult, error) {
	if p.cfg.Signer == nil {
		return CheckpointResult{}, ErrLifecycleUnsigned
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return CheckpointResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Lock this organizer's pending queue. Row locks do not block inserts, so
	// concurrent redemptions keep queueing changes while this runs; a second
	// worker blocks here, then re-reads and finds nothing pending.
	rows, err := tx.QueryContext(ctx, `
		SELECT change_id, ticket_id, sequence, head_hash FROM lifecycle_head_changes
		WHERE organizer_id=$1 AND checkpoint_id IS NULL ORDER BY change_id FOR UPDATE`, organizerID)
	if err != nil {
		return CheckpointResult{}, err
	}
	type change struct {
		id       int64
		ticketID uuid.UUID
		sequence int64
		hash     []byte
	}
	var changes []change
	for rows.Next() {
		var c change
		if err := rows.Scan(&c.id, &c.ticketID, &c.sequence, &c.hash); err != nil {
			_ = rows.Close()
			return CheckpointResult{}, err
		}
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return CheckpointResult{}, err
	}
	_ = rows.Close()
	if len(changes) == 0 {
		return CheckpointResult{}, tx.Commit()
	}

	// Read the chain head. O(1) by the (organizer_id, sequence) unique index —
	// walking the whole chain here would grow unboundedly for a long-lived
	// organizer, every interval, forever. Full-chain verification is
	// VerifyLifecycle's job, not the hot loop's.
	var prev LastRoot
	err = tx.QueryRowContext(ctx, `SELECT sequence,root FROM lifecycle_checkpoints WHERE organizer_id=$1 ORDER BY sequence DESC LIMIT 1`, organizerID).
		Scan(&prev.Sequence, &prev.Root)
	if errors.Is(err, sql.ErrNoRows) {
		prev = LastRoot{Sequence: 0, Root: lifecycle.GenesisHash()}
	} else if err != nil {
		return CheckpointResult{}, err
	}
	if observed.Sequence > 0 {
		if prev.Sequence < observed.Sequence ||
			(prev.Sequence == observed.Sequence && !bytes.Equal(prev.Root, observed.Root)) {
			return CheckpointResult{}, fmt.Errorf("%w: organizer %s was at sequence %d, database says %d",
				ErrCheckpointRegression, organizerID, observed.Sequence, prev.Sequence)
		}
	}

	// One leaf per ticket, the latest head among this delta's changes. The
	// dedup is what keeps the Merkle root unambiguous (see lifecycle.MerkleRoot).
	latest := map[uuid.UUID]change{}
	ids := make([]int64, 0, len(changes))
	for _, c := range changes {
		ids = append(ids, c.id)
		if existing, ok := latest[c.ticketID]; !ok || c.id > existing.id {
			latest[c.ticketID] = c
		}
	}
	leaves := make([]lifecycle.Leaf, 0, len(latest))
	for _, c := range latest {
		leaves = append(leaves, lifecycle.Leaf{TicketID: c.ticketID, Sequence: c.sequence, HeadHash: c.hash})
	}
	root, err := lifecycle.MerkleRoot(leaves)
	if err != nil {
		return CheckpointResult{}, err
	}

	cp := lifecycle.Checkpoint{
		OrganizerID: organizerID, Sequence: prev.Sequence + 1,
		PreviousRoot: prev.Root, Root: root, LeafCount: len(leaves),
		KeyID: p.cfg.Signer.KeyID(), CreatedAt: p.now(),
	}
	signature := p.cfg.Signer.SignCheckpoint(cp)
	var checkpointID int64
	if err = tx.QueryRowContext(ctx, `
		INSERT INTO lifecycle_checkpoints(organizer_id,sequence,previous_root,root,leaf_count,canonical_version,key_id,signature,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING checkpoint_id`,
		cp.OrganizerID, cp.Sequence, cp.PreviousRoot, cp.Root, cp.LeafCount, lifecycle.CanonicalVersion,
		cp.KeyID, signature, cp.CreatedAt).Scan(&checkpointID); err != nil {
		return CheckpointResult{}, err
	}
	// Every claimed change is assigned, not just the deduped leaves: an
	// unassigned older change would be re-queued forever.
	if _, err = tx.ExecContext(ctx, `UPDATE lifecycle_head_changes SET checkpoint_id=$1 WHERE change_id = ANY($2)`,
		checkpointID, ids); err != nil {
		return CheckpointResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return CheckpointResult{}, err
	}
	return CheckpointResult{OrganizerID: organizerID, Sequence: cp.Sequence, Root: root, LeafCount: len(leaves)}, nil
}

// OldestPendingChange reports the age of the oldest uncheckpointed head change.
// Freshness is monitored, never gated: ADR-021 §Consequences asks for monitoring,
// and the checkpoint is scaffolding that buys no detection today (§D2) — taking a
// turnstile offline because scaffolding is stale would be the brittle door §D6
// exists to avoid.
func (p *Postgres) OldestPendingChange(ctx context.Context) (time.Time, bool, error) {
	var oldest sql.NullTime
	if err := p.db.QueryRowContext(ctx, `SELECT min(created_at) FROM lifecycle_head_changes WHERE checkpoint_id IS NULL`).Scan(&oldest); err != nil {
		return time.Time{}, false, err
	}
	if !oldest.Valid {
		return time.Time{}, false, nil
	}
	return oldest.Time, true, nil
}

// BackfillLifecycle adopts existing lifecycle history into the chain, one ticket
// per transaction, and returns how many tickets it chained.
//
// It cannot prove legacy rows were honest — QR credentials anchor ticket
// identity, not event history (ADR-021 §D9). Existing history is adopted as the
// baseline, and everything before that baseline is outside what the trail can
// speak to. That is a property of the scheme, not a bug in it.
//
// Resumable by construction: a ticket is chained atomically or not at all, so an
// interrupted run leaves no half-chain and the next run simply finds the ticket
// still unchained. This runs in its own one-shot job rather than in `migrate`,
// because it Ed25519-signs a head per ticket and that cost scales with history —
// ADR-008's 30-second fail-fast deadline still bounds the migrate job, and a
// backfill that blows it would leave the service unable to start at all
// (ADR-021 §D9 as amended for ADR-022).
func (p *Postgres) BackfillLifecycle(ctx context.Context, batch int) (int, error) {
	if p.cfg.Signer == nil {
		return 0, ErrLifecycleUnsigned
	}
	if batch <= 0 {
		batch = 128
	}
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		rows, err := p.db.QueryContext(ctx, `
			SELECT t.id FROM tickets t
			WHERE EXISTS (SELECT 1 FROM lifecycle_events e WHERE e.ticket_id = t.id)
			  AND NOT EXISTS (SELECT 1 FROM lifecycle_heads h WHERE h.ticket_id = t.id)
			ORDER BY t.id LIMIT $1`, batch)
		if err != nil {
			return total, err
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return total, err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return total, err
		}
		_ = rows.Close()
		if len(ids) == 0 {
			return total, nil
		}
		for _, id := range ids {
			if err := p.backfillTicket(ctx, id); err != nil {
				return total, fmt.Errorf("backfill ticket %s: %w", id, err)
			}
			total++
		}
	}
}

func (p *Postgres) backfillTicket(ctx context.Context, ticketID uuid.UUID) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var id TicketIdentity
	if err = tx.QueryRowContext(ctx, `SELECT order_id,organizer_id,slot_id FROM tickets WHERE id=$1 FOR UPDATE`, ticketID).
		Scan(&id.OrderID, &id.OrganizerID, &id.SlotID); err != nil {
		return err
	}
	// A ticket with some integrity rows but no head cannot come from this
	// function — it chains a ticket atomically — so it is pre-existing damage,
	// not a resumable partial. Treat it as corruption rather than continuing a
	// chain whose start we did not build.
	var covered int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM lifecycle_event_integrity WHERE ticket_id=$1`, ticketID).Scan(&covered); err != nil {
		return err
	}
	if covered > 0 {
		return fmt.Errorf("ticket has %d integrity rows and no head: partial coverage, not a resumable backfill", covered)
	}

	// (occurred_at, id) is the order History already exposes, so the chain adopts
	// the order the trail has always presented rather than inventing a new one.
	rows, err := tx.QueryContext(ctx, `SELECT id,event_type,occurred_at FROM lifecycle_events WHERE ticket_id=$1 ORDER BY occurred_at,id`, ticketID)
	if err != nil {
		return err
	}
	type legacy struct {
		id         uuid.UUID
		eventType  string
		occurredAt time.Time
	}
	var events []legacy
	for rows.Next() {
		var e legacy
		if err := rows.Scan(&e.id, &e.eventType, &e.occurredAt); err != nil {
			_ = rows.Close()
			return err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	if len(events) == 0 {
		return nil
	}

	prev := lifecycle.GenesisHash()
	var sequence int64
	var entryHash []byte
	for _, e := range events {
		sequence++
		occurredAt := lifecycle.Normalize(e.occurredAt)
		canonical := lifecycle.CanonicalEvent(lifecycle.Event{
			TicketID: ticketID, OrderID: id.OrderID, OrganizerID: id.OrganizerID, SlotID: id.SlotID,
			Sequence: sequence, EventID: e.id, Type: e.eventType, OccurredAt: occurredAt,
		})
		entryHash = lifecycle.HashEntry(prev, canonical)
		if _, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_event_integrity(event_id,ticket_id,sequence,canonical_version,previous_hash,entry_hash) VALUES($1,$2,$3,$4,$5,$6)`,
			e.id, ticketID, sequence, lifecycle.CanonicalVersion, prev, entryHash); err != nil {
			return err
		}
		prev = entryHash
	}
	signature := p.cfg.Signer.SignHead(ticketID, sequence, entryHash)
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO lifecycle_heads(ticket_id,organizer_id,last_sequence,canonical_version,last_hash,key_id,signature,changed_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		ticketID, id.OrganizerID, sequence, lifecycle.CanonicalVersion, entryHash, p.cfg.Signer.KeyID(), signature, p.now()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_head_changes(ticket_id,organizer_id,sequence,head_hash) VALUES($1,$2,$3,$4)`,
		ticketID, id.OrganizerID, sequence, entryHash); err != nil {
		return err
	}
	return tx.Commit()
}

// SealEpoch retains the current head signature of every chained ticket under the
// key that signed it, before that key is rotated away (ADR-021 §D5).
//
// It is a copy, not a re-signing: a head's stored signature already binds ticket,
// sequence, canonical version and key id, so it IS the epoch signature for the
// key it was made under. That means sealing needs no private key at all.
//
// Run it at quiescence, immediately before rotating: ON CONFLICT DO NOTHING keeps
// the first seal per (ticket, key), so a head that advances after sealing is not
// re-sealed under the same key.
//
// This raises the work of a current-key compromise from "re-sign a head" to
// "re-sign a head and delete these rows". It is not containment: the rows live in
// the database the adversary owns, and because ADR-021 surrenders global
// ticket-set completeness, nothing says a row must exist — so a deleted epoch
// signature is indistinguishable from a ticket that never had one. Real
// containment needs an externally retained rotation manifest (TKT-11).
func (p *Postgres) SealEpoch(ctx context.Context) (int, error) {
	result, err := p.db.ExecContext(ctx, `
		INSERT INTO lifecycle_head_epoch_signatures(ticket_id,key_id,ticket_sequence,canonical_version,head_hash,signature,sealed_at)
		SELECT ticket_id,key_id,last_sequence,canonical_version,last_hash,signature,$1 FROM lifecycle_heads
		ON CONFLICT (ticket_id,key_id) DO NOTHING`, p.now())
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// AlarmMessage is one owed integrity alarm.
type AlarmMessage struct {
	EventID  uuid.UUID
	Subject  string
	Envelope []byte
	ClaimID  uuid.UUID
	Attempts int
}

// ClaimAlarms leases a batch of unpublished alarms, oldest first.
func (p *Postgres) ClaimAlarms(ctx context.Context, batch int, lease time.Duration) ([]AlarmMessage, error) {
	claimID := uuid.New()
	rows, err := p.db.QueryContext(ctx, `
		UPDATE lifecycle_integrity_alarm_outbox SET claim_id=$1, lease_until=now()+$2::interval, attempts=attempts+1
		WHERE event_id IN (
			SELECT event_id FROM lifecycle_integrity_alarm_outbox
			WHERE published_at IS NULL AND dead_lettered_at IS NULL AND next_attempt_at<=now()
			  AND (lease_until IS NULL OR lease_until<=now())
			ORDER BY next_attempt_at LIMIT $3 FOR UPDATE SKIP LOCKED)
		RETURNING event_id, subject, envelope, attempts`,
		claimID, fmt.Sprintf("%d seconds", int(lease.Seconds())), batch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AlarmMessage
	for rows.Next() {
		m := AlarmMessage{ClaimID: claimID}
		if err := rows.Scan(&m.EventID, &m.Subject, &m.Envelope, &m.Attempts); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ReleaseAlarm returns a failed alarm to the queue with the cause recorded and a
// backoff applied, so a permanently failing row cannot starve newer ones.
func (p *Postgres) ReleaseAlarm(ctx context.Context, eventID, claimID uuid.UUID, cause error) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE lifecycle_integrity_alarm_outbox
		SET claim_id=NULL, lease_until=NULL, last_error=$3,
		    next_attempt_at=now()+least(attempts,6)*interval '5 seconds',
		    dead_lettered_at=CASE WHEN attempts>=$4 THEN now() ELSE NULL END
		WHERE event_id=$1 AND claim_id=$2`, eventID, claimID, cause.Error(), MaxAlarmAttempts)
	return err
}

// MarkAlarmPublished retires an alarm, but only if this claimant still holds it.
func (p *Postgres) MarkAlarmPublished(ctx context.Context, eventID, claimID uuid.UUID) (bool, error) {
	result, err := p.db.ExecContext(ctx, `UPDATE lifecycle_integrity_alarm_outbox SET published_at=now(), claim_id=NULL, lease_until=NULL WHERE event_id=$1 AND claim_id=$2 AND published_at IS NULL`, eventID, claimID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// OldestUnpublishedAlarm reports the age of the oldest owed alarm, for the
// backlog metric. Like checkpoint freshness this is monitored, not gated: a
// broker blip must not close a turnstile.
func (p *Postgres) OldestUnpublishedAlarm(ctx context.Context) (time.Time, bool, error) {
	var oldest sql.NullTime
	if err := p.db.QueryRowContext(ctx, `SELECT min(created_at) FROM lifecycle_integrity_alarm_outbox WHERE published_at IS NULL AND dead_lettered_at IS NULL`).Scan(&oldest); err != nil {
		return time.Time{}, false, err
	}
	if !oldest.Valid {
		return time.Time{}, false, nil
	}
	return oldest.Time, true, nil
}
