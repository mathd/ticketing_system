package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Presale unlock codes (TKT-239 / ADR-064): WHO may sell on a gated channel.
//
// A code grants ACCESS ONLY. It is not a discount and changes no price — that is
// TKT-7's, and a code on the money path would be a different ticket with
// different rules.

// PresaleCode is an issued unlock code. MaxRedemptions nil means unlimited;
// OpensAt/ClosesAt nil mean unbounded on that side.
type PresaleCode struct {
	OrganizerID    uuid.UUID  `json:"organizer_id"`
	Channel        string     `json:"channel"`
	Code           string     `json:"code"`
	MaxRedemptions *int32     `json:"max_redemptions,omitempty"`
	OpensAt        *time.Time `json:"opens_at,omitempty"`
	ClosesAt       *time.Time `json:"closes_at,omitempty"`
}

// PresaleCodeStatus is the OPERATOR view of a code: the one place the five
// refusal causes are distinguishable. The public path returns
// ErrPresaleCodeInvalid for all of them (see its comment) — this type exists so
// an operator debugging "my code doesn't work" gets an answer without that
// answer being reachable from the storefront.
type PresaleCodeStatus struct {
	PresaleCode
	Redeemed   int64  `json:"redeemed"`
	Remaining  *int64 `json:"remaining,omitempty"`
	WindowOpen bool   `json:"window_open"`
	Exhausted  bool   `json:"exhausted"`
}

// redeemPresaleCode decides whether `code` may be redeemed for `qty` more units
// on (org, channel), and MUST be called inside the claim transaction, after the
// row lock on pool `slot` and after the caller has judged the channel's window on
// that pool's allocation.
//
// THE ROW LOCK IS THE POINT OF THIS FUNCTION.
//
// Every other derived count in this package is scoped by pool_id, so the pool
// row FOR UPDATE serializes it: two holds on the same slot queue behind each
// other and each sees the other's committed rows. A presale code is NOT
// pool-scoped — it is (organizer_id, channel_code, code) and spans every slot in
// the presale, which is the entire point of a code. So two holds on DIFFERENT
// slots take DIFFERENT pool locks, never block each other, and both read the
// same pre-existing usage. Without a second lock a code capped at N is redeemed
// N+1 times, and the single-slot contention test that mirrors
// TestChannelAllocationContention passes while it happens, because a one-pool
// fixture cannot construct the race.
//
// So: SELECT ... FOR UPDATE on the presale_codes row, which serializes ALL
// redemptions of one code across every slot. Precedent is in this service —
// refund_returns.go locks the pool row and then a claims row in the same
// transaction. ADR-029's advisory lock is catalog's answer to a case where no
// row exists to lock; here the row exists, so the ordinary mechanism applies.
//
// LOCK ORDER IS FIXED: pool first, then code, never the reverse. Two paths lock a
// presale_codes row: this one, and IssuePresaleCode's upsert. Issuance takes no
// pool lock, so it holds the code lock alone and adds no second order. Every
// claim transaction takes its pool lock before the code lock, so two transactions
// can never each hold the lock the other needs next.
//
// DECIDE AFTER THE WAIT (TKT-544). The code lock can WAIT. Another redemption of
// the same code on a different pool holds it without updating the row, and an
// issuance holds it too. The pool lock this transaction already holds does not
// exclude either of them. So every time-dependent decision is taken in a statement
// that starts AFTER the lock statement returns:
//
//  1. the lock statement computes nothing that depends on time;
//  2. the CHANNEL's window is judged again: the caller judged it before this wait;
//  3. the code's cap and window are read;
//  4. usage is counted for a capped code.
//
// A value in the lock statement's own target list is not enough. PostgreSQL
// computed the old combined statement's window before the wait, and did not
// compute it again when the holder ended without an update: TKT-380 probe 1
// schedule W admitted 6 of 6 times after closes_at. That explanation is an
// inference that matches the observation (ADR-084 §4). Moving the predicate into
// WHERE would not help either: PostgreSQL's Read Committed documentation says the
// WHERE clause is checked again only for a row that the holder updated (documented
// behaviour, not executed for this ticket).
//
// A boundary that passes AFTER these reads, during the counting or the insert, is
// outside the guarantee: the decision is judged once the last wait before it ends,
// not at commit.
//
// Returns ErrChannelWindowClosed when the channel's window closed during the wait,
// and ErrPresaleCodeInvalid, uniformly, for every refusal of the code.
func redeemPresaleCode(ctx context.Context, tx *sql.Tx, org, slot uuid.UUID, channel, code string, qty int32) error {
	// 1. Take the code row lock. The statement reads no term of the code: see
	// above for why a term read here would be stale.
	//
	// An empty or unknown code has no row to lock, which is correct: it is
	// refused below, and a refusal needs no serialization. It still goes through
	// step 2 first, so the channel's answer cannot depend on whether the code
	// exists.
	//
	// The channel is part of the primary key, so a code issued on another channel
	// simply does not match here — the wrong-channel case needs no separate
	// branch and cannot accidentally report a different refusal.
	codeRowLocked := false
	if code != "" {
		var locked int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM presale_codes WHERE organizer_id=$1 AND channel_code=$2 AND code=$3 FOR UPDATE`,
			org, channel, code).Scan(&locked)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			codeRowLocked = true
		}
	}

	// 2. Judge the channel's window again, with the same predicate, on the same
	// allocation row the caller read. The row itself cannot have changed: the
	// only writer of channel_allocations (ReplaceChannelAllocations) takes the
	// pool lock this transaction holds. Only the clock moved. So a missing row is
	// a fault, not a refusal, and it returns as an error.
	//
	// Precedence stays window -> code (ADR-064 §5): a channel that closed during
	// the wait refuses as closed, whatever the code is.
	var channelOpen bool
	if err := tx.QueryRowContext(ctx,
		`SELECT (`+windowOpen+`) FROM channel_allocations WHERE pool_id=$1 AND channel_code=$2`,
		slot, channel).Scan(&channelOpen); err != nil {
		return err
	}
	if !channelOpen {
		return ErrChannelWindowClosed
	}
	if !codeRowLocked {
		// No code presented, or no such code on this channel. The same refusal for
		// both: telling the caller "you forgot the code" versus "that code is
		// wrong" is already half an oracle.
		return ErrPresaleCodeInvalid
	}

	// 3. The terms, read after the lock. The row cannot disappear while this
	// transaction holds its lock; a missing row still gets the uniform refusal.
	var maxRedemptions sql.NullInt32
	var codeOpen bool
	err := tx.QueryRowContext(ctx,
		`SELECT max_redemptions, (`+codeWindowOpen+`) FROM presale_codes
		 WHERE organizer_id=$1 AND channel_code=$2 AND code=$3`,
		org, channel, code).Scan(&maxRedemptions, &codeOpen)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrPresaleCodeInvalid
	case err != nil:
		return err
	}
	if !codeOpen {
		return ErrPresaleCodeInvalid
	}
	if !maxRedemptions.Valid {
		// Unlimited: nothing to count, so nothing to serialize beyond the row lock
		// already held.
		return nil
	}

	// Derived usage, never a counter (ADR-010, ADR-024). Read under the row lock
	// just taken, so a concurrent redemption on any other slot has either already
	// committed and is visible, or is still queued behind us.
	var redeemed int64
	if err := tx.QueryRowContext(ctx, codeRedeemedQuantity, org, channel, code).Scan(&redeemed); err != nil {
		return err
	}
	// int64 throughout: two valid int32 counters can wrap a 32-bit sum, the same
	// trap CreateHold's capacity arithmetic already documents.
	if redeemed+int64(qty) > int64(maxRedemptions.Int32) {
		return ErrPresaleCodeInvalid
	}
	return nil
}

// IssuePresaleCode creates or replaces a code. Operator surface, internal only.
func (p *Postgres) IssuePresaleCode(ctx context.Context, in PresaleCode) (PresaleCode, error) {
	if err := validatePresaleCode(in); err != nil {
		return PresaleCode{}, err
	}
	var max sql.NullInt32
	if in.MaxRedemptions != nil {
		max = sql.NullInt32{Int32: *in.MaxRedemptions, Valid: true}
	}
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO presale_codes(organizer_id, channel_code, code, max_redemptions, opens_at, closes_at)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (organizer_id, channel_code, code) DO UPDATE
		 SET max_redemptions=EXCLUDED.max_redemptions, opens_at=EXCLUDED.opens_at,
		     closes_at=EXCLUDED.closes_at, updated_at=now()`,
		in.OrganizerID, in.Channel, in.Code, max, in.OpensAt, in.ClosesAt)
	if err != nil {
		return PresaleCode{}, err
	}
	return in, nil
}

// validatePresaleCode rejects what the schema would reject, in Go, so the API
// answers 400 rather than surfacing a constraint violation as a 500.
//
// It does NOT normalize. Codes are exact opaque strings like channel codes
// (ADR-024): trimming or case-folding here would disagree with the exact-match
// lookup in redeemPresaleCode, and a code that can be issued but never redeemed
// is worse than one that is rejected outright.
//
// No HTTP route calls issuance today. The 400 that problem() maps for
// ErrPresaleCodeInvalidInput is not reachable over HTTP until one does. TKT-309
// owns that decision.
func validatePresaleCode(in PresaleCode) error {
	if in.OrganizerID == uuid.Nil {
		return ErrPresaleCodeInvalidInput
	}
	if l := len(in.Channel); l < 1 || l > 100 {
		return ErrPresaleCodeInvalidInput
	}
	if l := len(in.Code); l < 1 || l > 100 {
		return ErrPresaleCodeInvalidInput
	}
	if in.MaxRedemptions != nil && *in.MaxRedemptions <= 0 {
		return ErrPresaleCodeInvalidInput
	}
	if in.OpensAt != nil && in.ClosesAt != nil && !in.OpensAt.Before(*in.ClosesAt) {
		return ErrPresaleCodeInvalidInput
	}
	return nil
}

// ErrPresaleCodeInvalidInput is an operator-facing 400: the code as SUBMITTED is
// malformed. Distinct from ErrPresaleCodeInvalid, which is the buyer-facing
// uniform refusal — conflating them would leak the operator's diagnostics onto
// the public path.
var ErrPresaleCodeInvalidInput = errors.New("invalid presale code definition")

// PresaleCodeStatuses is the operator read: every code on a channel with its
// derived redemption count. Internal only — see PresaleCodeStatus.
func (p *Postgres) PresaleCodeStatuses(ctx context.Context, org uuid.UUID, channel string) ([]PresaleCodeStatus, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT code, max_redemptions, opens_at, closes_at, (`+codeWindowOpen+`),
		        (SELECT COALESCE(sum(`+consumedQuantity+`),0)::bigint FROM claims c
		         WHERE c.organizer_id=p.organizer_id AND c.channel_code=p.channel_code
		           AND c.presale_code=p.code AND `+consumingClaims+`)
		 FROM presale_codes p
		 WHERE organizer_id=$1 AND channel_code=$2
		 ORDER BY code`, org, channel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []PresaleCodeStatus{}
	for rows.Next() {
		s := PresaleCodeStatus{PresaleCode: PresaleCode{OrganizerID: org, Channel: channel}}
		var max sql.NullInt32
		if err := rows.Scan(&s.Code, &max, &s.OpensAt, &s.ClosesAt, &s.WindowOpen, &s.Redeemed); err != nil {
			return nil, err
		}
		if max.Valid {
			m := max.Int32
			s.MaxRedemptions = &m
			remaining := int64(m) - s.Redeemed
			if remaining < 0 {
				remaining = 0
			}
			s.Remaining = &remaining
			s.Exhausted = remaining == 0
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
