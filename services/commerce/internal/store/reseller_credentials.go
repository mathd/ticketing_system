package store

// Partner (reseller) API credentials — the third machine identity class
// (TKT-240 / ADR-056). See migration 0020 for why the credential is per-reseller,
// stored as a SHA-256 hash rather than bcrypt, and what adversary that does and
// does not address.
//
// This file owns credential storage, authentication and revocation ONLY. What a
// resolved credential is then ALLOWED to do is the API layer's question — but the
// answer must be built from the ResellerCredential this package returns, never
// from the request. ADR-053 is the worked example of the other arrangement.

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

// ErrResellerCredentialUnknown is the ONE refusal for every failing credential
// lookup: an unknown token, a revoked credential, a malformed value, an empty
// header. One error rather than four because the difference is information for an
// operator reading a log, not for whoever presented the token — and a partner
// integration that can distinguish "revoked" from "never existed" can enumerate.
var ErrResellerCredentialUnknown = errors.New("reseller credential is not recognised")

// ResellerCredential is one partner's API credential and, crucially, its SCOPE.
// It never carries the token: the plaintext exists exactly once, in
// EnrolResellerCredential's return value, and is not recoverable afterwards.
type ResellerCredential struct {
	ID          uuid.UUID
	ResellerID  uuid.UUID
	OrganizerID uuid.UUID
	ChannelCode string
	Label       string
	CreatedAt   time.Time
	RevokedAt   *time.Time
}

// resellerTokenBytes is the entropy in a partner token. 256 bits, matching
// scanner enrolment and password-reset tokens. The whole argument for a fast hash
// (migration 0020) rests on this value being generated rather than chosen.
const resellerTokenBytes = 32

// hashResellerToken is the stored form. Trimmed first, because the token travels
// through an HTTP header and through whatever the partner pasted it into, and both
// are places a stray space survives — an untrimmed hash would refuse a correct
// token in a way indistinguishable from a revoked credential.
func hashResellerToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// EnrolResellerCredential issues one credential and returns it with its plaintext
// token, which is the only time that value exists outside the caller's hands.
//
// The channel is stored exactly as given (ADR-024): no trimming beyond the
// surrounding whitespace a shell adds, no case folding. A channel that differs by
// case is a different channel everywhere else in the platform and must be here too.
func EnrolResellerCredential(ctx context.Context, db *sql.DB, organizer, reseller uuid.UUID, channel, label string) (ResellerCredential, string, error) {
	label = strings.TrimSpace(label)
	if organizer == uuid.Nil || reseller == uuid.Nil || channel == "" || label == "" {
		return ResellerCredential{}, "", errors.New("organizer id, reseller id, channel code and a non-empty label are required")
	}
	if len(channel) > 100 {
		return ResellerCredential{}, "", errors.New("channel code is at most 100 characters")
	}
	raw := make([]byte, resellerTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return ResellerCredential{}, "", fmt.Errorf("generate reseller token: %w", err)
	}
	token := hex.EncodeToString(raw)

	cred := ResellerCredential{ID: uuid.New(), ResellerID: reseller, OrganizerID: organizer, ChannelCode: channel, Label: label}
	err := db.QueryRowContext(ctx,
		`INSERT INTO reseller_credentials(id, reseller_id, organizer_id, channel_code, token_hash, label)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`,
		cred.ID, reseller, organizer, channel, hashResellerToken(token), label).Scan(&cred.CreatedAt)
	if err != nil {
		return ResellerCredential{}, "", fmt.Errorf("enrol reseller credential: %w", err)
	}
	return cred, token, nil
}

// AuthenticateResellerCredential resolves a presented token to a live credential
// AND THE SCOPE IT WAS ISSUED FOR.
//
// The lookup is by HASH, so the query carries no credential and a slow-query log
// or a statement dump cannot leak one. Revocation is part of the WHERE clause
// rather than a field the caller is trusted to check: a revoked credential must be
// unable to sell by construction, not by every call site remembering to look.
//
// Returning the scope is not a convenience. Resolving a credential and then
// discarding what it was issued FOR is exactly how access's scanner enrolment was
// platform-wide while looking per-organizer, and how ADR-053's staff credential
// reaches across tenants. The caller gets the organizer and channel from here or
// it does not get them at all.
func AuthenticateResellerCredential(ctx context.Context, db *sql.DB, token string) (ResellerCredential, error) {
	if strings.TrimSpace(token) == "" {
		return ResellerCredential{}, ErrResellerCredentialUnknown
	}
	var cred ResellerCredential
	err := db.QueryRowContext(ctx,
		`SELECT id, reseller_id, organizer_id, channel_code, label, created_at
		   FROM reseller_credentials
		  WHERE token_hash = $1 AND revoked_at IS NULL`,
		hashResellerToken(token)).Scan(&cred.ID, &cred.ResellerID, &cred.OrganizerID, &cred.ChannelCode, &cred.Label, &cred.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ResellerCredential{}, ErrResellerCredentialUnknown
	}
	if err != nil {
		return ResellerCredential{}, fmt.Errorf("authenticate reseller credential: %w", err)
	}
	return cred, nil
}

// RevokeResellerCredential retires a credential. Idempotent: revoking an
// already-revoked credential is a success, because the operator's intent ("this
// partner must not sell") is already true and an error would send them looking for
// a problem that does not exist.
func RevokeResellerCredential(ctx context.Context, db *sql.DB, id uuid.UUID) error {
	res, err := db.ExecContext(ctx,
		`UPDATE reseller_credentials SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("revoke reseller credential: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrResellerCredentialUnknown
	}
	return nil
}

// ListResellerCredentials is the operator's view of one organizer's partners,
// revoked rows included: "which credential did we revoke and when" is the question
// asked after a partner reports a leak.
func ListResellerCredentials(ctx context.Context, db *sql.DB, organizer uuid.UUID) ([]ResellerCredential, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, reseller_id, organizer_id, channel_code, label, created_at, revoked_at
		   FROM reseller_credentials WHERE organizer_id = $1 ORDER BY created_at DESC`, organizer)
	if err != nil {
		return nil, fmt.Errorf("list reseller credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ResellerCredential
	for rows.Next() {
		var c ResellerCredential
		if err := rows.Scan(&c.ID, &c.ResellerID, &c.OrganizerID, &c.ChannelCode, &c.Label, &c.CreatedAt, &c.RevokedAt); err != nil {
			return nil, fmt.Errorf("scan reseller credential: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reseller credentials: %w", err)
	}
	return out, nil
}

// ReservationBelongsToReseller answers whether a reservation is THIS partner's.
//
// The predicate is the point, and it is deliberately a SQL predicate rather than
// a load-then-compare in Go. All four terms are bound together in the WHERE
// clause: a caller that knows another partner's reservation id cannot confirm it
// by naming its own organizer, because the row must match the organizer, the
// channel AND the reseller at once.
//
// ADR-053 is why this is written this way. There, both the list's organizer and
// the update's are caller-supplied, so a stolen credential enumerates a victim's
// channels and then mutates the ids it just learned — every individual statement
// correct, the composition wrong. Here the scope terms come from the credential
// row (see AuthenticateResellerCredential) and only the reservation id comes from
// the caller.
//
// A missing row and a row belonging to someone else are the SAME answer: false.
// Distinguishing them would let a partner probe for the existence of other
// partners' reservations.
func ReservationBelongsToReseller(ctx context.Context, db *sql.DB, reservation, organizer uuid.UUID, channel string, reseller uuid.UUID) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM reservations
		 WHERE id = $1 AND organizer_id = $2 AND channel_code = $3 AND reseller_id = $4)`,
		reservation, organizer, channel, reseller).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("resolve partner reservation: %w", err)
	}
	return exists, nil
}
