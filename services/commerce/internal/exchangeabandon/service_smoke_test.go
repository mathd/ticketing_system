//go:build smoke

package exchangeabandon

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/accessadmission"
	"ticketing/services/commerce/internal/store"
)

// TKT-498 (plan v2 Build order 3-6). The abandonment service against a real Commerce database
// and the strict fakes in world_smoke_test.go.

func abandon(t *testing.T, db *sql.DB, ctx context.Context, w *world) (Result, error) {
	t.Helper()
	return w.service(db).Abandon(ctx, w.f.Org, w.f.Exchange, w.f.attribution())
}

func mustAbandon(t *testing.T, db *sql.DB, ctx context.Context, w *world) Result {
	t.Helper()
	res, err := abandon(t, db, ctx, w)
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	return res
}

func sessionUser(t *testing.T, db *sql.DB, ctx context.Context) string {
	t.Helper()
	var u string
	if err := db.QueryRowContext(ctx, `SELECT session_user`).Scan(&u); err != nil {
		t.Fatal(err)
	}
	return u
}

// assertTerminal checks the terminal row and the one compensating fact in both journals.
func assertTerminal(t *testing.T, db *sql.DB, ctx context.Context, w *world) row {
	t.Helper()
	f := w.f
	r := readRow(t, db, ctx, f)
	if !r.StartedAt.Valid || r.Operator.String != operatorName || r.Reason.String != operatorReason ||
		r.SessionUser.String != sessionUser(t, db, ctx) || !r.ReturnedAt.Valid || !r.AbandonedAt.Valid || r.FailedRef.Valid {
		t.Fatalf("terminal row = %+v", r)
	}
	if n := abandonedFacts(t, db, ctx, f); n != 1 {
		t.Fatalf("%d local order.exchange.abandoned facts, want 1", n)
	}
	w.mu.Lock()
	journalled, ok := w.journal[f.factID()]
	entries := len(w.journal)
	receipts := w.receipts[f.receipt()]
	w.mu.Unlock()
	if !ok || entries != 1 {
		t.Fatalf("payments journal holds %d entries, want the one compensating fact %s", entries, f.factID())
	}
	if receipts != f.Quantity {
		t.Fatalf("inventory holds receipt %s for %d, want the full target quantity %d", f.receipt(), receipts, f.Quantity)
	}
	if !journalled.OccurredAt.Equal(r.ReturnedAt.Time) {
		t.Fatalf("fact occurred_at %s, want the recorded target return %s", journalled.OccurredAt, r.ReturnedAt.Time)
	}
	// The fact restores the source gross and removes the target gross, retained fee included.
	p := journalled.Payload()
	if journalled.Amount != f.TargetGross || p["source_gross"] != strconv.FormatInt(f.SourceGross, 10) ||
		p["delta_amount"] != strconv.FormatInt(f.Delta, 10) || p["order_id"] != f.SourceOrder.String() ||
		p["exchange_id"] != f.Exchange.String() || journalled.BuyerID != f.Buyer || journalled.Currency != "EUR" {
		t.Fatalf("journalled fact %+v payload %v, want target gross %d, source gross %d, delta %d",
			journalled, p, f.TargetGross, f.SourceGross, f.Delta)
	}
	var localAmount int64
	var localOccurred sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT amount, occurred_at FROM order_facts WHERE fact_id=$1 AND organizer_id=$2`,
		f.factID(), f.Org).Scan(&localAmount, &localOccurred); err != nil || localAmount != f.TargetGross ||
		!localOccurred.Time.Equal(r.ReturnedAt.Time) {
		t.Fatalf("local fact amount %d at %v (err %v), want %d at %s", localAmount, localOccurred, err, f.TargetGross, r.ReturnedAt.Time)
	}
	return r
}

// ---------------------------------------------------------------------------------------------
// The three branches.

// COS1 at this tier. An upgrade proves the source admitted, then the replacement unissued, reads
// the exact capture, refunds the exact delta once under the derived key, returns the whole target
// claim once, and journals one fact that carries the refund's payments fact.
func TestAbandonAnUpgradeRefundsTheDeltaReturnsTheTargetAndJournals(t *testing.T) {
	db, ctx := abandonDB(t)
	w := newWorld(t, seed(t, db, ctx, upgradeUnit))
	res := mustAbandon(t, db, ctx, w)
	if res.Replay {
		t.Fatal("a first abandonment reported replay=true")
	}
	want := []string{"access:source", "access:replacement", "payments:operation", "payments:abandonment-leg",
		"payments:refund", "inventory:return", "payments:fact"}
	w.mu.Lock()
	got := append([]string(nil), w.calls...)
	leg := w.legs[w.f.receipt().String()]
	movements := w.movements
	w.mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	}
	if movements != 1 || leg == nil || !leg.completed || leg.amount != w.f.Delta {
		t.Fatalf("payments moved %d refund(s), leg %+v; want one completed leg for %d", movements, leg, w.f.Delta)
	}
	r := assertTerminal(t, db, ctx, w)
	if r.RefundFact.UUID != leg.factID || r.WrittenOff.Valid {
		t.Fatalf("row refund fact %v written off %v, want payments' fact %s and no write-off", r.RefundFact, r.WrittenOff, leg.factID)
	}
	w.mu.Lock()
	p := w.journal[w.f.factID()].Payload()
	w.mu.Unlock()
	if p["refund_fact_id"] != leg.factID.String() || p["written_off_amount"] != "" {
		t.Fatalf("payload = %v, want refund_fact_id %s and no write-off", p, leg.factID)
	}
	if res.Exchange.AbandonedAt.IsZero() || !res.Exchange.AbandonedAt.Equal(r.AbandonedAt.Time) {
		t.Fatalf("result exchange abandoned at %s, row %s", res.Exchange.AbandonedAt, r.AbandonedAt.Time)
	}
}

// The downgrade and even branches run end to end in the running stack
// (smoke/exchange_abandonment_test.go), which asserts their payments reads, write-off and journal
// payload. This tier keeps the upgrade's call order and the refusal and retry cases.

// COS4. An identical command after completion answers the original record with replay=true and
// makes no Access, payments or inventory call. Times and row counts do not move.
func TestATerminalAbandonmentReplaysWithZeroRemoteCalls(t *testing.T) {
	for _, unit := range []int64{upgradeUnit, downgradeUnit, evenUnit} {
		t.Run(strconv.FormatInt(unit, 10), func(t *testing.T) {
			db, ctx := abandonDB(t)
			w := newWorld(t, seed(t, db, ctx, unit))
			mustAbandon(t, db, ctx, w)
			before := readRow(t, db, ctx, w.f)
			facts := countRows(t, db, ctx, `SELECT count(*) FROM order_facts WHERE organizer_id=$1`, w.f.Org)
			w.resetCalls()

			// The trimmed pair is the identity: surrounding whitespace is the same operator.
			attribution, err := store.NewAbandonmentAttribution("  "+operatorName+"\t", "\n"+operatorReason+" ")
			if err != nil {
				t.Fatal(err)
			}
			res, err := w.service(db).Abandon(ctx, w.f.Org, w.f.Exchange, attribution)
			if err != nil || !res.Replay {
				t.Fatalf("replay = %+v, %v; want replay=true", res.Replay, err)
			}
			if n := w.total(); n != 0 {
				t.Fatalf("a terminal replay made %d remote call(s): %v", n, w.calls)
			}
			if after := readRow(t, db, ctx, w.f); after != before {
				t.Fatalf("a replay changed the row: %+v -> %+v", before, after)
			}
			if n := countRows(t, db, ctx, `SELECT count(*) FROM order_facts WHERE organizer_id=$1`, w.f.Org); n != facts {
				t.Fatalf("a replay changed the local facts: %d -> %d", facts, n)
			}
			if !res.Exchange.AbandonedAt.Equal(before.AbandonedAt.Time) || !res.Exchange.AbandonmentStartedAt.Equal(before.StartedAt.Time) {
				t.Fatalf("replay answered times %s/%s, want the originals %s/%s", res.Exchange.AbandonmentStartedAt,
					res.Exchange.AbandonedAt, before.StartedAt.Time, before.AbandonedAt.Time)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// Access: each predicate alone, with every earlier one satisfied.

func TestAccessRefusalsStopBeforeAnyReservationOrMoney(t *testing.T) {
	type setup func(w *world)
	source := func(a accessadmission.Admission) setup {
		return func(w *world) { w.admission[w.f.SourceOrder] = a }
	}
	replacement := func(a accessadmission.Admission) setup {
		return func(w *world) { w.admission[w.f.ReplacementOrder] = a }
	}
	for _, tc := range []struct {
		name            string
		setup           setup
		want            error
		replacementRead bool
	}{
		{"source unavailable", func(w *world) { w.accessErr[w.f.SourceOrder] = accessadmission.ErrUnavailable }, ErrUnavailable, false},
		{"source not admitted", source(accessadmission.Admission{Admitted: false, IssuedCount: quantity}), ErrSourceNotAdmitted, false},
		{"source partly issued", source(accessadmission.Admission{Admitted: true, IssuedCount: quantity - 1}), ErrSourceNotAdmitted, false},
		{"source over issued", source(accessadmission.Admission{Admitted: true, IssuedCount: quantity + 1}), ErrSourceNotAdmitted, false},
		{"source unissued", source(accessadmission.Admission{Admitted: true, IssuedCount: 0}), ErrSourceNotAdmitted, false},
		{"replacement unavailable", func(w *world) { w.accessErr[w.f.ReplacementOrder] = accessadmission.ErrUnavailable }, ErrUnavailable, true},
		{"replacement issued", replacement(accessadmission.Admission{Admitted: false, IssuedCount: 1}), ErrReplacementIssued, true},
		{"replacement issued and admitted", replacement(accessadmission.Admission{Admitted: true, IssuedCount: quantity}), ErrReplacementIssued, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := abandonDB(t)
			w := newWorld(t, seed(t, db, ctx, upgradeUnit))
			tc.setup(w)
			_, err := abandon(t, db, ctx, w)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if r := readRow(t, db, ctx, w.f); r.StartedAt.Valid {
				t.Fatal("an Access refusal reserved the abandonment")
			}
			if w.count("access:source") != 1 {
				t.Fatalf("source read %d time(s), want 1", w.count("access:source"))
			}
			if got := w.count("access:replacement") == 1; got != tc.replacementRead {
				t.Fatalf("replacement read = %t, want %t: the replacement is read only after the source passes", got, tc.replacementRead)
			}
			if n := w.total() - w.count("access:source") - w.count("access:replacement"); n != 0 {
				t.Fatalf("an Access refusal made %d payments or inventory call(s): %v", n, w.calls)
			}
		})
	}
}

// A missing original gross fact stops the command before any remote call.
func TestAMissingGrossFactStopsBeforeAnyRemoteCall(t *testing.T) {
	db, ctx := abandonDB(t)
	f := seed(t, db, ctx, upgradeUnit)
	if _, err := db.ExecContext(ctx, `DELETE FROM order_facts WHERE fact_id=$1`, store.ExchangeSoldFactID(f.Exchange)); err != nil {
		t.Fatal(err)
	}
	w := newWorld(t, f)
	if _, err := abandon(t, db, ctx, w); !errors.Is(err, store.ErrExchangeGrossFactsInconsistent) {
		t.Fatalf("err = %v, want ErrExchangeGrossFactsInconsistent", err)
	}
	if n := w.total(); n != 0 || readRow(t, db, ctx, f).StartedAt.Valid {
		t.Fatalf("a missing gross fact made %d call(s) or reserved", n)
	}
}

// ---------------------------------------------------------------------------------------------
// Original money evidence, each mismatch alone.

func TestDeltaEvidenceThatDisagreesStopsBeforeTheReservation(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	str := func(v string) *string { return &v }
	for _, tc := range []struct {
		name  string
		unit  int64
		setup func(w *world)
	}{
		{"upgrade capture under another key", upgradeUnit, func(w *world) { w.captureKey = "exchange-charge:" + uuid.NewString() }},
		{"upgrade capture unresolved", upgradeUnit, func(w *world) { w.capture.Resolved = false }},
		{"upgrade capture without captured amount", upgradeUnit, func(w *world) { w.capture.CapturedAmount = nil }},
		{"upgrade capture of another amount", upgradeUnit, func(w *world) { w.capture.CapturedAmount = i64(w.f.Delta + 1) }},
		{"upgrade capture in another currency", upgradeUnit, func(w *world) { w.capture.Currency = str("USD") }},
		{"upgrade confirmation of another amount", upgradeUnit, func(w *world) {
			w.capture.ConfirmedAmount, w.capture.ConfirmedCurrency = i64(w.f.Delta-1), str("EUR")
		}},
		{"upgrade confirmation in another currency", upgradeUnit, func(w *world) {
			w.capture.ConfirmedAmount, w.capture.ConfirmedCurrency = i64(w.f.Delta), str("USD")
		}},
		{"upgrade partial confirmation", upgradeUnit, func(w *world) { w.capture.ConfirmedAmount = i64(w.f.Delta) }},
		{"downgrade leg missing", downgradeUnit, func(w *world) { w.downgrade = LegEvidence{} }},
		{"downgrade leg incomplete", downgradeUnit, func(w *world) { w.downgrade.Completed = false }},
		{"downgrade leg of another amount", downgradeUnit, func(w *world) { w.downgrade.Amount = -w.f.Delta + 1 }},
		{"downgrade leg in another currency", downgradeUnit, func(w *world) { w.downgrade.Currency = "USD" }},
		{"downgrade partial confirmation", downgradeUnit, func(w *world) { w.downgrade.ConfirmedCurrency = str("EUR") }},
		{"downgrade confirmation of another amount", downgradeUnit, func(w *world) {
			w.downgrade.ConfirmedAmount, w.downgrade.ConfirmedCurrency = i64(-w.f.Delta+1), str("EUR")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := abandonDB(t)
			w := newWorld(t, seed(t, db, ctx, tc.unit))
			tc.setup(w)
			_, err := abandon(t, db, ctx, w)
			if !errors.Is(err, ErrDeltaEvidence) {
				t.Fatalf("err = %v, want ErrDeltaEvidence", err)
			}
			if readRow(t, db, ctx, w.f).StartedAt.Valid {
				t.Fatal("disagreeing evidence reserved the abandonment")
			}
			if w.count("payments:refund") != 0 || w.count("inventory:return") != 0 || w.count("payments:fact") != 0 {
				t.Fatalf("calls = %v", w.calls)
			}
		})
	}
}

// Matching provider confirmations are accepted on both branches.
func TestMatchingConfirmationsAreAccepted(t *testing.T) {
	for _, unit := range []int64{upgradeUnit, downgradeUnit} {
		db, ctx := abandonDB(t)
		w := newWorld(t, seed(t, db, ctx, unit))
		amount, currency := w.f.Delta, "EUR"
		if unit == upgradeUnit {
			w.capture.ConfirmedAmount, w.capture.ConfirmedCurrency = &amount, &currency
		} else {
			amount = -amount
			w.downgrade.ConfirmedAmount, w.downgrade.ConfirmedCurrency = &amount, &currency
		}
		mustAbandon(t, db, ctx, w)
		assertTerminal(t, db, ctx, w)
	}
}

// ---------------------------------------------------------------------------------------------
// The upgrade refund after the reservation.

// A derived leg payments already holds must match before the POST replays it.
func TestAnAbandonmentLegOfAnotherAmountStopsBeforeThePost(t *testing.T) {
	for _, tc := range []struct {
		name string
		leg  leg
	}{
		{"another amount", leg{amount: 1, currency: "EUR"}},
		{"another currency", leg{currency: "USD"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := abandonDB(t)
			w := newWorld(t, seed(t, db, ctx, upgradeUnit))
			l := tc.leg
			if l.amount == 0 {
				l.amount = w.f.Delta
			}
			w.legs[w.f.receipt().String()] = &l
			_, err := abandon(t, db, ctx, w)
			if !errors.Is(err, ErrDeltaEvidence) {
				t.Fatalf("err = %v, want ErrDeltaEvidence", err)
			}
			r := readRow(t, db, ctx, w.f)
			if !r.StartedAt.Valid || r.RefundFact.Valid || w.count("payments:refund") != 0 || w.count("inventory:return") != 0 {
				t.Fatalf("row %+v calls %v: want a started row with no refund and no POST", r, w.calls)
			}
		})
	}
}

// Conclusive refusals, unavailability and an answer for other money all leave the row started
// with no refund progress, no capacity return and no terminal marker.
func TestRefundFailuresLeaveTheAbandonmentPending(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(w *world)
		want  error
	}{
		{"exhausted ceiling", func(w *world) { w.otherLegs = 1 }, ErrRefused},
		{"unavailable", func(w *world) { w.refundBefore = always(ErrUnavailable) }, ErrUnavailable},
		{"answer for another amount", func(w *world) {
			w.refundAnswer = func(a RefundAnswer) RefundAnswer { a.Amount++; return a }
		}, ErrUnavailable},
		{"answer in another currency", func(w *world) {
			w.refundAnswer = func(a RefundAnswer) RefundAnswer { a.Currency = "USD"; return a }
		}, ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := abandonDB(t)
			w := newWorld(t, seed(t, db, ctx, upgradeUnit))
			tc.setup(w)
			if _, err := abandon(t, db, ctx, w); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			r := readRow(t, db, ctx, w.f)
			if !r.StartedAt.Valid || r.RefundFact.Valid || r.FailedRef.Valid || r.ReturnedAt.Valid || r.AbandonedAt.Valid {
				t.Fatalf("row = %+v, want started with no progress", r)
			}
			if w.count("inventory:return") != 0 || w.count("payments:fact") != 0 {
				t.Fatalf("calls = %v", w.calls)
			}
		})
	}
}

// D12. The typed provider failure is recorded once and stops. The next same-pair run reports it
// with no remote call at all.
func TestARecordedProviderFailureStopsWithoutAnyLaterCall(t *testing.T) {
	db, ctx := abandonDB(t)
	w := newWorld(t, seed(t, db, ctx, upgradeUnit))
	w.refundAfterBind = always(&ProviderRefundFailedError{ProviderRef: "re_failed_upgrade"})
	res, err := abandon(t, db, ctx, w)
	if !errors.Is(err, store.ErrAbandonmentRefundFailed) || res.Exchange.AbandonmentRefundFailedRef != "re_failed_upgrade" {
		t.Fatalf("err = %v ref %q, want ErrAbandonmentRefundFailed with the provider reference", err, res.Exchange.AbandonmentRefundFailedRef)
	}
	r := readRow(t, db, ctx, w.f)
	if r.FailedRef.String != "re_failed_upgrade" || r.RefundFact.Valid || r.ReturnedAt.Valid || r.AbandonedAt.Valid {
		t.Fatalf("row = %+v, want the failure recorded and nothing after it", r)
	}
	if w.count("inventory:return") != 0 || w.count("payments:fact") != 0 || abandonedFacts(t, db, ctx, w.f) != 0 {
		t.Fatalf("a failed refund went on: %v", w.calls)
	}
	w.resetCalls()
	if _, err := abandon(t, db, ctx, w); !errors.Is(err, store.ErrAbandonmentRefundFailed) {
		t.Fatalf("second run err = %v, want the recorded failure", err)
	}
	if n := w.total(); n != 0 {
		t.Fatalf("the second run made %d remote call(s) after a recorded provider failure: %v", n, w.calls)
	}
}

// P2-3. The first 422 is lost in transit; payments answers 422 again for the same key on the
// retry. The failure is recorded once, and the run after it makes no call.
func TestALostProviderFailureIsRecordedOnceOnTheRetry(t *testing.T) {
	db, ctx := abandonDB(t)
	w := newWorld(t, seed(t, db, ctx, upgradeUnit))
	failed := &ProviderRefundFailedError{ProviderRef: "re_lost_once"}
	w.refundAfterBind = func(n int) error {
		if n == 1 {
			return ErrUnavailable // the provider failed; its 422 never arrived
		}
		return failed
	}
	if _, err := abandon(t, db, ctx, w); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("first run err = %v, want ErrUnavailable", err)
	}
	if r := readRow(t, db, ctx, w.f); r.FailedRef.Valid || r.RefundFact.Valid {
		t.Fatalf("a lost answer recorded progress: %+v", r)
	}
	if _, err := abandon(t, db, ctx, w); !errors.Is(err, store.ErrAbandonmentRefundFailed) {
		t.Fatalf("retry err = %v, want the recorded failure", err)
	}
	if r := readRow(t, db, ctx, w.f); r.FailedRef.String != "re_lost_once" {
		t.Fatalf("failure ref = %v", r.FailedRef)
	}
	w.resetCalls()
	if _, err := abandon(t, db, ctx, w); !errors.Is(err, store.ErrAbandonmentRefundFailed) || w.total() != 0 {
		t.Fatalf("third run err = %v with %d call(s), want the recorded failure and none", err, w.total())
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.refundCalls != 2 || w.movements != 0 {
		t.Fatalf("payments got %d partial refund(s) and moved %d, want 2 and 0", w.refundCalls, w.movements)
	}
}

// ---------------------------------------------------------------------------------------------
// Inventory.

func TestInventoryAnswersThatDoNotReturnTheWholeTargetLeaveItPending(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(w *world)
		want  error
	}{
		{"wrong hold", func(w *world) {
			w.returnAnswer = func(c CapacityReturn) CapacityReturn { c.HoldID = w.f.SourceHold; return c }
		}, ErrCapacityReturnMismatch},
		{"wrong quantity", func(w *world) {
			w.returnAnswer = func(c CapacityReturn) CapacityReturn { c.Quantity = 1; return c }
		}, ErrCapacityReturnMismatch},
		{"capacity left unreturned", func(w *world) {
			w.returnAnswer = func(c CapacityReturn) CapacityReturn { c.Unreturned = 1; return c }
		}, ErrCapacityReturnMismatch},
		{"refused", func(w *world) { w.returnBefore = always(ErrRefused) }, ErrRefused},
		{"unavailable", func(w *world) { w.returnBefore = always(ErrUnavailable) }, ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := abandonDB(t)
			w := newWorld(t, seed(t, db, ctx, downgradeUnit))
			tc.setup(w)
			if _, err := abandon(t, db, ctx, w); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			r := readRow(t, db, ctx, w.f)
			if !r.StartedAt.Valid || r.ReturnedAt.Valid || r.AbandonedAt.Valid || r.WrittenOff.Valid {
				t.Fatalf("row = %+v, want started with no target progress", r)
			}
			if w.count("payments:fact") != 0 || abandonedFacts(t, db, ctx, w.f) != 0 {
				t.Fatalf("calls = %v", w.calls)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// The journal (D7, D13).

// A refused or unavailable journal leaves the returned target pending, and the same command
// resumes from the recorded target return once the journal accepts the fact. The
// interruption after a successful but lost append stays in
// TestAnInterruptedAbandonmentResumesFromItsRecordedProgress.
func TestJournalRefusalsLeaveTheReturnedTargetPending(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(w *world)
		want  error
	}{
		{"payments refuses the fact", func(w *world) { w.appendBefore = always(ErrRefused) }, ErrRefused},
		{"journal unavailable", func(w *world) { w.appendBefore = always(ErrUnavailable) }, ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := abandonDB(t)
			w := newWorld(t, seed(t, db, ctx, upgradeUnit))
			tc.setup(w)
			if _, err := abandon(t, db, ctx, w); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			r := readRow(t, db, ctx, w.f)
			if !r.ReturnedAt.Valid || r.AbandonedAt.Valid {
				t.Fatalf("row = %+v, want the target returned and no terminal marker", r)
			}
			w.mu.Lock()
			journalled := len(w.journal)
			w.mu.Unlock()
			if journalled != 0 || abandonedFacts(t, db, ctx, w.f) != 0 {
				t.Fatalf("journal holds %d, local facts %d; want none", journalled, abandonedFacts(t, db, ctx, w.f))
			}

			// The same command once the journal accepts the fact resumes from the recorded target
			// return: no new proof, refund or return, and the recorded instants stay.
			w.mu.Lock()
			w.appendBefore = nil
			w.mu.Unlock()
			w.resetCalls()
			if res := mustAbandon(t, db, ctx, w); res.Replay {
				t.Fatal("a resumed run reported replay=true")
			}
			for _, name := range []string{"access:source", "payments:refund", "inventory:return"} {
				if n := w.count(name); n != 0 {
					t.Fatalf("the resumed run repeated %s %d time(s): %v", name, n, w.calls)
				}
			}
			done := assertTerminal(t, db, ctx, w)
			if !done.StartedAt.Time.Equal(r.StartedAt.Time) || !done.ReturnedAt.Time.Equal(r.ReturnedAt.Time) {
				t.Fatalf("the resumed run moved the reservation or target-return time: %+v -> %+v", r, done)
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.movements != 1 || len(w.legs) != 1 || done.RefundFact.UUID != w.legs[w.f.receipt().String()].factID {
				t.Fatalf("payments moved %d refund(s) over %d leg(s); row fact %v", w.movements, len(w.legs), done.RefundFact)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// Operator ownership, retries and interruptions.

// The first trimmed pair owns the abandonment. Another operator, or another reason, is refused
// before any remote call: while pending, after a recorded failure, and after completion.
func TestTheFirstOperatorAndReasonPairOwnsTheAbandonment(t *testing.T) {
	others := []store.AbandonmentAttribution{
		{Operator: "Grace Operator", Reason: operatorReason},
		{Operator: operatorName, Reason: "another reason"},
	}
	refuseOthers := func(t *testing.T, db *sql.DB, ctx context.Context, w *world) {
		t.Helper()
		before := readRow(t, db, ctx, w.f)
		for _, a := range others {
			w.resetCalls()
			if _, err := w.service(db).Abandon(ctx, w.f.Org, w.f.Exchange, a); !errors.Is(err, store.ErrAbandonmentAttributionConflict) {
				t.Fatalf("pair %+v: err = %v, want ErrAbandonmentAttributionConflict", a, err)
			}
			if n := w.total(); n != 0 {
				t.Fatalf("pair %+v made %d remote call(s)", a, n)
			}
		}
		if after := readRow(t, db, ctx, w.f); after != before {
			t.Fatalf("another pair changed the row: %+v -> %+v", before, after)
		}
	}

	t.Run("pending", func(t *testing.T) {
		db, ctx := abandonDB(t)
		w := newWorld(t, seed(t, db, ctx, upgradeUnit))
		w.refundBefore = once(ErrUnavailable)
		if _, err := abandon(t, db, ctx, w); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
		refuseOthers(t, db, ctx, w)
		mustAbandon(t, db, ctx, w)
		assertTerminal(t, db, ctx, w)
	})
	t.Run("after a recorded failure", func(t *testing.T) {
		db, ctx := abandonDB(t)
		w := newWorld(t, seed(t, db, ctx, upgradeUnit))
		w.refundAfterBind = always(&ProviderRefundFailedError{ProviderRef: "re_owned"})
		if _, err := abandon(t, db, ctx, w); !errors.Is(err, store.ErrAbandonmentRefundFailed) {
			t.Fatalf("err = %v", err)
		}
		refuseOthers(t, db, ctx, w)
	})
	t.Run("after completion", func(t *testing.T) {
		db, ctx := abandonDB(t)
		w := newWorld(t, seed(t, db, ctx, evenUnit))
		mustAbandon(t, db, ctx, w)
		refuseOthers(t, db, ctx, w)
	})
}

// Two identical pairs race through the proof and the reservation. One reserves; both finish on
// the same durable effects: one refund movement, one receipt, one journal entry, one local fact.
func TestConcurrentIdenticalPairsProduceOneSetOfEffects(t *testing.T) {
	db, ctx := abandonDB(t)
	w := newWorld(t, seed(t, db, ctx, upgradeUnit))
	// Hold both commands at their replacement read until both are there, so both pass the proof
	// before either reserves.
	var arrived sync.WaitGroup
	arrived.Add(2)
	release := make(chan struct{})
	var holdOnce sync.Once
	w.accessHook = func(order uuid.UUID) {
		if order != w.f.ReplacementOrder {
			return
		}
		arrived.Done()
		holdOnce.Do(func() { go func() { arrived.Wait(); close(release) }() })
		<-release
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = w.service(db).Abandon(ctx, w.f.Org, w.f.Exchange, w.f.attribution())
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("command %d: %v", i, err)
		}
	}
	assertTerminal(t, db, ctx, w)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.movements != 1 || len(w.legs) != 1 || len(w.receipts) != 1 || len(w.journal) != 1 {
		t.Fatalf("effects: %d movement(s), %d leg(s), %d receipt(s), %d journal entr(ies); want one of each",
			w.movements, len(w.legs), len(w.receipts), len(w.journal))
	}
}

// Each pause point: the command stops after one durable step, and the same pair resumes from
// the recorded progress. The resumed run repeats no completed step: no second Access proof or
// reservation, no second payments movement, no second receipt, no second journal entry.
func TestAnInterruptedAbandonmentResumesFromItsRecordedProgress(t *testing.T) {
	for _, tc := range []struct {
		name string
		unit int64
		// interrupt fails exactly one step of the first run.
		interrupt func(w *world)
		// check runs on the first run's row.
		check func(t *testing.T, r row)
		// resumed lists calls the resumed run must NOT make.
		resumedNot []string
	}{
		{"after the reservation", upgradeUnit,
			func(w *world) {
				w.legErr = func(source, _ string) error {
					if source == w.f.chargeKey() {
						w.legErr = nil
						return ErrUnavailable
					}
					return nil
				}
			},
			func(t *testing.T, r row) {
				if !r.StartedAt.Valid || r.RefundFact.Valid {
					t.Fatalf("row = %+v, want only the reservation", r)
				}
			},
			[]string{"access:source", "access:replacement", "payments:operation"}},
		{"after the payments bind", upgradeUnit,
			func(w *world) { w.refundAfterBind = once(ErrUnavailable) },
			func(t *testing.T, r row) {
				if r.RefundFact.Valid {
					t.Fatalf("row = %+v, want no refund progress", r)
				}
			},
			[]string{"access:source", "payments:operation"}},
		{"after payments completed the refund, before Commerce recorded it", upgradeUnit,
			func(w *world) { w.refundAfterDone = once(ErrUnavailable) },
			func(t *testing.T, r row) {
				if r.RefundFact.Valid {
					t.Fatalf("row = %+v, want no refund progress", r)
				}
			},
			[]string{"access:source", "payments:operation"}},
		{"after Commerce recorded the refund", upgradeUnit,
			func(w *world) { w.returnBefore = once(ErrUnavailable) },
			func(t *testing.T, r row) {
				if !r.RefundFact.Valid || r.ReturnedAt.Valid {
					t.Fatalf("row = %+v, want the refund recorded and no target progress", r)
				}
			},
			[]string{"access:source", "payments:operation", "payments:abandonment-leg", "payments:refund"}},
		{"after inventory returned the target, before Commerce recorded it", downgradeUnit,
			func(w *world) { w.returnAfter = once(ErrUnavailable) },
			func(t *testing.T, r row) {
				if r.ReturnedAt.Valid {
					t.Fatalf("row = %+v, want no target progress", r)
				}
			},
			[]string{"access:source", "payments:downgrade-leg"}},
		{"after the journal appended, before the terminal Commerce commit", upgradeUnit,
			func(w *world) { w.appendAfter = once(ErrUnavailable) },
			func(t *testing.T, r row) {
				if !r.ReturnedAt.Valid || r.AbandonedAt.Valid {
					t.Fatalf("row = %+v, want the target returned and no terminal marker", r)
				}
			},
			[]string{"access:source", "payments:refund", "inventory:return"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := abandonDB(t)
			w := newWorld(t, seed(t, db, ctx, tc.unit))
			tc.interrupt(w)
			if _, err := abandon(t, db, ctx, w); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("first run err = %v, want ErrUnavailable", err)
			}
			first := readRow(t, db, ctx, w.f)
			tc.check(t, first)
			if abandonedFacts(t, db, ctx, w.f) != 0 {
				t.Fatal("an interrupted run wrote the local compensating fact")
			}
			w.resetCalls()
			res := mustAbandon(t, db, ctx, w)
			if res.Replay {
				t.Fatal("a resumed run reported replay=true")
			}
			for _, name := range tc.resumedNot {
				if n := w.count(name); n != 0 {
					t.Fatalf("the resumed run repeated %s %d time(s): %v", name, n, w.calls)
				}
			}
			r := assertTerminal(t, db, ctx, w)
			if !r.StartedAt.Time.Equal(first.StartedAt.Time) {
				t.Fatalf("the resumed run moved the reservation time %s -> %s", first.StartedAt.Time, r.StartedAt.Time)
			}
			if first.ReturnedAt.Valid && !r.ReturnedAt.Time.Equal(first.ReturnedAt.Time) {
				t.Fatalf("the resumed run moved the target-return time %s -> %s", first.ReturnedAt.Time, r.ReturnedAt.Time)
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			if tc.unit == upgradeUnit && (w.movements != 1 || len(w.legs) != 1 || r.RefundFact.UUID != w.legs[w.f.receipt().String()].factID) {
				t.Fatalf("payments moved %d refund(s) over %d leg(s); row fact %v", w.movements, len(w.legs), r.RefundFact)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// Replacement refunds at the reservation (D10).

func bindReplacementRefund(t *testing.T, db *sql.DB, ctx context.Context, f fixture, key string) uuid.UUID {
	t.Helper()
	r, err := store.BindOrderRefund(ctx, db, store.RefundRequest{
		OrderID: f.ReplacementOrder, OrganizerID: f.Org, Quantity: 1, IdempotencyKey: key,
		Actor: "support", Reason: "target slot cancelled",
	})
	if err != nil {
		t.Fatalf("bind replacement refund: %v", err)
	}
	return r.ID
}

func TestReplacementRefundsDecideTheReservation(t *testing.T) {
	t.Run("a completed refund refuses", func(t *testing.T) {
		db, ctx := abandonDB(t)
		f := seed(t, db, ctx, upgradeUnit)
		id := bindReplacementRefund(t, db, ctx, f, "completed")
		if err := store.CompleteOrderRefund(ctx, db, f.Org, id, uuid.New()); err != nil {
			t.Fatal(err)
		}
		w := newWorld(t, f)
		if _, err := abandon(t, db, ctx, w); !errors.Is(err, store.ErrReplacementRefundCompleted) {
			t.Fatalf("err = %v, want ErrReplacementRefundCompleted", err)
		}
		if readRow(t, db, ctx, f).StartedAt.Valid || w.count("payments:replacement-leg") != 0 || w.count("payments:refund") != 0 {
			t.Fatalf("calls = %v", w.calls)
		}
	})
	t.Run("a pending refund with a leg refuses", func(t *testing.T) {
		for _, found := range []LegEvidence{
			{Found: true, Completed: false, Amount: 1000, Currency: "EUR"},
			{Found: true, Completed: true, Amount: 1000, Currency: "EUR"},
		} {
			db, ctx := abandonDB(t)
			f := seed(t, db, ctx, upgradeUnit)
			id := bindReplacementRefund(t, db, ctx, f, "bound")
			w := newWorld(t, f)
			w.replacementLegs[id.String()] = found
			if _, err := abandon(t, db, ctx, w); !errors.Is(err, ErrReplacementRefundLeg) {
				t.Fatalf("err = %v, want ErrReplacementRefundLeg", err)
			}
			if readRow(t, db, ctx, f).StartedAt.Valid || w.count("payments:refund") != 0 {
				t.Fatalf("calls = %v", w.calls)
			}
		}
	})
	t.Run("an indeterminate leg read stops for a retry", func(t *testing.T) {
		db, ctx := abandonDB(t)
		f := seed(t, db, ctx, upgradeUnit)
		bindReplacementRefund(t, db, ctx, f, "unknown")
		w := newWorld(t, f)
		w.legErr = func(source, _ string) error {
			if source == f.ReplacementKey {
				return ErrUnavailable
			}
			return nil
		}
		if _, err := abandon(t, db, ctx, w); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
		if readRow(t, db, ctx, f).StartedAt.Valid {
			t.Fatal("an indeterminate leg read reserved the abandonment")
		}
	})
	t.Run("a pending refund with a conclusive 404 proceeds and stays pending", func(t *testing.T) {
		db, ctx := abandonDB(t)
		f := seed(t, db, ctx, upgradeUnit)
		id := bindReplacementRefund(t, db, ctx, f, "absent")
		w := newWorld(t, f)
		mustAbandon(t, db, ctx, w)
		assertTerminal(t, db, ctx, w)
		if w.count("payments:replacement-leg") != 1 {
			t.Fatalf("the pending refund's leg was read %d time(s), want 1", w.count("payments:replacement-leg"))
		}
		var status string
		if err := db.QueryRowContext(ctx, `SELECT status FROM order_refunds WHERE organizer_id=$1 AND id=$2`, f.Org, id).Scan(&status); err != nil || status != "pending" {
			t.Fatalf("replacement refund status = %q (err %v), want pending", status, err)
		}
	})
	t.Run("a refund bound between inspection and the locked recheck is inspected again", func(t *testing.T) {
		db, ctx := abandonDB(t)
		f := seed(t, db, ctx, upgradeUnit)
		bindReplacementRefund(t, db, ctx, f, "first")
		w := newWorld(t, f)
		var late uuid.UUID
		w.onReplacementLeg = func(string) {
			if late == uuid.Nil {
				late = bindReplacementRefund(t, db, ctx, f, "late")
			}
		}
		mustAbandon(t, db, ctx, w)
		w.mu.Lock()
		var lateReads int
		for i, c := range w.calls {
			_ = i
			if c == "payments:replacement-leg" {
				lateReads++
			}
		}
		w.mu.Unlock()
		// First inspection: one leg read. Second inspection: both refunds.
		if lateReads != 3 {
			t.Fatalf("replacement leg reads = %d, want 3 (one, then both after the set changed)", lateReads)
		}
		assertTerminal(t, db, ctx, w)
	})
}

// R2-5. A full valid refund set fits the command deadline. The replacement holds 50 units; 48
// pending quantity-one refunds exist, and one more is bound during each of the first two
// inspections, so the command inspects three times (48, 49 and 50 leg reads) and then completes.
// With every remote call at its full timeout, the run still fits CommandDeadline.
func TestAFullValidRefundSetFitsTheCommandDeadline(t *testing.T) {
	db, ctx := abandonDB(t)
	f := seedQuantity(t, db, ctx, upgradeUnit, 50)
	for i := 0; i < 48; i++ {
		bindReplacementRefund(t, db, ctx, f, "pending-"+strconv.Itoa(i))
	}
	w := newWorld(t, f)
	reads := 0
	w.onReplacementLeg = func(string) {
		reads++
		switch reads {
		case 48, 48 + 49:
			bindReplacementRefund(t, db, ctx, f, "late-"+strconv.Itoa(reads))
		}
	}
	mustAbandon(t, db, ctx, w)
	assertTerminal(t, db, ctx, w)
	if reads != 48+49+50 {
		t.Fatalf("replacement leg reads = %d, want 147 over three inspections", reads)
	}
	total := w.total()
	if total != 2+1+147+4 {
		t.Fatalf("the command made %d remote calls, want 154: %v", total, w.calls)
	}
	const perCall = 10 * time.Second
	if worst := time.Duration(total)*perCall + DatabaseAllowance; worst > CommandDeadline(perCall) {
		t.Fatalf("%d calls at %v each plus %v of database work = %v, over the %v deadline",
			total, perCall, DatabaseAllowance, worst, CommandDeadline(perCall))
	}
}

// wireOver is the world with one change: the call that `route` names goes through the real HTTP
// client to a server, so the client's decoder judges a literal wire answer. An empty route
// sends every call to the world.
type wireOver struct {
	*world
	client HTTP
	route  *string
}

func (p wireOver) Admission(ctx context.Context, org, order uuid.UUID) (accessadmission.Admission, error) {
	if *p.route == "replacement-admission" && order == p.f.ReplacementOrder {
		return p.client.Admission(ctx, org, order)
	}
	return p.world.Admission(ctx, org, order)
}

func (p wireOver) ChargeOperation(ctx context.Context, org uuid.UUID, key string) (ChargeEvidence, error) {
	if *p.route == "charge" {
		return p.client.ChargeOperation(ctx, org, key)
	}
	return p.world.ChargeOperation(ctx, org, key)
}

func (p wireOver) RefundLeg(ctx context.Context, org uuid.UUID, source, refund string) (LegEvidence, error) {
	if *p.route == "replacement-leg" && source == p.f.ReplacementKey {
		return p.client.RefundLeg(ctx, org, source, refund)
	}
	return p.world.RefundLeg(ctx, org, source, refund)
}

func (p wireOver) PartialRefund(ctx context.Context, r RefundRequest) (RefundAnswer, error) {
	if *p.route == "partial-refund" {
		return p.client.PartialRefund(ctx, r)
	}
	return p.world.PartialRefund(ctx, r)
}

// wireService builds the service over wireOver, with every routed call answered by `status` and
// `body`.
func wireService(t *testing.T, db *sql.DB, w *world, route *string, status int, body string) *Service {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(status)
		_, _ = rw.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	over := wireOver{world: w, client: NewHTTP(srv.URL, srv.URL, srv.URL, "internal", "payments", 5*time.Second), route: route}
	return New(db, over, over, w)
}

// RA1-4. With the same pending replacement refund, only payments' own leg-absence answer lets
// the reservation proceed. A route-level 404 is not proof that no leg exists: the command stops
// for a retry before the reservation and before any money or capacity call.
func TestOnlyPaymentsLegAbsenceAnswerAdmitsAPendingReplacementRefund(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		reserves   bool
	}{
		{"payments' leg-absence answer", `{"error":"refund leg not found"}`, true},
		{"a route-level 404", "404 page not found\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := abandonDB(t)
			f := seed(t, db, ctx, upgradeUnit)
			bindReplacementRefund(t, db, ctx, f, "pending")
			w := newWorld(t, f)
			route := "replacement-leg"
			_, err := wireService(t, db, w, &route, http.StatusNotFound, tc.body).Abandon(ctx, f.Org, f.Exchange, f.attribution())
			if tc.reserves {
				if err != nil {
					t.Fatalf("abandon: %v", err)
				}
				assertTerminal(t, db, ctx, w)
				return
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v, want ErrUnavailable", err)
			}
			if readRow(t, db, ctx, f).StartedAt.Valid || w.count("payments:refund") != 0 || w.count("inventory:return") != 0 {
				t.Fatalf("a route-level 404 reserved or moved something: calls %v", w.calls)
			}
		})
	}
}

// R2-3. Each answer that would establish zero replacement tickets, the delta capture or a
// replacement leg's absence is decoded strictly through the real client. A contradictory or
// duplicated answer stops the command for a retry before the reservation and before any money
// or capacity call. Each refusal has a control that differs only in the refused property.
func TestContradictoryEvidenceStopsBeforeTheReservation(t *testing.T) {
	delta := strconv.FormatInt(int64(quantity)*(upgradeUnit-sourceUnit), 10)
	for _, tc := range []struct {
		name, route string
		status      int
		body        string
		pendingLeg  bool
		reserves    bool
	}{
		{"replacement admission control", "replacement-admission", 200, `{"admitted":false,"issued_count":0}`, false, true},
		{"replacement admitted with no issued ticket", "replacement-admission", 200, `{"admitted":true,"issued_count":0}`, false, false},
		{"replacement admission with a duplicate field", "replacement-admission", 200, `{"admitted":false,"issued_count":1,"issued_count":0}`, false, false},
		{"delta capture control", "charge", 200, `{"resolved":true,"captured_amount":` + delta + `,"currency":"EUR"}`, false, true},
		{"delta capture with a duplicate field", "charge", 200, `{"resolved":true,"captured_amount":1,"captured_amount":` + delta + `,"currency":"EUR"}`, false, false},
		{"leg absence with a duplicate field", "replacement-leg", 404, `{"error":"route","error":"refund leg not found"}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := abandonDB(t)
			f := seed(t, db, ctx, upgradeUnit)
			if tc.pendingLeg {
				bindReplacementRefund(t, db, ctx, f, "pending")
			}
			w := newWorld(t, f)
			route := tc.route
			_, err := wireService(t, db, w, &route, tc.status, tc.body).Abandon(ctx, f.Org, f.Exchange, f.attribution())
			if tc.reserves {
				if err != nil {
					t.Fatalf("abandon: %v", err)
				}
				assertTerminal(t, db, ctx, w)
				return
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v, want ErrUnavailable", err)
			}
			if readRow(t, db, ctx, f).StartedAt.Valid || w.count("payments:refund") != 0 ||
				w.count("inventory:return") != 0 || w.count("payments:fact") != 0 {
				t.Fatalf("contradictory evidence reserved or wrote something: calls %v", w.calls)
			}
		})
	}
}

// RA2-4. A 422 whose provider-failure fields are duplicated is not the typed failure: the refund
// failure reference stays empty, and the same command run again completes the abandonment.
func TestAnAmbiguousProviderFailureStaysRetryable(t *testing.T) {
	db, ctx := abandonDB(t)
	f := seed(t, db, ctx, upgradeUnit)
	w := newWorld(t, f)
	route := "partial-refund"
	svc := wireService(t, db, w, &route, http.StatusUnprocessableEntity,
		`{"error":"x","code":"other","code":"provider_refund_failed","provider_ref":"re_ambiguous"}`)
	if _, err := svc.Abandon(ctx, f.Org, f.Exchange, f.attribution()); !errors.Is(err, ErrUnavailable) || errors.Is(err, store.ErrAbandonmentRefundFailed) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if r := readRow(t, db, ctx, f); !r.StartedAt.Valid || r.FailedRef.Valid || r.RefundFact.Valid {
		t.Fatalf("row = %+v, want started with no refund failure reference and no refund", r)
	}
	route = ""
	if _, err := svc.Abandon(ctx, f.Org, f.Exchange, f.attribution()); err != nil {
		t.Fatalf("the same command after an ambiguous 422: %v", err)
	}
	assertTerminal(t, db, ctx, w)
}

// R1-5. A started abandonment is judged against the candidate definition again before it resumes
// any remote effect: a resume skips fresh eligibility, so rows that became inconsistent after the
// reservation stop the command with no Access, payments or inventory call.
func TestAStartedAbandonmentWithInconsistentRowsStopsBeforeAnyRemoteCall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(t *testing.T, db *sql.DB, ctx context.Context, f fixture)
	}{
		{"source reservation in another organizer", func(t *testing.T, db *sql.DB, ctx context.Context, f fixture) {
			if _, err := db.ExecContext(ctx, `UPDATE reservations SET organizer_id=$2 WHERE id=(SELECT reservation_id FROM orders WHERE id=$1)`, f.SourceOrder, uuid.New()); err != nil {
				t.Fatal(err)
			}
		}},
		{"replacement reservation in another organizer", func(t *testing.T, db *sql.DB, ctx context.Context, f fixture) {
			if _, err := db.ExecContext(ctx, `UPDATE reservations SET organizer_id=$2 WHERE id=(SELECT reservation_id FROM orders WHERE id=$1)`, f.ReplacementOrder, uuid.New()); err != nil {
				t.Fatal(err)
			}
		}},
		{"replacement reservation on another hold", func(t *testing.T, db *sql.DB, ctx context.Context, f fixture) {
			if _, err := db.ExecContext(ctx, `UPDATE reservations SET hold_id=$2 WHERE id=(SELECT reservation_id FROM orders WHERE id=$1)`, f.ReplacementOrder, uuid.New()); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := abandonDB(t)
			f := seed(t, db, ctx, upgradeUnit)
			e, err := store.LoadSettledExchange(ctx, db, f.Org, f.Exchange)
			if err != nil {
				t.Fatal(err)
			}
			if _, reserved, err := store.ReserveExchangeAbandonment(ctx, db, store.AbandonmentReservation{Expected: e, Attribution: f.attribution()}); err != nil || !reserved {
				t.Fatalf("reserve: reserved=%v err=%v", reserved, err)
			}
			tc.apply(t, db, ctx, f)
			w := newWorld(t, f)
			if _, err := abandon(t, db, ctx, w); !errors.Is(err, store.ErrExchangeAbandonmentInconsistent) {
				t.Fatalf("err = %v, want ErrExchangeAbandonmentInconsistent", err)
			}
			if n := w.total(); n != 0 {
				t.Fatalf("an inconsistent started abandonment made %d remote call(s): %v", n, w.calls)
			}
		})
	}
}
