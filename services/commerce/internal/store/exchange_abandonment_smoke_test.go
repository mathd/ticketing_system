//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-498. The settled-unswitched exchange listing and the store layer of the abandonment
// command, against real PostgreSQL: every claim here is about a SQL predicate, a lock or a
// constraint.
//
// The fixture writes rows directly. Same-ID rows in two organizers, a replacement reservation
// in another organizer and the other inconsistent shapes are schema-representable but outside
// normal writer behaviour (plan v2, Reachability), and only direct writes can express them.

// abandonFixture is one settled, unswitched exchange with DISTINCT source and replacement
// lines, as the exchange handler writes them: the replacement reservation holds the target
// hold, and the replacement order's checkout key is `exchange:<id>`.
//
// The amounts carry a retained fee of 200 so that face and gross differ on both lines:
//
//	source       2 x 1500 = 3000 face, 3200 gross
//	target       2 x unit  = 2*unit face, 2*unit + 200 gross
//	delta        target face - source face
type abandonFixture struct {
	Org, ExchangeID                                      uuid.UUID
	SourceOrder, SourceReservation, SourceHold           uuid.UUID
	ReplacementOrder, ReplacementReservation, TargetHold uuid.UUID
	Buyer                                                uuid.UUID
	Quantity                                             int32
	SourceTotal, SourceGross                             int64
	TargetTotal, TargetGross, Delta                      int64
	SettledAt                                            time.Time
}

type abandonSeedOpts struct {
	// Org and ExchangeID default to fresh values. Passing the same ExchangeID under two
	// organizers builds the same-ID pair the scope tests need, each with its own parents.
	Org, ExchangeID uuid.UUID
	// TargetUnit defaults to 2000: an upgrade of +1000.
	TargetUnit int64
	// SettledAt defaults to one hour ago.
	SettledAt time.Time
	// Unsettled leaves settlement and the replacement rows out.
	Unsettled bool
	// GrossFacts also writes the two original gross facts (their ids derive from the exchange
	// id alone, so a same-ID pair can carry them on one side only).
	GrossFacts bool
}

const (
	fixtureQuantity    = int32(2)
	fixtureSourceUnit  = int64(1500)
	fixtureRetainedFee = int64(200)
)

func seedAbandonable(t *testing.T, db *sql.DB, ctx context.Context, o abandonSeedOpts) abandonFixture {
	t.Helper()
	f := abandonFixture{
		Org: o.Org, ExchangeID: o.ExchangeID,
		SourceOrder: uuid.New(), SourceReservation: uuid.New(), SourceHold: uuid.New(),
		ReplacementOrder: uuid.New(), ReplacementReservation: uuid.New(), TargetHold: uuid.New(),
		Buyer: uuid.New(), Quantity: fixtureQuantity, SettledAt: o.SettledAt,
	}
	if f.Org == uuid.Nil {
		f.Org = uuid.New()
	}
	if f.ExchangeID == uuid.Nil {
		f.ExchangeID = uuid.New()
	}
	if f.SettledAt.IsZero() {
		f.SettledAt = time.Now().Add(-time.Hour)
	}
	f.SettledAt = f.SettledAt.UTC().Truncate(time.Microsecond)
	unit := o.TargetUnit
	if unit == 0 {
		unit = 2000
	}
	f.SourceTotal = fixtureSourceUnit * int64(f.Quantity)
	f.SourceGross = f.SourceTotal + fixtureRetainedFee
	f.TargetTotal = unit * int64(f.Quantity)
	f.TargetGross = f.TargetTotal + fixtureRetainedFee
	f.Delta = f.TargetTotal - f.SourceTotal

	slot, ticketType, targetSlot, targetType := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(t, db, ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'EUR','completed')`,
		f.SourceReservation, f.Org, f.SourceHold, slot, ticketType, f.Buyer, f.Quantity, fixtureSourceUnit, f.SourceGross, f.SourceTotal)
	mustExec(t, db, ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref)
		VALUES($1,$2,'completed',$3,'fingerprint',$4)`,
		f.SourceOrder, f.SourceReservation, "src-"+f.SourceOrder.String(), uuid.New())

	var replacementOrder, settledAt any
	if !o.Unsettled {
		mustExec(t, db, ctx, `
			INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'EUR','completed')`,
			f.ReplacementReservation, f.Org, f.TargetHold, targetSlot, targetType, f.Buyer, f.Quantity, unit, f.TargetGross, f.TargetTotal)
		mustExec(t, db, ctx, `
			INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
			VALUES($1,$2,'completed',$3,'exchange')`,
			f.ReplacementOrder, f.ReplacementReservation, "exchange:"+f.ExchangeID.String())
		replacementOrder, settledAt = f.ReplacementOrder, f.SettledAt
	}
	mustExec(t, db, ctx, `
		INSERT INTO order_exchanges(organizer_id,id,source_order_id,replacement_order_id,target_ticket_type_id,
		                            idempotency_key,request_fingerprint,quantity,source_total,source_gross_total,
		                            target_total,delta_amount,target_unit_amount,target_hold_id,
		                            replacement_reservation_id,target_slot_id,basis_at,
		                            currency,actor,reason,created_at,settled_at)
		VALUES($1,$2,$3,$4,$5,$6,'fp',$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,
		       'EUR','ops@example.test','buyer asked for another date',$16,$17)`,
		f.Org, f.ExchangeID, f.SourceOrder, replacementOrder, targetType,
		"xkey-"+f.ExchangeID.String()+"-"+f.Org.String(), f.Quantity, f.SourceTotal, f.SourceGross,
		f.TargetTotal, f.Delta, unit, f.TargetHold, f.ReplacementReservation, targetSlot,
		f.SettledAt.Add(-time.Minute), settledAt)
	if o.GrossFacts {
		for _, leg := range []struct {
			id     uuid.UUID
			typ    string
			amount int64
		}{
			{ExchangeReversedFactID(f.ExchangeID), "order.exchange.reversed", f.SourceGross},
			{ExchangeSoldFactID(f.ExchangeID), "order.exchange.sold", f.TargetGross},
		} {
			mustExec(t, db, ctx, `
				INSERT INTO order_facts(fact_id,order_id,organizer_id,buyer_id,fact_type,amount,currency,occurred_at)
				VALUES($1,$2,$3,$4,$5,$6,'EUR',$7)`,
				leg.id, f.SourceOrder, f.Org, f.Buyer, leg.typ, leg.amount, f.SettledAt.Add(-time.Minute))
		}
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM order_exchanges WHERE organizer_id=$1 AND id=$2`, f.Org, f.ExchangeID)
		for _, order := range []uuid.UUID{f.SourceOrder, f.ReplacementOrder} {
			_, _ = db.Exec(`DELETE FROM order_refunds WHERE order_id=$1`, order)
			_, _ = db.Exec(`DELETE FROM order_facts WHERE order_id=$1`, order)
			_, _ = db.Exec(`DELETE FROM completion_outbox WHERE order_id=$1`, order)
			_, _ = db.Exec(`DELETE FROM orders WHERE id=$1`, order)
		}
		_, _ = db.Exec(`DELETE FROM reservations WHERE id = ANY($1)`, []uuid.UUID{f.SourceReservation, f.ReplacementReservation})
	})
	return f
}

func exchangeIDs(rows []SettledExchange) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func mustExec(t *testing.T, db *sql.DB, ctx context.Context, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func listedIDs(t *testing.T, db *sql.DB, ctx context.Context, org uuid.UUID) []SettledExchange {
	t.Helper()
	rows, err := ListSettledUnswitchedExchanges(ctx, db, org)
	if err != nil {
		t.Fatalf("list settled unswitched exchanges: %v", err)
	}
	return rows
}

// Two organizers hold an exchange with the SAME id, each over its own source and replacement
// lines. Each organizer's listing returns its own row with its own parents, and an organizer
// with no exchange lists nothing.
func TestSettledUnswitchedListingIsOrganizerScoped(t *testing.T) {
	db, ctx := outboxDB(t)
	shared := uuid.New()
	a := seedAbandonable(t, db, ctx, abandonSeedOpts{ExchangeID: shared})
	b := seedAbandonable(t, db, ctx, abandonSeedOpts{ExchangeID: shared, TargetUnit: 1000})

	for _, f := range []abandonFixture{a, b} {
		got := listedIDs(t, db, ctx, f.Org)
		if len(got) != 1 {
			t.Fatalf("organizer %s lists %d rows, want exactly its own one", f.Org, len(got))
		}
		r := got[0]
		if r.OrganizerID != f.Org || r.ID != shared || r.SourceOrderID != f.SourceOrder ||
			r.ReplacementOrderID != f.ReplacementOrder || r.TargetHoldID != f.TargetHold ||
			r.Quantity != f.Quantity || r.DeltaAmount != f.Delta || r.Currency != "EUR" ||
			!r.SettledAt.Equal(f.SettledAt) {
			t.Fatalf("organizer %s listed exchange %s over source %s and replacement %s (delta %d, settled %s), "+
				"want its own exchange over source %s and replacement %s (delta %d, settled %s)",
				f.Org, r.ID, r.SourceOrderID, r.ReplacementOrderID, r.DeltaAmount, r.SettledAt,
				f.SourceOrder, f.ReplacementOrder, f.Delta, f.SettledAt)
		}
	}
	if got := listedIDs(t, db, ctx, uuid.New()); len(got) != 0 {
		t.Fatalf("an organizer with no exchange listed %d rows", len(got))
	}
}

// A candidate is settled and unswitched. An unsettled exchange and a switched one (the only
// way to return the source capacity, by 0011's CHECK) are excluded, each next to a candidate
// of the same organizer so that the exclusion is the predicate's and not the scope's.
func TestSettledUnswitchedListingExcludesUnsettledAndSwitchedRows(t *testing.T) {
	db, ctx := outboxDB(t)
	org := uuid.New()
	candidate := seedAbandonable(t, db, ctx, abandonSeedOpts{Org: org})
	seedAbandonable(t, db, ctx, abandonSeedOpts{Org: org, Unsettled: true})
	switched := seedAbandonable(t, db, ctx, abandonSeedOpts{Org: org})
	if err := MarkExchangeTicketsSwitched(ctx, db, org, switched.ExchangeID); err != nil {
		t.Fatal(err)
	}
	returned := seedAbandonable(t, db, ctx, abandonSeedOpts{Org: org})
	if err := MarkExchangeTicketsSwitched(ctx, db, org, returned.ExchangeID); err != nil {
		t.Fatal(err)
	}
	if err := MarkExchangeCapacityReturned(ctx, db, org, returned.ExchangeID); err != nil {
		t.Fatal(err)
	}

	got := listedIDs(t, db, ctx, org)
	if len(got) != 1 || got[0].ID != candidate.ExchangeID {
		t.Fatalf("listed %v, want only the settled unswitched candidate %s", exchangeIDs(got), candidate.ExchangeID)
	}
}

// inconsistencyCase breaks exactly one part of the candidate definition on an otherwise valid
// fixture. The same table drives the listing (the row is left out) and the load (the command
// refuses), so the two read the same eligibility model.
type inconsistencyCase struct {
	name  string
	apply func(t *testing.T, db *sql.DB, ctx context.Context, f abandonFixture)
}

var inconsistencyCases = []inconsistencyCase{
	{"source reservation in another organizer", func(t *testing.T, db *sql.DB, ctx context.Context, f abandonFixture) {
		mustExec(t, db, ctx, `UPDATE reservations SET organizer_id=$2 WHERE id=$1`, f.SourceReservation, uuid.New())
	}},
	{"replacement reservation in another organizer", func(t *testing.T, db *sql.DB, ctx context.Context, f abandonFixture) {
		mustExec(t, db, ctx, `UPDATE reservations SET organizer_id=$2 WHERE id=$1`, f.ReplacementReservation, uuid.New())
	}},
	{"replacement order on another reservation", func(t *testing.T, db *sql.DB, ctx context.Context, f abandonFixture) {
		// The other reservation takes over the target hold, so only the reservation identity
		// differs: same organizer, same hold, same quantity, completed.
		other := uuid.New()
		mustExec(t, db, ctx, `UPDATE reservations SET hold_id=$2 WHERE id=$1`, f.ReplacementReservation, uuid.New())
		mustExec(t, db, ctx, `
			INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
			SELECT $2,organizer_id,$3,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status
			FROM reservations WHERE id=$1`, f.ReplacementReservation, other, f.TargetHold)
		mustExec(t, db, ctx, `UPDATE orders SET reservation_id=$2 WHERE id=$1`, f.ReplacementOrder, other)
		t.Cleanup(func() {
			_, _ = db.Exec(`UPDATE orders SET reservation_id=$2 WHERE id=$1`, f.ReplacementOrder, f.ReplacementReservation)
			_, _ = db.Exec(`DELETE FROM reservations WHERE id=$1`, other)
		})
	}},
	{"replacement reservation on another hold", func(t *testing.T, db *sql.DB, ctx context.Context, f abandonFixture) {
		mustExec(t, db, ctx, `UPDATE reservations SET hold_id=$2 WHERE id=$1`, f.ReplacementReservation, uuid.New())
	}},
	{"replacement is the source line", func(t *testing.T, db *sql.DB, ctx context.Context, f abandonFixture) {
		// The shape the reversal sweep's own fixture uses: one reservation on both sides. Every
		// other predicate holds; only the source and target holds coincide.
		mustExec(t, db, ctx, `
			UPDATE order_exchanges SET replacement_order_id=$3, replacement_reservation_id=$4, target_hold_id=$5
			WHERE organizer_id=$1 AND id=$2`, f.Org, f.ExchangeID, f.SourceOrder, f.SourceReservation, f.SourceHold)
	}},
	{"source quantity differs", func(t *testing.T, db *sql.DB, ctx context.Context, f abandonFixture) {
		mustExec(t, db, ctx, `UPDATE reservations SET quantity=quantity+1 WHERE id=$1`, f.SourceReservation)
	}},
	{"replacement quantity differs", func(t *testing.T, db *sql.DB, ctx context.Context, f abandonFixture) {
		mustExec(t, db, ctx, `UPDATE reservations SET quantity=quantity+1 WHERE id=$1`, f.ReplacementReservation)
	}},
	{"replacement order not completed", func(t *testing.T, db *sql.DB, ctx context.Context, f abandonFixture) {
		mustExec(t, db, ctx, `UPDATE orders SET status='refunded' WHERE id=$1`, f.ReplacementOrder)
	}},
	{"replacement reservation not completed", func(t *testing.T, db *sql.DB, ctx context.Context, f abandonFixture) {
		mustExec(t, db, ctx, `UPDATE reservations SET status='failed' WHERE id=$1`, f.ReplacementReservation)
	}},
}

// Each inconsistency removes the row from the listing, and the listing still shows a valid
// candidate of the same organizer next to it.
func TestSettledUnswitchedListingRequiresAConsistentReplacement(t *testing.T) {
	db, ctx := outboxDB(t)
	for _, c := range inconsistencyCases {
		t.Run(c.name, func(t *testing.T) {
			org := uuid.New()
			valid := seedAbandonable(t, db, ctx, abandonSeedOpts{Org: org})
			broken := seedAbandonable(t, db, ctx, abandonSeedOpts{Org: org})
			c.apply(t, db, ctx, broken)
			got := listedIDs(t, db, ctx, org)
			if len(got) != 1 || got[0].ID != valid.ExchangeID {
				t.Fatalf("listed %v, want only the consistent candidate %s", exchangeIDs(got), valid.ExchangeID)
			}
		})
	}
}

// Oldest settlement first, and the exchange id breaks a tie, so two runs over the same state
// print the same order. The rows are inserted against that order.
func TestSettledUnswitchedListingOrdersBySettlementThenID(t *testing.T) {
	db, ctx := outboxDB(t)
	org := uuid.New()
	early := time.Now().Add(-3 * time.Hour)
	late := time.Now().Add(-1 * time.Hour)
	tie := []uuid.UUID{uuid.New(), uuid.New()}
	sort.Slice(tie, func(i, j int) bool { return tie[i].String() < tie[j].String() })

	lateRow := seedAbandonable(t, db, ctx, abandonSeedOpts{Org: org, SettledAt: late})
	seedAbandonable(t, db, ctx, abandonSeedOpts{Org: org, ExchangeID: tie[1], SettledAt: early})
	seedAbandonable(t, db, ctx, abandonSeedOpts{Org: org, ExchangeID: tie[0], SettledAt: early})

	got := listedIDs(t, db, ctx, org)
	want := []uuid.UUID{tie[0], tie[1], lateRow.ExchangeID}
	if len(got) != len(want) {
		t.Fatalf("listed %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("row %d is %s, want %s: order by settled_at, then id", i, got[i].ID, want[i])
		}
	}
}

var _ = errors.Is
