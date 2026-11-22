package main

import (
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// TKT-525. `record-refund-outcome` validates every argument BEFORE it opens the database or
// calls payments. Both are listeners that count connections, so a validation that moved below
// either one is observed as a dial, not inferred from an error string.

func listenForPaymentsDials(t *testing.T) func() int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var mu sync.Mutex
	var n int
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			n++
			mu.Unlock()
			_ = c.Close()
		}
	}()
	t.Setenv("PAYMENTS_URL", "http://"+ln.Addr().String())
	t.Setenv("INTERNAL_SERVICE_TOKEN", abandonInternalToken)
	t.Setenv("PAYMENTS_INTERNAL_TOKEN", abandonPaymentsToken)
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

func TestRecordRefundOutcomeRefusesInvalidCommandsBeforeAnyConnection(t *testing.T) {
	org, refund := uuid.NewString(), uuid.NewString()
	valid := []string{org, refund, "refunded_outside", "Ada Operator", "SEPA-0001", "paid by bank transfer"}
	with := func(i int, v string) []string {
		out := append([]string(nil), valid...)
		out[i] = v
		return out
	}
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"no arguments", nil, "usage:"},
		{"too few for refunded_outside", valid[:5], "usage:"},
		{"too many for refunded_outside", append(append([]string(nil), valid...), "extra"), "usage:"},
		{"malformed organizer", with(0, "not-a-uuid"), "organizer id"},
		{"nil organizer", with(0, uuid.Nil.String()), "organizer id"},
		{"malformed refund", with(1, "not-a-uuid"), "refund id"},
		{"nil refund", with(1, uuid.Nil.String()), "refund id"},
		{"unknown kind", with(2, "refunded"), "outcome kind"},
		{"blank operator", with(3, "  "), "operator name"},
		{"blank external reference", with(4, "\t"), "external reference"},
		{"blank reason", with(5, " "), "reason"},
		{"overlong operator", with(3, strings.Repeat("a", 201)), "operator name"},
		{"overlong external reference", with(4, strings.Repeat("a", 201)), "external reference"},
		{"overlong reason", with(5, strings.Repeat("a", 501)), "reason"},
		{"too few for abandoned", []string{org, refund, "abandoned", "Ada Operator"}, "usage:"},
		{"too many for abandoned", []string{org, refund, "abandoned", "Ada Operator", "re_typed", "nobody will refund"}, "usage:"},
		{"blank abandoned operator", []string{org, refund, "abandoned", " ", "nobody will refund"}, "operator name"},
		{"blank abandoned reason", []string{org, refund, "abandoned", "Ada Operator", "\t"}, "reason"},
		{"overlong abandoned reason", []string{org, refund, "abandoned", "Ada Operator", strings.Repeat("a", 501)}, "reason"},
		{"nil organizer for abandoned", []string{uuid.Nil.String(), refund, "abandoned", "Ada Operator", "nobody will refund"}, "organizer id"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dials := listenForDials(t)
			paymentsDials := listenForPaymentsDials(t)
			err := recordRefundOutcome(c.args)
			if err == nil {
				t.Fatal("an invalid command was accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to name %q", err, c.want)
			}
			if dials() != 0 || paymentsDials() != 0 {
				t.Fatalf("an invalid command reached the database (%d) or payments (%d)", dials(), paymentsDials())
			}
		})
	}
}

// validOperatorSettings points the command at the database dial counter and at a payments that
// counts requests, with two valid, distinct credentials. It also points the PG* defaults at the
// dial counter, so a command that opened the database with an empty DATABASE_URL is counted, and
// clears PGSERVICE, which pgx would otherwise read from the shell.
func validOperatorSettings(t *testing.T, payments string) {
	t.Helper()
	validAbandonSettings(t, payments)
	t.Setenv("PGSERVICE", "")
}

const (
	missingPaymentsTokenError = "PAYMENTS_INTERNAL_TOKEN required: no default is shipped, run `make up` once to generate a local credential"
	shortPaymentsTokenError   = "PAYMENTS_INTERNAL_TOKEN must be at least 32 bytes (`make up` generates a conformant value)"
	equalPaymentsTokenError   = "PAYMENTS_INTERNAL_TOKEN must not equal INTERNAL_SERVICE_TOKEN: the separate credential exists " +
		"so a compromise elsewhere does not reach the money surface, and an identical value removes that boundary while looking configured"
)

// TKT-595. Every setting is checked before the database is opened, one at a time, in both forms.
// Each row changes one setting of an otherwise valid configuration. The database counter is the
// observable: the abandoned form inspects the refund and reserves its abandonment through the
// handle the command opens, so zero connections also means no reservation. The payments counter
// cannot fail under a mutation here, because both forms read the database first and the listener
// closes every connection; the positive controls below pin that order.
func TestRecordRefundOutcomeRefusesInvalidConfiguration(t *testing.T) {
	dials := listenForDials(t)
	opens := countOpens(t)
	payments, calls := recordingDependency(t)
	forms := map[string][]string{
		"refunded_outside": {uuid.NewString(), uuid.NewString(), "refunded_outside", "Ada Operator", "SEPA-0001", "paid by bank transfer"},
		"abandoned":        {uuid.NewString(), uuid.NewString(), "abandoned", "Ada Operator", "nobody will refund this attempt"},
	}
	rows := []struct {
		name, env, value, want string
	}{
		{"PAYMENTS_INTERNAL_TOKEN unset with a valid shared token", "PAYMENTS_INTERNAL_TOKEN", "", missingPaymentsTokenError},
		{"PAYMENTS_INTERNAL_TOKEN of 31 bytes", "PAYMENTS_INTERNAL_TOKEN", strings.Repeat("p", 31), shortPaymentsTokenError},
		{"PAYMENTS_INTERNAL_TOKEN equal to the shared token", "PAYMENTS_INTERNAL_TOKEN", abandonInternalToken, equalPaymentsTokenError},
		{"DATABASE_URL empty", "DATABASE_URL", "", "DATABASE_URL is required: an empty value would select the shell's PostgreSQL defaults"},
		{"PAYMENTS_URL empty", "PAYMENTS_URL", "", "PAYMENTS_URL is required"},
		{"PAYMENTS_URL with a bare fragment delimiter", "PAYMENTS_URL", payments + "#",
			"PAYMENTS_URL must be an absolute http or https base URL with a host name, a port in 1-65535 if any, and no query or fragment"},
	}
	for form, args := range forms {
		for _, tc := range rows {
			t.Run(form+"/"+tc.name, func(t *testing.T) {
				validOperatorSettings(t, payments)
				t.Setenv(tc.env, tc.value)
				before, beforeCalls, beforeOpens := settled(dials), calls(), opens()
				err := recordRefundOutcome(args)
				if err == nil || err.Error() != tc.want {
					t.Errorf("with %s=%q: err = %v, want %q", tc.env, tc.value, err, tc.want)
				}
				if n := opens() - beforeOpens; n != 0 {
					t.Errorf("with %s=%q the command opened the database %d time(s) before it refused", tc.env, tc.value, n)
				}
				if n := settled(dials) - before; n != 0 {
					t.Errorf("with %s=%q the command reached the database %d time(s)", tc.env, tc.value, n)
				}
				if n := calls() - beforeCalls; n != 0 {
					t.Errorf("with %s=%q the command reached payments %d time(s)", tc.env, tc.value, n)
				}
			})
		}
	}
}

// The valid command passes validation and reaches the database: the refusals above are
// refusals of THOSE inputs, not of everything. Both forms read the refund before payments.
func TestRecordRefundOutcomeValidCommandReachesTheDatabase(t *testing.T) {
	for name, args := range map[string][]string{
		"refunded_outside": {uuid.NewString(), uuid.NewString(), "refunded_outside", "Ada Operator", "SEPA-0001", "paid by bank transfer"},
		"abandoned":        {uuid.NewString(), uuid.NewString(), "abandoned", "Ada Operator", "nobody will refund this attempt"},
	} {
		t.Run(name, func(t *testing.T) {
			dials := listenForDials(t)
			opens := countOpens(t)
			paymentsDials := listenForPaymentsDials(t)
			err := recordRefundOutcome(args)
			if err == nil {
				t.Fatal("the command succeeded against a listener that closes every connection")
			}
			if n := opens(); n != 1 {
				t.Fatalf("a valid command opened the database %d time(s), want 1 (err=%v)", n, err)
			}
			if dials() == 0 {
				t.Fatalf("a valid command never reached the database (err=%v)", err)
			}
			if paymentsDials() != 0 {
				t.Fatalf("the command called payments before it could read the refund (%d dials)", paymentsDials())
			}
		})
	}
}
