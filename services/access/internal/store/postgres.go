package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

var ErrTicketCredential = fmt.Errorf("ticket credential does not match a redeemable ticket")

//go:embed all:migrations
var migrationsFS embed.FS

func Migrate(ctx context.Context, db *sql.DB) error {
	f, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, f)
	if err != nil {
		return err
	}
	_, err = p.Up(ctx)
	return err
}

type Ticket struct {
	ID, OrderID, GuestOrderRef, OrganizerID, BuyerID, SlotID, TicketTypeID uuid.UUID
	Payload                                                                string
	IssuedAt                                                               time.Time
}

// LifecycleEvent is one trail entry as readers see it. Sequence is the
// integrity chain's authoritative order (ADR-025 §D5); it is nil only for
// legacy rows the backfill job has not adopted yet, so after normal startup
// every served event carries it. OccurredAt is the claimed physical time and
// never reorders chained events.
type LifecycleEvent struct {
	ID         uuid.UUID `json:"id"`
	Type       string    `json:"type"`
	Sequence   *int64    `json:"sequence,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

type IssueInput struct {
	EventID uuid.UUID
	Tickets []Ticket
}

// RedeemInput contains only signed immutable ticket facts. It deliberately
// excludes mutable admission policy, which belongs to later lifecycle work.
//
// OccurrenceID is the scanner-minted identity of this physical gate decision
// (ADR-025 §D3); Nil means an old scanner and today's semantics exactly,
// including the grandfathered deterministic redeemed id. When set, OccurredAt
// is the device's claimed admission time and must be non-zero.
type RedeemInput struct {
	TicketID, OrderID, OrganizerID, SlotID uuid.UUID
	OccurrenceID                           uuid.UUID
	OccurredAt                             time.Time
}

type RedeemResult struct {
	// Accepted means the holder went through the door. It stays true for
	// ADR-021 §D6's degraded admission: the gate opened, and a caller asking
	// "did they get in" must not have to know why.
	Accepted bool
	// Decision says which of the five outcomes this was. Accepted alone cannot
	// tell a clean redemption from a fail-open admission, and that difference is
	// the whole point of the alarm.
	Decision   Decision
	OccurredAt time.Time
	// Replayed marks a transport retry matched by occurrence id. ADR-025 §D3
	// forbids returning a replay as a bare success: actuation must be able to
	// tell a first-time result from a replayed one.
	Replayed bool
}

type Postgres struct {
	db  *sql.DB
	cfg Config
}

// New builds the store. Config carries the lifecycle signer and keyring: every
// append writes a signed integrity row (ADR-021 §D1), so there is no valid
// configuration in which a writer has no signer. Pass a Config with a nil Signer
// only for the verify-only paths, which hold public keys and cannot append.
func New(db *sql.DB, cfg Config) *Postgres { return &Postgres{db: db, cfg: cfg} }

func (p *Postgres) Issue(ctx context.Context, in IssueInput) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `INSERT INTO consumed_events(event_id) VALUES($1) ON CONFLICT DO NOTHING`, in.EventID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return tx.Commit()
	}
	for _, t := range in.Tickets {
		if err := p.insertIssuedTicket(ctx, tx, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// insertIssuedTicket writes one ticket row and its `issued` event on the caller's
// transaction. Extracted so ordinary issuance and the exchange switch (TKT-166) cannot
// drift apart: two copies of "insert the row, then chain its first lifecycle event" would
// be two places to forget the append, and the verifier would call the second one tampering.
func (p *Postgres) insertIssuedTicket(ctx context.Context, tx *sql.Tx, t Ticket) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO tickets(id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, t.ID, t.OrderID, t.GuestOrderRef, t.OrganizerID, t.BuyerID, t.SlotID, t.TicketTypeID, t.Payload, t.IssuedAt); err != nil {
		return err
	}
	// The ticket row was created in this transaction, so nothing else can see
	// it yet: the append needs no FOR UPDATE to serialize against.
	issued := uuid.NewSHA1(uuid.NameSpaceOID, []byte(t.ID.String()+":issued"))
	_, err := p.appendLifecycle(ctx, tx, appendInput{
		TicketID: t.ID, OrderID: t.OrderID, OrganizerID: t.OrganizerID, SlotID: t.SlotID,
		EventID: issued, Type: "issued", OccurredAt: t.IssuedAt,
	})
	return err
}

func (p *Postgres) PendingDeliveries(ctx context.Context, orderID uuid.UUID) ([]Ticket, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT t.id,t.order_id,t.guest_order_ref,t.organizer_id,t.buyer_id,t.slot_id,t.ticket_type_id,t.qr_payload,t.issued_at FROM tickets t WHERE t.order_id=$1 AND NOT EXISTS (SELECT 1 FROM lifecycle_events l WHERE l.ticket_id=t.id AND l.event_type='delivered') ORDER BY t.issued_at`, orderID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Ticket
	for rows.Next() {
		var t Ticket
		if err := rows.Scan(&t.ID, &t.OrderID, &t.GuestOrderRef, &t.OrganizerID, &t.BuyerID, &t.SlotID, &t.TicketTypeID, &t.Payload, &t.IssuedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (p *Postgres) DeliveryID(ctx context.Context, ticketID uuid.UUID) (uuid.UUID, error) {
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(ticketID.String()+":delivery"))
	_, err := p.db.ExecContext(ctx, `INSERT INTO delivery_attempts(ticket_id,message_id) VALUES($1,$2) ON CONFLICT(ticket_id) DO NOTHING`, ticketID, id)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// MarkDelivered records delivery, at most once per ticket.
//
// The redelivery case is why this locks and re-checks rather than leaning on
// ON CONFLICT DO NOTHING as it used to: a no-op insert must not reach the append
// path, or it would chain an event that was never written and collide on the
// integrity row's primary key — turning a silently-successful redelivery into a
// hard error. Idempotency is settled before the append, not by it.
func (p *Postgres) MarkDelivered(ctx context.Context, ticketID, messageID uuid.UUID) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Same ticket row Redeem locks: the chain serializes per ticket and never
	// per organizer (ADR-021 §D1, ADR-010).
	var id TicketIdentity
	err = tx.QueryRowContext(ctx, `SELECT order_id,organizer_id,slot_id FROM tickets WHERE id=$1 FOR UPDATE`, ticketID).
		Scan(&id.OrderID, &id.OrganizerID, &id.SlotID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTicketCredential
		}
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE delivery_attempts SET accepted_at=now() WHERE ticket_id=$1 AND message_id=$2`, ticketID, messageID); err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM lifecycle_events WHERE ticket_id=$1 AND event_type='delivered')`, ticketID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return tx.Commit()
	}
	eventID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(ticketID.String()+":delivered"))
	if _, err = p.appendLifecycle(ctx, tx, appendInput{
		TicketID: ticketID, OrderID: id.OrderID, OrganizerID: id.OrganizerID, SlotID: id.SlotID,
		EventID: eventID, Type: "delivered",
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *Postgres) Tickets(ctx context.Context, ref uuid.UUID) ([]Ticket, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at FROM tickets WHERE guest_order_ref=$1 ORDER BY issued_at,id`, ref)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Ticket
	for rows.Next() {
		var t Ticket
		if err := rows.Scan(&t.ID, &t.OrderID, &t.GuestOrderRef, &t.OrganizerID, &t.BuyerID, &t.SlotID, &t.TicketTypeID, &t.Payload, &t.IssuedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// History reads a ticket's trail in the integrity chain's order (ADR-025 §D5).
// The (occurred_at,id) tail of the ORDER BY only ever decides among unchained
// legacy rows before the backfill job runs — the historical read order, kept so
// pre-backfill reads stay stable.
func (p *Postgres) History(ctx context.Context, ticketID uuid.UUID) ([]LifecycleEvent, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT e.id, e.event_type, i.sequence, e.occurred_at
		FROM lifecycle_events e LEFT JOIN lifecycle_event_integrity i
		  ON i.event_id = e.id AND i.ticket_id = e.ticket_id
		WHERE e.ticket_id=$1 ORDER BY i.sequence NULLS LAST, e.occurred_at, e.id`, ticketID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LifecycleEvent
	for rows.Next() {
		var e LifecycleEvent
		if err := rows.Scan(&e.ID, &e.Type, &e.Sequence, &e.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *Postgres) TicketForQR(ctx context.Context, ref, ticket uuid.UUID) (string, error) {
	var payload string
	err := p.db.QueryRowContext(ctx, `SELECT qr_payload FROM tickets WHERE id=$1 AND guest_order_ref=$2`, ticket, ref).Scan(&payload)
	if err != nil {
		return "", fmt.Errorf("ticket QR: %w", err)
	}
	return payload, nil
}

// Redeem is the historical single-entry entry point. It now routes through
// Scan (TKT-87) so every caller is policy-correct: on a single or unknown
// slot it behaves byte-for-byte as it always has; on a pass slot it records
// an entry, never a redemption (ADR-025 §D1).
func (p *Postgres) Redeem(ctx context.Context, in RedeemInput) (RedeemResult, error) {
	return p.Scan(ctx, ScanInput{RedeemInput: in, Direction: AdmissionEntry})
}

// redeemSingle appends exactly one redemption event. Locking the canonical
// ticket before reading the trace makes a concurrent loser observe the
// winner's timestamp instead of leaking a unique-constraint error to the
// scanner.
//
// The chain is verified under that same lock, before the duplicate check, since
// ADR-003 §Decision 2 decides admission FROM the trace: a tampered trace is a
// tampered answer, and there is deliberately no independent read model to
// contradict it. A chain that does not verify still admits, once
// (ADR-021 §Decision 6) — see degradedScan for why that is the safer error.
//
// Admission history is the union of the trace and the quarantine record
// (ADR-025 §Decision 2): a degraded admission lives only on the quarantine
// side, so the verified path must consult it too — checked before the
// redeemed-event lookup, because already_redeemed would hide the degraded
// admission and skip §D6's escalation.
func (p *Postgres) redeemSingle(ctx context.Context, in RedeemInput) (RedeemResult, error) {
	if in.OccurrenceID != uuid.Nil {
		if in.OccurrenceID.Version() != 4 || in.OccurrenceID.Variant() != uuid.RFC4122 {
			return RedeemResult{}, fmt.Errorf("occurrence id %s is not a UUIDv4 (ADR-025 §D3)", in.OccurrenceID)
		}
		if in.OccurredAt.IsZero() {
			return RedeemResult{}, errors.New("a gate occurrence carries its claimed admission time")
		}
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

	// A refunded ticket is refused BEFORE the chain is verified, and the ordering is
	// the point (TKT-157, ADR-038). A chain that does not verify takes the §D6
	// degraded posture and ADMITS ONCE; checking the refund after it would let a
	// refunded ticket through the gate exactly once, which is the failure this denial
	// exists to prevent. Commercial validity is not a chain-health question.
	refunded, err := ticketRefunded(ctx, tx, in.TicketID)
	if err != nil {
		return RedeemResult{}, err
	}
	if refunded {
		if err = tx.Commit(); err != nil {
			return RedeemResult{}, err
		}
		return RedeemResult{Decision: DecisionRefunded, OccurredAt: p.now()}, nil
	}
	// Same position, same reason, different fact (TKT-166, ADR-039). An exchanged ticket
	// has a live replacement somewhere; letting the degraded posture admit it once would
	// admit the exchange twice.
	exchanged, err := ticketExchanged(ctx, tx, in.TicketID)
	if err != nil {
		return RedeemResult{}, err
	}
	if exchanged {
		if err = tx.Commit(); err != nil {
			return RedeemResult{}, err
		}
		return RedeemResult{Decision: DecisionExchanged, OccurredAt: p.now()}, nil
	}

	if chainErr := p.verifyTicketChain(ctx, tx, in.TicketID, id); chainErr != nil {
		// degradedScan commits the transaction itself: the quarantine record and
		// the owed alarm must land whatever this scan decides.
		return p.degradedScan(ctx, tx, in.TicketID, id, in.OccurrenceID, chainErr)
	}

	// Occurrence identity resolves before any quarantine denial (ADR-025 §D3
	// binding order): a lost-response retry must get its original result back,
	// never a second-scan escalation.
	if in.OccurrenceID != uuid.Nil {
		replayed, result, replayErr := p.replayByOccurrence(ctx, tx, in.TicketID, in.OccurrenceID)
		if replayErr != nil {
			return RedeemResult{}, replayErr
		}
		if replayed {
			if err = tx.Commit(); err != nil {
				return RedeemResult{}, err
			}
			return result, nil
		}
	}

	var quarantineReason string
	var quarantinedAt time.Time
	// Live degraded admissions only (admitted_at set): reconciliation-learned
	// records are recordings of admissions that already happened elsewhere and
	// never turn a verified scan into a denial.
	err = tx.QueryRowContext(ctx, `SELECT reason,admitted_at FROM lifecycle_integrity_quarantine WHERE ticket_id=$1 AND admitted_at IS NOT NULL`, in.TicketID).
		Scan(&quarantineReason, &quarantinedAt)
	if err == nil {
		// Already took its one degraded admission (ADR-021 §D6), and the replay
		// check above says this is not that occurrence's retry: deny and
		// escalate, exactly as the degraded path does.
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

	var redeemedAt time.Time
	err = tx.QueryRowContext(ctx, `SELECT occurred_at FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, in.TicketID).Scan(&redeemedAt)
	if err == nil {
		if err = tx.Commit(); err != nil {
			return RedeemResult{}, err
		}
		return RedeemResult{Decision: DecisionAlreadyRedeemed, OccurredAt: redeemedAt}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RedeemResult{}, err
	}

	// The scanner's occurrence id becomes the event id — one identity model, no
	// exceptions (ADR-025 §D3). The deterministic id is grandfathered for old
	// scanners only.
	eventID := in.OccurrenceID
	var occurredAt time.Time
	if in.OccurrenceID == uuid.Nil {
		eventID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(in.TicketID.String()+":redeemed"))
	} else {
		occurredAt = in.OccurredAt
	}
	redeemedAt, err = p.appendLifecycle(ctx, tx, appendInput{
		TicketID: in.TicketID, OrderID: id.OrderID, OrganizerID: id.OrganizerID, SlotID: id.SlotID,
		EventID: eventID, Type: "redeemed", OccurredAt: occurredAt,
	})
	if err != nil {
		// Same reasoning as RecordAdmission: the ticket lock serializes
		// same-ticket callers, so a taken event id can only be this occurrence
		// id landing on another ticket concurrently.
		if in.OccurrenceID != uuid.Nil && errors.Is(err, errEventIDTaken) {
			return RedeemResult{}, fmt.Errorf("occurrence %s: %w", in.OccurrenceID, ErrOccurrenceCollision)
		}
		return RedeemResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return RedeemResult{}, err
	}
	return RedeemResult{Accepted: true, Decision: DecisionAccepted, OccurredAt: redeemedAt}, nil
}

// replayByOccurrence resolves an occurrence id against the whole admission
// record — the trail first, then the quarantine side (admission history is the
// union of both, ADR-025 §D2). This is pure identity equality, never a decision
// from the trace: it only ever hands back what this same occurrence already
// got, so it is safe on the degraded path too.
func (p *Postgres) replayByOccurrence(ctx context.Context, tx *sql.Tx, ticketID, occ uuid.UUID) (bool, RedeemResult, error) {
	var storedTicket uuid.UUID
	var storedType string
	var storedAt time.Time
	err := tx.QueryRowContext(ctx, `SELECT ticket_id,event_type,occurred_at FROM lifecycle_events WHERE id=$1`, occ).
		Scan(&storedTicket, &storedType, &storedAt)
	if err == nil {
		// Any recorded admission occurrence — redeemed, entry or exit —
		// replays its original result here: this helper also serves the
		// degraded path, where §D3's identity-before-denial order must hold
		// for pass events too (ai-review G4). A duplicate_admit stays a
		// collision: its original outcome was a conflict recording, not an
		// acceptance this result shape can honestly replay.
		switch {
		case storedTicket != ticketID:
			return false, RedeemResult{}, fmt.Errorf("occurrence %s: %w", occ, ErrOccurrenceCollision)
		case storedType == "redeemed", storedType == string(AdmissionEntry), storedType == string(AdmissionExit):
			return true, RedeemResult{Accepted: true, Decision: DecisionAccepted, OccurredAt: storedAt, Replayed: true}, nil
		default:
			return false, RedeemResult{}, fmt.Errorf("occurrence %s: %w", occ, ErrOccurrenceCollision)
		}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, RedeemResult{}, err
	}

	var quarantinedTicket uuid.UUID
	var admittedAt, occurredAt sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT ticket_id,admitted_at,occurred_at FROM lifecycle_integrity_quarantine WHERE occurrence_id=$1`, occ).
		Scan(&quarantinedTicket, &admittedAt, &occurredAt)
	if err == nil {
		if quarantinedTicket != ticketID {
			return false, RedeemResult{}, fmt.Errorf("occurrence %s: %w", occ, ErrOccurrenceCollision)
		}
		at := occurredAt.Time
		if admittedAt.Valid {
			at = admittedAt.Time
		}
		return true, RedeemResult{Accepted: true, Decision: DecisionAdmittedDegraded, OccurredAt: at, Replayed: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, RedeemResult{}, err
	}
	return false, RedeemResult{}, nil
}

// AdmissionEventType is a repeatable admission event (ADR-025 §D1). The
// singleton types (issued, delivered, redeemed) are deliberately not values of
// this type: they have their own write paths and their own uniqueness.
type AdmissionEventType string

const (
	AdmissionEntry          AdmissionEventType = "entry"
	AdmissionExit           AdmissionEventType = "exit"
	AdmissionDuplicateAdmit AdmissionEventType = "duplicate_admit"
)

// isUniqueViolation reports a Postgres unique violation (SQLSTATE 23505)
// without importing driver-specific error types (same shape as catalog's).
func isUniqueViolation(err error) bool {
	type coder interface{ SQLState() string }
	var c coder
	return errors.As(err, &c) && c.SQLState() == "23505"
}

// ErrOccurrenceCollision is an occurrence id reused across tickets or event
// types. That is never a transport retry — treating it as one would hand back
// another admission's result — so it is an error, not a replay.
var ErrOccurrenceCollision = errors.New("occurrence id already recorded for a different ticket or event type")

// RecordAdmissionInput is one physical gate occurrence (ADR-025 §D3): the
// occurrence id is minted by the scanner, persisted before the gate opens, and
// reused verbatim on transport retries. OccurredAt is the device's claimed
// admission time — recorded, never attested (§D5).
type RecordAdmissionInput struct {
	TicketID, OrderID, OrganizerID, SlotID uuid.UUID
	OccurrenceID                           uuid.UUID
	Type                                   AdmissionEventType
	OccurredAt                             time.Time
}

// RecordAdmissionResult reports what was stored. Replayed distinguishes a
// retry from a first write — ADR-025 §D3 forbids returning a replay as a bare
// success, because actuation must be able to tell them apart.
type RecordAdmissionResult struct {
	Event    LifecycleEvent
	Replayed bool
}

// RecordAdmission appends one repeatable admission event, idempotently by
// occurrence id. Replay is resolved under the ticket lock BEFORE
// appendLifecycle (ADR-025 §D4): the append path is never invoked for an
// already-recorded occurrence.
//
// Unlike Redeem, this does not verify the chain first: Redeem decides admission
// FROM the trace (ADR-003 §D2), while this records an admission that already
// physically happened — the same posture as Issue and MarkDelivered. What a
// reconciliation caller should do when a ticket's chain is broken (quarantine
// widening, §D6 alarms) is deferred scope and lives with those tickets.
func (p *Postgres) RecordAdmission(ctx context.Context, in RecordAdmissionInput) (RecordAdmissionResult, error) {
	switch in.Type {
	case AdmissionEntry, AdmissionExit, AdmissionDuplicateAdmit:
	default:
		return RecordAdmissionResult{}, fmt.Errorf("event type %q is not a repeatable admission type", in.Type)
	}
	if in.OccurrenceID == uuid.Nil || in.OccurrenceID.Version() != 4 || in.OccurrenceID.Variant() != uuid.RFC4122 {
		return RecordAdmissionResult{}, fmt.Errorf("occurrence id %s is not a UUIDv4 (ADR-025 §D3)", in.OccurrenceID)
	}
	if in.OccurredAt.IsZero() {
		return RecordAdmissionResult{}, errors.New("a gate occurrence carries its claimed admission time")
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return RecordAdmissionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var id TicketIdentity
	err = tx.QueryRowContext(ctx, `SELECT order_id,organizer_id,slot_id FROM tickets WHERE id=$1 FOR UPDATE`, in.TicketID).
		Scan(&id.OrderID, &id.OrganizerID, &id.SlotID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RecordAdmissionResult{}, ErrTicketCredential
		}
		return RecordAdmissionResult{}, err
	}
	if id.OrderID != in.OrderID || id.OrganizerID != in.OrganizerID || id.SlotID != in.SlotID {
		return RecordAdmissionResult{}, ErrTicketCredential
	}

	// Replay check, under the lock: the event id IS the occurrence id.
	var stored LifecycleEvent
	var storedTicket uuid.UUID
	err = tx.QueryRowContext(ctx, `
		SELECT e.id, e.ticket_id, e.event_type, i.sequence, e.occurred_at
		FROM lifecycle_events e LEFT JOIN lifecycle_event_integrity i
		  ON i.event_id = e.id AND i.ticket_id = e.ticket_id
		WHERE e.id=$1`, in.OccurrenceID).
		Scan(&stored.ID, &storedTicket, &stored.Type, &stored.Sequence, &stored.OccurredAt)
	if err == nil {
		if storedTicket != in.TicketID || stored.Type != string(in.Type) {
			return RecordAdmissionResult{}, fmt.Errorf("occurrence %s: %w", in.OccurrenceID, ErrOccurrenceCollision)
		}
		if err = tx.Commit(); err != nil {
			return RecordAdmissionResult{}, err
		}
		return RecordAdmissionResult{Event: stored, Replayed: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RecordAdmissionResult{}, err
	}

	occurredAt, err := p.appendLifecycle(ctx, tx, appendInput{
		TicketID: in.TicketID, OrderID: id.OrderID, OrganizerID: id.OrganizerID, SlotID: id.SlotID,
		EventID: in.OccurrenceID, Type: string(in.Type), OccurredAt: in.OccurredAt,
	})
	if err != nil {
		// The ticket lock serializes same-ticket callers, so the event id
		// being taken can only be the same occurrence id landing on ANOTHER
		// ticket concurrently — its replay check and ours both ran before
		// either insert committed. Same answer as the visible case. Any other
		// unique violation from the chain is corruption and passes through.
		if errors.Is(err, errEventIDTaken) {
			return RecordAdmissionResult{}, fmt.Errorf("occurrence %s: %w", in.OccurrenceID, ErrOccurrenceCollision)
		}
		return RecordAdmissionResult{}, err
	}
	var sequence int64
	if err = tx.QueryRowContext(ctx, `SELECT sequence FROM lifecycle_event_integrity WHERE event_id=$1`, in.OccurrenceID).Scan(&sequence); err != nil {
		return RecordAdmissionResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return RecordAdmissionResult{}, err
	}
	return RecordAdmissionResult{
		Event: LifecycleEvent{ID: in.OccurrenceID, Type: string(in.Type), Sequence: &sequence, OccurredAt: occurredAt},
	}, nil
}
