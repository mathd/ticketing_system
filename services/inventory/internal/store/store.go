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
	// ErrChannelWindowClosed: the channel has an allocation with headroom and the
	// pool has capacity, but the channel's sales window is not open right now
	// (TKT-238 / ADR-054).
	//
	// A SEPARATE sentinel from ErrUnavailable, and that is the point rather than
	// tidiness: "this channel is not selling yet" and "this channel is sold out"
	// lead a caller to opposite actions — wait for the window, versus join a
	// waitlist or stop offering. Collapsing them into the code-less sellout shape
	// is exactly what COS-3 forbids, and it is what would happen by default,
	// because ErrUnavailable is the natural thing to return.
	ErrChannelWindowClosed = errors.New("channel sales window closed")
	// ErrPresaleCodeInvalid: the allocation requires an unlock code and the one
	// presented will not do (TKT-239 / ADR-064).
	//
	// DELIBERATELY UNIFORM across five distinct causes — no code, unknown code, a
	// code issued on another channel, an exhausted code, and a code outside its
	// validity window. A refusal that distinguished them would be an ENUMERATION
	// ORACLE: an attacker submitting candidates learns "that code exists but is
	// spent" versus "no such code", which is how presale codes get scraped. The
	// message and the wire body are identical for all five; only the internal
	// operator read tells them apart.
	//
	// Name the adversary (ADR-021): this defeats a scraper reading response
	// bodies. It does NOT defeat one measuring latency — an exhausted-code
	// refusal costs a usage aggregation an unknown-code refusal does not — nor
	// one who simply observes that a channel is gated. Rate limiting is ADR-051's
	// and is not claimed here.
	ErrPresaleCodeInvalid = errors.New("invalid presale code")
	ErrIdempotency        = errors.New("idempotency key reused with different request")
	// ErrAllocationCapsExceedCapacity: the submitted allocation set sums above the
	// pool's capacity (TKT-244). WRAPS ErrUnavailable rather than replacing it, so
	// every existing caller matching the sentinel behaves exactly as before — the
	// added code is additive information, not a re-classification.
	//
	// It names no channel on purpose: the sum is a property of the whole set, so
	// every row shares the blame equally and attributing it to one would point the
	// operator at an arbitrary field.
	ErrAllocationCapsExceedCapacity = fmt.Errorf("%w: channel allocations exceed pool capacity", ErrUnavailable)
	// ErrAllocationRevisionMismatch: the caller presented an allocation-set revision
	// that is not the one the pool currently holds (TKT-250, amending ADR-024).
	//
	// The form was populated before another writer committed. The pool row lock
	// serializes the two transactions and cannot help: it orders the writes, and the
	// staleness is in the READ that filled the form. Without this the second save
	// silently overwrites the first operator's caps and deletes any row created since.
	//
	// WRAPS ErrConflict for the same additive reason as the two refusals above: every
	// existing caller matching the sentinel keeps behaving exactly as it did, and the
	// code is extra information rather than a re-classification.
	//
	// It names no channel, because staleness is a property of the whole SET rather than
	// of any one row — the same reasoning that keeps ErrAllocationCapsExceedCapacity
	// channel-less. The only useful remedy is to reload and re-apply.
	//
	// Name the adversary (ADR-021): this is honest-writer lost-update protection. It
	// does not authenticate anyone and is not tamper-evidence — a caller who can write
	// inventory's database, or who holds the shared internal token, can present or set
	// any revision at all.
	ErrAllocationRevisionMismatch = fmt.Errorf("%w: allocation set revision mismatch", ErrConflict)
	// ErrAllocationWindowReversed: a submitted allocation's sales window closes at or
	// before it opens (TKT-307). Migration 0013's channel_allocations_window_order CHECK
	// makes that unrepresentable; this is the Go guard in front of it, so the API answers
	// 400 with a message the operator can act on rather than surfacing the constraint
	// violation as a 500.
	//
	// It does NOT wrap ErrUnavailable or ErrConflict the way the two refusals above do.
	// Those are 409s — the submitted set is well-formed and the pool cannot accept it. A
	// reversed window is malformed input, which is a different answer (400) and a
	// different remedy: fix the field, not the number.
	ErrAllocationWindowReversed = errors.New("allocation sales window closes at or before it opens")
)

// AllocationCapBelowConsumption is the refusal for a cap set below what that channel has
// already sold or is holding (TKT-244). It CARRIES THE CHANNEL, which is the whole point:
// the editor must put the message beside the offending row's cap input, and only the
// server knows which row failed.
//
// A client cannot re-derive this the way TKT-236's channel form re-derives its bounds.
// Those bounds are static, so a local re-check reaches the same answer; consumption is
// live and moves between the GET that filled the form and the PUT that submits it, so a
// local guess can name the wrong row with total confidence.
//
// Wraps ErrConflict for the same additive reason as above.
func AllocationCapBelowConsumption(channel string) error {
	return allocationCapBelowConsumption{channel: channel}
}

type allocationCapBelowConsumption struct{ channel string }

func (e allocationCapBelowConsumption) Error() string {
	// The channel is operator-supplied and opaque (ADR-024: no normalization, no case
	// folding), so it is quoted rather than interpolated bare.
	return fmt.Sprintf("%v: channel %q is allocated below its current consumption", ErrConflict, e.channel)
}

// Unwrap keeps errors.Is(err, ErrConflict) true for every pre-existing caller.
func (e allocationCapBelowConsumption) Unwrap() error { return ErrConflict }

// Channel is the offending allocation's raw code, echoed verbatim to the client.
func (e allocationCapBelowConsumption) Channel() string { return e.channel }

// AllocationWindowReversed is the refusal for a sales window whose close is not strictly
// after its open (TKT-307).
//
// It exists so the API answers 400 rather than surfacing migration 0013's
// `channel_allocations_window_order` CHECK as an unmapped pgx error, which problem()
// classifies 500. That is the rule `validatePresaleCode` already states for the same
// class of input ("so the API answers 400 rather than surfacing a constraint violation
// as a 500"); the allocation editor is a staff form and was not getting it.
//
// The CHECK STAYS. This validates in Go so the operator gets a message they can act on,
// not so the database stops being the arbiter — the store is not the table's only
// possible writer, and a Go guard alone would make a reversed window merely unlikely
// rather than unrepresentable.
//
// It gets its OWN sentinel and its own case in problem(), and the first attempt at this
// did neither — it wrapped ErrSeatSetInvalid to reach problem()'s existing 400 branch,
// which looked like the smaller change and was wrong (ai-review [high]). problem()
// matches `belowConsumption` on the STRUCTURAL interface{ Channel() string }, and it runs
// FIRST, so any refusal that names its channel is claimed by that branch before its own.
// The window refusal therefore answered 409 `allocation_cap_below_consumption` — telling
// the operator a cap was too low when the real problem was a backwards window, which is a
// worse failure than the 500 this ticket set out to fix. A store-tier test could not see
// it: the error unwrapped to ErrSeatSetInvalid exactly as asserted, and the misrouting
// happened one tier up.
//
// It CARRIES THE CHANNEL for the reason AllocationCapBelowConsumption does: the editor
// submits a whole set and must put the message beside the row the operator has to fix.
// Naming a channel is what puts it in belowConsumption's path, so the two facts are not
// independent — every future per-row refusal needs its own case placed BEFORE that
// branch, and TestAllocationRefusalsCarryAMachineReadableCodeAndTheOffendingChannel is
// where that gets caught.
func AllocationWindowReversed(channel string) error {
	return allocationWindowReversed{channel: channel}
}

type allocationWindowReversed struct{ channel string }

func (e allocationWindowReversed) Error() string {
	// Both field names, because the editor has two timestamp inputs and either could be
	// the one that is wrong. The channel is operator-supplied and opaque (ADR-024), so
	// it is quoted rather than interpolated bare.
	return fmt.Sprintf("%v: channel %q has closes_at at or before opens_at", ErrAllocationWindowReversed, e.channel)
}

// Unwrap keeps errors.Is(err, ErrAllocationWindowReversed) true, the way the two
// allocation refusals above unwrap to ErrUnavailable and ErrConflict.
func (e allocationWindowReversed) Unwrap() error { return ErrAllocationWindowReversed }

// Channel is the offending allocation's raw code, echoed verbatim to the client.
func (e allocationWindowReversed) Channel() string { return e.channel }

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
	ID           uuid.UUID  `json:"hold_id"`
	OrganizerID  uuid.UUID  `json:"organizer_id"`
	PoolID       uuid.UUID  `json:"slot_id"`
	Quantity     int32      `json:"quantity"`
	TicketTypeID uuid.UUID  `json:"ticket_type_id,omitempty"`
	UnitAmount   int64      `json:"unit_amount,omitempty"`
	Currency     string     `json:"currency,omitempty"`
	Status       string     `json:"status"`
	Channel      string     `json:"channel,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	ServerTime   time.Time  `json:"server_time"`
	// snapshotTime is TRANSACTION-START time (now()), and it is deliberately NOT
	// ServerTime. A replay reads a claim under the pool lock and must answer two
	// different questions with two different clocks (TKT-148):
	//
	//   - "is this claim still alive?" is a DECISION that writes -- expired() flips the
	//     row, appends history, releases seats and returns ErrConflict. It stays on
	//     transaction-start time so that a request queued on the pool lock judges
	//     liveness exactly as it did before this ticket. Moving it to advancing time
	//     would kill more in-flight holds under contention, which is a liveness
	//     semantics change on a money path and belongs to its own ticket.
	//   - "how long does the buyer have?" is a client-facing REFERENCE. That is
	//     ServerTime, and it must advance, or a replayed hold reports a countdown
	//     inflated by the lock wait while a fresh grant of the same hold does not.
	//
	// Zero on a fresh grant, where the two coincide by construction.
	snapshotTime time.Time
	Kind         string `json:"-"`
	Purpose      string `json:"-"`
	Label        string `json:"-"`
}

// liveClaims is the single predicate deciding which claims count against capacity:
// finalizing always, held while unexpired. Operational holds have expires_at NULL
// (enforced by claims_kind_shape) and therefore never expire. Every capacity read and
// every sweep must derive from this predicate or its complement — a hand-rolled
// `expires_at > now()` silently frees every operational hold.
const liveClaims = `((status='held' AND (expires_at IS NULL OR expires_at > now())) OR status='finalizing')`

func (c Claim) expired() bool {
	// See Claim.snapshotTime: liveness is decided on transaction-start time. A fresh
	// grant leaves snapshotTime zero and falls back to ServerTime, where the two are
	// the same instant anyway.
	at := c.snapshotTime
	if at.IsZero() {
		at = c.ServerTime
	}
	return c.Status == "held" && c.ExpiresAt != nil && !c.ExpiresAt.After(at)
}

// sweepExpired flips due buyer holds to expired and records the expiry in claim_history,
// inside the caller's pool-locked transaction (ADR-010: expiry precedes capacity accounting).
func sweepExpired(ctx context.Context, tx *sql.Tx, pool uuid.UUID) error {
	_, err := tx.ExecContext(ctx, `WITH swept AS (
			UPDATE claims SET status='expired', updated_at=now()
			WHERE pool_id=$1 AND status='held' AND expires_at IS NOT NULL AND expires_at<=now()
			RETURNING id, organizer_id, quantity)
		INSERT INTO claim_history(id, organizer_id, claim_id, action, actor, reason, quantity, quantity_after, status_after)
		SELECT gen_random_uuid(), organizer_id, id, 'expire', 'system', 'ttl_elapsed', quantity, 0, 'expired' FROM swept`, pool)
	if err != nil {
		return err
	}
	// A seated claim's seats are released in the SAME transaction as its expiry flip
	// (TKT-80/ADR-031): an already-expired claim is never revisited, so a decoupled
	// update would block the seat forever. No-op on GA pools (no claim_seats rows).
	if err = releaseSeatsForTerminal(ctx, tx, pool); err != nil {
		return err
	}
	// Expiry lowers demand: settle a draining capacity cut (TKT-76), same lock, no-op
	// without a pending target.
	return reconcileCapacity(ctx, tx, pool)
}

// releaseSeatsForTerminal flips released_at on any live seat row whose claim has reached
// a terminal state (expired/released) in this pool. Idempotent (guarded by released_at
// IS NULL) and pool-scoped; a no-op for GA pools. Runs inside the caller's pool-locked
// transaction so the seat release commits atomically with the claim's terminal flip.
func releaseSeatsForTerminal(ctx context.Context, tx *sql.Tx, pool uuid.UUID) error {
	_, err := tx.ExecContext(ctx, `UPDATE claim_seats SET released_at=now()
		WHERE pool_id=$1 AND released_at IS NULL
		  AND claim_id IN (SELECT id FROM claims WHERE pool_id=$1 AND status IN ('expired','released'))`, pool)
	return err
}

func appendHistory(ctx context.Context, tx *sql.Tx, org, claim uuid.UUID, related *uuid.UUID, action, actor, reason string, qty, qtyAfter int32, statusAfter string, key, fp *string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO claim_history(id, organizer_id, claim_id, related_claim_id, action, actor, reason, quantity, quantity_after, status_after, idempotency_key, request_fingerprint)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, uuid.New(), org, claim, related, action, actor, reason, qty, qtyAfter, statusAfter, key, fp)
	return err
}

type Availability struct {
	SlotID    uuid.UUID `json:"slot_id"`
	Capacity  int32     `json:"capacity"`
	Held      int32     `json:"held"`
	Confirmed int32     `json:"confirmed"`
	Available int32     `json:"available"`
	// OfferingStatus is open|closed|archived (archived wins). Counters stay factual
	// while not open; only claimable availability is zeroed (TKT-75 AC3).
	OfferingStatus string `json:"offering_status"`
}

type Postgres struct {
	db  *sql.DB
	ttl time.Duration
	// quarantineCap overrides MaxCatalogQuarantinePending in tests; 0 = the constant.
	quarantineCap int
	// baScan overrides MaxBestAvailableScan in tests; 0 = the constant (seat_claims.go).
	baScan int32
	// The availability-cache invalidation seam (TKT-205) —
	// availability_invalidation.go.
	invalidatorFields
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
		return p.commitAvailability(tx, slotID)
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO inventory_pools(slot_id,organizer_id,capacity,source_event_id) VALUES($1,$2,$3,$4)
		ON CONFLICT(slot_id) DO NOTHING`, slotID, organizerID, capacity, eventID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return p.commitAvailability(tx, slotID)
	}
	// Existing pool: take the ADR-010 pool lock FIRST, then decide in a fresh statement
	// snapshot. A single upsert cannot do this safely — its WHERE subqueries evaluate
	// against the pre-wait snapshot, so an adjustment committed while the upsert queued
	// on the row lock stays invisible and gets overwritten (TKT-76 ai-review round 2).
	// The overwrite guard covers claims, confirmed quantity, AND adjustment history
	// (claim_history.pool_id is non-NULL only on adjustment records): inventory owns
	// capacity after any staff adjustment (ADR-005 amendment).
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, slotID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE inventory_pools SET capacity=$1, updated_at=now()
		WHERE slot_id=$2 AND organizer_id=$3 AND confirmed_quantity=0
		AND NOT EXISTS(SELECT 1 FROM claims WHERE pool_id=$2)
		AND NOT EXISTS(SELECT 1 FROM claim_history WHERE pool_id=$2)`, capacity, slotID, organizerID)
	if err != nil {
		return err
	}
	return p.commitAvailability(tx, slotID)
}

// fingerprint stays byte-identical to the pre-channel format when channel is empty, so
// idempotency records created before the channel migration keep replaying (ADR-009).
func fingerprint(org, slot, ticketType uuid.UUID, qty int32, unitAmount int64, currency, channel, presaleCode string, reseller uuid.UUID) string {
	s := fmt.Sprintf("%s:%s:%s:%d:%d:%s", org, slot, ticketType, qty, unitAmount, currency)
	if channel != "" {
		s += ":" + channel
	}
	// The presale code is appended ONLY when non-empty, for exactly the reason the
	// channel above is: an unconditional append rehashes EVERY claim in the
	// database, so every in-flight retry stops replaying and re-executes instead —
	// a double-sell on retry, system-wide, from what looks like adding a field
	// (TKT-239 plan-review). TestFingerprintStaysByteIdenticalWithoutAPresaleCode
	// pins the legacy bytes with golden literals.
	//
	// It must be IN the fingerprint though: two holds sharing an idempotency key
	// but presenting different codes are different requests, and replaying one as
	// the other would grant the second the first's redemption.
	//
	// FRAMED, not concatenated (ai-review finding 3). Both channel and code are
	// arbitrary opaque strings that MAY CONTAIN COLONS, so a bare ":" join is
	// ambiguous: (channel="a", code="b:c") and (channel="a:b", code="c") produced
	// byte-identical input and therefore the same hash — measured, not theorised.
	// The second request would then replay the first BEFORE its allocation or code
	// was ever checked. Length-prefixing removes the ambiguity: no two distinct
	// (channel, code) pairs can produce the same framing.
	//
	// Only code-bearing requests are framed. A code-less request keeps the exact
	// legacy algorithm, so nothing already in a database is rehashed — the
	// collision needs a non-empty code to be reachable at all.
	if presaleCode != "" {
		s += fmt.Sprintf(":c%d:%s:p%d:%s", len(channel), channel, len(presaleCode), presaleCode)
	}
	// The RESELLER is part of the request identity (TKT-246 ai-review [high] F1).
	//
	// Two reseller credentials may legally share an organizer, and inventory's claims
	// are unique on (organizer_id, idempotency_key) alone. Without the reseller here,
	// reseller B reusing A's key with identical terms matched A's fingerprint and was
	// REPLAYED A's authorized hold -- returned before the sold_by check runs at all,
	// so the seller guard was bypassed by an idempotency collision rather than beaten.
	// Commerce separating its own reservation ids was not enough: it forwards the
	// caller's key to inventory unchanged.
	//
	// Appended ONLY when non-empty, and framed, for the two reasons above it: an
	// unconditional append rehashes every claim in the database and stops every
	// in-flight retry from replaying, and a bare join is ambiguous between adjacent
	// opaque fields. uuid.Nil (no reseller proven) is the pre-TKT-246 caller and must
	// hash byte-identically to before.
	if reseller != uuid.Nil {
		s += fmt.Sprintf(":r%d:%s", len(reseller.String()), reseller)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

// HoldOption carries the optional, rarely-set inputs to CreateHold.
//
// A variadic option rather than a tenth positional parameter, and that is a
// safety choice, not a style one: the signature already ends in FOUR adjacent
// bare strings (currency, channel, key) across 70 call sites, and a fifth would
// be transposable with any of them while still compiling. `WithPresaleCode(x)`
// cannot be passed in the wrong slot.
type HoldOption func(*holdOptions)

type holdOptions struct {
	presaleCode string
	reseller    uuid.UUID
}

// WithPresaleCode supplies the unlock code for a gated channel (TKT-239).
// Absent, or on an ungated allocation, it is ignored.
func WithPresaleCode(code string) HoldOption {
	return func(o *holdOptions) { o.presaleCode = code }
}

// WithReseller supplies the AUTHENTICATED reseller identity of the caller (TKT-246).
//
// Absent (uuid.Nil) means "no reseller identity was proven", which is what every
// pre-existing caller passes and what an unauthenticated request is. It is not a
// wildcard: an absent identity may consume only an UNBOUND allocation.
//
// The value must come from a verified credential (commerce's partner scope, ADR-056),
// never from a request body. Inventory cannot check that for itself — it trusts its
// internal caller — so this is honest-writer authorization, not tamper-evidence
// (ADR-021). Naming it here because the option is the exact place a future caller
// would be tempted to pass a body field.
func WithReseller(reseller uuid.UUID) HoldOption {
	return func(o *holdOptions) { o.reseller = reseller }
}

// resellerScope is the idempotency namespace a caller writes and reads in (TKT-246
// ai-review [high] F1, restructured after pass 2's [high] F4).
//
// NULL for a public caller, the reseller's id for a partner. Migration 0016 makes
// uniqueness cover it, so the two namespaces cannot name the same row.
//
// WHY A COLUMN AND NOT A KEY PREFIX. claims were UNIQUE (organizer_id,
// idempotency_key), and two reseller credentials may legally share an organizer, so
// reseller A and reseller B both sending "1" landed on one row. That was an
// AUTHORIZATION BYPASS rather than a mere collision: CreateHold looks a claim up by
// that key and returns a fingerprint-matching row as a REPLAY before it reads sold_by,
// so B was handed A's authorized hold on A's bound allocation with the seller guard
// never running.
//
// The first fix derived "r:<uuid>:<key>" in Go. It closed the handover and opened a
// denial of service: public keys are arbitrary raw strings in the SAME column, so a
// public caller can send that exact derived string first, take the row, and permanently
// deny that reseller that key -- targeted, given a predictable key and a known reseller
// id. A prefix inside a shared namespace is a naming convention, and an attacker gets
// to use it too. The namespace has to be a field the caller does not supply.
//
// The stored idempotency_key is therefore the caller's, VERBATIM, on both paths. Every
// claim in every database was written under the bare key, and transforming it would
// strand in-flight retries into second holds.
func resellerScope(reseller uuid.UUID) any {
	if reseller == uuid.Nil {
		return nil
	}
	return reseller
}

// sellerAdmits answers whether a caller may consume this allocation.
//
// One predicate, two call sites (CreateHold and PlaceGroupReservation). It is a
// function rather than two inline conditions for the reason consumedQuantity's comment
// gives about copied SQL: a rule duplicated across claim paths forks, and the fork is
// invisible until a hold succeeds where it should not have.
//
// NULL sold_by = unbound = anyone, which is every allocation that predates TKT-246.
// A bound allocation requires an exact match, so a DIFFERENT reseller is refused as
// firmly as an anonymous caller — a bare "is bound" boolean would have let reseller B
// consume reseller A's stock.
func sellerAdmits(soldBy uuid.NullUUID, caller uuid.UUID) bool {
	if !soldBy.Valid {
		return true
	}
	return caller != uuid.Nil && soldBy.UUID == caller
}

func (p *Postgres) CreateHold(ctx context.Context, org, slot, ticketType uuid.UUID, qty int32, unitAmount int64, currency, channel, key string, opts ...HoldOption) (Claim, bool, error) {
	var o holdOptions
	for _, opt := range opts {
		opt(&o)
	}
	presaleCode := o.presaleCode
	if qty <= 0 {
		return Claim{}, false, fmt.Errorf("quantity must be positive")
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var capacity, confirmed int32
	var target sql.NullInt32
	var lifecycle, closure, kind string
	// closure_status stays the LAST column before FROM: the lock-handshake tests pin the
	// pool-lock statement by the LIKE pattern "%closure_status FROM inventory_pools%FOR
	// UPDATE%" (docs/learnings/2026-07-16-lock-handshakes-pin-the-exact-statement.md), so
	// new columns go before it, never between it and FROM.
	err = tx.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity,target_capacity,lifecycle_status,inventory_kind,closure_status FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE`, slot, org).Scan(&capacity, &confirmed, &target, &lifecycle, &kind, &closure)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, false, ErrNotFound
	}
	if err != nil {
		return Claim{}, false, err
	}
	// A seated pool is claimed seat-by-seat (CreateSeatHold), never through the GA
	// quantity path — else fungible tickets would sell over reserved seats (AC2).
	if kind == "seated" {
		return Claim{}, false, ErrPoolKindMismatch
	}
	// A draining cut (TKT-76) admits against the requested target, not the clamped
	// ceiling — new demand may never grow past what the organizer asked for.
	limit := capacity
	if target.Valid {
		limit = target.Int32
	}
	var existing Claim
	var fp string
	// Scoped by reseller_scope, matching migration 0016's two partial unique indexes:
	// `IS NOT DISTINCT FROM` rather than `=` because the public scope is NULL and NULL
	// = NULL is unknown, which would find nothing and place a second hold on every
	// public retry. The lookup and the uniqueness must agree exactly or a replay
	// becomes a duplicate claim.
	// ONE fingerprint for this request, computed HERE — before the requiresCode
	// normalisation at "an ungated allocation ignores the code" below clears
	// presaleCode. Both the replay comparison and the INSERT use this value.
	//
	// Computing it twice is what TKT-296 D1 was: the comparison ran before the
	// clear and saw the code, the INSERT ran after and did not, so a first request
	// stored code-less bytes and its own identical retry compared code-bearing ones
	// and was refused as key reuse — a leaked hold plus a failed checkout retry, on
	// the buyer path. PlaceGroupReservation never had the bug because it already
	// computes fp once, pre-clear (reservations.go:51-55).
	//
	// The FORMAT is untouched: fingerprint() still appends channel and code only
	// when non-empty, so every code-less record in every database keeps its exact
	// bytes (TestFingerprintStaysByteIdenticalWithoutAPresaleCode). Only requests
	// that actually carry a code against an ungated channel change, and only
	// forward: a row written before this change stored code-less bytes, so its
	// retry still mismatches until its TTL expires. That is today's behaviour, not
	// a regression this introduces.
	requestFP := fingerprint(org, slot, ticketType, qty, unitAmount, currency, channel, presaleCode, o.reseller)

	err = tx.QueryRowContext(ctx, `SELECT id,organizer_id,pool_id,quantity,status,COALESCE(channel_code,''),expires_at,clock_timestamp(),now(),request_fingerprint,ticket_type_id,unit_amount,currency FROM claims WHERE organizer_id=$1 AND idempotency_key=$2 AND reseller_scope IS NOT DISTINCT FROM $3 AND staff_scope IS NULL`, org, key, resellerScope(o.reseller)).
		Scan(&existing.ID, &existing.OrganizerID, &existing.PoolID, &existing.Quantity, &existing.Status, &existing.Channel, &existing.ExpiresAt, &existing.ServerTime, &existing.snapshotTime, &fp, &existing.TicketTypeID, &existing.UnitAmount, &existing.Currency)
	if err == nil {
		if fp != requestFP {
			return Claim{}, false, ErrIdempotency
		}
		if existing.expired() {
			if _, err = tx.ExecContext(ctx, `UPDATE claims SET status='expired',updated_at=now() WHERE id=$1 AND status='held'`, existing.ID); err != nil {
				return Claim{}, false, err
			}
			if err = appendHistory(ctx, tx, org, existing.ID, nil, "expire", "system", "ttl_elapsed", existing.Quantity, 0, "expired", nil, nil); err != nil {
				return Claim{}, false, err
			}
			if err = p.commitAvailability(tx, slot); err != nil {
				return Claim{}, false, err
			}
			return Claim{}, false, ErrConflict
		}
		return existing, true, p.commitAvailability(tx, slot)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Claim{}, false, err
	}
	// Offering guard sits AFTER idempotency replay: replaying a pre-closure hold
	// returns its original outcome — a replay is not a new hold (TKT-75 AC2).
	if err = guardOffering(lifecycle, closure); err != nil {
		return Claim{}, false, err
	}
	if err = sweepExpired(ctx, tx, slot); err != nil {
		return Claim{}, false, err
	}
	// The channel's SALES WINDOW is judged before any capacity arithmetic
	// (TKT-238 ai-review finding 1).
	//
	// Precedence, not tidiness. A window is a property of the requested channel;
	// capacity is a property of the pool. Checking capacity first made the same
	// request answer `channel_window_closed` while the pool had room and the
	// code-less "insufficient capacity" once it did not — so a closed presale
	// read as a sellout exactly when the on-sale was busiest. Worse, the load
	// harness classifies a code-less 409 as an expected capacity rejection, so a
	// run against a closed channel could be accepted as contention evidence.
	//
	// Verified before fixing: with pool headroom the refusal was the window; with
	// an operational hold consuming the pool it became "insufficient capacity"
	// for an otherwise identical request.
	//
	// Still under the pool FOR UPDATE and still on clock_timestamp(): moving it
	// earlier changes which refusal wins, never when the boundary is judged.
	var channelWindowOpen = true
	var chCap int32
	var haveAllocation bool
	var requiresCode bool
	var soldBy uuid.NullUUID
	if channel != "" {
		err = tx.QueryRowContext(ctx,
			`SELECT cap, requires_code, sold_by, (`+windowOpen+`) FROM channel_allocations WHERE pool_id=$1 AND channel_code=$2 AND `+activeAllocation,
			slot, channel).Scan(&chCap, &requiresCode, &soldBy, &channelWindowOpen)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// No ACTIVE allocation for this channel — released, or never
			// configured. That stays the code-less capacity refusal it has always
			// been: there is no channel here to be closed.
			haveAllocation = false
		case err != nil:
			return Claim{}, false, err
		default:
			haveAllocation = true
			if !channelWindowOpen {
				return Claim{}, false, ErrChannelWindowClosed
			}
			// WHO may sell this, judged after the window and before the code and
			// the capacity arithmetic (TKT-246, amending ADR-024).
			//
			// The order is the whole point, and each boundary was argued:
			//
			//   window -> seller -> code -> capacity
			//
			// Before capacity, for TKT-238's reason: authorization is a property of
			// the requested channel and capacity is a property of the pool, so a
			// gated channel must not read as a sellout precisely when the on-sale is
			// busiest. After the window, because a closed channel is selling to
			// nobody -- answering "you are not the seller" to a request a valid
			// seller would also have been refused is both misleading AND an oracle:
			// it tells an unauthorized caller that the channel is bound rather than
			// closed. Before the code for the same reason the window precedes the
			// code: no point redeeming a scarce unlock code against an allocation
			// the caller may not consume at all -- redeemPresaleCode MUTATES, and a
			// refusal after it would burn a redemption on a caller who was never
			// eligible.
			//
			// The refusal is ErrUnavailable, deliberately NOT a distinct sentinel.
			// A "seller mismatch" code would tell an unauthenticated prober that
			// this channel exists and is bound to someone else -- the enumeration
			// oracle that presale_code_invalid was made uniform to prevent
			// (openapi.yaml, TKT-239). sold_by is not guessable and must not become
			// discoverable through a refusal.
			if !sellerAdmits(soldBy, o.reseller) {
				return Claim{}, false, ErrUnavailable
			}
			// The unlock code is judged AFTER the channel's window and BEFORE any
			// capacity arithmetic (TKT-239 / ADR-064).
			//
			// Window first: a closed channel is not selling to anyone, so "wrong
			// code" would be a misleading answer to a request that a valid code
			// would also have refused. Capacity last, for the reason TKT-238's
			// ai-review established — a channel-property refusal must not be
			// masked by a full pool, or a gated presale reads as a sellout exactly
			// when the on-sale is busiest.
			//
			// redeemPresaleCode takes the presale_codes ROW LOCK. The pool lock
			// above does NOT serialize a code: a code spans slots, so two holds on
			// different pools take different pool locks. See its comment.
			if requiresCode {
				if err := redeemPresaleCode(ctx, tx, org, channel, presaleCode, qty); err != nil {
					return Claim{}, false, err
				}
			}
		}
	}
	if !requiresCode {
		// A code presented to an ungated allocation is NOT an error — it is
		// ignored, and deliberately not recorded. Recording it would let a caller
		// write arbitrary strings into an attribution column that reporting reads,
		// on a path where nothing validates them.
		presaleCode = ""
	}

	var held int32
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND `+liveClaims, slot).Scan(&held); err != nil {
		return Claim{}, false, err
	}
	// int64 math: three valid int32 counters can wrap a 32-bit sum (ai-review finding 4).
	if int64(confirmed)+int64(held)+int64(qty) > int64(limit) {
		return Claim{}, false, ErrUnavailable
	}
	if channel != "" {
		// The allocation and its window were read above, before the pool-capacity
		// check, so a closed window refuses as a closed window whatever the pool
		// looks like. What remains here is the CAP check, which is genuinely
		// capacity arithmetic and belongs beside the pool's.
		//
		// An absent active allocation is the code-less capacity refusal: there is
		// no channel here to be closed. That keeps "closed window" and "no such
		// channel" distinct, which selecting the predicate rather than filtering
		// on it is what makes possible.
		if !haveAllocation {
			return Claim{}, false, ErrUnavailable
		}
		var consumed int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(`+consumedQuantity+`),0) FROM claims WHERE pool_id=$1 AND channel_code=$2 AND `+consumingClaims, slot, channel).Scan(&consumed); err != nil {
			return Claim{}, false, err
		}
		if consumed+int64(qty) > int64(chCap) {
			return Claim{}, false, ErrUnavailable
		}
	} else {
		// A public hold may not eat capacity still reserved for active allocations.
		var reserved int64
		if err = tx.QueryRowContext(ctx, reservedForChannelsSQL, slot).Scan(&reserved); err != nil {
			return Claim{}, false, err
		}
		if int64(confirmed)+int64(held)+int64(qty)+reserved > int64(limit) {
			return Claim{}, false, ErrUnavailable
		}
	}
	c := Claim{ID: uuid.New(), OrganizerID: org, PoolID: slot, TicketTypeID: ticketType, Quantity: qty, UnitAmount: unitAmount, Currency: currency, Status: "held", Channel: channel, Kind: "buyer"}
	// clock_timestamp(), not now(): a buyer TTL is a duration GRANTED to a buyer, so it is
	// anchored to INSERT time, not transaction-start time. now() freezes when the
	// transaction begins, and every grant path takes the pool row lock before inserting
	// (ADR-010), so under contention the lock wait sits between the two and is silently
	// charged to the buyer's TTL. A hold that queued longer than its TTL was handed back
	// already expired: liveClaims would not count it and the seat was free for someone
	// else -- worst on an on-sale, which is exactly when holds matter (TKT-148).
	//
	// RETURNING moves with the anchor, and that is not cosmetic. server_time is the
	// buyer's clock-skew reference: the storefront countdown is expires_at - server_time
	// (web/storefront/src/components/HoldPicker.tsx), commerce gates conversion on the
	// same pair, and Claim.expired() is that comparison. Anchoring expires_at while
	// returning a transaction-start server_time trades a hold that is born dead for one
	// that OVERSTATES its remaining time by the length of the wait.
	//
	// The read side deliberately stays on now(): liveClaims and every capacity read want
	// one consistent snapshot per transaction, a grant wants real time. ADR-024 records
	// the split and the argument that the two clocks cannot combine into an oversell --
	// clock_timestamp() >= now(), so this can only move an expiry later.
	err = tx.QueryRowContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint,claim_kind,channel_code,presale_code,reseller_scope)
		VALUES($1,$2,$3,$4,$5,$6,$7,'held',clock_timestamp()+$8::interval,$9,$10,'buyer',NULLIF($11,''),NULLIF($12,''),$13) RETURNING expires_at,clock_timestamp()`, c.ID, org, slot, ticketType, qty, unitAmount, currency, p.ttl.String(), key, requestFP, channel, presaleCode, resellerScope(o.reseller)).Scan(&c.ExpiresAt, &c.ServerTime)
	if err != nil {
		return Claim{}, false, idempotencyConflict(err)
	}
	if err = appendHistory(ctx, tx, org, c.ID, nil, "create", "buyer", "public_hold", qty, qty, "held", nil, nil); err != nil {
		return Claim{}, false, err
	}
	return c, false, p.commitAvailability(tx, slot)
}

// idempotencyConflict maps a unique violation on any of the three claim idempotency
// namespaces to ErrIdempotency, which the API answers as 409.
//
// Without it a losing race returns the raw pgconn error, problem()'s default branch
// turns it into an unmapped 500, and the caller cannot tell "you reused a key" from
// "inventory is broken". That was TKT-296 D2's user-visible symptom: a public caller
// occupying a staff key made every later staff operation with that key 500 forever.
// The registry check upstream catches the ordinary case; this covers the race that
// slips between the check and the INSERT, and any path that never checked at all.
func idempotencyConflict(err error) error {
	for _, idx := range []string{
		"claims_public_idempotency",
		"claims_staff_idempotency",
		"claims_reseller_idempotency",
	} {
		if isUniqueViolation(err, idx) {
			return ErrIdempotency
		}
	}
	return err
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
	err = tx.QueryRowContext(ctx, `SELECT id,organizer_id,pool_id,quantity,status,COALESCE(channel_code,''),expires_at,now(),COALESCE(ticket_type_id,'00000000-0000-0000-0000-000000000000'::uuid),COALESCE(unit_amount,0),COALESCE(currency,''),claim_kind FROM claims WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, id, org).
		Scan(&c.ID, &c.OrganizerID, &c.PoolID, &c.Quantity, &c.Status, &c.Channel, &c.ExpiresAt, &c.ServerTime, &c.TicketTypeID, &c.UnitAmount, &c.Currency, &c.Kind)
	if err != nil {
		return Claim{}, err
	}
	// Operational holds transition through their own staff endpoints only; the checkout
	// lifecycle must never finalize, confirm or release one.
	if c.Kind != "buyer" {
		return Claim{}, ErrConflict
	}
	if c.expired() {
		c.Status = "expired"
		if _, err = tx.ExecContext(ctx, `UPDATE claims SET status='expired',updated_at=now() WHERE id=$1`, id); err != nil {
			return Claim{}, err
		}
		if err = appendHistory(ctx, tx, org, id, nil, "expire", "system", "ttl_elapsed", c.Quantity, 0, "expired", nil, nil); err != nil {
			return Claim{}, err
		}
		if err = releaseSeatsForTerminal(ctx, tx, pool); err != nil {
			return Claim{}, err
		}
		if err = reconcileCapacity(ctx, tx, pool); err != nil {
			return Claim{}, err
		}
	}
	if c.Status == target {
		return c, p.commitAvailability(tx, pool)
	}
	// A checkout may crash after confirm succeeds but before commerce persists
	// completion. Treat replaying its earlier finalize step as already satisfied.
	if target == "finalizing" && c.Status == "confirmed" {
		return c, p.commitAvailability(tx, pool)
	}
	// A release against an expired claim is vacuously satisfied: expiry already freed
	// the seats (the branch above / an earlier pass), so the obligation a release
	// discharges is gone either way. Answering conflict here made expiry
	// indistinguishable from `confirmed` — a genuinely sold seat — and commerce
	// recovery parked refundable orders on the difference (TKT-115). Confirm of an
	// expired claim stays a conflict below: expired can never buy a seat.
	if target == "released" && c.Status == "expired" {
		return c, p.commitAvailability(tx, pool)
	}
	if target == "finalizing" && c.Status == "held" {
		c.Status = target
		_, err = tx.ExecContext(ctx, `UPDATE claims SET status='finalizing',updated_at=now() WHERE id=$1`, id)
		if err != nil {
			return c, err
		}
		if err = appendHistory(ctx, tx, org, id, nil, "finalize", "commerce", "checkout", c.Quantity, c.Quantity, "finalizing", nil, nil); err != nil {
			return c, err
		}
		return c, p.commitAvailability(tx, pool)
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
	action, after := "confirm", c.Quantity
	if target == "released" {
		action, after = "release", 0
		// Release lowers demand: settle a draining capacity cut (TKT-76). A seated
		// claim's seats free in the same txn (ADR-031).
		if err = releaseSeatsForTerminal(ctx, tx, pool); err != nil {
			return Claim{}, err
		}
		if err = reconcileCapacity(ctx, tx, pool); err != nil {
			return Claim{}, err
		}
	}
	if err = appendHistory(ctx, tx, org, id, nil, action, "commerce", "checkout", c.Quantity, after, target, nil, nil); err != nil {
		return Claim{}, err
	}
	return c, p.commitAvailability(tx, pool)
}

// Availability reports pool aggregates (capacity/held/confirmed) plus a channel-scoped
// `available`: with channel empty it is the public/default claimable quantity (net of
// capacity still reserved for active allocations); with a channel it is that channel's
// claimable quantity (zero when no active allocation exists). ADR-004: 5s cache tier.
func (p *Postgres) Availability(ctx context.Context, org, slot uuid.UUID, channel string) (Availability, error) {
	var a Availability
	a.SlotID = slot
	var target sql.NullInt32
	var lifecycle, closure string
	err := p.db.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity,target_capacity,lifecycle_status,closure_status,(SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND `+liveClaims+`) FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2`, slot, org).Scan(&a.Capacity, &a.Confirmed, &target, &lifecycle, &closure, &a.Held)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.OfferingStatus = offeringStatus(lifecycle, closure)
	// Effective capacity re-derives the clamp floor from live claims (TKT-76): during a
	// draining cut it follows demand down and settles at the target, sweeper or not.
	a.Capacity = effectiveCapacity(a.Capacity, target, a.Confirmed, a.Held)
	remaining := int64(a.Capacity) - int64(a.Confirmed) - int64(a.Held)
	if a.OfferingStatus != "open" {
		remaining = 0
	}
	if channel != "" {
		// Same statement, one more column — NOT a third query. The availability read
		// is pinned at exactly two statements by smoke/onsale_read_proof_test.go,
		// matched on hardcoded SQL fragments, so a separate window lookup would
		// break that proof numerically rather than visibly.
		var chCap int32
		var windowIsOpen bool
		err = p.db.QueryRowContext(ctx,
			`SELECT cap, (`+windowOpen+`) FROM channel_allocations WHERE pool_id=$1 AND channel_code=$2 AND `+activeAllocation,
			slot, channel).Scan(&chCap, &windowIsOpen)
		if errors.Is(err, sql.ErrNoRows) {
			a.Available = 0
			return a, nil
		}
		if err == nil && !windowIsOpen {
			// Nothing is claimable on a closed channel, so the read says 0 — the same
			// answer the claim path would give. The read cannot say WHY without a new
			// public field, and it deliberately does not get one: the staff breakdown
			// carries the window (ADR-054), and adding a public field later is
			// additive while retracting one is a contract break.
			a.Available = 0
			return a, nil
		}
		if err != nil {
			return a, err
		}
		var consumed int64
		if err = p.db.QueryRowContext(ctx, `SELECT COALESCE(sum(`+consumedQuantity+`),0) FROM claims WHERE pool_id=$1 AND channel_code=$2 AND `+consumingClaims, slot, channel).Scan(&consumed); err != nil {
			return a, err
		}
		a.Available = clampAvailable(min(remaining, int64(chCap)-consumed))
		return a, nil
	}
	var reserved int64
	if err = p.db.QueryRowContext(ctx, reservedForChannelsSQL, slot).Scan(&reserved); err != nil {
		return a, err
	}
	a.Available = clampAvailable(remaining - reserved)
	return a, nil
}
