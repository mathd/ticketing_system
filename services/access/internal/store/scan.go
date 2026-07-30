package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrExitNotApplicable is an exit occurrence against a single-policy slot:
// single tickets have no entry/exit vocabulary (ADR-025 §D1), so the
// occurrence is refused rather than recorded — a scanner fleet bug to
// surface, not history to mint.
var ErrExitNotApplicable = errors.New("exit is not applicable to a single-entry slot")

// ScanInput is one live gate decision. Direction defaults to entry; exit is
// meaningful only for pass (multi/count_limited) slots.
type ScanInput struct {
	RedeemInput
	Direction AdmissionEventType
}

// Scan is the policy-aware live gate entry point (TKT-87). It resolves the
// slot's projected re_entry policy and dispatches: single (and unknown)
// slots run the unchanged redemption path; pass slots run the entry/exit
// path, which never appends `redeemed` (ADR-025 §D1). Redeem delegates here,
// so EVERY caller is policy-correct — there is no path that redeems a pass
// ticket by picking the wrong method.
func (p *Postgres) Scan(ctx context.Context, in ScanInput) (RedeemResult, error) {
	direction := in.Direction
	if direction == "" {
		direction = AdmissionEntry
	}
	if direction != AdmissionEntry && direction != AdmissionExit {
		return RedeemResult{}, fmt.Errorf("direction %q is not an admission direction", direction)
	}
	policy, err := p.slotPolicy(ctx, p.db, in.SlotID)
	if err != nil {
		return RedeemResult{}, err
	}
	if !policy.IsPass() {
		if direction == AdmissionExit {
			return RedeemResult{Decision: DecisionExitNotApplicable, OccurredAt: p.now()}, nil
		}
		return p.redeemSingle(ctx, in.RedeemInput)
	}
	return p.admitPass(ctx, in.RedeemInput, direction, policy)
}

// admitPass decides one live pass admission FROM the trace (ADR-003 §D2),
// under the ticket lock, and appends the factual entry/exit through the
// chained append path. Live denials append nothing and owe no alarm: a denied
// scan is not an admission, and keeping denials off the trail is what lets a
// late cross-device fact revise the derived picture (ADR-025 §D2).
func (p *Postgres) admitPass(ctx context.Context, in RedeemInput, direction AdmissionEventType, policy ReEntryPolicy) (RedeemResult, error) {
	// No occurrence id, no pass admission (§D3): a server-minted id voids
	// retry idempotency; a derived deterministic id forges a lost-response
	// retry into evidence of a second admission. Denied distinguishably —
	// the fixable cause is the scanner's protocol version.
	if in.OccurrenceID == uuid.Nil {
		return RedeemResult{Decision: DecisionOccurrenceRequired, OccurredAt: p.now()}, nil
	}
	if in.OccurrenceID.Version() != 4 || in.OccurrenceID.Variant() != uuid.RFC4122 {
		return RedeemResult{}, fmt.Errorf("occurrence id %s is not a UUIDv4 (ADR-025 §D3)", in.OccurrenceID)
	}
	if in.OccurredAt.IsZero() {
		return RedeemResult{}, errors.New("a gate occurrence carries its claimed admission time")
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return RedeemResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var id TicketIdentity
	err = tx.QueryRowContext(ctx, `SELECT order_id,organizer_id,slot_id FROM tickets WHERE id=$1 FOR UPDATE`, in.TicketID).
		Scan(&id.OrderID, &id.OrganizerID, &id.SlotID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RedeemResult{}, ErrTicketCredential
		}
		return RedeemResult{}, err
	}
	if id.OrderID != in.OrderID || id.OrganizerID != in.OrganizerID || id.SlotID != in.SlotID {
		return RedeemResult{}, ErrTicketCredential
	}

	// A chain that does not verify takes the unchanged §D6 posture for
	// ENTRIES — admit once, alarm, deny later distinct occurrences. Policy
	// evaluation needs a verified trace (count and inside-state are
	// trace-derived), so degraded pass scans are not policy-evaluated.
	// degradedScan commits itself.
	//
	// An EXIT admits nobody, so it must never consume the ticket's one
	// degraded admission (ai-review K1): replay resolves first (§D3's binding
	// identity-before-denial order), then the exit is denied distinguishably,
	// records nothing, and still owes the integrity alarm — the operator
	// learns the chain is corrupt either way.
	if chainErr := p.verifyTicketChain(ctx, tx, in.TicketID, id); chainErr != nil {
		if direction == AdmissionExit {
			replayed, result, replayErr := p.replayAdmissionOccurrence(ctx, tx, in.TicketID, in.OccurrenceID, direction)
			if replayErr != nil {
				return RedeemResult{}, replayErr
			}
			if !replayed {
				mode, modeErr := organizerMode(ctx, tx, id.OrganizerID)
				if modeErr != nil {
					return RedeemResult{}, modeErr
				}
				if alarmErr := p.oweAlarm(ctx, tx, id.OrganizerID, in.TicketID, chainErr.Error(), DecisionExitUnverified, mode); alarmErr != nil {
					return RedeemResult{}, alarmErr
				}
				result = RedeemResult{Decision: DecisionExitUnverified, OccurredAt: p.now()}
			}
			if err = tx.Commit(); err != nil {
				return RedeemResult{}, err
			}
			return result, nil
		}
		return p.degradedScan(ctx, tx, in.TicketID, id, in.OccurrenceID, chainErr)
	}

	// Occurrence replay before any denial (§D3 binding order), bound to the
	// requested direction.
	replayed, result, err := p.replayAdmissionOccurrence(ctx, tx, in.TicketID, in.OccurrenceID, direction)
	if err != nil {
		return RedeemResult{}, err
	}
	if replayed {
		if err = tx.Commit(); err != nil {
			return RedeemResult{}, err
		}
		return result, nil
	}

	// Already took its one degraded admission and this is not that
	// occurrence's retry: deny and escalate — same as the redemption path.
	var quarantineReason string
	var quarantinedAt time.Time
	err = tx.QueryRowContext(ctx, `SELECT reason,admitted_at FROM lifecycle_integrity_quarantine WHERE ticket_id=$1 AND admitted_at IS NOT NULL`, in.TicketID).
		Scan(&quarantineReason, &quarantinedAt)
	if err == nil {
		mode, modeErr := organizerMode(ctx, tx, id.OrganizerID)
		if modeErr != nil {
			return RedeemResult{}, modeErr
		}
		if alarmErr := p.oweAlarm(ctx, tx, id.OrganizerID, in.TicketID, quarantineReason, DecisionIntegrityQuarantined, mode); alarmErr != nil {
			return RedeemResult{}, alarmErr
		}
		if err = tx.Commit(); err != nil {
			return RedeemResult{}, err
		}
		return RedeemResult{Decision: DecisionIntegrityQuarantined, OccurredAt: quarantinedAt}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RedeemResult{}, err
	}

	facts, err := p.admissionFacts(ctx, tx, in.TicketID)
	if err != nil {
		return RedeemResult{}, err
	}
	if verdict := EvaluateLiveAdmission(policy, facts, direction); verdict != DecisionAccepted {
		if err = tx.Commit(); err != nil {
			return RedeemResult{}, err
		}
		return RedeemResult{Decision: verdict, OccurredAt: p.now()}, nil
	}

	occurredAt, err := p.appendLifecycle(ctx, tx, appendInput{
		TicketID: in.TicketID, OrderID: id.OrderID, OrganizerID: id.OrganizerID, SlotID: id.SlotID,
		EventID: in.OccurrenceID, Type: string(direction), OccurredAt: in.OccurredAt,
	})
	if err != nil {
		// The ticket lock serializes same-ticket callers: a taken event id can
		// only be this occurrence id landing on another ticket concurrently.
		if errors.Is(err, errEventIDTaken) {
			return RedeemResult{}, fmt.Errorf("occurrence %s: %w", in.OccurrenceID, ErrOccurrenceCollision)
		}
		return RedeemResult{}, err
	}
	// Re-evaluate the derived conflict projection with the new fact included:
	// a live exit is what withdraws a reconciliation-raised exit_required.
	if err = p.evaluatePolicyAlarms(ctx, tx, in.TicketID, id, policy); err != nil {
		return RedeemResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return RedeemResult{}, err
	}
	return RedeemResult{Accepted: true, Decision: DecisionAccepted, OccurredAt: occurredAt}, nil
}

// replayAdmissionOccurrence resolves an occurrence id against the admission
// union for the pass path, bound to the requested direction. An entry-direction
// request may match a row recorded under the single vocabulary (`redeemed`,
// `duplicate_admit`) — a ticket admitted before the policy projection existed —
// because that physical occurrence IS recorded; an exit request matches only a
// stored exit, since no exit can predate the pass vocabulary. Any other
// mismatch is a collision, never a replay.
func (p *Postgres) replayAdmissionOccurrence(ctx context.Context, tx *sql.Tx, ticketID, occ uuid.UUID, direction AdmissionEventType) (bool, RedeemResult, error) {
	matches := func(stored string) bool {
		if direction == AdmissionExit {
			return stored == string(AdmissionExit)
		}
		return stored == string(AdmissionEntry) || stored == "redeemed" || stored == string(AdmissionDuplicateAdmit)
	}

	var storedTicket uuid.UUID
	var storedType string
	var storedAt time.Time
	err := tx.QueryRowContext(ctx, `SELECT ticket_id,event_type,occurred_at FROM lifecycle_events WHERE id=$1`, occ).
		Scan(&storedTicket, &storedType, &storedAt)
	if err == nil {
		if storedTicket != ticketID || !matches(storedType) {
			return false, RedeemResult{}, fmt.Errorf("occurrence %s: %w", occ, ErrOccurrenceCollision)
		}
		return true, RedeemResult{Accepted: true, Decision: DecisionAccepted, OccurredAt: storedAt, Replayed: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, RedeemResult{}, err
	}

	var quarantinedTicket uuid.UUID
	var quarantinedType string
	var admittedAt, occurredAt sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT ticket_id,event_type,admitted_at,occurred_at FROM lifecycle_integrity_quarantine WHERE occurrence_id=$1`, occ).
		Scan(&quarantinedTicket, &quarantinedType, &admittedAt, &occurredAt)
	if err == nil {
		if quarantinedTicket != ticketID {
			return false, RedeemResult{}, fmt.Errorf("occurrence %s: %w", occ, ErrOccurrenceCollision)
		}
		if admittedAt.Valid {
			// A live degraded admission is un-directioned: its retry replays
			// whatever the chain state is now (§D3, the identity rule extends
			// to degraded admissions).
			return true, RedeemResult{Accepted: true, Decision: DecisionAdmittedDegraded, OccurredAt: admittedAt.Time, Replayed: true}, nil
		}
		if !matches(quarantinedType) {
			return false, RedeemResult{}, fmt.Errorf("occurrence %s: %w", occ, ErrOccurrenceCollision)
		}
		return true, RedeemResult{Accepted: true, Decision: DecisionAccepted, OccurredAt: occurredAt.Time, Replayed: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, RedeemResult{}, err
	}
	return false, RedeemResult{}, nil
}

// admissionFacts loads the union the policy evaluates over (ADR-025 §D2):
// trail entry/exit rows plus quarantine-side facts — reconciliation-learned
// typed entry/exit, and the live degraded admission as an un-directioned
// entry-equivalent (it consumed a real admission).
func (p *Postgres) admissionFacts(ctx context.Context, tx *sql.Tx, ticketID uuid.UUID) ([]AdmissionFact, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT e.id, e.event_type, e.occurred_at, COALESCE(i.sequence, 0), false
		FROM lifecycle_events e LEFT JOIN lifecycle_event_integrity i
		  ON i.event_id = e.id AND i.ticket_id = e.ticket_id
		WHERE e.ticket_id=$1 AND e.event_type IN ('entry','exit','redeemed','duplicate_admit')
		UNION ALL
		SELECT COALESCE(q.occurrence_id, q.quarantine_id), q.event_type,
		       COALESCE(q.admitted_at, q.occurred_at), 0, (q.admitted_at IS NOT NULL)
		FROM lifecycle_integrity_quarantine q
		WHERE q.ticket_id=$1 AND (q.admitted_at IS NOT NULL OR q.event_type IN ('entry','exit','redeemed'))`, ticketID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AdmissionFact
	for rows.Next() {
		var f AdmissionFact
		var eventType string
		if err := rows.Scan(&f.OccurrenceID, &eventType, &f.OccurredAt, &f.Sequence, &f.Undirected); err != nil {
			return nil, err
		}
		// A redemption or duplicate_admit recorded under the single
		// vocabulary is a physical admission (ai-review G3, second-pass S5):
		// the replay path already honors those occurrences, so the state
		// derivation must consume their allowance too — un-directioned
		// entry-equivalents, exactly like a live degraded admission.
		if eventType == "redeemed" || eventType == string(AdmissionDuplicateAdmit) {
			f.Type = AdmissionEntry
			f.Undirected = true
		} else {
			f.Type = AdmissionEventType(eventType)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// policyConflictID is the stable identity consumers upsert on: one id per
// (ticket, rule, occurrence) conflict, shared by its raise and withdrawal.
func policyConflictID(ticketID uuid.UUID, c PolicyConflict) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(SubjectAdmissionPolicyConflictAlarm+":"+ticketID.String()+":"+string(c.Rule)+":"+c.OccurrenceID.String()))
}

// policyConflictAlarmData is the derived-conflict alarm payload. Bounded
// bounded identifiers, enums and operational scalars; no free text, no nested
// objects (ADR-025 §D9, amended TKT-119). Revisable is always true: this
// class is a projection that can be withdrawn, never trail evidence.
type policyConflictAlarmData struct {
	AlarmID      uuid.UUID `json:"alarm_id"`
	ConflictID   uuid.UUID `json:"conflict_id"`
	OrganizerID  uuid.UUID `json:"organizer_id"`
	TicketID     uuid.UUID `json:"ticket_id"`
	SlotID       uuid.UUID `json:"slot_id"`
	Rule         string    `json:"rule"`
	OccurrenceID uuid.UUID `json:"occurrence_id"`
	Status       string    `json:"status"`
	Version      int       `json:"version"`
	Revisable    bool      `json:"revisable"`
}

// evaluatePolicyAlarms recomputes the ticket's derived conflict set from the
// union and owes one raise/withdraw alarm per transition (ADR-025 §D2:
// alarmed conservatively, revisable — an alarm can be withdrawn, an appended
// event cannot). The pass_policy_conflicts rows exist only so this diff
// survives the outbox drain; they are rebuildable and never consulted by an
// admission decision (ADR-003 §D2). Caller holds the ticket lock, which
// serializes same-ticket evaluation.
func (p *Postgres) evaluatePolicyAlarms(ctx context.Context, tx *sql.Tx, ticketID uuid.UUID, id TicketIdentity, policy ReEntryPolicy) error {
	facts, err := p.admissionFacts(ctx, tx, ticketID)
	if err != nil {
		return err
	}
	derived := DerivePolicyConflicts(policy, facts)

	rows, err := tx.QueryContext(ctx, `SELECT rule,occurrence_id,status,version FROM pass_policy_conflicts WHERE ticket_id=$1 FOR UPDATE`, ticketID)
	if err != nil {
		return err
	}
	current := map[PolicyConflict]string{}
	versions := map[PolicyConflict]int{}
	for rows.Next() {
		var rule, status string
		var occ uuid.UUID
		var version int
		if err := rows.Scan(&rule, &occ, &status, &version); err != nil {
			_ = rows.Close()
			return err
		}
		c := PolicyConflict{Rule: PolicyConflictRule(rule), OccurrenceID: occ}
		current[c] = status
		versions[c] = version
	}
	if err := rows.Close(); err != nil {
		return err
	}

	raises, withdraws := DiffPolicyConflicts(current, derived)
	transition := func(c PolicyConflict, status string) error {
		version := versions[c] + 1
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pass_policy_conflicts(ticket_id,organizer_id,slot_id,rule,occurrence_id,status,version)
			VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT(ticket_id,rule,occurrence_id) DO UPDATE SET status=EXCLUDED.status,version=EXCLUDED.version,updated_at=now()`,
			ticketID, id.OrganizerID, id.SlotID, string(c.Rule), c.OccurrenceID, status, version); err != nil {
			return err
		}
		conflictID := policyConflictID(ticketID, c)
		alarmID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(conflictID.String()+":"+fmt.Sprint(version)))
		envelope, err := policyConflictAlarmEnvelope(alarmID, p.now(), policyConflictAlarmData{
			AlarmID: alarmID, ConflictID: conflictID, OrganizerID: id.OrganizerID, TicketID: ticketID,
			SlotID: id.SlotID, Rule: string(c.Rule), OccurrenceID: c.OccurrenceID,
			Status: status, Version: version, Revisable: true,
		})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_integrity_alarm_outbox(event_id,subject,envelope) VALUES($1,$2,$3)`,
			alarmID, SubjectAdmissionPolicyConflictAlarm, envelope)
		return err
	}
	for _, c := range raises {
		if err := transition(c, "raised"); err != nil {
			return err
		}
	}
	for _, c := range withdraws {
		if err := transition(c, "withdrawn"); err != nil {
			return err
		}
	}
	return nil
}
