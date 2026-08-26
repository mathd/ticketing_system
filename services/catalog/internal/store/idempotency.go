package store

// Create idempotency for events, performances and ticket types (TKT-200).
//
// The shape mirrors commerce (order_refunds, order_exchanges): a caller-chosen
// key scoped by organizer, plus a fingerprint of the request, so a repeat of the
// SAME request replays the first resource and a repeat with a DIFFERENT body is
// refused instead of silently handing back somebody else's row.
//
// Two deliberate divergences from commerce, both because catalog is not
// commerce:
//
//  1. Ids stay database-generated. Commerce derives ids from (organizer, key)
//     because its downstream journal fact ids are deterministic and a retry must
//     re-derive the same facts. Catalog has no such downstream — all three
//     tables mint ids with gen_random_uuid() — so derivation would buy a
//     property nothing consumes while letting a client-chosen string determine a
//     primary key. The UNIQUE index closes the race either way.
//
//  2. The key is optional at this layer. The API refuses an empty one, so an
//     empty value reaching here means a non-contract writer, and it stores NULL:
//     outside the partial unique index rather than colliding with every other
//     keyless row.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// nullableKey renders an idempotency key for storage. Empty becomes NULL, which
// the partial unique index does not constrain — see 0020_create_idempotency.sql
// for why that is correct rather than a loophole.
func nullableKey(key string) any {
	if key == "" {
		return nil
	}
	return key
}

// fingerprint hashes the canonical form of a request's meaningful fields.
//
// Fields are joined with a NUL separator, which no field here can contain, so
// two different field lists cannot render to the same string — the ambiguity a
// plain concatenation would introduce ("ab"+"c" == "a"+"bc").
//
// THE VALUES PASSED HERE MUST BE THE NORMALIZED ONES — the values actually
// written to the row, after defaulting. Fingerprinting the raw request while
// storing the normalized value makes `kind: ""` and `kind: "performance"` two
// fingerprints for one identical row, so a replay of a semantically identical
// request would 409.
func fingerprint(fields ...string) string {
	var buf []byte
	for _, f := range fields {
		buf = append(buf, f...)
		buf = append(buf, 0)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

func fingerprintInt(v int64) string { return strconv.FormatInt(v, 10) }

// performanceFingerprint covers every field a create writes, with kind and mode
// passed in already normalized (see fingerprint's contract). Optional pointers
// render as a fixed sentinel when nil, so "absent" and the empty string are
// distinguishable.
func performanceFingerprint(in PerformanceInput, kind, mode string) string {
	opt := func(p *string) string {
		if p == nil {
			return "\x01nil"
		}
		return *p
	}
	// Canonicalized to the DATABASE's representation, not Go's (ai-review
	// [medium]). The fingerprint's whole contract is "same stored row, same
	// hash", so any precision Go carries and Postgres discards is a way for two
	// requests that become one identical row to hash differently — and a retry
	// reconstructed from the stored value would then be refused as a conflict.
	//
	// starts_at is fingerprinted from the value the INSERT will use:
	// CreatePerformance normalizes it once, before either (normalizeStartsAt),
	// so the two representations cannot disagree. Two representations of one
	// instant is exactly how a retry ends up conflicting with its own original.
	starts := "\x01nil"
	if in.StartsAt != nil {
		starts = in.StartsAt.UTC().Format(timeFingerprintLayout)
	}
	// operating_date is a DATE — a CALENDAR date, not an instant. Its fields are
	// read in their own location and never converted: a caller passing midnight
	// in Asia/Tokyo means 2026-09-01, and .UTC() would turn that into
	// 2026-08-31 in the hash while the column still stores 2026-09-01 (ai-review
	// pass 2 [medium]). The contract path always parses a bare YYYY-MM-DD into
	// UTC midnight, so this is invisible there — but a direct store caller in a
	// positive-offset zone would get a false conflict on retry.
	operating := "\x01nil"
	if in.OperatingDate != nil {
		y, m, d := in.OperatingDate.Date()
		operating = fmt.Sprintf("%04d-%02d-%02d", y, int(m), d)
	}
	seatMap := "\x01nil"
	if in.SeatMapID != nil {
		seatMap = in.SeatMapID.String()
	}
	maxEntries := "\x01nil"
	if in.ReEntry.MaxEntries != nil {
		maxEntries = fingerprintInt(int64(*in.ReEntry.MaxEntries))
	}
	return fingerprint(
		in.EventID.String(), in.VenueID.String(), kind, starts, operating,
		opt(in.OpensAt), opt(in.ClosesAt), in.Timezone, mode, maxEntries,
		strconv.FormatBool(in.ReEntry.RequiresExit), seatMap,
	)
}

// normalizeStartsAt drops sub-microsecond precision from an instant, so the
// value fingerprinted and the value INSERTed are the same value.
//
// Truncation, not an attempt to reproduce Postgres's rounding — and the reason
// is that once this runs, there is no rounding left to reproduce. A whole number
// of microseconds is stored by timestamptz unchanged, so the stored value equals
// the normalized value for ANY rule that lands on a microsecond boundary, and a
// retry echoing the stored instant matches by construction.
//
// This replaced a half-to-even implementation that modelled Postgres exactly.
// It was deleted rather than kept: with normalization happening BEFORE the
// insert, no input exists for which round and truncate produce different
// outcomes, and mutating one into the other left every test green. A dead
// mechanism with a green test beside it reads as a guarantee.
//
// The rounding rule still matters to anyone hashing an instant that has already
// been stored, which is why the history is here: Postgres rounds half to EVEN
// ('…0000005Z' -> '…00', '…0000025Z' -> '…000002', '…0000035Z' -> '…000004'),
// which is neither Go's time.Truncate nor Go's time.Round.
func normalizeStartsAt(t time.Time) time.Time {
	return t.Truncate(time.Microsecond)
}

// timeFingerprintLayout pins the rendering of an instant in a fingerprint. UTC,
// with MICROsecond digits — the precision timestamptz actually keeps. Two
// callers sending the same instant in different offsets must fingerprint alike,
// because the row stores one instant; and two instants Postgres cannot tell
// apart must fingerprint alike for the same reason.
const timeFingerprintLayout = "2006-01-02T15:04:05.000000Z07:00"

// nullableFingerprint stores a fingerprint only alongside a key. A fingerprint
// on a keyless row would be dead weight: nothing can ever look it up, because
// the only lookup path is by (organizer, key).
func nullableFingerprint(key, print string) any {
	if key == "" {
		return nil
	}
	return print
}

// beforeIdempotentInsert is a test-only barrier, nil in production.
//
// It exists because the race this ticket closes is otherwise UNTESTABLE at the
// only tier that can host it. Two goroutines released together still have to hit
// a window measured in microseconds, so a test that merely starts them in
// parallel passes just as happily against a naive check-then-insert — verified,
// not assumed: with the guard replaced by a Go-side check-then-insert and the
// unique index dropped, a 30-iteration parallel test stayed green.
//
// Making the interleaving deterministic is what turns "we hope this races" into
// "this races". The hook fires after any pre-insert read and immediately before
// the insert, which is exactly the gap a check-then-insert leaves open.
var beforeIdempotentInsert func()

func idempotencyBarrier() {
	if beforeIdempotentInsert != nil {
		beforeIdempotentInsert()
	}
}

// replayLookup finds the row a (organizer, key) pair already created, and
// reports whether the stored fingerprint agrees with this request's.
//
// found=false means no row yet — the caller inserts. found=true with
// match=false means the key was reused for different terms, which is
// ErrIdempotencyConflict rather than a replay.
//
// The caller passes the table name as a constant from its own code, never from
// input: these are three fixed call sites, not a generic query builder.
func replayLookup(ctx context.Context, q rowQuerier, table string, org uuid.UUID, key, want string) (id uuid.UUID, found, match bool, err error) {
	if key == "" {
		return uuid.Nil, false, false, nil
	}
	var got sql.NullString
	err = q.QueryRowContext(ctx,
		`SELECT id, request_fingerprint FROM `+table+` WHERE organizer_id=$1 AND idempotency_key=$2`,
		org, key).Scan(&id, &got)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, false, false, nil
	}
	if err != nil {
		return uuid.Nil, false, false, err
	}
	return id, true, got.Valid && got.String == want, nil
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
