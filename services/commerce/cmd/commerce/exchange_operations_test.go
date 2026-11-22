package main

import (
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// TKT-255. Argument and environment validation for the wedged-exchange commands, asserted
// without a database and without payments.
//
// WHAT THESE CAN AND CANNOT PROVE, written down because getting it wrong cost two rewrites.
// recovery_operations_test.go frames its equivalents as proving validation happens "before
// sql.Open is reached". That framing does not survive contact with the driver: `sql.Open`
// with pgx NEVER errors and never connects — it stores a DSN and returns a pool, resolving
// the connection on first use. So no arrangement of DATABASE_URL makes a check placed after
// it behave differently from one placed before, and a test claiming to pin that ordering
// cannot fail. Two attempts here did exactly that before the reason was found.
//
// What these tests DO prove is the contract that actually matters to an operator: every bad
// invocation is refused with an error naming the specific thing that was wrong, and no
// database or payments service is required to get that answer. That is what makes the
// commands usable during an incident, and it is falsifiable — delete a check and its case
// goes red.
//
// The one genuine ordering claim left is asserted by observation rather than by error text:
// TestUnwindExchangeRefusesInvalidConfiguration counts TCP connections to a listener the test
// owns, and TestUnwindExchangeValidCommandReachesTheDatabase shows that the listener sees a valid
// command.

func TestListWedgedExchangesRefusesArguments(t *testing.T) {
	err := listWedgedExchanges([]string{"unexpected"})
	if err == nil {
		t.Fatal("list-wedged-exchanges accepted an argument it takes none of")
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("err = %v, want a usage error", err)
	}
}

func TestUnwindExchangeRefusesWrongArity(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"only-an-org"},
		{"org", "exchange"},
		{"org", "exchange", "reason", "extra"},
	} {
		err := unwindExchange(args)
		if err == nil {
			t.Fatalf("unwind-exchange accepted %d argument(s)", len(args))
		}
		if !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("args %v: err = %v, want a usage error", args, err)
		}
	}
}

// The two ids are refused SEPARATELY and each error names which one was wrong.
//
// One case per id rather than one case for "a malformed id": an operator running this during
// an incident is copying two uuids off a listing, and "exchange id: invalid UUID" sends them
// to the right column while a generic message sends them to both.
func TestUnwindExchangeRefusesEachMalformedIDDistinguishably(t *testing.T) {
	good := uuid.New().String()

	err := unwindExchange([]string{"not-a-uuid", good, "a reason"})
	if err == nil {
		t.Fatal("unwind-exchange accepted a malformed organizer id")
	}
	if !strings.Contains(err.Error(), "organizer id") {
		t.Fatalf("err = %v, want the error to name the organizer id", err)
	}

	// The organizer id is VALID here, so the organizer check cannot be what refused — which
	// is what makes this a test of the exchange-id check rather than of the first one again.
	err = unwindExchange([]string{good, "not-a-uuid", "a reason"})
	if err == nil {
		t.Fatal("unwind-exchange accepted a malformed exchange id")
	}
	if !strings.Contains(err.Error(), "exchange id") {
		t.Fatalf("err = %v, want the error to name the exchange id", err)
	}
}

// A blank reason is refused before anything else is touched.
//
// Both ids are valid, so this is genuinely the reason check refusing and not an earlier one
// short-circuiting it.
func TestUnwindExchangeRefusesABlankReasonBeforeConnecting(t *testing.T) {
	org, exchangeID := uuid.New().String(), uuid.New().String()
	for _, reason := range []string{"", "   ", "\t\n"} {
		err := unwindExchange([]string{org, exchangeID, reason})
		if err == nil {
			t.Fatalf("unwind-exchange accepted a blank reason %q", reason)
		}
		if !strings.Contains(err.Error(), "reason is required") {
			t.Fatalf("reason %q: err = %v, want the reason error", reason, err)
		}
	}
}

// Payments configuration is required, and each unusable setting is named before the database.
//
// The unwind's whole purpose is to refuse unless payments' own records say no money moved, so a
// command with no way to ask payments cannot do its job. It must say which setting is wrong
// rather than fail later with something that reads like a payments outage. The credential is
// the dedicated one, validated as payments validates it (TKT-595): payments refuses the shared
// token, a short one and one equal to the shared one. The no-basis and zero-delta branches
// change Commerce without a payments request, so the check must come before the database.
//
// The dial counter is the ordering half: it observes what the command DID (whether it reached
// the database at all) rather than what it returned. Each row changes one setting of an
// otherwise valid configuration.
func TestUnwindExchangeRefusesInvalidConfiguration(t *testing.T) {
	dials := listenForDials(t)
	payments, calls := recordingDependency(t)
	args := []string{uuid.New().String(), uuid.New().String(), "a reason"}
	for _, tc := range []struct {
		name, env, value, want string
	}{
		{"PAYMENTS_INTERNAL_TOKEN unset with a valid shared token", "PAYMENTS_INTERNAL_TOKEN", "", missingPaymentsTokenError},
		{"PAYMENTS_INTERNAL_TOKEN of 31 bytes", "PAYMENTS_INTERNAL_TOKEN", strings.Repeat("p", 31), shortPaymentsTokenError},
		{"PAYMENTS_INTERNAL_TOKEN equal to the shared token", "PAYMENTS_INTERNAL_TOKEN", abandonInternalToken, equalPaymentsTokenError},
		{"PAYMENTS_URL empty", "PAYMENTS_URL", "", "PAYMENTS_URL is required: the unwind refuses unless payments' own " +
			"records say no money moved, and it cannot ask without one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validOperatorSettings(t, payments)
			t.Setenv(tc.env, tc.value)
			before, beforeCalls := settled(dials), calls()
			err := unwindExchange(args)
			if err == nil || err.Error() != tc.want {
				t.Errorf("with %s=%q: err = %v, want %q", tc.env, tc.value, err, tc.want)
			}
			if n := settled(dials) - before; n != 0 {
				t.Errorf("with %s=%q the command reached the database %d time(s): the configuration check ran "+
					"too late, and an operator learns about it only after the command has done real work", tc.env, tc.value, n)
			}
			if n := calls() - beforeCalls; n != 0 {
				t.Errorf("with %s=%q the command reached payments %d time(s)", tc.env, tc.value, n)
			}
		})
	}
}

// A valid invocation does reach the database: the refusals above are refusals of THOSE settings,
// and the counter can see a connection.
func TestUnwindExchangeValidCommandReachesTheDatabase(t *testing.T) {
	dials := listenForDials(t)
	payments, calls := recordingDependency(t)
	validOperatorSettings(t, payments)
	err := unwindExchange([]string{uuid.New().String(), uuid.New().String(), "a reason"})
	if err == nil {
		t.Fatal("the command succeeded against a listener that closes every connection")
	}
	if n := settled(dials); n == 0 {
		t.Fatalf("a valid command never reached the database (err=%v)", err)
	}
	if n := calls(); n != 0 {
		t.Fatalf("the command called payments before it could read the exchange (%d requests)", n)
	}
}

// listenForDials points DATABASE_URL at a socket this test owns and returns a function
// reporting how many times something connected to it.
//
// Connections are accepted and immediately closed. pgx then fails its startup handshake,
// which is fine: the count is the observable, not the outcome.
func listenForDials(t *testing.T) func() int {
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

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", "postgres://u:p@127.0.0.1:"+port+"/db?sslmode=disable&connect_timeout=2")
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// Every bad argument is refused with an error naming the thing that was wrong, and no
// database or payments service is needed to get that answer.
//
// The dial counter is asserted here too: with payments fully configured, a bad ARGUMENT must
// still stop the command before it reaches the store. That is the falsifiable part — move
// any of these four checks below the store call and the count goes to one.
func TestUnwindExchangeRefusesEveryBadArgumentWithoutReachingTheDatabase(t *testing.T) {
	dials := listenForDials(t)
	t.Setenv("PAYMENTS_URL", "http://payments.invalid")
	t.Setenv("INTERNAL_SERVICE_TOKEN", abandonInternalToken)
	t.Setenv("PAYMENTS_INTERNAL_TOKEN", abandonPaymentsToken)

	good := uuid.New().String()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"arity", []string{"one"}, "usage:"},
		{"organizer id", []string{"nope", good, "a reason"}, "organizer id"},
		{"exchange id", []string{good, "nope", "a reason"}, "exchange id"},
		{"blank reason", []string{good, good, "   "}, "reason is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := unwindExchange(tc.args)
			if err == nil {
				t.Fatalf("args %v were accepted", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	if err := listWedgedExchanges([]string{"unexpected"}); err == nil ||
		!strings.Contains(err.Error(), "usage:") {
		t.Errorf("list-wedged-exchanges err = %v, want a usage error", err)
	}
	if n := dials(); n != 0 {
		t.Errorf("a bad argument reached the database %d time(s). Argument validation has to "+
			"refuse before the store is called, or an operator who mistyped a uuid during an "+
			"incident waits on a connection to find out", n)
	}
}

// An absent hold renders distinguishably from a zero one in the listing.
//
// The same distinction `nullableField` draws for the recovery listing, and it carries real
// meaning here: `target_hold=<none>` says the exchange never recorded a basis and therefore
// never reached payments, while a zero uuid would read as a hold that exists and is empty.
func TestNullableUUIDDistinguishesAbsentFromZero(t *testing.T) {
	if got := nullableUUID(uuid.Nil); got != "<none>" {
		t.Fatalf("the zero uuid rendered as %q, want <none>", got)
	}
	id := uuid.New()
	if got := nullableUUID(id); got != id.String() {
		t.Fatalf("a real hold rendered as %q, want %s", got, id)
	}
}

func TestCommandRegistryInvokesEveryCommerceCallback(t *testing.T) {
	var invoked string
	withoutArgs := func(name string) func() error {
		return func() error { invoked = name; return nil }
	}
	withArgs := func(name string) func([]string) error {
		return func([]string) error { invoked = name; return nil }
	}
	callbacks := commandCallbacks{
		migrate: withoutArgs("migrate"), healthcheck: func() int { invoked = "healthcheck"; return 7 },
		enrolReseller: withArgs("enrol-reseller"), revokeReseller: withArgs("revoke-reseller"),
		listResellers: withArgs("list-resellers"), listParked: withArgs("list-parked"),
		unparkOrder: withArgs("unpark-order"), listWedgedExchanges: withArgs("list-wedged-exchanges"),
		unwindExchange: withArgs("unwind-exchange"), recordRefundOutcome: withArgs("record-refund-outcome"),
		listSettledUnswitchedExchanges: withArgs("list-settled-unswitched-exchanges"),
		abandonExchange:                withArgs("abandon-exchange"),
	}
	registry := commandRegistry(callbacks)
	names := []string{
		"migrate", "healthcheck", "enrol-reseller", "revoke-reseller", "list-resellers",
		"list-parked", "unpark-order", "list-wedged-exchanges", "unwind-exchange", "record-refund-outcome",
		"list-settled-unswitched-exchanges", "abandon-exchange",
	}
	if len(registry) != len(names) {
		t.Fatalf("registry has %d commands, test names %d", len(registry), len(names))
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			invoked = ""
			got := execute([]string{name, "tail"}, callbacks, func() error {
				t.Fatal("server ran after a command was selected")
				return nil
			})
			wantExit := 0
			if name == "healthcheck" {
				wantExit = 7
			}
			if got.Name != name || got.Err != nil || got.ExitCode != wantExit {
				t.Fatalf("dispatch result = %+v, want %s with exit %d", got, name, wantExit)
			}
			if invoked != name {
				t.Fatalf("invoked %q, want %q", invoked, name)
			}
		})
	}
}
