package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SubjectAdmissionConflictAlarm carries admission-conflict alarms (ADR-025
// §D6): reconciliation found an offline admit the authoritative trace would
// have rejected. Deliberately a separate operational class from the integrity
// alarms — the chain is valid; the world disagreed with it. Its own schema-1
// contract, following ADR-017 thereafter.
const SubjectAdmissionConflictAlarm = "platform.access.admission-conflict.alarm"

// AdmissionSkewBound is how far a device-claimed admission time may sit from
// the server clock before reconciliation flags it. Validation records and
// flags, never rejects (ADR-025 §D5): device time is claimed, not attested,
// and dropping a physical admission from the record is the worse failure.
const AdmissionSkewBound = 24 * time.Hour

// ReconcileOccurrence is one offline gate decision being synced after
// reconnect. Identity fields come from the verified QR credential; OccurredAt
// is the device's persisted admission time (ADR-025 §D6).
type ReconcileOccurrence struct {
	TicketID, OrderID, OrganizerID, SlotID uuid.UUID
	OccurrenceID                           uuid.UUID
	OccurredAt                             time.Time
	// Type is the factual admission direction the device recorded (TKT-87).
	// Zero value means entry — an old scanner's occurrence keeps today's
	// semantics exactly. Exit is meaningful only against pass slots; on a
	// single slot it is refused per occurrence (ErrExitNotApplicable), never
	// recorded (ADR-025 §D1).
	Type AdmissionEventType
}

// ReconcileOutcome says what reconciliation did with one occurrence.
type ReconcileOutcome string

const (
	// ReconcileRecorded — the occurrence entered the record: as the ticket's
	// redemption, or as a quarantine-side record when the chain is broken.
	ReconcileRecorded ReconcileOutcome = "recorded"
	// ReconcileConflict — the trace already held a different admission;
	// duplicate_admit was appended and a conflict alarm owed (ADR-025 §D6).
	ReconcileConflict ReconcileOutcome = "conflict"
	// ReconcileSynced — this occurrence was already recorded; a replay, never
	// a second event or alarm.
	ReconcileSynced ReconcileOutcome = "synced"
)

// ReconcileResult reports one occurrence's reconciliation.
type ReconcileResult struct {
	OccurrenceID uuid.UUID
	Outcome      ReconcileOutcome
	// OccurredAt is the stored admission time for this occurrence.
	OccurredAt time.Time
	// SkewFlagged marks a device time outside AdmissionSkewBound. Recorded and
	// flagged, never rejected (§D5).
	SkewFlagged bool
}

// ReconcileAdmission records one offline occurrence (ADR-025 §D2/§D6).
// Reconciliation of an admission that already physically happened is
// recording, not deciding: it cannot retroactively deny, so every occurrence
// lands somewhere — the trail when the chain verifies, the quarantine side
// when it does not. Idempotent by occurrence id; each call is its own
// transaction so one bad occurrence never rolls back a batch.
func (p *Postgres) ReconcileAdmission(ctx context.Context, in ReconcileOccurrence) (ReconcileResult, error) {
	if in.OccurrenceID == uuid.Nil || in.OccurrenceID.Version() != 4 || in.OccurrenceID.Variant() != uuid.RFC4122 {
		return ReconcileResult{}, fmt.Errorf("occurrence id %s is not a UUIDv4 (ADR-025 §D3)", in.OccurrenceID)
	}
	if in.OccurredAt.IsZero() {
		return ReconcileResult{}, errors.New("a gate occurrence carries its claimed admission time")
	}
	direction := in.Type
	if direction == "" {
		direction = AdmissionEntry
	}
	if direction != AdmissionEntry && direction != AdmissionExit {
		return ReconcileResult{}, fmt.Errorf("event type %q is not a reconcilable admission direction", direction)
	}
	skewFlagged := in.OccurredAt.Sub(p.now()).Abs() > AdmissionSkewBound

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return ReconcileResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var id TicketIdentity
	err = tx.QueryRowContext(ctx, `SELECT order_id,organizer_id,slot_id FROM tickets WHERE id=$1 FOR UPDATE`, in.TicketID).
		Scan(&id.OrderID, &id.OrganizerID, &id.SlotID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ReconcileResult{}, ErrTicketCredential
		}
		return ReconcileResult{}, err
	}
	if id.OrderID != in.OrderID || id.OrganizerID != in.OrganizerID || id.SlotID != in.SlotID {
		return ReconcileResult{}, ErrTicketCredential
	}

	policy, err := p.slotPolicy(ctx, tx, id.SlotID)
	if err != nil {
		return ReconcileResult{}, err
	}
	if !policy.IsPass() && direction == AdmissionExit {
		// Single tickets have no exit vocabulary (ADR-025 §D1): refuse this
		// occurrence — its own per-item rejection, never a recorded fact and
		// never a failed batch.
		return ReconcileResult{}, ErrExitNotApplicable
	}

	// Replay before anything else (ADR-025 §D4): an occurrence already in the
	// record — trail or quarantine side — is synced, never re-recorded. The
	// match is bound to the factual type (plan verdict 3): an entry-direction
	// occurrence may match a row recorded under the single vocabulary (the
	// ticket predates the policy projection), an exit matches only an exit.
	synced, syncedAt, err := p.reconcileReplay(ctx, tx, in.TicketID, in.OccurrenceID, direction)
	if err != nil {
		return ReconcileResult{}, err
	}
	if synced {
		if err = tx.Commit(); err != nil {
			return ReconcileResult{}, err
		}
		return ReconcileResult{OccurrenceID: in.OccurrenceID, Outcome: ReconcileSynced, OccurredAt: syncedAt, SkewFlagged: skewFlagged}, nil
	}

	if chainErr := p.verifyTicketChain(ctx, tx, in.TicketID, id); chainErr != nil {
		// Appending onto an unverified predecessor would poison the chain
		// (ADR-021 §D6), so the occurrence lands as a quarantine-side record:
		// repeatable per ticket, keyed by occurrence, device time preserved,
		// admitted_at NULL — this is a recording, not a live admission. The
		// factual direction is preserved (TKT-87): without it a quarantined
		// entry and exit are indistinguishable and the derived projection
		// could never re-evaluate them. No conflict alarm: the integrity
		// alarm class owns broken chains, and every live scan of this ticket
		// already raises it.
		if _, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,occurrence_id,occurred_at,event_type) VALUES($1,$2,$3,$4,$5,$6)`,
			in.TicketID, id.OrganizerID, chainErr.Error(), in.OccurrenceID, in.OccurredAt, quarantineEventType(policy, direction)); err != nil {
			return ReconcileResult{}, err
		}
		if err = tx.Commit(); err != nil {
			return ReconcileResult{}, err
		}
		return ReconcileResult{OccurrenceID: in.OccurrenceID, Outcome: ReconcileRecorded, OccurredAt: in.OccurredAt, SkewFlagged: skewFlagged}, nil
	}

	if policy.IsPass() {
		// Pass reconciliation records factual entry/exit only — never
		// `redeemed`, never `duplicate_admit` (ADR-025 §D2). Policy conflicts
		// are derived and revisable: re-evaluated with the new fact included,
		// alarmed conservatively through the raise/withdraw class.
		occurredAt, appendErr := p.appendLifecycle(ctx, tx, appendInput{
			TicketID: in.TicketID, OrderID: id.OrderID, OrganizerID: id.OrganizerID, SlotID: id.SlotID,
			EventID: in.OccurrenceID, Type: string(direction), OccurredAt: in.OccurredAt,
		})
		if appendErr != nil {
			if errors.Is(appendErr, errEventIDTaken) {
				return ReconcileResult{}, fmt.Errorf("occurrence %s: %w", in.OccurrenceID, ErrOccurrenceCollision)
			}
			return ReconcileResult{}, appendErr
		}
		if err = p.evaluatePolicyAlarms(ctx, tx, in.TicketID, id, policy); err != nil {
			return ReconcileResult{}, err
		}
		if err = tx.Commit(); err != nil {
			return ReconcileResult{}, err
		}
		return ReconcileResult{OccurrenceID: in.OccurrenceID, Outcome: ReconcileRecorded, OccurredAt: occurredAt, SkewFlagged: skewFlagged}, nil
	}

	var redeemedAt time.Time
	err = tx.QueryRowContext(ctx, `SELECT occurred_at FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, in.TicketID).Scan(&redeemedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// No prior admission: this occurrence IS the redemption, with the
		// scanner's id and the device-claimed time (§D3/§D5).
		occurredAt, appendErr := p.appendLifecycle(ctx, tx, appendInput{
			TicketID: in.TicketID, OrderID: id.OrderID, OrganizerID: id.OrganizerID, SlotID: id.SlotID,
			EventID: in.OccurrenceID, Type: "redeemed", OccurredAt: in.OccurredAt,
		})
		if appendErr != nil {
			if errors.Is(appendErr, errEventIDTaken) {
				return ReconcileResult{}, fmt.Errorf("occurrence %s: %w", in.OccurrenceID, ErrOccurrenceCollision)
			}
			return ReconcileResult{}, appendErr
		}
		if err = tx.Commit(); err != nil {
			return ReconcileResult{}, err
		}
		return ReconcileResult{OccurrenceID: in.OccurrenceID, Outcome: ReconcileRecorded, OccurredAt: occurredAt, SkewFlagged: skewFlagged}, nil
	}
	if err != nil {
		return ReconcileResult{}, err
	}

	// The trace already holds a different admission and this ticket is
	// single-entry: the offline admit is a conflict (ADR-025 §D6). Append
	// duplicate_admit — visibility, not judgment on which admission was
	// "real" — and owe the conflict alarm.
	occurredAt, err := p.appendLifecycle(ctx, tx, appendInput{
		TicketID: in.TicketID, OrderID: id.OrderID, OrganizerID: id.OrganizerID, SlotID: id.SlotID,
		EventID: in.OccurrenceID, Type: string(AdmissionDuplicateAdmit), OccurredAt: in.OccurredAt,
	})
	if err != nil {
		if errors.Is(err, errEventIDTaken) {
			return ReconcileResult{}, fmt.Errorf("occurrence %s: %w", in.OccurrenceID, ErrOccurrenceCollision)
		}
		return ReconcileResult{}, err
	}
	if err = p.oweConflictAlarm(ctx, tx, id.OrganizerID, in.TicketID, in.OccurrenceID, in.OccurredAt, skewFlagged); err != nil {
		return ReconcileResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return ReconcileResult{}, err
	}
	return ReconcileResult{OccurrenceID: in.OccurrenceID, Outcome: ReconcileConflict, OccurredAt: occurredAt, SkewFlagged: skewFlagged}, nil
}

// quarantineEventType is the factual type a quarantine-side record carries.
// Pass facts keep their direction; single-vocabulary occurrences keep the
// column's redeemed default semantics explicitly.
func quarantineEventType(policy ReEntryPolicy, direction AdmissionEventType) string {
	if policy.IsPass() {
		return string(direction)
	}
	return "redeemed"
}

// reconcileReplay reports whether the occurrence is already recorded, on
// either side of the admission union, and the stored time if so. A hit on
// another ticket is a collision, never a replay; the type binding mirrors
// replayAdmissionOccurrence — an entry-direction occurrence may match a row
// recorded under the single vocabulary, an exit matches only an exit, and a
// live degraded admission replays un-directioned (§D3).
func (p *Postgres) reconcileReplay(ctx context.Context, tx *sql.Tx, ticketID, occ uuid.UUID, direction AdmissionEventType) (bool, time.Time, error) {
	matches := func(stored string) bool {
		if direction == AdmissionExit {
			return stored == string(AdmissionExit)
		}
		return stored == string(AdmissionEntry) || stored == "redeemed" || stored == string(AdmissionDuplicateAdmit)
	}
	var storedTicket uuid.UUID
	var storedType string
	var storedAt time.Time
	err := tx.QueryRowContext(ctx, `SELECT ticket_id,event_type,occurred_at FROM lifecycle_events WHERE id=$1`, occ).Scan(&storedTicket, &storedType, &storedAt)
	if err == nil {
		if storedTicket != ticketID || !matches(storedType) {
			return false, time.Time{}, fmt.Errorf("occurrence %s: %w", occ, ErrOccurrenceCollision)
		}
		return true, storedAt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, time.Time{}, err
	}
	var quarantinedTicket uuid.UUID
	var quarantinedType string
	var admittedAt, occurredAt sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT ticket_id,event_type,admitted_at,occurred_at FROM lifecycle_integrity_quarantine WHERE occurrence_id=$1`, occ).
		Scan(&quarantinedTicket, &quarantinedType, &admittedAt, &occurredAt)
	if err == nil {
		if quarantinedTicket != ticketID {
			return false, time.Time{}, fmt.Errorf("occurrence %s: %w", occ, ErrOccurrenceCollision)
		}
		if admittedAt.Valid {
			return true, admittedAt.Time, nil
		}
		if !matches(quarantinedType) {
			return false, time.Time{}, fmt.Errorf("occurrence %s: %w", occ, ErrOccurrenceCollision)
		}
		return true, occurredAt.Time, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, time.Time{}, err
	}
	return false, time.Time{}, nil
}

// conflictAlarmData is the admission-conflict alarm payload. Bounded
// identifiers and enums only (ADR-025 §D9).
type conflictAlarmData struct {
	AlarmID          uuid.UUID `json:"alarm_id"`
	OrganizerID      uuid.UUID `json:"organizer_id"`
	TicketID         uuid.UUID `json:"ticket_id"`
	OccurrenceID     uuid.UUID `json:"occurrence_id"`
	DeviceOccurredAt time.Time `json:"device_occurred_at"`
	SkewFlagged      bool      `json:"skew_flagged"`
}

// oweConflictAlarm commits an admission-conflict alarm into the shared alarm
// outbox — same durable-owing shape as the integrity class (0003): the
// duplicate_admit append and the owed alarm land in one transaction.
func (p *Postgres) oweConflictAlarm(ctx context.Context, tx *sql.Tx, organizerID, ticketID, occurrenceID uuid.UUID, deviceOccurredAt time.Time, skewFlagged bool) error {
	id := uuid.New()
	envelope, err := json.Marshal(struct {
		ID         uuid.UUID         `json:"id"`
		Type       string            `json:"type"`
		OccurredAt time.Time         `json:"occurred_at"`
		Schema     int               `json:"schema"`
		Data       conflictAlarmData `json:"data"`
	}{
		ID: id, Type: SubjectAdmissionConflictAlarm, OccurredAt: p.now(), Schema: 1,
		Data: conflictAlarmData{
			AlarmID: id, OrganizerID: organizerID, TicketID: ticketID,
			OccurrenceID: occurrenceID, DeviceOccurredAt: deviceOccurredAt, SkewFlagged: skewFlagged,
		},
	})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_integrity_alarm_outbox(event_id,subject,envelope) VALUES($1,$2,$3)`,
		id, SubjectAdmissionConflictAlarm, envelope)
	return err
}
