package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"ticketing/services/access/internal/lifecycle"
)

// SubjectIntegrityAlarm carries integrity alarms to whatever the deployment has
// attached to the durable (ADR-021 §D6: "TKT-67 owns routing; an unmonitored
// deployment must not run this scheme in fail-open").
const SubjectIntegrityAlarm = "platform.access.lifecycle-integrity.alarm"

// ErrLifecycleUnsigned is returned when a write path is asked to append without
// a signer. A store built for `verify-lifecycle` holds public keys only; if one
// ever reaches an append, that is a wiring bug and must not silently produce an
// unsigned head.
var ErrLifecycleUnsigned = errors.New("lifecycle store has no signer: cannot append to the trail")

// Decision is a scan's outcome. It exists because "accepted" alone cannot
// distinguish a clean redemption from ADR-021 §D6's degraded admission, and an
// operator who cannot tell those apart has no use for the alarm.
type Decision string

const (
	DecisionAccepted        Decision = "accepted"
	DecisionAlreadyRedeemed Decision = "already_redeemed"
	// DecisionAdmittedDegraded is §D6's fail-open: the chain did not verify and
	// the holder was admitted anyway, once. The trade is deliberate — a
	// verification failure is likelier our bug than an attacker, and denying a
	// real customer at a live turnstile is the worse failure.
	DecisionAdmittedDegraded Decision = "admitted_degraded"
	// DecisionIntegrityQuarantined is the second scan of a ticket that already
	// took its one degraded admission.
	DecisionIntegrityQuarantined Decision = "integrity_quarantined"
	// DecisionIntegrityOperatorControlled is a corrupt-chain scan for an
	// organizer a human has flipped to operator-controlled deny.
	DecisionIntegrityOperatorControlled Decision = "integrity_operator_controlled"
)

// Mode is an organizer's degraded-mode posture (ADR-021 §D6).
type Mode string

const (
	ModeNormal        Mode = "normal"
	ModeOperatorDeny  Mode = "operator_deny"
	ModeOperatorAdmit Mode = "operator_admit"
)

// Policy bounds the degraded mode. ADR-021 §D6 is blunt that these bound OUR
// BUGS — a canonicalization drift or a botched rotation corrupting many chains
// at once — and not an adversary, who deletes the quarantine row and resets the
// window between scans. Do not describe them as containment.
type Policy struct {
	// FailureThreshold is how many distinct first-time corrupt tickets inside
	// Window flip the organizer to operator-controlled. Crossing it means the
	// "our bug" explanation is wearing thin, and the choice to keep admitting
	// becomes a human's, made knowingly, rather than a default taken at 3am.
	FailureThreshold int
	Window           time.Duration
}

// DefaultPolicy: three distinct corrupt tickets in a minute. One is noise (a
// single bad row); three inside a minute is a pattern no plausible bug produces
// per-ticket at random.
func DefaultPolicy() Policy { return Policy{FailureThreshold: 3, Window: time.Minute} }

// Config wires the trail into the store.
type Config struct {
	// Signer is nil for verify-only callers (`access verify-lifecycle`), which is
	// what makes ADR-021 §D4's "verify without the power to write" structural
	// rather than a promise.
	Signer  *lifecycle.Signer
	Keyring *lifecycle.Keyring
	Policy  Policy
	Now     func() time.Time
}

func (p *Postgres) now() time.Time {
	if p.cfg.Now != nil {
		return lifecycle.Normalize(p.cfg.Now())
	}
	return lifecycle.Normalize(time.Now())
}

// errEventIDTaken is the lifecycle_events primary key refusing an event id
// that already exists — the append-window shape of occurrence reuse.
var errEventIDTaken = errors.New("lifecycle event id already recorded")

// chainState is a ticket's head as the append path found it.
type chainState struct {
	sequence int64
	hash     []byte
}

// loadHead reads a ticket's current head. Callers hold the ticket row lock, so
// no second lock is taken here: ADR-021 §D1 and ADR-010 both forbid introducing
// any organizer-wide serialization on this path, and a head row lock per ticket
// would be redundant with the ticket lock that already serializes it.
func loadHead(ctx context.Context, tx *sql.Tx, ticketID uuid.UUID) (chainState, error) {
	var st chainState
	err := tx.QueryRowContext(ctx, `SELECT last_sequence,last_hash FROM lifecycle_heads WHERE ticket_id=$1`, ticketID).
		Scan(&st.sequence, &st.hash)
	if errors.Is(err, sql.ErrNoRows) {
		return chainState{sequence: 0, hash: lifecycle.GenesisHash()}, nil
	}
	if err != nil {
		return chainState{}, err
	}
	return st, nil
}

// appendInput is one lifecycle event to chain.
type appendInput struct {
	TicketID, OrderID, OrganizerID, SlotID uuid.UUID
	EventID                                uuid.UUID
	Type                                   string
	// OccurredAt is zero when the column default supplies the time.
	OccurredAt time.Time
}

// appendLifecycle writes the lifecycle row and its integrity row in one
// transaction, then advances and re-signs the head and queues the head-change
// snapshot for the next checkpoint.
//
// It must be called only when the lifecycle event is genuinely new. MarkDelivered
// inserts with ON CONFLICT DO NOTHING, so calling this after a no-op insert would
// try to chain an event that was never written: the integrity row would collide on
// its primary key and today's silent-success redelivery would become a hard error.
// Resolve idempotency before calling, never inside.
func (p *Postgres) appendLifecycle(ctx context.Context, tx *sql.Tx, in appendInput) (time.Time, error) {
	if p.cfg.Signer == nil {
		return time.Time{}, ErrLifecycleUnsigned
	}
	head, err := loadHead(ctx, tx, in.TicketID)
	if err != nil {
		return time.Time{}, fmt.Errorf("load head: %w", err)
	}

	// Insert first and read the stored timestamp back. timestamptz keeps
	// microseconds and some rows take their time from the column default, so the
	// canonical form has to cover the value the database actually kept —
	// signing anything else canonicalizes different bytes on reload.
	var occurredAt time.Time
	if in.OccurredAt.IsZero() {
		err = tx.QueryRowContext(ctx, `INSERT INTO lifecycle_events(id,ticket_id,event_type) VALUES($1,$2,$3) RETURNING occurred_at`,
			in.EventID, in.TicketID, in.Type).Scan(&occurredAt)
	} else {
		err = tx.QueryRowContext(ctx, `INSERT INTO lifecycle_events(id,ticket_id,event_type,occurred_at) VALUES($1,$2,$3,$4) RETURNING occurred_at`,
			in.EventID, in.TicketID, in.Type, in.OccurredAt).Scan(&occurredAt)
	}
	if err != nil {
		// Only THIS statement's unique violation means the event id itself is
		// taken. Later 23505s in the chain (an integrity sequence re-derived
		// from a stale or missing head) are corruption states and must keep
		// their own diagnostics.
		if isUniqueViolation(err) {
			return time.Time{}, fmt.Errorf("insert lifecycle event: %w", errEventIDTaken)
		}
		return time.Time{}, fmt.Errorf("insert lifecycle event: %w", err)
	}
	occurredAt = lifecycle.Normalize(occurredAt)

	sequence := head.sequence + 1
	canonical := lifecycle.CanonicalEvent(lifecycle.Event{
		TicketID: in.TicketID, OrderID: in.OrderID, OrganizerID: in.OrganizerID, SlotID: in.SlotID,
		Sequence: sequence, EventID: in.EventID, Type: in.Type, OccurredAt: occurredAt,
	})
	entryHash := lifecycle.HashEntry(head.hash, canonical)

	if _, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_event_integrity(event_id,ticket_id,sequence,canonical_version,previous_hash,entry_hash) VALUES($1,$2,$3,$4,$5,$6)`,
		in.EventID, in.TicketID, sequence, lifecycle.CanonicalVersion, head.hash, entryHash); err != nil {
		return time.Time{}, fmt.Errorf("insert integrity row: %w", err)
	}

	signature := p.cfg.Signer.SignHead(in.TicketID, sequence, entryHash)
	keyID := p.cfg.Signer.KeyID()
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO lifecycle_heads(ticket_id,organizer_id,last_sequence,canonical_version,last_hash,key_id,signature,changed_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT(ticket_id) DO UPDATE SET last_sequence=EXCLUDED.last_sequence,canonical_version=EXCLUDED.canonical_version,
			last_hash=EXCLUDED.last_hash,key_id=EXCLUDED.key_id,signature=EXCLUDED.signature,changed_at=EXCLUDED.changed_at`,
		in.TicketID, in.OrganizerID, sequence, lifecycle.CanonicalVersion, entryHash, keyID, signature, occurredAt); err != nil {
		return time.Time{}, fmt.Errorf("advance head: %w", err)
	}
	// The snapshot carries the head's own signature, not just its hash: the
	// checkpoint signs this table's contents, so an unsigned snapshot would let
	// anyone who can INSERT here get a forged head into a signed root.
	if _, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_head_changes(ticket_id,organizer_id,sequence,head_hash,canonical_version,key_id,signature) VALUES($1,$2,$3,$4,$5,$6,$7)`,
		in.TicketID, in.OrganizerID, sequence, entryHash, lifecycle.CanonicalVersion, keyID, signature); err != nil {
		return time.Time{}, fmt.Errorf("queue head change: %w", err)
	}
	return occurredAt, nil
}

// TicketIdentity is the ticket-row half of the canonical form.
type TicketIdentity struct{ OrderID, OrganizerID, SlotID uuid.UUID }

// verifyTicketChain recomputes a ticket's whole chain and checks its head
// signature. The caller holds the ticket row lock.
//
// It returns a plain error describing what failed. Every case listed in ADR-021
// §Threat model under "detected cryptographically" surfaces here: coverage in
// both directions, sequence gaps, broken links, unknown key ids, head mismatch.
func (p *Postgres) verifyTicketChain(ctx context.Context, tx *sql.Tx, ticketID uuid.UUID, id TicketIdentity) error {
	// The join binds i.ticket_id = e.ticket_id, it does not merely match on
	// event_id. Without that, an integrity row reassigned to another ticket still
	// joined here and this ticket's scan read clean, while the mismatch was only
	// ever caught by the offline verifier — the gate and the audit disagreeing
	// about the same row (PR #51 review, R2).
	rows, err := tx.QueryContext(ctx, `
		SELECT e.id, e.event_type, e.occurred_at, i.sequence, i.previous_hash, i.entry_hash, i.canonical_version
		FROM lifecycle_events e LEFT JOIN lifecycle_event_integrity i
		  ON i.event_id = e.id AND i.ticket_id = e.ticket_id
		WHERE e.ticket_id=$1 ORDER BY i.sequence`, ticketID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	prev := lifecycle.GenesisHash()
	var count int64
	var last []byte
	for rows.Next() {
		var eventID uuid.UUID
		var eventType string
		var occurredAt time.Time
		var sequence sql.NullInt64
		var previousHash, entryHash []byte
		var version sql.NullInt64
		if err := rows.Scan(&eventID, &eventType, &occurredAt, &sequence, &previousHash, &entryHash, &version); err != nil {
			return err
		}
		// A lifecycle row with no integrity row means the append path was
		// bypassed — an event inserted straight into the table.
		if !sequence.Valid {
			return fmt.Errorf("lifecycle event %s has no integrity row", eventID)
		}
		// Dispatch on the stored version at the gate too, not only in the audit.
		// A version exists because the rules can change; verifying an unknown
		// variant with today's rules judges it by the wrong ones (ADR-017 §5b′).
		if version.Int64 != lifecycle.CanonicalVersion {
			return fmt.Errorf("integrity row for event %s declares canonical version %d, this build writes %d",
				eventID, version.Int64, lifecycle.CanonicalVersion)
		}
		count++
		if sequence.Int64 != count {
			return fmt.Errorf("sequence gap at %d (expected %d) on ticket %s", sequence.Int64, count, ticketID)
		}
		if !bytes.Equal(previousHash, prev) {
			return fmt.Errorf("broken chain link at sequence %d on ticket %s", sequence.Int64, ticketID)
		}
		canonical := lifecycle.CanonicalEvent(lifecycle.Event{
			TicketID: ticketID, OrderID: id.OrderID, OrganizerID: id.OrganizerID, SlotID: id.SlotID,
			Sequence: sequence.Int64, EventID: eventID, Type: eventType, OccurredAt: lifecycle.Normalize(occurredAt),
		})
		want := lifecycle.HashEntry(prev, canonical)
		if !bytes.Equal(want, entryHash) {
			return fmt.Errorf("entry hash mismatch at sequence %d on ticket %s", sequence.Int64, ticketID)
		}
		prev = want
		last = want
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// The other coverage direction: an integrity row whose event does not exist,
	// or whose ticket disagrees with it, is a forged link rather than a gap.
	var orphans int64
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM lifecycle_event_integrity i
		WHERE i.ticket_id=$1 AND NOT EXISTS (SELECT 1 FROM lifecycle_events e WHERE e.id=i.event_id AND e.ticket_id=i.ticket_id)`,
		ticketID).Scan(&orphans); err != nil {
		return err
	}
	if orphans > 0 {
		return fmt.Errorf("%d integrity rows on ticket %s reference no matching lifecycle event", orphans, ticketID)
	}

	var headSeq int64
	var headHash, signature []byte
	var keyID string
	var headVersion int64
	err = tx.QueryRowContext(ctx, `SELECT last_sequence,last_hash,key_id,signature,canonical_version FROM lifecycle_heads WHERE ticket_id=$1`, ticketID).
		Scan(&headSeq, &headHash, &keyID, &signature, &headVersion)
	if errors.Is(err, sql.ErrNoRows) {
		if count == 0 {
			return nil // No events, no head: a ticket that has not been chained yet.
		}
		return fmt.Errorf("ticket %s has %d chained events and no head", ticketID, count)
	}
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("ticket %s has a head and no events", ticketID)
	}
	if headVersion != lifecycle.CanonicalVersion {
		return fmt.Errorf("head for ticket %s declares canonical version %d, this build writes %d", ticketID, headVersion, lifecycle.CanonicalVersion)
	}
	if headSeq != count || !bytes.Equal(headHash, last) {
		return fmt.Errorf("head mismatch on ticket %s: head is sequence %d, chain reaches %d", ticketID, headSeq, count)
	}
	if p.cfg.Keyring == nil {
		return errors.New("no lifecycle keyring configured")
	}
	if err := p.cfg.Keyring.VerifyHead(ticketID, headSeq, keyID, headHash, signature); err != nil {
		return fmt.Errorf("head signature on ticket %s: %w", ticketID, err)
	}
	return nil
}

// alarmData is the integrity alarm payload. Bounded identifiers and enums only:
// no QR payload, no buyer, no guest reference, no raw event body (ADR-003 §D3).
type alarmData struct {
	AlarmID     uuid.UUID `json:"alarm_id"`
	OrganizerID uuid.UUID `json:"organizer_id"`
	TicketID    uuid.UUID `json:"ticket_id"`
	Reason      string    `json:"reason"`
	Disposition string    `json:"disposition"`
	Mode        string    `json:"mode"`
	OccurredAt  time.Time `json:"occurred_at"`
}

// oweAlarm commits an alarm with the decision that caused it. Publishing happens
// later, from the outbox: an admission must never happen without an owed alarm,
// because §D6's fail-open is only defensible while the alarm reaches someone.
func (p *Postgres) oweAlarm(ctx context.Context, tx *sql.Tx, organizerID, ticketID uuid.UUID, reason string, disposition Decision, mode Mode) error {
	id := uuid.New()
	occurredAt := p.now()
	envelope, err := json.Marshal(struct {
		ID         uuid.UUID `json:"id"`
		Type       string    `json:"type"`
		OccurredAt time.Time `json:"occurred_at"`
		Schema     int       `json:"schema"`
		Data       alarmData `json:"data"`
	}{
		ID: id, Type: SubjectIntegrityAlarm, OccurredAt: occurredAt, Schema: 1,
		Data: alarmData{
			AlarmID: id, OrganizerID: organizerID, TicketID: ticketID,
			Reason: reason, Disposition: string(disposition), Mode: string(mode), OccurredAt: occurredAt,
		},
	})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_integrity_alarm_outbox(event_id,subject,envelope) VALUES($1,$2,$3)`,
		id, SubjectIntegrityAlarm, envelope)
	return err
}

func organizerMode(ctx context.Context, tx *sql.Tx, organizerID uuid.UUID) (Mode, error) {
	var mode string
	err := tx.QueryRowContext(ctx, `SELECT mode FROM lifecycle_integrity_organizer_state WHERE organizer_id=$1`, organizerID).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return ModeNormal, nil
	}
	if err != nil {
		return "", err
	}
	return Mode(mode), nil
}

// degradedScan applies ADR-021 §D6 to a ticket whose chain did not verify.
//
// The chain is left strictly alone. Appending a redemption onto an unverified
// predecessor would chain from a corrupt previous_hash and poison the ticket
// permanently, turning one detectable bad row into an unrecoverable history — so
// the degraded admission is recorded only in the quarantine table, which is
// append-only for exactly this reason and is explicitly not cryptographic
// evidence.
func (p *Postgres) degradedScan(ctx context.Context, tx *sql.Tx, ticketID uuid.UUID, id TicketIdentity, occurrenceID uuid.UUID, cause error) (RedeemResult, error) {
	reason := cause.Error()
	now := p.now()

	// Occurrence identity resolves before the quarantine denial on this path
	// too (ADR-025 §D3 binding order) — and before anything else: an occurrence
	// already in the record (trail or quarantine side) replays its original
	// result, whatever state the chain is in now. Without the trail half, a
	// retry of a redemption recorded while the chain was healthy would take a
	// fresh degraded admission here and double-record one physical occurrence.
	if occurrenceID != uuid.Nil {
		replayed, result, replayErr := p.replayByOccurrence(ctx, tx, ticketID, occurrenceID)
		if replayErr != nil {
			return RedeemResult{}, replayErr
		}
		if replayed {
			if commitErr := tx.Commit(); commitErr != nil {
				return RedeemResult{}, commitErr
			}
			return result, nil
		}
	}

	var quarantinedAt time.Time
	err := tx.QueryRowContext(ctx, `SELECT admitted_at FROM lifecycle_integrity_quarantine WHERE ticket_id=$1 AND admitted_at IS NOT NULL`, ticketID).Scan(&quarantinedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return RedeemResult{}, err
	}
	mode, modeErr := organizerMode(ctx, tx, id.OrganizerID)
	if modeErr != nil {
		return RedeemResult{}, modeErr
	}

	// Already took its one admission: deny every later scan, whatever the
	// organizer's mode. This is the single case where the scheme refuses rather
	// than merely records (ADR-021 §Threat model, "not prevented, by construction").
	if err == nil {
		if alarmErr := p.oweAlarm(ctx, tx, id.OrganizerID, ticketID, reason, DecisionIntegrityQuarantined, mode); alarmErr != nil {
			return RedeemResult{}, alarmErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return RedeemResult{}, commitErr
		}
		return RedeemResult{Decision: DecisionIntegrityQuarantined, OccurredAt: quarantinedAt}, nil
	}

	// A human has taken control of this organizer and chosen deny. No admission,
	// so no quarantine row: nothing was admitted to record.
	if mode == ModeOperatorDeny {
		if alarmErr := p.oweAlarm(ctx, tx, id.OrganizerID, ticketID, reason, DecisionIntegrityOperatorControlled, mode); alarmErr != nil {
			return RedeemResult{}, alarmErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return RedeemResult{}, commitErr
		}
		return RedeemResult{Decision: DecisionIntegrityOperatorControlled, OccurredAt: now}, nil
	}

	// First failure for this ticket: admit once, and record that we did — with
	// the occurrence id when the scanner sent one, so its retry can be matched
	// (ADR-025 §D3: the identity rule extends to degraded admissions).
	if _, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,admitted_at,occurrence_id) VALUES($1,$2,$3,$4,$5)`,
		ticketID, id.OrganizerID, reason, now, uuid.NullUUID{UUID: occurrenceID, Valid: occurrenceID != uuid.Nil}); err != nil {
		return RedeemResult{}, err
	}

	// The window is counted from the quarantine rows themselves — one per
	// first-time corrupt ticket — so there is no counter to drift.
	//
	// Under READ COMMITTED this count sees only COMMITTED siblings, so N
	// simultaneous first-time failures can each see just their own row and none
	// crosses the threshold; the flip then happens on the next scan, once they
	// have committed. The escalation is therefore DELAYED by roughly the
	// in-flight width, not bypassed (PR #51 review, R3).
	//
	// That is deliberate, and the alternative is worse. Making the count exact
	// means serializing this path on an organizer row — and the scenario this
	// protection exists for is a canonicalization bug corrupting every chain at
	// doors-open, where EVERY scan takes the degraded path and would then queue
	// behind that one row. That is precisely the organizer-wide contention
	// ADR-021 §Option 1 rejected the money journal for, reintroduced at the worst
	// possible moment. §D6's purpose is to escalate to a human, not to cap
	// admissions precisely — it already concedes fail-open is unbounded against
	// an adversary — so an approximate rate that converges is the right trade.
	var recent int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE organizer_id=$1 AND admitted_at > $2`,
		id.OrganizerID, now.Add(-p.policy().Window)).Scan(&recent); err != nil {
		return RedeemResult{}, err
	}
	if recent >= p.policy().FailureThreshold {
		// Above this rate "our bug" stops being the likely story. Flip to
		// operator-controlled deny: the choice to keep admitting becomes a
		// human's, made knowingly. This scan still takes its admission.
		//
		// The WHERE makes this flip conditional and atomic, and it is load-bearing:
		// the mode was read before this statement, so an unconditional upsert
		// would let an in-flight degraded scan silently revert a human who chose
		// operator_admit in the meantime. The system may only move an organizer
		// OUT of normal; once a human has touched the mode, it is theirs.
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO lifecycle_integrity_organizer_state(organizer_id,mode,mode_set_at,mode_set_by) VALUES($1,$2,$3,'system')
			ON CONFLICT(organizer_id) DO UPDATE SET mode=EXCLUDED.mode,mode_set_at=EXCLUDED.mode_set_at,mode_set_by=EXCLUDED.mode_set_by
			WHERE lifecycle_integrity_organizer_state.mode=$4`,
			id.OrganizerID, string(ModeOperatorDeny), now, string(ModeNormal)); err != nil {
			return RedeemResult{}, err
		}
		// Re-read rather than assume: the flip is conditional, so the authority
		// on the resulting mode is the database, not this goroutine's earlier read.
		if mode, err = organizerMode(ctx, tx, id.OrganizerID); err != nil {
			return RedeemResult{}, err
		}
	}
	if err = p.oweAlarm(ctx, tx, id.OrganizerID, ticketID, reason, DecisionAdmittedDegraded, mode); err != nil {
		return RedeemResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return RedeemResult{}, err
	}
	return RedeemResult{Accepted: true, Decision: DecisionAdmittedDegraded, OccurredAt: now}, nil
}

func (p *Postgres) policy() Policy {
	if p.cfg.Policy.FailureThreshold <= 0 || p.cfg.Policy.Window <= 0 {
		return DefaultPolicy()
	}
	return p.cfg.Policy
}

// SetMode records an operator's explicit choice for an organizer (ADR-021 §D6:
// "the choice to keep admitting is then a human's, made knowingly").
func (p *Postgres) SetMode(ctx context.Context, organizerID uuid.UUID, mode Mode, by string) error {
	switch mode {
	case ModeNormal, ModeOperatorDeny, ModeOperatorAdmit:
	default:
		return fmt.Errorf("unknown integrity mode %q", mode)
	}
	if by == "" {
		return errors.New("an operator decision must record who made it")
	}
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO lifecycle_integrity_organizer_state(organizer_id,mode,mode_set_at,mode_set_by) VALUES($1,$2,$3,$4)
		ON CONFLICT(organizer_id) DO UPDATE SET mode=EXCLUDED.mode,mode_set_at=EXCLUDED.mode_set_at,mode_set_by=EXCLUDED.mode_set_by`,
		organizerID, string(mode), p.now(), by)
	return err
}

// Mode reads an organizer's current posture.
func (p *Postgres) Mode(ctx context.Context, organizerID uuid.UUID) (Mode, error) {
	var mode string
	err := p.db.QueryRowContext(ctx, `SELECT mode FROM lifecycle_integrity_organizer_state WHERE organizer_id=$1`, organizerID).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return ModeNormal, nil
	}
	if err != nil {
		return "", err
	}
	return Mode(mode), nil
}
