package store

import (
	"context"
	"database/sql"
	"embed"
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

type LifecycleEvent struct {
	ID         uuid.UUID `json:"id"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurred_at"`
}

type IssueInput struct {
	EventID uuid.UUID
	Tickets []Ticket
}

// RedeemInput contains only signed immutable ticket facts. It deliberately
// excludes mutable admission policy, which belongs to later lifecycle work.
type RedeemInput struct {
	TicketID, OrderID, OrganizerID, SlotID uuid.UUID
}

type RedeemResult struct {
	Accepted   bool
	OccurredAt time.Time
}

type Postgres struct{ db *sql.DB }

func New(db *sql.DB) *Postgres { return &Postgres{db: db} }

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
		if _, err = tx.ExecContext(ctx, `INSERT INTO tickets(id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, t.ID, t.OrderID, t.GuestOrderRef, t.OrganizerID, t.BuyerID, t.SlotID, t.TicketTypeID, t.Payload, t.IssuedAt); err != nil {
			return err
		}
		issued := uuid.NewSHA1(uuid.NameSpaceOID, []byte(t.ID.String()+":issued"))
		if _, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_events(id,ticket_id,event_type,occurred_at) VALUES($1,$2,'issued',$3)`, issued, t.ID, t.IssuedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
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

func (p *Postgres) MarkDelivered(ctx context.Context, ticketID, messageID uuid.UUID) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE delivery_attempts SET accepted_at=now() WHERE ticket_id=$1 AND message_id=$2`, ticketID, messageID); err != nil {
		return err
	}
	eventID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(ticketID.String()+":delivered"))
	if _, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_events(id,ticket_id,event_type) VALUES($1,$2,'delivered') ON CONFLICT(ticket_id,event_type) DO NOTHING`, eventID, ticketID); err != nil {
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

func (p *Postgres) History(ctx context.Context, ticketID uuid.UUID) ([]LifecycleEvent, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT id,event_type,occurred_at FROM lifecycle_events WHERE ticket_id=$1 ORDER BY occurred_at,id`, ticketID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LifecycleEvent
	for rows.Next() {
		var e LifecycleEvent
		if err := rows.Scan(&e.ID, &e.Type, &e.OccurredAt); err != nil {
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

// Redeem appends exactly one redemption event. Locking the canonical ticket
// before reading the trace makes a concurrent loser observe the winner's
// timestamp instead of leaking a unique-constraint error to the scanner.
func (p *Postgres) Redeem(ctx context.Context, in RedeemInput) (RedeemResult, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return RedeemResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var orderID, organizerID, slotID uuid.UUID
	err = tx.QueryRowContext(ctx, `SELECT order_id,organizer_id,slot_id FROM tickets WHERE id=$1 FOR UPDATE`, in.TicketID).Scan(&orderID, &organizerID, &slotID)
	if err != nil {
		if err == sql.ErrNoRows {
			return RedeemResult{}, ErrTicketCredential
		}
		return RedeemResult{}, err
	}
	if orderID != in.OrderID || organizerID != in.OrganizerID || slotID != in.SlotID {
		return RedeemResult{}, ErrTicketCredential
	}

	var redeemedAt time.Time
	err = tx.QueryRowContext(ctx, `SELECT occurred_at FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, in.TicketID).Scan(&redeemedAt)
	if err == nil {
		if err = tx.Commit(); err != nil {
			return RedeemResult{}, err
		}
		return RedeemResult{OccurredAt: redeemedAt}, nil
	}
	if err != sql.ErrNoRows {
		return RedeemResult{}, err
	}

	eventID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(in.TicketID.String()+":redeemed"))
	err = tx.QueryRowContext(ctx, `INSERT INTO lifecycle_events(id,ticket_id,event_type) VALUES($1,$2,'redeemed') RETURNING occurred_at`, eventID, in.TicketID).Scan(&redeemedAt)
	if err != nil {
		return RedeemResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return RedeemResult{}, err
	}
	return RedeemResult{Accepted: true, OccurredAt: redeemedAt}, nil
}
