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
	db   *sql.DB
	keys *Keyring
}

func loadExistingFact(ctx context.Context, tx *sql.Tx, f Fact) (Entry, bool, error) {
	var existing Entry
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT organizer_id,sequence,fact_type,occurred_at,buyer_id,amount,currency,payload,previous_hash,entry_hash,key_id,signature FROM journal_entries WHERE fact_id=$1`, f.ID).Scan(&existing.OrganizerID, &existing.Sequence, &existing.Type, &existing.OccurredAt, &existing.BuyerID, &existing.Amount, &existing.Currency, &raw, &existing.PreviousHash, &existing.EntryHash, &existing.KeyID, &existing.Signature)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	existing.ID = f.ID
	if err := json.Unmarshal(raw, &existing.Payload); err != nil {
		return Entry{}, false, err
	}
	want, _ := canonical(f, existing.Sequence)
	got, _ := canonical(existing.Fact, existing.Sequence)
	if !hmac.Equal(want, got) {
		return Entry{}, false, refuse("fact id reused with different content")
	}
	return existing, true, nil
}

// New builds a journal over an already-validated keyring. There is deliberately no
// single-key constructor: it would keep the pre-rotation model (one key, all
// history invalid the moment it changes) constructible for no caller that exists.
func New(db *sql.DB, keys *Keyring) *Journal {
	if keys == nil {
		// Fail here rather than on the first Append: a nil ring otherwise surfaces as a
		// nil-map panic deep inside a money write, where the stack says nothing about
		// the actual mistake (a caller that skipped NewKeyring).
		panic("store.New: journal keyring is required")
	}
	return &Journal{db: db, keys: keys}
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

// Refusal marks an error whose TEXT is safe to hand back to the caller: it was
// written here, in Go, and names nothing about the database.
//
// The handlers used to echo `err.Error()` on every 400 and 409 arm. Most of those
// arms are reached by hand-written refusals, but not all of them — `Append` and
// `BindRefundLeg` return a wrapped pgx error from any failing statement down the
// same path, and that error carries table, column and constraint names. Marking
// the safe ones is what lets a handler tell the two apart without also flattening
// a useful refusal into "internal error" (ai-review S10).
//
// A wrapper rather than a sentinel to wrap WITH, so the message the caller reads
// is unchanged from before.
type Refusal struct{ error }

// Unwrap keeps errors.Is working through the marker for anything ever wrapped in one.
func (r Refusal) Unwrap() error { return r.error }

func refuse(msg string) error { return Refusal{errors.New(msg)} }

// IsRefusal reports whether err's text may be shown to the caller.
func IsRefusal(err error) bool {
	var r Refusal
	return errors.As(err, &r)
}

// SupportedCurrencies is the set the money paths are proven for.
//
// A SET rather than a bare `== "EUR"` because the shape is what makes adding one
// a decision instead of an edit: every code here has exponent 2 — one minor unit
// is 1/100 — and this platform's integer minor units assume that everywhere,
// silently. JPY has exponent 0 and KWD has 3, so admitting either without first
// carrying a per-currency exponent through pricing, fees, splits, settlement and
// every formatter is a 100x or 1000x error in the ledger, not a validation gap
// (ai-review S13).
//
// The journal is where this is enforced because the journal is what has to be
// true. The charge boundary hard-coded EUR while this accepted any three-letter
// code, so the assumption was pinned at the outer edge and open at the durable
// one — exactly backwards for an append-only trail nothing can go back and fix.
var SupportedCurrencies = map[string]bool{"EUR": true}

// moneyMovingTypes are the facts that assert money actually moved. They must
// carry a POSITIVE amount: a zero-amount capture or refund is not a fact about
// money, it is a caller-side quantity guard that failed open, and the journal is
// append-only so it cannot be taken back (ai-review S13).
//
// Deliberately NOT every type. `order.created`, `order.completed` and
// `order.failed` are legitimately zero — a comp ticket is a real order that moves
// no money — and `payment.declined` / `payment.timeout` carry the amount that was
// ATTEMPTED, which says nothing about what moved. Requiring positivity there
// would refuse honest facts, and a journal that refuses honest facts gets worked
// around.
//
// `order.exchange.reversed` / `.sold` are excluded for the same reason, and one
// more. They are legitimately zero — exchange a comp, or exchange down to a
// zero-price replacement with no retained fee, and one leg carries 0 — and they
// are a PAIR that commerce writes SEQUENTIALLY (exchanges.go exchangeFacts).
// Refusing the second leg after the first is already appended does not prevent a
// bad trail, it CREATES one: a permanent unmatched reversal in an append-only
// journal, which is worse than the zero it refused.
var moneyMovingTypes = map[string]bool{
	"payment.authorized": true,
	"payment.captured":   true,
	"payment.voided":     true,
	"payment.refunded":   true,
	"order.refunded":     true,
}

func validate(f Fact) error {
	if f.ID == uuid.Nil || f.OrganizerID == uuid.Nil || f.BuyerID == uuid.Nil || f.Amount < 0 {
		return refuse("invalid journal fact")
	}
	if !SupportedCurrencies[f.Currency] {
		return refuse(fmt.Sprintf("currency %q is not supported: this platform's integer minor units assume exponent 2 (see store.SupportedCurrencies)", f.Currency))
	}
	// payment.voided / payment.refunded are compensating facts (ADR-016 §Decision 4): a
	// void or refund is an appended entry, never a mutation of the authorize/capture it
	// reverses. Added in TKT-56 Slice 1; the compensation slice is what emits them.
	// order.refunded joins the vocabulary (TKT-156): the compensating fact commerce
	// appends when a completed order is refunded, in whole or in part. ADR-003 —
	// corrections are new entries, never edits to the sale fact.
	//
	// order.exchange.reversed / .sold join it for TKT-158. They are a PAIR and both are
	// written whichever way the money went: the provider moves only the difference, but
	// the trail records that a line worth X was reversed and one worth Y was sold. What
	// the provider does and what the trail records are not the same fact (ADR-039 §1).
	allowedTypes := map[string]bool{"order.created": true, "order.completed": true, "order.failed": true, "order.refunded": true, "order.exchange.reversed": true, "order.exchange.sold": true, "payment.authorized": true, "payment.captured": true, "payment.declined": true, "payment.timeout": true, "payment.voided": true, "payment.refunded": true}
	if !allowedTypes[f.Type] {
		return refuse("unsupported journal fact type")
	}
	for k := range f.Payload {
		l := strings.ToLower(k)
		if strings.Contains(l, "email") || strings.Contains(l, "name") || strings.Contains(l, "pan") || strings.Contains(l, "cvv") {
			return refuse(fmt.Sprintf("PII/payment field %q forbidden in journal", k))
		}
		if k != "order_id" {
			return refuse(fmt.Sprintf("payload field %q is not allowed", k))
		}
	}
	if _, err := uuid.Parse(f.Payload["order_id"]); err != nil {
		return refuse("valid order_id payload required")
	}
	if moneyMovingTypes[f.Type] && f.Amount <= 0 {
		return refuse(fmt.Sprintf("%s must carry a positive amount", f.Type))
	}
	return nil
}

// Append writes a fact with no settlement. Every caller but the capture path
// uses this.
func (j *Journal) Append(ctx context.Context, f Fact) (Entry, bool, error) {
	return j.AppendWithSettlement(ctx, f, nil)
}

// AppendWithSettlement writes a fact AND its ledger lines in ONE transaction.
//
// `settlement` is nil for every fact that is not a capture. A payment.captured
// fact with nil settlement will be refused at commit by the deferred trigger,
// which is the point: the invariant lives in the database, not in the
// discipline of callers.
func (j *Journal) AppendWithSettlement(ctx context.Context, f Fact, settlement []SettlementEntry) (Entry, bool, error) {
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
	existing, found, err := loadExistingFact(ctx, tx, f)
	if err != nil {
		return Entry{}, false, err
	}
	if found {
		return existing, true, tx.Commit()
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
	// A concurrent identical append can pass the optimistic check above while
	// waiting for this organizer lock. Re-read under the lock so the loser is a
	// clean replay rather than a primary-key violation.
	existing, found, err = loadExistingFact(ctx, tx, f)
	if err != nil {
		return Entry{}, false, err
	}
	if found {
		return existing, true, tx.Commit()
	}
	seq++
	canon, err := canonical(f, seq)
	if err != nil {
		return Entry{}, false, err
	}
	sum := hash(prev, canon)
	sig := j.keys.sign(sum)
	_, err = tx.ExecContext(ctx, `INSERT INTO journal_entries(fact_id,organizer_id,sequence,fact_type,occurred_at,buyer_id,amount,currency,payload,previous_hash,entry_hash,key_id,signature) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, f.ID, f.OrganizerID, seq, f.Type, f.OccurredAt, f.BuyerID, f.Amount, f.Currency, f.Payload, prev, sum, j.keys.ActiveKeyID(), sig)
	if err != nil {
		return Entry{}, false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE journal_heads SET last_sequence=$2,last_hash=$3 WHERE organizer_id=$1`, f.OrganizerID, seq, sum)
	if err != nil {
		return Entry{}, false, err
	}
	// Settlement rides THIS transaction (TKT-217 / ADR-048). Not a separate call
	// and not a later finalizer: the entries then exist if and only if the
	// captured fact does, so "a capture that journals but does not settle" is not
	// a state the database can hold.
	//
	// Replay needs nothing extra. The fact id is deterministic, and the two
	// existing-fact branches above return before reaching here — so a replayed
	// append writes neither the fact nor the entries.
	if err = insertSettlement(ctx, tx, f, settlement); err != nil {
		return Entry{}, false, err
	}
	return Entry{Fact: f, Sequence: seq, PreviousHash: prev, EntryHash: sum, KeyID: j.keys.ActiveKeyID(), Signature: sig}, false, tx.Commit()
}

func (j *Journal) Verify(ctx context.Context) error {
	return j.verify(ctx, nil)
}

// verify is Verify with a test seam. `afterEntries` runs once the entry scan has been
// fully consumed and closed, and before the head scan — the exact window in which a
// concurrent append used to make an intact journal look desynchronized. Production
// passes nil; only TestJournalVerifyReadsEntriesAndHeadsFromOneSnapshot passes a
// callback, because the interleaving cannot be constructed from outside this function.
func (j *Journal) verify(ctx context.Context, afterEntries func()) error {
	// Both scans read one snapshot (TKT-254). Without this they were two statements on a
	// pooled *sql.DB -- two snapshots, possibly two connections -- so an append that
	// committed between them made the head scan see a sequence the entry scan never had,
	// and an intact journal was reported as `journal head mismatch`. ADR-003 puts this
	// check in the gate and ADR-021 requires that an integrity claim not cry wolf; a
	// verifier that raises a false alarm against a live database teaches operators to
	// distrust the alarm.
	//
	// This is NOT the fix for TKT-254's smoke flake, and must not be read as one. That
	// flake is a row the harness deleted and never restored -- durable corruption, which
	// a consistent snapshot still reports, correctly. The flake's fix is the commerce
	// quiesce in scripts/smoke.sh. Deleting either change because the other exists
	// reopens a different defect.
	//
	// ReadOnly is enforcement, not decoration: it makes "this transaction exists to take
	// a snapshot, not to write" a rule PostgreSQL applies rather than a comment a future
	// edit can walk past. First REPEATABLE READ in this repo -- every other BeginTx here
	// passes nil, which is READ COMMITTED and correct for the write paths that use it.
	tx, err := j.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT fact_id,organizer_id,sequence,fact_type,occurred_at,buyer_id,amount,currency,payload,previous_hash,entry_hash,key_id,signature FROM journal_entries ORDER BY organizer_id,sequence`)
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
		// Two independent failures, two messages (TKT-254). These used to share one
		// `broken chain` string behind a disjunction, and because `||` short-circuits,
		// the log could not say which condition fired or what was expected — a missing
		// row and an edited link are very different incidents, and telling them apart
		// took a full investigation.
		if want := seqByOrg[e.OrganizerID] + 1; e.Sequence != want {
			return fmt.Errorf("journal sequence gap organizer=%s expected_sequence=%d observed_sequence=%d",
				e.OrganizerID, want, e.Sequence)
		}
		if !hmac.Equal(prev, e.PreviousHash) {
			return fmt.Errorf("journal previous hash mismatch organizer=%s sequence=%d expected_previous_hash=%s observed_previous_hash=%s",
				e.OrganizerID, e.Sequence, Hex(prev), Hex(e.PreviousHash))
		}
		c, _ := canonical(e.Fact, e.Sequence)
		sum := hash(prev, c)
		if !hmac.Equal(sum, e.EntryHash) {
			return fmt.Errorf("invalid hash organizer=%s sequence=%d", e.OrganizerID, e.Sequence)
		}
		// Resolve the key the entry itself names, so a journal spanning a rotation
		// verifies end to end (ADR-016 §Decision 8). An entry naming a key the ring
		// does not hold fails here — that is the retirement consequence made visible,
		// never a skipped entry.
		if err := j.keys.verify(e.KeyID, sum, e.Signature); err != nil {
			return fmt.Errorf("%w organizer=%s sequence=%d", err, e.OrganizerID, e.Sequence)
		}
		prevByOrg[e.OrganizerID] = sum
		seqByOrg[e.OrganizerID] = e.Sequence
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if afterEntries != nil {
		afterEntries()
	}
	headRows, err := tx.QueryContext(ctx, `SELECT organizer_id,last_sequence,last_hash FROM journal_heads`)
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

// OperationRequest is the durable original charge request bound with the operation
// (TKT-114/S2). It is what lets a compensation fact be built from the row alone
// (buyer/order/amount/currency) and lets Status replay the exact original create when the
// provider reference was lost to a crash. PaymentMethodRef is sensitive operational data:
// stored for replay only, never journalled, logged, or returned by an endpoint.
type OperationRequest struct {
	OrderID          uuid.UUID
	BuyerID          uuid.UUID
	Amount           int64
	Currency         string
	PaymentMethodRef string
	// SettlementDigest identifies the attribution this charge bound with
	// (TKT-219). The request fingerprint deliberately does not cover the
	// settlement plan -- it is not part of what the buyer is being charged -- so
	// without this an unresolved operation whose lease expired could be retried
	// with a different plan and re-attribute money the provider already took.
	SettlementDigest string
}

// nullableDigest keeps an empty digest out of the column as NULL, so "no plan
// was recorded" and "a plan resolving to nothing" stay distinguishable.
func nullableDigest(d string) any {
	if d == "" {
		return nil
	}
	return d
}

func (j *Journal) BindOperation(ctx context.Context, org uuid.UUID, key, fingerprint string, req OperationRequest) (string, uuid.UUID, time.Time, bool, error) {
	result, err := j.db.ExecContext(ctx, `INSERT INTO payment_operations(organizer_id,idempotency_key,request_fingerprint,order_id,buyer_id,request_amount,request_currency,payment_method_ref,settlement_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, org, key, fingerprint, req.OrderID, req.BuyerID, req.Amount, req.Currency, req.PaymentMethodRef, nullableDigest(req.SettlementDigest))
	if err != nil {
		return "", uuid.Nil, time.Time{}, false, err
	}
	var stored string
	var status sql.NullString
	var factID uuid.NullUUID
	var occurredAt, leaseUntil time.Time
	var storedDigest sql.NullString
	err = j.db.QueryRowContext(ctx, `SELECT request_fingerprint,status,fact_id,occurred_at,lease_until,settlement_digest FROM payment_operations WHERE organizer_id=$1 AND idempotency_key=$2`, org, key).Scan(&stored, &status, &factID, &occurredAt, &leaseUntil, &storedDigest)
	if err != nil {
		return "", uuid.Nil, time.Time{}, false, err
	}
	if stored != fingerprint {
		return "", uuid.Nil, time.Time{}, false, refuse("idempotency key reused with different request")
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 && !status.Valid {
		// An operation that already bound a plan keeps it. The provider may
		// ALREADY have captured against this key -- an unresolved row is exactly
		// the case where nobody knows -- so letting a retry substitute a different
		// attribution would record that money as owed to someone else. Same
		// disposition as a reused key with a different request, because that is
		// what it is (TKT-219, ADR-048 §3e).
		//
		// A NULL digest is a row bound before the column existed. There is nothing
		// to compare against and nothing to recover it from, so the retry's plan is
		// adopted below -- the behaviour that preceded this check, and no worse.
		if storedDigest.Valid && storedDigest.String != req.SettlementDigest {
			return "", uuid.Nil, time.Time{}, false, refuse("idempotency key reused with a different settlement plan")
		}
		taken, err := j.db.ExecContext(ctx, `UPDATE payment_operations SET lease_until=now()+interval '30 seconds', settlement_digest=COALESCE(settlement_digest,$3) WHERE organizer_id=$1 AND idempotency_key=$2 AND status IS NULL AND lease_until<=now()`, org, key, nullableDigest(req.SettlementDigest))
		if err != nil {
			return "", uuid.Nil, time.Time{}, false, err
		}
		rows, _ := taken.RowsAffected()
		if rows == 0 {
			return "", uuid.Nil, time.Time{}, false, refuse("payment operation in progress")
		}
	}
	return status.String, factID.UUID, occurredAt.UTC().Truncate(time.Microsecond), status.Valid, nil
}

// Operation is the recorded outcome of a payment operation, read without binding.
type Operation struct {
	Status     string
	FactID     uuid.UUID
	OccurredAt time.Time
	// Resolved is false when the operation exists but carries no terminal result: the
	// charge was bound and is still in flight, or the process driving it died. Callers
	// must not read that as "no side effect" — it is the payment_unknown case.
	Resolved bool
	// The fingerprint of the request that bound this operation. The charge path
	// compares it BEFORE validating a settlement plan, so a reused key with a
	// different request is still answered with the documented 409 rather than a
	// plan error (TKT-217, third review pass).
	RequestFingerprint string
	// The durable original request (TKT-114/S2). Zero values on rows bound before
	// migration 0002 — a compensation cannot be built from such a row.
	OrderID          uuid.UUID
	BuyerID          uuid.UUID
	RequestAmount    int64
	RequestCurrency  string
	PaymentMethodRef string
	// Provider evidence, written by CompleteOperation (charge path) or
	// RecordProviderState (status resolution). Empty until either has run.
	ProviderPaymentRef string
	ProviderChargeRef  string
	ProviderState      string
	AuthorizedAmount   int64
	CapturedAmount     int64
}

// ProviderResult is the provider evidence persisted onto an operation row: references,
// the normalized provider state, and the amounts that state proves.
type ProviderResult struct {
	PaymentRef       string
	ChargeRef        string
	State            string
	AuthorizedAmount int64
	CapturedAmount   int64
}

// LookupOperation reads an operation's recorded outcome. Strictly read-only, unlike
// BindOperation, which inserts and takes a lease: commerce's recovery runner calls this
// to resolve an ambiguous order, and binding there would fabricate an operation for an
// order that may never have charged.
//
// Returns found=false when no operation exists for the key — evidence the charge was
// never submitted, which is what lets recovery release the claim safely.
func (j *Journal) LookupOperation(ctx context.Context, org uuid.UUID, key string) (Operation, bool, error) {
	var op Operation
	var status sql.NullString
	var factID, orderID, buyerID uuid.NullUUID
	var occurredAt time.Time
	var reqAmount, authAmount, capAmount sql.NullInt64
	var reqCurrency, pmRef, provPayRef, provChRef, provState sql.NullString
	var fingerprint string
	err := j.db.QueryRowContext(ctx, `SELECT status,fact_id,occurred_at,order_id,buyer_id,request_amount,request_currency,payment_method_ref,provider_payment_ref,provider_charge_ref,provider_state,authorized_amount,captured_amount,request_fingerprint FROM payment_operations WHERE organizer_id=$1 AND idempotency_key=$2`, org, key).
		Scan(&status, &factID, &occurredAt, &orderID, &buyerID, &reqAmount, &reqCurrency, &pmRef, &provPayRef, &provChRef, &provState, &authAmount, &capAmount, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, false, nil
	}
	if err != nil {
		return Operation{}, false, err
	}
	op.Resolved = status.Valid
	op.RequestFingerprint = fingerprint
	op.Status = status.String
	op.FactID = factID.UUID
	op.OccurredAt = occurredAt.UTC().Truncate(time.Microsecond)
	op.OrderID, op.BuyerID = orderID.UUID, buyerID.UUID
	op.RequestAmount, op.RequestCurrency, op.PaymentMethodRef = reqAmount.Int64, reqCurrency.String, pmRef.String
	op.ProviderPaymentRef, op.ProviderChargeRef, op.ProviderState = provPayRef.String, provChRef.String, provState.String
	op.AuthorizedAmount, op.CapturedAmount = authAmount.Int64, capAmount.Int64
	return op, true, nil
}

func (j *Journal) CompleteOperation(ctx context.Context, org uuid.UUID, key, status string, factID uuid.UUID, prov ProviderResult) error {
	_, err := j.db.ExecContext(ctx, `UPDATE payment_operations SET status=$3,fact_id=$4,provider_payment_ref=NULLIF($5,''),provider_charge_ref=NULLIF($6,''),provider_state=NULLIF($7,''),authorized_amount=$8,captured_amount=$9,provider_state_at=now() WHERE organizer_id=$1 AND idempotency_key=$2 AND status IS NULL`, org, key, status, factID, prov.PaymentRef, prov.ChargeRef, prov.State, prov.AuthorizedAmount, prov.CapturedAmount)
	return err
}

// RecordProviderState persists provider evidence learned by a Status resolution WITHOUT
// touching the operation's terminal status/fact — those belong to the charge and recovery
// flows. This is what turns a retrieved/replayed provider answer into the durable evidence
// the void/refund state checks read (TKT-114/S2; the S3 recovery slice consumes it).
// Provider-state writes are MONOTONIC: a status answer that raced a completion must not
// regress durable evidence (ai-review B4 — a delayed "authorized" observation overwriting
// "captured" would let a void through on captured money). NULL and "authorized" may
// progress to anything; every other recorded state accepts only an idempotent re-write of
// itself.
// The boolean reports whether the write LANDED: false means the guard blocked a stale
// observation, and the caller must answer from the stored evidence instead of the
// provider result it failed to record (second-pass P2-2).
func (j *Journal) RecordProviderState(ctx context.Context, org uuid.UUID, key string, prov ProviderResult) (bool, error) {
	res, err := j.db.ExecContext(ctx, `UPDATE payment_operations SET provider_payment_ref=COALESCE(NULLIF($3,''),provider_payment_ref),provider_charge_ref=COALESCE(NULLIF($4,''),provider_charge_ref),provider_state=NULLIF($5,''),authorized_amount=$6,captured_amount=$7,provider_state_at=now() WHERE organizer_id=$1 AND idempotency_key=$2 AND (provider_state IS NULL OR provider_state='authorized' OR provider_state=$5)`, org, key, prov.PaymentRef, prov.ChargeRef, prov.State, prov.AuthorizedAmount, prov.CapturedAmount)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// CompensationKey derives the bounded, versioned provider idempotency key for a
// compensation from its database identity (ADR-032 §Refund; plan-final). Deterministic by
// construction: a crashed compensation that re-binds re-derives the SAME key, so its
// provider call lands on the provider's idempotency layer instead of issuing a second
// void/refund. NUL separators prevent concatenation collisions.
func CompensationKey(org uuid.UUID, sourceKey, kind string) string {
	sum := sha256.Sum256([]byte(org.String() + "\x00" + sourceKey + "\x00" + kind))
	return "psp-comp-v1:" + hex.EncodeToString(sum[:])
}

// Compensation is one durable void/refund attempt against a source operation.
type Compensation struct {
	Kind        string
	ProviderKey string // deterministic provider idempotency key (CompensationKey)
	Status      string // "" until completed; then "voided"/"refunded"
	ProviderRef string // provider compensation reference (re_… for refunds)
	FactID      uuid.UUID
	Amount      int64
	Currency    string
	Completed   bool
	// BoundAt is the row's stable creation time. The compensating fact's OccurredAt MUST
	// come from here, not the clock: the fact ID is deterministic and the journal's replay
	// dedupe compares the full canonical fact, so a retry across the append/complete crash
	// boundary must reconstruct byte-identical content (ai-review B1).
	BoundAt time.Time
}

// BindCompensation inserts-or-loads the one compensation row for (org, sourceKey, kind),
// recording the amount/currency evidence the compensation was decided on. The PK makes a
// concurrent duplicate converge on the same row — and therefore the same deterministic
// provider key — so two racing refund calls cannot issue two provider refunds.
// It runs inside a transaction that first takes `SELECT … FOR UPDATE` on the SAME
// payment_operations row BindRefundLeg locks, and that is what makes the whole-vs-partial
// exclusion real rather than a check (TKT-156 ai-review, critical). Two autocommit
// statements — "no legs exist" then "insert the compensation" — leave a window in which a
// partial leg binds between them; both rows then exist, both derive distinct provider
// keys, and the partial amount plus the entire captured amount are both refunded. The
// shared row lock is the only thing that serializes two paths writing different tables.
func (j *Journal) BindCompensation(ctx context.Context, org uuid.UUID, sourceKey, kind string, amount int64, currency string) (Compensation, error) {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return Compensation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM payment_operations WHERE organizer_id=$1 AND idempotency_key=$2 FOR UPDATE`, org, sourceKey).Scan(&exists); err != nil {
		return Compensation{}, err
	}
	// A compensation that already exists RESUMES: the exclusion below must not judge it,
	// because a leg bound afterwards cannot un-make a whole refund already in progress.
	if c, found, err := lookupCompensationTx(ctx, tx, org, sourceKey, kind); err != nil {
		return Compensation{}, err
	} else if found {
		return c, tx.Commit()
	}
	if kind == "refund" {
		var legs int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM payment_refund_legs WHERE organizer_id=$1 AND source_idempotency_key=$2`, org, sourceKey).Scan(&legs); err != nil {
			return Compensation{}, err
		}
		if legs > 0 {
			return Compensation{}, ErrRefundLegsBound
		}
	}
	providerKey := CompensationKey(org, sourceKey, kind)
	if _, err := tx.ExecContext(ctx, `INSERT INTO payment_compensations(organizer_id,source_idempotency_key,kind,provider_idempotency_key,amount,currency) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, org, sourceKey, kind, providerKey, amount, currency); err != nil {
		return Compensation{}, err
	}
	c, found, err := lookupCompensationTx(ctx, tx, org, sourceKey, kind)
	if err != nil {
		return Compensation{}, err
	}
	if !found {
		return Compensation{}, errors.New("compensation row missing after bind")
	}
	return c, tx.Commit()
}

// LookupCompensation reads a compensation row without binding one — the read-only replay
// check (ai-review B5): a completed compensation must answer as a replay BEFORE any
// eligibility re-derivation, whose evidence may legitimately have moved on since.
func (j *Journal) LookupCompensation(ctx context.Context, org uuid.UUID, sourceKey, kind string) (Compensation, bool, error) {
	return lookupCompensationTx(ctx, j.db, org, sourceKey, kind)
}

func lookupCompensationTx(ctx context.Context, q rowQuerier, org uuid.UUID, sourceKey, kind string) (Compensation, bool, error) {
	var c Compensation
	var status, providerRef sql.NullString
	var factID uuid.NullUUID
	var amt sql.NullInt64
	var cur sql.NullString
	var boundAt time.Time
	err := q.QueryRowContext(ctx, `SELECT kind,provider_idempotency_key,status,provider_ref,fact_id,amount,currency,bound_at FROM payment_compensations WHERE organizer_id=$1 AND source_idempotency_key=$2 AND kind=$3`, org, sourceKey, kind).
		Scan(&c.Kind, &c.ProviderKey, &status, &providerRef, &factID, &amt, &cur, &boundAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Compensation{}, false, nil
	}
	if err != nil {
		return Compensation{}, false, err
	}
	c.Status, c.ProviderRef, c.FactID = status.String, providerRef.String, factID.UUID
	c.Amount, c.Currency = amt.Int64, cur.String
	c.Completed = status.Valid
	c.BoundAt = boundAt.UTC().Truncate(time.Microsecond)
	return c, true, nil
}

// RecordCompensationProviderRef persists the provider's compensation reference on a
// still-bound row (a pending refund's re_ — ai-review B3): the next attempt resolves that
// reference instead of re-submitting the refund.
func (j *Journal) RecordCompensationProviderRef(ctx context.Context, org uuid.UUID, sourceKey, kind, providerRef string) error {
	_, err := j.db.ExecContext(ctx, `UPDATE payment_compensations SET provider_ref=$4 WHERE organizer_id=$1 AND source_idempotency_key=$2 AND kind=$3 AND status IS NULL`, org, sourceKey, kind, providerRef)
	return err
}

// CompleteCompensation records the durable provider result and the journalled fact for a
// bound compensation. Only the first completion writes (status IS NULL guard) — a replay
// keeps the original result.
func (j *Journal) CompleteCompensation(ctx context.Context, org uuid.UUID, sourceKey, kind, status, providerRef string, factID uuid.UUID) error {
	_, err := j.db.ExecContext(ctx, `UPDATE payment_compensations SET status=$4,provider_ref=NULLIF($5,''),fact_id=$6,completed_at=now() WHERE organizer_id=$1 AND source_idempotency_key=$2 AND kind=$3 AND status IS NULL`, org, sourceKey, kind, status, providerRef, factID)
	return err
}

func Hex(b []byte) string { return hex.EncodeToString(b) }
