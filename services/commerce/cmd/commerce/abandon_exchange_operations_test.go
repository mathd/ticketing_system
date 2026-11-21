package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/exchangeabandon"
	store "ticketing/services/commerce/internal/store"
)

// TKT-498. `abandon-exchange` validates every argument and every required setting before it
// opens the database or calls a service. The dial counter (listenForDials) observes the
// database; a recording server stands for Access, payments and inventory.

func recordingDependency(t *testing.T) (url string, calls func() int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, n.Load
}

func TestAbandonExchangeRefusesBadArgumentsBeforeAnyDependency(t *testing.T) {
	dials := listenForDials(t)
	dep, calls := recordingDependency(t)
	validAbandonSettings(t, dep)
	org, exchange := uuid.NewString(), uuid.NewString()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no arguments", nil, "usage:"},
		{"three arguments", []string{org, exchange, "Ada"}, "usage:"},
		{"five arguments", []string{org, exchange, "Ada", "reason", "extra"}, "usage:"},
		{"malformed organizer", []string{"not-a-uuid", exchange, "Ada", "reason"}, "organizer id"},
		{"nil organizer", []string{uuid.Nil.String(), exchange, "Ada", "reason"}, "organizer id"},
		{"malformed exchange", []string{org, "not-a-uuid", "Ada", "reason"}, "exchange id"},
		{"nil exchange", []string{org, uuid.Nil.String(), "Ada", "reason"}, "exchange id"},
		{"blank operator", []string{org, exchange, "  \t", "reason"}, "operator name"},
		{"blank reason", []string{org, exchange, "Ada", " \n "}, "reason"},
		{"operator over 200 characters", []string{org, exchange, strings.Repeat("é", 201), "reason"}, "operator name"},
		{"reason over 500 characters", []string{org, exchange, "Ada", strings.Repeat("r", 501)}, "reason"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := abandonExchange(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("abandonExchange(%q) = %v, want an error naming %q", tc.args, err, tc.want)
			}
		})
	}
	if n := dials(); n != 0 {
		t.Errorf("a bad argument reached the database %d time(s)", n)
	}
	if n := calls(); n != 0 {
		t.Errorf("a bad argument reached a service %d time(s)", n)
	}
}

const (
	abandonInternalToken = "internal-service-token-for-abandon-tests-0123456789"
	abandonPaymentsToken = "payments-internal-token-for-abandon-tests-0123456789"
)

// validAbandonSettings sets every setting the command needs, with the database at the dial
// counter. It also points libpq's PG* defaults at the counter, so a command that opened the
// database with an empty DATABASE_URL would be counted.
func validAbandonSettings(t *testing.T, dep string) {
	t.Helper()
	for _, name := range []string{"ACCESS_URL", "PAYMENTS_URL", "INVENTORY_URL"} {
		t.Setenv(name, dep)
	}
	t.Setenv("INTERNAL_SERVICE_TOKEN", abandonInternalToken)
	t.Setenv("PAYMENTS_INTERNAL_TOKEN", abandonPaymentsToken)
	u, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGHOST", u.Hostname())
	t.Setenv("PGPORT", u.Port())
	t.Setenv("PGUSER", "u")
	t.Setenv("PGDATABASE", "db")
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGCONNECT_TIMEOUT", "2")
}

// settled reads the dial counter once it has stopped moving: the listener counts on its own
// goroutine, so a dial can land just after the command returns.
func settled(count func() int) int {
	n := count()
	for {
		time.Sleep(30 * time.Millisecond)
		if m := count(); m != n {
			n = m
			continue
		}
		return n
	}
}

// Each setting is checked before the database, one at a time: a command that cannot finish
// must not start a reservation it cannot drive (R2-4, RA2-3). Every case changes one setting of
// an otherwise valid configuration.
func TestAbandonExchangeRefusesEachUnusableSettingBeforeAnyDependency(t *testing.T) {
	dials := listenForDials(t)
	dep, calls := recordingDependency(t)
	args := []string{uuid.NewString(), uuid.NewString(), "Ada Operator", "source admitted"}
	for _, tc := range []struct {
		name, env, value, want string
	}{
		{"ACCESS_URL unset", "ACCESS_URL", "", "ACCESS_URL"},
		{"PAYMENTS_URL unset", "PAYMENTS_URL", "", "PAYMENTS_URL"},
		{"INVENTORY_URL unset", "INVENTORY_URL", "", "INVENTORY_URL"},
		{"ACCESS_URL without a scheme", "ACCESS_URL", "access:8080", "ACCESS_URL"},
		{"ACCESS_URL relative", "ACCESS_URL", "/access", "ACCESS_URL"},
		{"PAYMENTS_URL not http", "PAYMENTS_URL", "ftp://payments:8080", "PAYMENTS_URL"},
		{"INVENTORY_URL without a host", "INVENTORY_URL", "http://", "INVENTORY_URL"},
		{"INVENTORY_URL with a query", "INVENTORY_URL", "http://inventory:8080/?x=1", "INVENTORY_URL"},
		{"INVENTORY_URL with an empty query", "INVENTORY_URL", "http://inventory:8080/?", "INVENTORY_URL"},
		{"ACCESS_URL with a fragment", "ACCESS_URL", "http://access:8080/#x", "ACCESS_URL"},
		{"INTERNAL_SERVICE_TOKEN unset", "INTERNAL_SERVICE_TOKEN", "", "INTERNAL_SERVICE_TOKEN"},
		{"PAYMENTS_INTERNAL_TOKEN unset", "PAYMENTS_INTERNAL_TOKEN", "", "PAYMENTS_INTERNAL_TOKEN"},
		{"PAYMENTS_INTERNAL_TOKEN equal to the shared token", "PAYMENTS_INTERNAL_TOKEN", abandonInternalToken, "must not equal"},
		{"DATABASE_URL unset", "DATABASE_URL", "", "DATABASE_URL"},
		{"DATABASE_URL blank", "DATABASE_URL", "  ", "DATABASE_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validAbandonSettings(t, dep)
			t.Setenv(tc.env, tc.value)
			before, beforeCalls := settled(dials), calls()
			err := abandonExchange(args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("with %s=%q: err = %v, want it to name %q", tc.env, tc.value, err, tc.want)
			}
			if n := settled(dials) - before; n != 0 {
				t.Errorf("with %s=%q the command reached the database %d time(s)", tc.env, tc.value, n)
			}
			if n := calls() - beforeCalls; n != 0 {
				t.Errorf("with %s=%q the command reached a service %d time(s)", tc.env, tc.value, n)
			}
		})
	}
}

// A valid invocation does open the database: the test above is falsifiable only if the counter
// can see a connection.
func TestAbandonExchangeWithValidArgumentsReachesTheDatabase(t *testing.T) {
	dials := listenForDials(t)
	dep, _ := recordingDependency(t)
	validAbandonSettings(t, dep)
	if err := abandonExchange([]string{uuid.NewString(), uuid.NewString(), " Ada Operator ", " source admitted "}); err == nil {
		t.Fatal("the command succeeded against a database that refuses every connection")
	}
	if settled(dials) == 0 {
		t.Fatal("a valid invocation never reached the database")
	}
}

// R2-5. The command deadline covers the longest command at the per-call timeout: 160 remote
// calls of 10 s (two Access reads, the delta read, three inspections of up to 51 leg reads, and
// the abandonment leg read, refund, capacity return and journal append) plus 2 minutes of
// database work. The value is written out so that a change to it is a visible decision.
func TestAbandonExchangeDeadlineCoversTheLongestCommand(t *testing.T) {
	if got, want := abandonDeadline(), 160*10*time.Second+2*time.Minute; got != want {
		t.Fatalf("abandonDeadline() = %v, want %v", got, want)
	}
}

// The terminal record names the attribution, every instant and the branch's evidence.
func TestAbandonedRecordPrintsTheBranchEvidence(t *testing.T) {
	at := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	base := store.SettledExchange{OrganizerID: uuid.New(), ID: uuid.New(), Quantity: 2, Currency: "EUR",
		AbandonmentOperator: "Ada Operator", AbandonmentSessionUser: "commerce", AbandonmentReason: "source admitted",
		AbandonmentStartedAt: at, AbandonmentTargetReturnedAt: at.Add(time.Second), AbandonedAt: at.Add(2 * time.Second)}
	upgrade, downgrade, even := base, base, base
	upgrade.DeltaAmount, upgrade.AbandonmentRefundFactID = 1000, uuid.New()
	downgrade.DeltaAmount, downgrade.AbandonmentWrittenOff = -700, 700
	for _, tc := range []struct {
		e         store.SettledExchange
		replay    bool
		want, not []string
	}{
		{upgrade, false, []string{"delta=+1000 EUR", "refund_fact=" + upgrade.AbandonmentRefundFactID.String(), "replay=false"}, []string{"written_off="}},
		{downgrade, true, []string{"delta=-700 EUR", "written_off=700", "replay=true"}, []string{"refund_fact="}},
		{even, false, []string{"delta=0 EUR"}, []string{"refund_fact=", "written_off="}},
	} {
		var out bytes.Buffer
		printAbandoned(&out, exchangeabandon.Result{Exchange: tc.e, Replay: tc.replay})
		line := out.String()
		for _, want := range append(tc.want, `operator="Ada Operator"`, `session_user="commerce"`,
			"started_at=2026-11-21T10:00:00Z", "target_returned_at=2026-11-21T10:00:01Z", "abandoned_at=2026-11-21T10:00:02Z") {
			if !strings.Contains(line, want) {
				t.Errorf("%q does not contain %q", line, want)
			}
		}
		for _, not := range tc.not {
			if strings.Contains(line, not) {
				t.Errorf("%q contains %q", line, not)
			}
		}
	}
}

// A recorded provider failure is reported as a stop with its reference, never as retryable.
func TestAbandonRefusalNamesTheRecordedProviderFailure(t *testing.T) {
	var out bytes.Buffer
	e := store.SettledExchange{AbandonmentStarted: true, AbandonmentRefundFailedRef: "re_failed"}
	reportAbandonRefusal(&out, uuid.New(), e, errors.Join(store.ErrAbandonmentRefundFailed))
	if !strings.Contains(out.String(), "STOPPED") || !strings.Contains(out.String(), "re_failed") ||
		!strings.Contains(out.String(), "TKT-588") {
		t.Fatalf("report = %q, want a stop naming the provider reference and TKT-588", out.String())
	}
}
