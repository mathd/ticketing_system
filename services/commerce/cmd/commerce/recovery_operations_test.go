package main

import (
	"database/sql"
	"strings"
	"testing"
)

// TKT-146. Argument validation, asserted without a database.
//
// Every case here must be refused BEFORE sql.Open is reached, which is what makes the
// test meaningful without a DSN: if validation moved below the connection, these would
// stop returning a usage error and start returning a connection error, and the
// substring assertions would go red.

func TestListParkedRefusesArguments(t *testing.T) {
	err := listParked([]string{"unexpected"})
	if err == nil {
		t.Fatal("list-parked accepted an argument it takes none of")
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("err = %v, want a usage error", err)
	}
}

func TestUnparkOrderRefusesWrongArity(t *testing.T) {
	for _, args := range [][]string{{}, {"only-an-id"}, {"id", "reason", "extra"}} {
		err := unparkOrder(args)
		if err == nil {
			t.Fatalf("unpark-order accepted %d argument(s)", len(args))
		}
		if !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("args %v: err = %v, want a usage error", args, err)
		}
	}
}

func TestUnparkOrderRefusesAMalformedOrderID(t *testing.T) {
	err := unparkOrder([]string{"not-a-uuid", "a reason"})
	if err == nil {
		t.Fatal("unpark-order accepted a malformed order id")
	}
	if !strings.Contains(err.Error(), "order id") {
		t.Fatalf("err = %v, want the error to name the order id", err)
	}
}

// An absent value and an empty one are different facts about a parked order, and the
// listing must not collapse them: `last_error=<none>` says the park recorded no cause,
// while an empty rendering reads as a cause that was recorded and was blank.
func TestNullableFieldDistinguishesAbsentFromEmpty(t *testing.T) {
	if got := nullableField(sql.NullString{}); got != "<none>" {
		t.Fatalf("absent rendered as %q, want <none>", got)
	}
	if got := nullableField(sql.NullString{Valid: true, String: ""}); got != "<none>" {
		t.Fatalf("empty rendered as %q, want <none>", got)
	}
	if got := nullableField(sql.NullString{Valid: true, String: "psp unreachable"}); got != "psp unreachable" {
		t.Fatalf("a present value rendered as %q", got)
	}
}
