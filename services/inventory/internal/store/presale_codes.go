package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// pool row lock.
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
// LOCK ORDER IS FIXED: pool first, then code, never the reverse. Nothing else in
// inventory locks a presale_codes row, so this is the only order that exists and
// two claim transactions can never hold one another's next lock. The cross-pool
// cleanup (reclaimExpiredPresaleUsage) takes pool and claim locks and NO code lock,
// and it runs only after the refused admission rolled back, so it adds no order.
//
// Returns ErrPresaleCodeInvalid for every refusal EXCEPT exhaustion, which returns
// the unexported presaleCapExceeded (TKT-543, D5). Exhaustion is the one cause an
// expired, unswept claim can produce, so it is the one the callers try to reclaim
// for. They convert it to ErrPresaleCodeInvalid before returning, so the caller
// still sees one refusal for all five causes.
func redeemPresaleCode(ctx context.Context, tx *sql.Tx, org uuid.UUID, channel, code string, qty int32) error {
	if code == "" {
		// A gated allocation with no code presented. Same refusal as a wrong one:
		// telling the caller "you forgot the code" versus "that code is wrong" is
		// already half an oracle.
		return ErrPresaleCodeInvalid
	}

	// Lock the code row and read its terms in one statement. An unknown code has
	// no row to lock, which is correct: it is refused before any counting, and
	// refusing needs no serialization.
	//
	// The channel is part of the primary key, so a code issued on another channel
	// simply does not match here — the wrong-channel case needs no separate
	// branch and cannot accidentally report a different refusal.
	var maxRedemptions sql.NullInt32
	var windowOpen bool
	err := tx.QueryRowContext(ctx,
		`SELECT max_redemptions, (`+codeWindowOpen+`) FROM presale_codes
		 WHERE organizer_id=$1 AND channel_code=$2 AND code=$3 FOR UPDATE`,
		org, channel, code).Scan(&maxRedemptions, &windowOpen)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrPresaleCodeInvalid
	case err != nil:
		return err
	}
	if !windowOpen {
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
		return presaleCapExceeded{redeemed: redeemed, max: int64(maxRedemptions.Int32)}
	}
	return nil
}

// presaleCapExceeded is the exhaustion refusal of redeemPresaleCode, with the usage
// the admission counted under the code lock and the code's cap (TKT-543, D5, D7).
//
// It never leaves the store. CreateHold and PlaceGroupReservation consume it, may
// reclaim and retry, and return ErrPresaleCodeInvalid for a final refusal: the
// public refusal stays uniform across the five causes (ADR-064 §4).
//
// It deliberately does NOT wrap ErrPresaleCodeInvalid. A wrapper that forgot to
// convert it would then leak nothing visible, and the missing conversion would stay
// hidden until someone changed the message. Unwrapped, an escape is an unmapped
// error, which the API answers 500: TestPresaleRefusalsStayUniformAfterCrossPoolCleanup
// goes red on either path, and TestEveryInvalidPresaleCodeRefusesIdentically on the
// hold path.
type presaleCapExceeded struct {
	redeemed int64
	max      int64
}

func (e presaleCapExceeded) Error() string {
	return fmt.Sprintf("presale code usage %d plus this request exceeds its cap %d", e.redeemed, e.max)
}

func asPresaleCapExceeded(err error) (presaleCapExceeded, bool) {
	var e presaleCapExceeded
	return e, errors.As(err, &e)
}

// expiredPresalePoolsSQL is the cleanup's DISCOVERY (TKT-543, D2/D3): the pools
// other than the admission's that hold an expired, still-held claim citing exactly
// this (organizer, channel, code), with the quantity on each that a terminal write
// would stop counting (D7).
//
// The scope of the cleanup is decided HERE. sweepExpired expires every due claim of
// a pool it is given, whatever that claim cites, so a pool this query selects wrongly
// gets terminal writes nobody asked for. The cleanup's organizer-scoped lock is the
// only other line of defence, and it covers the organizer alone. Each predicate is
// therefore load-bearing, and TestRedemptionCountIsIndexBacked has a row that only
// it excludes (all but `expires_at IS NOT NULL`, which keeps sweepExpired's own
// spelling: a NULL expiry already fails `<=`).
//
// The admission's own pool is excluded because the admission already swept it
// inside its own transaction before it counted: its expired claims were not part
// of the usage that refused it.
//
// now() belongs to this statement alone: discovery runs outside any transaction,
// and sweepExpired rechecks status and expiry under the pool lock anyway. Backed by
// claims_presale_usage (organizer_id, channel_code, presale_code, status,
// expires_at), the same index as codeRedeemedQuantity (ADR-019).
const expiredPresalePoolsSQL = `SELECT pool_id, sum(quantity)::bigint FROM claims
	WHERE organizer_id=$1 AND channel_code=$2 AND presale_code=$3
	  AND status='held' AND expires_at IS NOT NULL AND expires_at <= now()
	  AND pool_id <> $4
	GROUP BY pool_id ORDER BY pool_id`

// presaleCleanupPoolLock locks ONE foreign pool for the cleanup (D9).
//
// Scoped to the organizer because sweepExpired filters on pool_id only: without the
// scope, a discovery mistake would sweep another organizer's pool, which this request
// has no business writing. With it, such a pool finds no row and is skipped.
//
// The marker exists for the tests. Transition locks a pool with the same statement
// minus the marker and the organizer, and a handshake must tell the cleanup's wait
// from the finalize's (plan amendment A1).
const presaleCleanupPoolLock = `SELECT 1 /* presale-cleanup pool lock */ FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE`

// presaleCleanupLockTimeout bounds how long ONE cleanup transaction waits for a
// foreign pool (D8).
//
// The cleanup WAITS rather than skipping a locked pool: the pools that matter are
// held by a finalize or a draw-down, which never sweep, and SKIP LOCKED would give
// up on exactly those. The bound stops a refused request from queuing behind a hot
// pool for a whole on-sale. When it fires, the cleanup stops and the admission
// returns the uniform refusal: a cleanup failure costs liveness, never correctness.
const presaleCleanupLockTimeout = 2 * time.Second

// reclaimExpiredPresaleUsage is the cross-pool cleanup (TKT-543, D2, D3, D7, D8, D9).
// It reports whether the caller should run its admission once more.
//
// WHY IT EXISTS. codeRedeemedQuantity counts an expired held claim until a terminal
// write commits (presaleUsageClaims). Terminal writes are pool-local and lazy (ADR-010:
// no sweeper is required), so an expired hold on a quiet pool would lock its code out
// of every other pool indefinitely. This is the "next mutation" that reclaims it.
//
// WHEN IT RUNS. Only after an admission was refused by the code count AND rolled back,
// so it starts holding no lock at all. When it waits for a foreign pool, its caller
// holds neither its own pool nor the code, so a mirror admission that holds that
// foreign pool and waits for the code is not waiting for anything of the caller's
// (TestPresaleMirrorAdmissionsReleaseLocksBeforeCleanup). The refusal comes after the
// window and seller checks, so a caller who may not sell never reaches it.
//
// WHAT IT COSTS. One discovery query, then one transaction per discovered pool, each
// committed before the next starts, in pool-id order. It returns false, and the caller
// refuses at once, when nothing it could reclaim would cover the shortfall (D7): if a
// code is genuinely exhausted, an on-sale of refused requests must not take the hot
// pool's lock twice each.
//
// ERRORS ARE NOT RETURNED (D8). A returned error would be a 500 that only a valid,
// exhausted code can produce, which is the oracle ADR-064 §4 forbids. Any error ends the
// cleanup and the caller refuses uniformly. The terminal writes already committed are
// valid on their own. The cost is that a persistent cleanup failure is silent.
func (p *Postgres) reclaimExpiredPresaleUsage(ctx context.Context, org, admissionPool uuid.UUID, channel, code string, qty int32, refused presaleCapExceeded) bool {
	pools, reclaimable, err := p.expiredPresalePools(ctx, org, channel, code, admissionPool)
	if err != nil || len(pools) == 0 {
		return false
	}
	if refused.redeemed-reclaimable+int64(qty) > refused.max {
		return false
	}
	for _, pool := range pools {
		if err = p.cleanupExpiredPresalePool(ctx, org, pool); err != nil {
			return false
		}
	}
	return true
}

// expiredPresalePools reads discovery completely and closes it before any cleanup
// transaction opens.
func (p *Postgres) expiredPresalePools(ctx context.Context, org uuid.UUID, channel, code string, exclude uuid.UUID) ([]uuid.UUID, int64, error) {
	rows, err := p.db.QueryContext(ctx, expiredPresalePoolsSQL, org, channel, code, exclude)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var pools []uuid.UUID
	var total int64
	for rows.Next() {
		var pool uuid.UUID
		var qty int64
		if err = rows.Scan(&pool, &qty); err != nil {
			return nil, 0, err
		}
		pools = append(pools, pool)
		total += qty
	}
	return pools, total, rows.Err()
}

// cleanupExpiredPresalePool writes the terminal state of every due claim in ONE foreign
// pool, in its own transaction, under that pool's lock only (D9).
//
// sweepExpired is reused unchanged: it rechecks status and expiry under the lock, and
// keeps history, seat release and capacity-cut reconciliation exactly as every other
// sweep does. A claim that a queued finalize or draw-down settled first is no longer
// due, so the cleanup sees the committed state and leaves it.
//
// NO offering guard. An archived or closed pool refuses new holds, so no hold sweeps it:
// an archived pool never again (archival is terminal, and AdjustCapacity refuses it too),
// a closed one only after it reopens or staff adjust its capacity. Guarding here would
// pin the code that long. classifySeatClaimsInPool is the precedent for a sweep without
// the guard.
func (p *Postgres) cleanupExpiredPresalePool(ctx context.Context, org, pool uuid.UUID) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	timeout := presaleCleanupLockTimeout
	if p.cleanupLockTimeout > 0 {
		timeout = p.cleanupLockTimeout
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, timeout.Milliseconds())); err != nil {
		return err
	}
	var locked int
	err = tx.QueryRowContext(ctx, presaleCleanupPoolLock, pool, org).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = sweepExpired(ctx, tx, pool); err != nil {
		return err
	}
	return p.commitAvailability(tx, pool)
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
