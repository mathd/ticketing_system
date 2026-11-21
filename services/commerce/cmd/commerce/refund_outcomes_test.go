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
	t.Setenv("PAYMENTS_INTERNAL_TOKEN", "payments-token")
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

// Payments configuration is required and named, and its absence is found before the database.
func TestRecordRefundOutcomeNeedsPaymentsConfigurationFirst(t *testing.T) {
	args := []string{uuid.NewString(), uuid.NewString(), "refunded_outside", "Ada Operator", "SEPA-0001", "paid by bank transfer"}
	dials := listenForDials(t)

	t.Setenv("PAYMENTS_URL", "")
	t.Setenv("PAYMENTS_INTERNAL_TOKEN", "a-token")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "")
	if err := recordRefundOutcome(args); err == nil || !strings.Contains(err.Error(), "PAYMENTS_URL") {
		t.Fatalf("err = %v, want it to name PAYMENTS_URL", err)
	}
	t.Setenv("PAYMENTS_URL", "http://payments.invalid")
	t.Setenv("PAYMENTS_INTERNAL_TOKEN", "")
	if err := recordRefundOutcome(args); err == nil || !strings.Contains(err.Error(), "PAYMENTS_INTERNAL_TOKEN") {
		t.Fatalf("err = %v, want it to name the credential", err)
	}
	if dials() != 0 {
		t.Fatalf("the command reached the database %d times without payments configuration", dials())
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
			paymentsDials := listenForPaymentsDials(t)
			err := recordRefundOutcome(args)
			if err == nil {
				t.Fatal("the command succeeded against a listener that closes every connection")
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
