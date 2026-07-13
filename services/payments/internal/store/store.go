package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

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

type Fact struct {
	ID          uuid.UUID         `json:"fact_id"`
	OrganizerID uuid.UUID         `json:"organizer_id"`
	Type        string            `json:"fact_type"`
	OccurredAt  time.Time         `json:"occurred_at"`
	BuyerID     uuid.UUID         `json:"buyer_id"`
	Amount      int64             `json:"amount"`
	Currency    string            `json:"currency"`
	Payload     map[string]string `json:"payload,omitempty"`
}

type Entry struct {
	Fact
	Sequence                           int64
	PreviousHash, EntryHash, Signature []byte
	KeyID                              string
}

type Journal struct {
	db    *sql.DB
	key   []byte
	keyID string
}

func New(db *sql.DB, keyID string, key []byte) *Journal {
	return &Journal{db: db, key: key, keyID: keyID}
}

func canonical(f Fact, seq int64) ([]byte, error) {
	payload, err := json.Marshal(f.Payload)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("1\n%s\n%d\n%s\n%s\n%s\n%s\n%d\n%s\n%s", f.OrganizerID, seq, f.ID, f.Type, f.OccurredAt.UTC().Format(time.RFC3339Nano), f.BuyerID, f.Amount, f.Currency, payload)), nil
}
func hash(prev, canonical []byte) []byte {
	h := sha256.New()
	_, _ = h.Write(prev)
	_, _ = h.Write(canonical)
	return h.Sum(nil)
}
func sign(key, sum []byte) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(sum)
	return h.Sum(nil)
}

func validate(f Fact) error {
	if f.ID == uuid.Nil || f.OrganizerID == uuid.Nil || f.BuyerID == uuid.Nil || f.Amount < 0 || len(f.Currency) != 3 {
		return errors.New("invalid journal fact")
	}
	allowedTypes := map[string]bool{"order.created": true, "order.completed": true, "order.failed": true, "payment.authorized": true, "payment.captured": true, "payment.declined": true, "payment.timeout": true}
	if !allowedTypes[f.Type] {
		return errors.New("unsupported journal fact type")
	}
	for k := range f.Payload {
		l := strings.ToLower(k)
		if strings.Contains(l, "email") || strings.Contains(l, "name") || strings.Contains(l, "pan") || strings.Contains(l, "cvv") {
			return fmt.Errorf("PII/payment field %q forbidden in journal", k)
		}
		if k != "order_id" {
			return fmt.Errorf("payload field %q is not allowed", k)
		}
	}
	if _, err := uuid.Parse(f.Payload["order_id"]); err != nil {
		return errors.New("valid order_id payload required")
	}
	return nil
}

func (j *Journal) Append(ctx context.Context, f Fact) (Entry, bool, error) {
	if err := validate(f); err != nil {
		return Entry{}, false, err
	}
	if f.OccurredAt.IsZero() {
		f.OccurredAt = time.Now().UTC()
	}
	// PostgreSQL timestamptz stores microseconds. Sign the persisted precision,
	// otherwise verification would canonicalize different bytes after reload.
	f.OccurredAt = f.OccurredAt.UTC().Truncate(time.Microsecond)
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var existing Entry
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT organizer_id,sequence,fact_type,occurred_at,buyer_id,amount,currency,payload,previous_hash,entry_hash,key_id,signature FROM journal_entries WHERE fact_id=$1`, f.ID).Scan(&existing.OrganizerID, &existing.Sequence, &existing.Type, &existing.OccurredAt, &existing.BuyerID, &existing.Amount, &existing.Currency, &raw, &existing.PreviousHash, &existing.EntryHash, &existing.KeyID, &existing.Signature)
	if err == nil {
		existing.ID = f.ID
		if err := json.Unmarshal(raw, &existing.Payload); err != nil {
			return Entry{}, false, err
		}
		want, _ := canonical(f, existing.Sequence)
		got, _ := canonical(existing.Fact, existing.Sequence)
		if !hmac.Equal(want, got) {
			return Entry{}, false, errors.New("fact id reused with different content")
		}
		return existing, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO journal_heads(organizer_id) VALUES($1) ON CONFLICT DO NOTHING`, f.OrganizerID)
	if err != nil {
		return Entry{}, false, err
	}
	var seq int64
	var prev []byte
	err = tx.QueryRowContext(ctx, `SELECT last_sequence,last_hash FROM journal_heads WHERE organizer_id=$1 FOR UPDATE`, f.OrganizerID).Scan(&seq, &prev)
	if err != nil {
		return Entry{}, false, err
	}
	seq++
	canon, err := canonical(f, seq)
	if err != nil {
		return Entry{}, false, err
	}
	sum := hash(prev, canon)
	sig := sign(j.key, sum)
	_, err = tx.ExecContext(ctx, `INSERT INTO journal_entries(fact_id,organizer_id,sequence,fact_type,occurred_at,buyer_id,amount,currency,payload,previous_hash,entry_hash,key_id,signature) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, f.ID, f.OrganizerID, seq, f.Type, f.OccurredAt, f.BuyerID, f.Amount, f.Currency, f.Payload, prev, sum, j.keyID, sig)
	if err != nil {
		return Entry{}, false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE journal_heads SET last_sequence=$2,last_hash=$3 WHERE organizer_id=$1`, f.OrganizerID, seq, sum)
	if err != nil {
		return Entry{}, false, err
	}
	return Entry{Fact: f, Sequence: seq, PreviousHash: prev, EntryHash: sum, KeyID: j.keyID, Signature: sig}, false, tx.Commit()
}

func (j *Journal) Verify(ctx context.Context) error {
	rows, err := j.db.QueryContext(ctx, `SELECT fact_id,organizer_id,sequence,fact_type,occurred_at,buyer_id,amount,currency,payload,previous_hash,entry_hash,key_id,signature FROM journal_entries ORDER BY organizer_id,sequence`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	prevByOrg := map[uuid.UUID][]byte{}
	seqByOrg := map[uuid.UUID]int64{}
	for rows.Next() {
		var e Entry
		var raw []byte
		if err := rows.Scan(&e.ID, &e.OrganizerID, &e.Sequence, &e.Type, &e.OccurredAt, &e.BuyerID, &e.Amount, &e.Currency, &raw, &e.PreviousHash, &e.EntryHash, &e.KeyID, &e.Signature); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &e.Payload); err != nil {
			return err
		}
		prev := prevByOrg[e.OrganizerID]
		if prev == nil {
			prev = make([]byte, 32)
		}
		if e.Sequence != seqByOrg[e.OrganizerID]+1 || !hmac.Equal(prev, e.PreviousHash) {
			return fmt.Errorf("broken chain organizer=%s sequence=%d", e.OrganizerID, e.Sequence)
		}
		if e.KeyID != j.keyID {
			return fmt.Errorf("unknown key id %q", e.KeyID)
		}
		c, _ := canonical(e.Fact, e.Sequence)
		sum := hash(prev, c)
		if !hmac.Equal(sum, e.EntryHash) || !hmac.Equal(sign(j.key, sum), e.Signature) {
			return fmt.Errorf("invalid hash/signature organizer=%s sequence=%d", e.OrganizerID, e.Sequence)
		}
		prevByOrg[e.OrganizerID] = sum
		seqByOrg[e.OrganizerID] = e.Sequence
	}
	if err := rows.Err(); err != nil {
		return err
	}
	headRows, err := j.db.QueryContext(ctx, `SELECT organizer_id,last_sequence,last_hash FROM journal_heads`)
	if err != nil {
		return err
	}
	defer func() { _ = headRows.Close() }()
	for headRows.Next() {
		var org uuid.UUID
		var seq int64
		var sum []byte
		if err := headRows.Scan(&org, &seq, &sum); err != nil {
			return err
		}
		if seqByOrg[org] != seq || !hmac.Equal(prevByOrg[org], sum) {
			return fmt.Errorf("journal head mismatch organizer=%s", org)
		}
		delete(seqByOrg, org)
	}
	if err := headRows.Err(); err != nil {
		return err
	}
	if len(seqByOrg) != 0 {
		return errors.New("journal entries missing head")
	}
	return nil
}

func (j *Journal) BindOperation(ctx context.Context, org uuid.UUID, key, fingerprint string) (string, uuid.UUID, time.Time, bool, error) {
	result, err := j.db.ExecContext(ctx, `INSERT INTO payment_operations(organizer_id,idempotency_key,request_fingerprint) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, org, key, fingerprint)
	if err != nil {
		return "", uuid.Nil, time.Time{}, false, err
	}
	var stored string
	var status sql.NullString
	var factID uuid.NullUUID
	var occurredAt, leaseUntil time.Time
	err = j.db.QueryRowContext(ctx, `SELECT request_fingerprint,status,fact_id,occurred_at,lease_until FROM payment_operations WHERE organizer_id=$1 AND idempotency_key=$2`, org, key).Scan(&stored, &status, &factID, &occurredAt, &leaseUntil)
	if err != nil {
		return "", uuid.Nil, time.Time{}, false, err
	}
	if stored != fingerprint {
		return "", uuid.Nil, time.Time{}, false, errors.New("idempotency key reused with different request")
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 && !status.Valid {
		taken, err := j.db.ExecContext(ctx, `UPDATE payment_operations SET lease_until=now()+interval '30 seconds' WHERE organizer_id=$1 AND idempotency_key=$2 AND status IS NULL AND lease_until<=now()`, org, key)
		if err != nil {
			return "", uuid.Nil, time.Time{}, false, err
		}
		rows, _ := taken.RowsAffected()
		if rows == 0 {
			return "", uuid.Nil, time.Time{}, false, errors.New("payment operation in progress")
		}
	}
	return status.String, factID.UUID, occurredAt.UTC().Truncate(time.Microsecond), status.Valid, nil
}

func (j *Journal) CompleteOperation(ctx context.Context, org uuid.UUID, key, status string, factID uuid.UUID) error {
	_, err := j.db.ExecContext(ctx, `UPDATE payment_operations SET status=$3,fact_id=$4 WHERE organizer_id=$1 AND idempotency_key=$2 AND status IS NULL`, org, key, status, factID)
	return err
}

func Hex(b []byte) string { return hex.EncodeToString(b) }
