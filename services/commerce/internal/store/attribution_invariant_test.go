package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Attribution durability, as a property of the SOURCE rather than of one run
// (TKT-221 ai-review [medium]).
//
// The design claim is "customer_id is written once, on the order INSERT, and
// nothing ever updates it — so it survives completion, recovery, refunds and
// cancellation runs by not being touched". A runtime test can only demonstrate
// that for the paths it happens to drive, and the risk is precisely the path
// nobody thought to drive: a future recovery or compensation UPDATE that clears
// it would pass a suite built around CompleteOrder.
//
// So this reads the candidate set from the system instead of hand-maintaining a
// list of paths to check — the shape
// docs/learnings/2026-08-05-a-hand-maintained-inventory-cannot-detect-its-own-drift.md
// asks for. Two properties, over every production Go file in the commerce service:
//
//  1. No `UPDATE orders SET …` mentions customer_id. That is the durability claim.
//  2. Every `INSERT INTO orders(…)` names customer_id. An insert path that forgets
//     silently creates orders that can never belong to anyone, and the buyer finds
//     out by not seeing their purchase.

// The alias group is not optional decoration: recovery.go already writes
// `UPDATE orders o SET …`, and the first version of these patterns — which
// required `orders` to be followed immediately by `SET` — did not match it
// (ai-review pass 2 [medium]). A scanner that silently skips a real statement is
// worse than no scanner, so TestTheScannerSeesTheShapesThatExist pins each shape
// it must recognise.
var (
	updateOrders = regexp.MustCompile(`(?is)UPDATE\s+orders\s+(?:(?:AS\s+)?[a-z_][a-z0-9_]*\s+)?SET\s+(.*?)(?:\x60|\bWHERE\b|;)`)
	insertOrders = regexp.MustCompile(`(?is)INSERT\s+INTO\s+orders\s*\(([^)]*)\)`)
)

func commerceProductionSQL(t *testing.T) map[string]string {
	t.Helper()
	sources := map[string]string{}
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		// Tests seed rows by hand and are allowed to say anything.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sources[path] = withoutLineComments(string(body))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) == 0 {
		t.Fatal("walked the commerce service and found no production Go files — the scan proves nothing")
	}
	return sources
}

func TestNoProductionCodeUpdatesOrderAttribution(t *testing.T) {
	sources := commerceProductionSQL(t)

	var statements int
	for path, body := range sources {
		for _, match := range updateOrders.FindAllStringSubmatch(body, -1) {
			statements++
			if strings.Contains(strings.ToLower(match[1]), "customer_id") {
				t.Errorf("%s updates orders.customer_id:\n\tUPDATE orders SET %s\n"+
					"Attribution is written once, on the INSERT in claimOrder, and must survive "+
					"completion and recovery untouched — an order can be completed minutes after "+
					"the request that established it is gone.", path, strings.TrimSpace(match[1]))
			}
		}
	}
	// A scan that matched nothing would pass silently while proving nothing —
	// exactly the failure this style of test exists to avoid.
	if statements == 0 {
		t.Fatal("found no `UPDATE orders SET` statements at all; the regex has stopped matching")
	}
}

func TestEveryProductionOrderInsertCarriesAttribution(t *testing.T) {
	sources := commerceProductionSQL(t)

	var statements int
	for path, body := range sources {
		for _, match := range insertOrders.FindAllStringSubmatch(body, -1) {
			statements++
			if !strings.Contains(strings.ToLower(match[1]), "customer_id") {
				t.Errorf("%s inserts an order without customer_id:\n\tINSERT INTO orders(%s)\n"+
					"An insert path that omits it silently creates an order that can never belong "+
					"to anyone; the buyer discovers it by not finding their purchase.",
					path, strings.TrimSpace(match[1]))
			}
		}
	}
	if statements == 0 {
		t.Fatal("found no `INSERT INTO orders(...)` statements at all; the regex has stopped matching")
	}
}

// withoutLineComments drops whole-line `//` comments before scanning.
//
// Necessary, not tidiness: refunds.go's doc comment quotes
// "`INSERT INTO orders(idempotency_key…)`" to explain where a key comes from, and
// the first version of this scan reported it as an insert path that had forgotten
// customer_id. A source scan that flags prose is a source scan somebody disables.
func withoutLineComments(body string) string {
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// A detector is only as good as its ability to see. These are the statement
// shapes that exist in this service today plus the ones a future author would
// plausibly write; if the patterns stop matching any of them, the invariant tests
// above go quietly green over a real violation (ai-review pass 2 [medium]).
func TestTheScannerSeesTheShapesThatExist(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		update    bool
	}{
		{"plain update", "UPDATE orders SET customer_id=NULL WHERE id=$1", true},
		{"aliased update", "UPDATE orders o SET customer_id=NULL WHERE o.id=$1", true},
		{"AS-aliased update", "UPDATE orders AS o SET customer_id=NULL WHERE o.id=$1", true},
		{"lowercase", "update orders set customer_id=null where id=$1", true},
		{"multi-line", "UPDATE orders\n\t\tSET customer_id=NULL\n\t\tWHERE id=$1", true},
		{"plain insert", "INSERT INTO orders(id,status) VALUES($1,$2)", false},
		{"insert select", "INSERT INTO orders(id,status)\n\t\tSELECT $1,'x' FROM orders WHERE id=$2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pattern, kind := insertOrders, "INSERT INTO orders(...)"
			if tc.update {
				pattern, kind = updateOrders, "UPDATE orders SET"
			}
			if pattern.FindStringSubmatch(tc.sql) == nil {
				t.Fatalf("the %s scanner does not see this shape, so the invariant it enforces "+
					"would pass over it:\n\t%s", kind, tc.sql)
			}
		})
	}
}

// The scanner reads Go source, not SQL, so it cannot see a statement assembled at
// runtime. That limit is real and is stated rather than implied: the guarantee is
// "no LITERAL production statement violates this", and a future author who builds
// order SQL by concatenation defeats it. Nothing in this service does that today,
// and this test is what makes the assumption checkable rather than silent.
func TestOrderSQLIsWrittenAsLiterals(t *testing.T) {
	for path, body := range commerceProductionSQL(t) {
		for _, suspicious := range []string{`"UPDATE orders" +`, `"INSERT INTO orders" +`, `+ " orders"`} {
			if strings.Contains(body, suspicious) {
				t.Errorf("%s appears to build order SQL by concatenation (%q); the source scanners "+
					"in this file cannot see through that and their guarantee no longer holds",
					path, suspicious)
			}
		}
	}
}
