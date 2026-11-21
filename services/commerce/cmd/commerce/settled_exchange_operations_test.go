package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	store "ticketing/services/commerce/internal/store"
)

// TKT-498. Argument validation and output of `list-settled-unswitched-exchanges`, without a
// database. The dial counter (listenForDials, exchange_operations_test.go) is the observable
// that a bad invocation stopped before the store: sql.Open with pgx connects to nothing, so an
// error alone cannot show that.

func TestListSettledUnswitchedExchangesRefusesBadArgumentsWithoutReachingTheDatabase(t *testing.T) {
	dials := listenForDials(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no organizer", nil, "usage:"},
		{"extra argument", []string{uuid.NewString(), "extra"}, "usage:"},
		{"malformed organizer", []string{"not-a-uuid"}, "organizer id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := listSettledUnswitchedExchanges(tc.args)
			if err == nil {
				t.Fatalf("args %v were accepted", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if n := dials(); n != 0 {
		t.Errorf("a bad argument reached the database %d time(s); validation must refuse first", n)
	}
}

// Each row names the exchange, both orders, the target hold, the settlement time, the quantity
// and the SIGNED delta with its currency. An operator reads the sign to know which branch the
// abandonment takes, so a positive delta carries its `+`.
func TestSettledUnswitchedRowsPrintEveryIdentityAndASignedDelta(t *testing.T) {
	settled := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	rows := []store.SettledExchange{
		{OrganizerID: uuid.New(), ID: uuid.New(), SourceOrderID: uuid.New(), ReplacementOrderID: uuid.New(),
			TargetHoldID: uuid.New(), Quantity: 2, DeltaAmount: 1000, Currency: "EUR", SettledAt: settled},
		{OrganizerID: uuid.New(), ID: uuid.New(), SourceOrderID: uuid.New(), ReplacementOrderID: uuid.New(),
			TargetHoldID: uuid.New(), Quantity: 3, DeltaAmount: -700, Currency: "EUR", SettledAt: settled},
		{OrganizerID: uuid.New(), ID: uuid.New(), SourceOrderID: uuid.New(), ReplacementOrderID: uuid.New(),
			TargetHoldID: uuid.New(), Quantity: 1, DeltaAmount: 0, Currency: "EUR", SettledAt: settled},
	}
	var out bytes.Buffer
	printSettledUnswitched(&out, rows)
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != len(rows) {
		t.Fatalf("printed %d lines for %d rows:\n%s", len(lines), len(rows), out.String())
	}
	for i, r := range rows {
		line := lines[i]
		for _, want := range []string{
			"organizer=" + r.OrganizerID.String(),
			"exchange=" + r.ID.String(),
			"source_order=" + r.SourceOrderID.String(),
			"replacement_order=" + r.ReplacementOrderID.String(),
			"target_hold=" + r.TargetHoldID.String(),
			"settled_at=2026-11-20T09:30:00Z",
		} {
			if !strings.Contains(line, want) {
				t.Errorf("row %d: %q does not contain %q", i, line, want)
			}
		}
	}
	for i, want := range []string{"quantity=2 delta=+1000 EUR", "quantity=3 delta=-700 EUR", "quantity=1 delta=0 EUR"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("row %d: %q does not contain %q", i, lines[i], want)
		}
	}
}

// A started abandonment shows its start, its operator and, after a terminal provider refund
// failure, the provider reference. A row with no abandonment says so.
func TestSettledUnswitchedRowsShowTheAbandonmentProgress(t *testing.T) {
	started := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	base := store.SettledExchange{ID: uuid.New(), Currency: "EUR", DeltaAmount: 1000, Quantity: 2}
	none := base
	pending := base
	pending.AbandonmentStarted, pending.AbandonmentStartedAt, pending.AbandonmentOperator = true, started, "Ada Operator"
	failed := pending
	failed.AbandonmentRefundFailedRef = "re_3Pq"
	returned := pending
	returned.AbandonmentTargetReturned = true

	for _, c := range []struct {
		name string
		row  store.SettledExchange
		want []string
		not  []string
	}{
		{"no abandonment", none, []string{"abandonment=none"}, []string{"operator=", "refund_failed_ref="}},
		{"started", pending, []string{"abandonment=started", "abandonment_started_at=2026-11-21T08:00:00Z", `operator="Ada Operator"`}, []string{"refund_failed_ref="}},
		{"provider failure", failed, []string{"abandonment=refund-failed", "refund_failed_ref=re_3Pq", `operator="Ada Operator"`}, nil},
		{"target returned", returned, []string{"abandonment=target-returned"}, []string{"refund_failed_ref="}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			printSettledUnswitched(&out, []store.SettledExchange{c.row})
			for _, w := range c.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("%q does not contain %q", out.String(), w)
				}
			}
			for _, n := range c.not {
				if strings.Contains(out.String(), n) {
					t.Errorf("%q contains %q", out.String(), n)
				}
			}
		})
	}
}
