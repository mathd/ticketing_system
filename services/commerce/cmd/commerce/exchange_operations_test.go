package main

import (
	"net"
	"os"
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
// TestUnwindExchangeReachesNoDatabaseWhenPaymentsIsUnconfigured counts TCP connections to a
// listener the test owns.

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

// Payments configuration is required, and each missing piece is named.
//
// The unwind's whole purpose is to refuse unless payments' own records say no money moved,
// so a command with no way to ask payments cannot do its job — and must say which piece is
// missing rather than failing later with something that reads like a payments outage.
//
// The dial counter is the ordering half, and it is the only ordering assertion in this file
// that can actually fail: it observes what the command DID (whether it reached the database
// at all) rather than what it returned. Deleting the payments checks entirely makes the
// command proceed to the store, dial, and fail on the connection — which this catches.
func TestUnwindExchangeReachesNoDatabaseWhenPaymentsIsUnconfigured(t *testing.T) {
	org, exchangeID := uuid.New().String(), uuid.New().String()
	dials := listenForDials(t)

	t.Setenv("PAYMENTS_URL", "")
	t.Setenv("PAYMENTS_INTERNAL_TOKEN", "a-token")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "")
	err := unwindExchange([]string{org, exchangeID, "a reason"})
	if err == nil {
		t.Fatal("unwind-exchange ran with no PAYMENTS_URL; it cannot establish whether money " +
			"moved without one, and proceeding would mean deciding on commerce's flags alone")
	}
	if !strings.Contains(err.Error(), "PAYMENTS_URL") {
		t.Fatalf("err = %v, want the error to name PAYMENTS_URL", err)
	}

	// With a URL but no credential of either kind, payments answers 401 to everything, which
	// this command would then have to read as indeterminate. Refusing up front says why.
	t.Setenv("PAYMENTS_URL", "http://payments.invalid")
	t.Setenv("PAYMENTS_INTERNAL_TOKEN", "")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "")
	err = unwindExchange([]string{org, exchangeID, "a reason"})
	if err == nil {
		t.Fatal("unwind-exchange ran with no payments credential")
	}
	if !strings.Contains(err.Error(), "PAYMENTS_INTERNAL_TOKEN") {
		t.Fatalf("err = %v, want the error to name the credential", err)
	}

	if n := dials(); n != 0 {
		t.Errorf("the command dialled the database %d time(s) before refusing on payments "+
			"configuration. sql.Open with pgx connects to nothing, so a non-zero count here means "+
			"the store was actually called — the payments checks ran too late, and an operator "+
			"with a misconfigured payments learns it only after the command has done real work", n)
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
	t.Setenv("PAYMENTS_INTERNAL_TOKEN", "a-token")

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

// The shared token is accepted as a fallback for the payments-specific one.
//
// Commerce's own `internalTokenFor` falls back the same way, and for the stated reason: a
// deployment that predates the credential split must keep working rather than sending an
// empty credential, which payments fails closed on and which reads as an outage rather than
// as a misconfiguration.
//
// Asserted by what the command does NEXT: with both ids valid, a reason, a URL and the
// shared token, validation is satisfied and the command proceeds to the database — so the
// error it returns must no longer be about configuration.
func TestTheSharedTokenIsAcceptedWhenThePaymentsOneIsAbsent(t *testing.T) {
	t.Setenv("PAYMENTS_URL", "http://payments.invalid")
	t.Setenv("PAYMENTS_INTERNAL_TOKEN", "")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "shared-token")
	t.Setenv("DATABASE_URL", "postgres://nobody@127.0.0.1:1/nothing")

	err := unwindExchange([]string{uuid.New().String(), uuid.New().String(), "a reason"})
	if err == nil {
		t.Fatal("the command somehow succeeded against an unreachable database")
	}
	if strings.Contains(err.Error(), "PAYMENTS_INTERNAL_TOKEN") || strings.Contains(err.Error(), "PAYMENTS_URL") {
		t.Fatalf("err = %v; the shared token must satisfy the credential requirement, as "+
			"internalTokenFor's own fallback does", err)
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

// The commands are reachable through main's dispatch.
//
// Not a test of main() — nothing here executes it — but of the one fact a dispatch bug would
// break silently: that these names are the ones wired. `os.Args`-driven dispatch has no
// compiler check, so a typo in either string sends the operator's command into `run()`, which
// tries to start a server and fails with something unrelated to what they typed.
func TestTheSubcommandNamesAreTheOnesDocumented(t *testing.T) {
	// The names appear in the usage strings the argument errors carry, which is what an
	// operator actually reads. If a name changes, the usage text and the dispatch have to
	// change together, and this couples them.
	err := listWedgedExchanges([]string{"x"})
	if err == nil || !strings.Contains(err.Error(), "list-wedged-exchanges") {
		t.Errorf("list usage = %v, want it to name the subcommand", err)
	}
	err = unwindExchange(nil)
	if err == nil || !strings.Contains(err.Error(), "unwind-exchange") {
		t.Errorf("unwind usage = %v, want it to name the subcommand", err)
	}
	// And the binary's own dispatch strings, read from the source of truth rather than
	// retyped: os.Args[1] is compared against these literals in main().
	for _, name := range []string{"list-wedged-exchanges", "unwind-exchange"} {
		if !strings.Contains(mainDispatchNames(), name) {
			t.Errorf("main() does not dispatch %q; the command would fall through to run() and "+
				"try to start a server", name)
		}
	}
}

// mainDispatchNames reads main.go and returns its text, so the test above compares the usage
// strings against the actual dispatch rather than against a copy of it.
func mainDispatchNames() string {
	b, err := os.ReadFile("main.go")
	if err != nil {
		return ""
	}
	return string(b)
}
