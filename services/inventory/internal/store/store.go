package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

//go:embed all:migrations
var migrationsFS embed.FS

var (
	ErrNotFound    = errors.New("not found")
	ErrUnavailable = errors.New("insufficient capacity")
	ErrConflict    = errors.New("conflicting terminal state")
	ErrIdempotency = errors.New("idempotency key reused with different request")
)

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

type Claim struct {
	ID           uuid.UUID `json:"hold_id"`
	OrganizerID  uuid.UUID `json:"organizer_id"`
	PoolID       uuid.UUID `json:"slot_id"`
	Quantity     int32     `json:"quantity"`
	TicketTypeID uuid.UUID `json:"ticket_type_id,omitempty"`
	UnitAmount   int64     `json:"unit_amount,omitempty"`
	Currency     string    `json:"currency,omitempty"`
	Status       string    `json:"status"`
	ExpiresAt    time.Time `json:"expires_at"`
	ServerTime   time.Time `json:"server_time"`
}

type Availability struct {
	SlotID    uuid.UUID `json:"slot_id"`
	Capacity  int32     `json:"capacity"`
	Held      int32     `json:"held"`
	Confirmed int32     `json:"confirmed"`
	Available int32     `json:"available"`
}

type Postgres struct {
	db  *sql.DB
	ttl time.Duration
}

func New(db *sql.DB, ttl time.Duration) *Postgres { return &Postgres{db: db, ttl: ttl} }

func (p *Postgres) Provision(ctx context.Context, eventID, slotID, organizerID uuid.UUID, capacity int32) error {
	if capacity <= 0 {
		return fmt.Errorf("capacity must be positive")
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `INSERT INTO consumed_events(event_id) VALUES($1) ON CONFLICT DO NOTHING`, eventID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO inventory_pools(slot_id,organizer_id,capacity,source_event_id) VALUES($1,$2,$3,$4)
		ON CONFLICT(slot_id) DO UPDATE SET capacity=EXCLUDED.capacity, updated_at=now()
		WHERE inventory_pools.organizer_id=EXCLUDED.organizer_id AND inventory_pools.confirmed_quantity=0
		AND NOT EXISTS(SELECT 1 FROM claims WHERE pool_id=EXCLUDED.slot_id)`, slotID, organizerID, capacity, eventID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func fingerprint(org, slot, ticketType uuid.UUID, qty int32, unitAmount int64, currency string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s:%d:%d:%s", org, slot, ticketType, qty, unitAmount, currency))))
}

func (p *Postgres) CreateHold(ctx context.Context, org, slot, ticketType uuid.UUID, qty int32, unitAmount int64, currency, key string) (Claim, bool, error) {
	if qty <= 0 {
		return Claim{}, false, fmt.Errorf("quantity must be positive")
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var capacity, confirmed int32
	err = tx.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE`, slot, org).Scan(&capacity, &confirmed)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, false, ErrNotFound
	}
	if err != nil {
		return Claim{}, false, err
	}
	var existing Claim
	var fp string
	err = tx.QueryRowContext(ctx, `SELECT id,organizer_id,pool_id,quantity,status,expires_at,now(),request_fingerprint,ticket_type_id,unit_amount,currency FROM claims WHERE organizer_id=$1 AND idempotency_key=$2`, org, key).
		Scan(&existing.ID, &existing.OrganizerID, &existing.PoolID, &existing.Quantity, &existing.Status, &existing.ExpiresAt, &existing.ServerTime, &fp, &existing.TicketTypeID, &existing.UnitAmount, &existing.Currency)
	if err == nil {
		if fp != fingerprint(org, slot, ticketType, qty, unitAmount, currency) {
			return Claim{}, false, ErrIdempotency
		}
		return existing, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Claim{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE claims SET status='expired',updated_at=now() WHERE pool_id=$1 AND status='held' AND expires_at<=now()`, slot); err != nil {
		return Claim{}, false, err
	}
	var held int32
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND status='held' AND expires_at>now()`, slot).Scan(&held); err != nil {
		return Claim{}, false, err
	}
	if confirmed+held+qty > capacity {
		return Claim{}, false, ErrUnavailable
	}
	c := Claim{ID: uuid.New(), OrganizerID: org, PoolID: slot, TicketTypeID: ticketType, Quantity: qty, UnitAmount: unitAmount, Currency: currency, Status: "held"}
	err = tx.QueryRowContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint)
		VALUES($1,$2,$3,$4,$5,$6,$7,'held',now()+$8::interval,$9,$10) RETURNING expires_at,now()`, c.ID, org, slot, ticketType, qty, unitAmount, currency, p.ttl.String(), key, fingerprint(org, slot, ticketType, qty, unitAmount, currency)).Scan(&c.ExpiresAt, &c.ServerTime)
	if err != nil {
		return Claim{}, false, err
	}
	return c, false, tx.Commit()
}

func (p *Postgres) Transition(ctx context.Context, org, id uuid.UUID, target string) (Claim, error) {
	var pool uuid.UUID
	err := p.db.QueryRowContext(ctx, `SELECT pool_id FROM claims WHERE id=$1 AND organizer_id=$2`, id, org).Scan(&pool)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, ErrNotFound
	}
	if err != nil {
		return Claim{}, err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, pool); err != nil {
		return Claim{}, err
	}
	var c Claim
	err = tx.QueryRowContext(ctx, `SELECT id,organizer_id,pool_id,quantity,status,expires_at,now(),ticket_type_id,unit_amount,currency FROM claims WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, id, org).
		Scan(&c.ID, &c.OrganizerID, &c.PoolID, &c.Quantity, &c.Status, &c.ExpiresAt, &c.ServerTime, &c.TicketTypeID, &c.UnitAmount, &c.Currency)
	if err != nil {
		return Claim{}, err
	}
	if c.Status == "held" && !c.ExpiresAt.After(c.ServerTime) {
		c.Status = "expired"
		if _, err = tx.ExecContext(ctx, `UPDATE claims SET status='expired',updated_at=now() WHERE id=$1`, id); err != nil {
			return Claim{}, err
		}
	}
	if c.Status == target {
		return c, tx.Commit()
	}
	if target == "finalizing" && c.Status == "held" {
		c.Status = target
		_, err = tx.ExecContext(ctx, `UPDATE claims SET status='finalizing',updated_at=now() WHERE id=$1`, id)
		if err != nil {
			return c, err
		}
		return c, tx.Commit()
	}
	if c.Status != "held" && c.Status != "finalizing" {
		return c, ErrConflict
	}
	if target == "confirmed" {
		if _, err = tx.ExecContext(ctx, `UPDATE inventory_pools SET confirmed_quantity=confirmed_quantity+$1,updated_at=now() WHERE slot_id=$2`, c.Quantity, pool); err != nil {
			return Claim{}, err
		}
	}
	c.Status = target
	if _, err = tx.ExecContext(ctx, `UPDATE claims SET status=$1,updated_at=now() WHERE id=$2`, target, id); err != nil {
		return Claim{}, err
	}
	return c, tx.Commit()
}

func (p *Postgres) Availability(ctx context.Context, org, slot uuid.UUID) (Availability, error) {
	var a Availability
	a.SlotID = slot
	err := p.db.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity,(SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND status='held' AND expires_at>now()) FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2`, slot, org).Scan(&a.Capacity, &a.Confirmed, &a.Held)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.Available = a.Capacity - a.Confirmed - a.Held
	return a, nil
}
