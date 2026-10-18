package store

// Enrolled scanner devices (ai-review S1). See migration 0009 for why the
// credential is per-device and stored hashed.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrScannerDeviceUnknown is the ONE refusal for every failing device lookup: an
// unenrolled token, a revoked device, a malformed value. One error rather than
// three because the caller is at a door with a phone, and the difference between
// "never enrolled" and "revoked at 19:04" is information for an operator reading
// a log, not for whoever is holding the phone.
var ErrScannerDeviceUnknown = errors.New("scanner device is not enrolled")

// ScannerDevice is one enrolled gate device as an operator sees it. It never
// carries the token: the plaintext exists exactly once, in EnrolScannerDevice's
// return value, and is not recoverable afterwards.
type ScannerDevice struct {
	ID          uuid.UUID
	OrganizerID uuid.UUID
	Label       string
	CreatedAt   time.Time
	RevokedAt   *time.Time
	LastSeenAt  *time.Time
}

// scannerTokenBytes is the entropy in an enrolment token. 256 bits, matching the
// password-reset tokens: the value is typed once into a pairing screen and then
// lives in the device's storage, so length costs an operator one paste and costs
// a guesser everything.
const scannerTokenBytes = 32

// hashScannerToken is the stored form. Trimmed first, because the token arrives
// through a pairing field a human pasted into and through an HTTP header, and
// both are places a stray space survives — an untrimmed hash would refuse a
// correct token in a way that looks like a revoked device.
func hashScannerToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// EnrolScannerDevice registers one device and returns it with its plaintext
// token, which is the only time that value exists outside the caller's hands.
func (p *Postgres) EnrolScannerDevice(ctx context.Context, organizer uuid.UUID, label string) (ScannerDevice, string, error) {
	label = strings.TrimSpace(label)
	if organizer == uuid.Nil || label == "" {
		return ScannerDevice{}, "", errors.New("organizer id and a non-empty label are required")
	}
	raw := make([]byte, scannerTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return ScannerDevice{}, "", fmt.Errorf("generate scanner token: %w", err)
	}
	token := hex.EncodeToString(raw)

	device := ScannerDevice{ID: uuid.New(), OrganizerID: organizer, Label: label}
	err := p.db.QueryRowContext(ctx,
		`INSERT INTO scanner_devices(id, organizer_id, label, token_hash)
		 VALUES ($1, $2, $3, $4) RETURNING created_at`,
		device.ID, organizer, label, hashScannerToken(token)).Scan(&device.CreatedAt)
	if err != nil {
		return ScannerDevice{}, "", fmt.Errorf("enrol scanner device: %w", err)
	}
	return device, token, nil
}

// AuthenticateScannerDevice resolves a presented token to a live device.
//
// The lookup is by HASH, so the query carries no credential and a slow-query log
// or a statement dump cannot leak one. Revocation is part of the WHERE clause
// rather than a field the caller is trusted to check: a revoked device must be
// unable to scan by construction, not by every call site remembering to look.
func (p *Postgres) AuthenticateScannerDevice(ctx context.Context, token string) (ScannerDevice, error) {
	if strings.TrimSpace(token) == "" {
		return ScannerDevice{}, ErrScannerDeviceUnknown
	}
	// A pre-decision step (TKT-557). The step completes after the complete row
	// read and BEFORE the no-row result becomes the unknown-device refusal, so
	// D39 expiry wins over a late revoked or unknown read.
	step := startBoundedStep(ctx, preDecisionStepBound, p.scheduler())
	defer step.release()
	var device ScannerDevice
	err := p.db.QueryRowContext(step.ctx,
		`SELECT id, organizer_id, label, created_at
		   FROM scanner_devices
		  WHERE token_hash = $1 AND revoked_at IS NULL`,
		hashScannerToken(token)).Scan(&device.ID, &device.OrganizerID, &device.Label, &device.CreatedAt)
	if avail := step.finish().unavailable(SiteDeviceLookup, err); avail != nil {
		return ScannerDevice{}, avail
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ScannerDevice{}, ErrScannerDeviceUnknown
	}
	if err != nil {
		return ScannerDevice{}, fmt.Errorf("authenticate scanner device: %w", err)
	}
	return device, nil
}

// TouchScannerDevice records that a device was seen. Advisory: it is a separate
// statement outside the scan's transaction and its failure is not a scan's
// failure — a turnstile must not refuse a valid ticket because a bookkeeping
// UPDATE lost a race (ADR-025 §D6's posture: the door opens).
//
// One autocommit statement under the shared step bound (TKT-557, D27 and D35).
// The subquery locks the row for this statement, or finds no row when another
// transaction holds it: a held row lock skips the update instead of waiting,
// and last_seen_at stays stale. A table lock, a pool wait, a reset ping, a
// dial or network I/O can still wait, and the PROVISIONAL touchStepBound ends
// that wait (TKT-560 qualifies it). Expiry during I/O can discard the
// connection. The result is discarded: Touch never changes authentication.
func (p *Postgres) TouchScannerDevice(ctx context.Context, id uuid.UUID) {
	step := startBoundedStep(ctx, touchStepBound, p.scheduler())
	defer step.release()
	_, _ = p.db.ExecContext(step.ctx, touchScannerDeviceSQL, id)
	_ = step.finish()
}

// touchScannerDeviceSQL is the advisory Touch. FOR NO KEY UPDATE, not FOR
// UPDATE: Touch changes last_seen_at, not the row key (D35).
const touchScannerDeviceSQL = `UPDATE scanner_devices SET last_seen_at = now()
	 WHERE id = (SELECT id FROM scanner_devices WHERE id = $1 FOR NO KEY UPDATE SKIP LOCKED)`

// RevokeScannerDevice retires a device. Idempotent: revoking an already-revoked
// device is a success, because the operator's intent ("this phone must not
// scan") is already true and answering an error would send them looking for a
// problem that does not exist.
func (p *Postgres) RevokeScannerDevice(ctx context.Context, id uuid.UUID) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE scanner_devices SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("revoke scanner device: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrScannerDeviceUnknown
	}
	return nil
}

// ListScannerDevices is the operator's view of one organizer's gates, revoked
// rows included: "which device did we revoke and when" is the question asked
// after a phone goes missing.
func (p *Postgres) ListScannerDevices(ctx context.Context, organizer uuid.UUID) ([]ScannerDevice, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, organizer_id, label, created_at, revoked_at, last_seen_at
		   FROM scanner_devices WHERE organizer_id = $1 ORDER BY created_at DESC`, organizer)
	if err != nil {
		return nil, fmt.Errorf("list scanner devices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ScannerDevice
	for rows.Next() {
		var d ScannerDevice
		if err := rows.Scan(&d.ID, &d.OrganizerID, &d.Label, &d.CreatedAt, &d.RevokedAt, &d.LastSeenAt); err != nil {
			return nil, fmt.Errorf("scan scanner device: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate scanner devices: %w", err)
	}
	return out, nil
}
