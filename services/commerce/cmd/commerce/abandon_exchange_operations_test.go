package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
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
	t.Setenv("ACCESS_URL", dep)
	t.Setenv("PAYMENTS_URL", dep)
	t.Setenv("INVENTORY_URL", dep)
	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-token")
	t.Setenv("PAYMENTS_INTERNAL_TOKEN", "payments-token")
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

// Each required setting is checked before the database: a command that cannot finish must not
// start a reservation it cannot drive.
func TestAbandonExchangeRefusesMissingSettingsBeforeAnyDependency(t *testing.T) {
	dials := listenForDials(t)
	dep, calls := recordingDependency(t)
	args := []string{uuid.NewString(), uuid.NewString(), "Ada Operator", "source admitted"}
	for _, missing := range []string{"ACCESS_URL", "PAYMENTS_URL", "INVENTORY_URL", "INTERNAL_SERVICE_TOKEN"} {
		t.Run(missing, func(t *testing.T) {
			for _, name := range []string{"ACCESS_URL", "PAYMENTS_URL", "INVENTORY_URL"} {
				t.Setenv(name, dep)
			}
			t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-token")
			t.Setenv(missing, "")
			err := abandonExchange(args)
			if err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("with %s unset: err = %v, want it named", missing, err)
			}
		})
	}
	if n := dials(); n != 0 {
		t.Errorf("a missing setting reached the database %d time(s)", n)
	}
	if n := calls(); n != 0 {
		t.Errorf("a missing setting reached a service %d time(s)", n)
	}
}

// A valid invocation does open the database: the two tests above are falsifiable only if the
// counter can see a connection.
func TestAbandonExchangeWithValidArgumentsReachesTheDatabase(t *testing.T) {
	dials := listenForDials(t)
	dep, _ := recordingDependency(t)
	for _, name := range []string{"ACCESS_URL", "PAYMENTS_URL", "INVENTORY_URL"} {
		t.Setenv(name, dep)
	}
	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-token")
	if err := abandonExchange([]string{uuid.NewString(), uuid.NewString(), " Ada Operator ", " source admitted "}); err == nil {
		t.Fatal("the command succeeded against a database that refuses every connection")
	}
	deadline := time.Now().Add(5 * time.Second)
	for dials() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if dials() == 0 {
		t.Fatal("a valid invocation never reached the database")
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
