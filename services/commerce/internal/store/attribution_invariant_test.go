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
// **What this does NOT do (ADR-021 — name the adversary): it stops an honest
// omission. It does not stop an author who is trying to defeat it.** A statement
// whose predicates live inside a dollar-quoted string, or in a subquery the regex
// reads as the outer WHERE, can satisfy this check while filtering nothing
// (ai-review pass 2). Closing that means parsing SQL rather than reading source,
// and the thing being defended against — a recovery or compensation path quietly
// rewriting attribution — is written by someone who does not know this guard
// exists, not by someone routing around it.
//
// The same boundary catalog's public-read guard states about itself
// (public_read_invalidation_test.go): *"it stops an honest omission. It does not
// stop someone editing this map in the same commit."*
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
	// Two captures: $1 is the SET clause, $2 is everything from WHERE to the end of
	// the raw string. The second exists so a predicate check can be bound to the
	// STATEMENT rather than to the file — searching the file passes when the
	// predicates live in a comment or a different function (ai-review [medium]).
	// ONLY and a schema qualifier are accepted PostgreSQL spellings, and a form the
	// scanner cannot SEE is the worst failure it has — a real attribution writer
	// invisible to the guard, with the exactly-one count still satisfied by the
	// legitimate one (ai-review pass 2 [high]).
	updateOrders = regexp.MustCompile(`(?is)UPDATE\s+(?:ONLY\s+)?(?:[a-z_][a-z0-9_]*\.)?orders\s+(?:(?:AS\s+)?[a-z_][a-z0-9_]*\s+)?SET\s+(.*?)(?:\bWHERE\b(.*?))?(?:\x60|;|$)`)
	insertOrders = regexp.MustCompile(`(?is)INSERT\s+INTO\s+(?:ONLY\s+)?(?:[a-z_][a-z0-9_]*\.)?orders\s*\(([^)]*)\)`)
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

// TKT-223 added the ONE sanctioned exception: the claim, which is the only
// NULL -> customer transition in the system.
//
// Allowlisted by TEXT and by COUNT, not by file. A file-level exemption would let
// a second attribution update land beside the claim and pass, which is the failure
// mode an allowlist actually has — and this repo has now been bitten twice by
// guards whose own bypass nobody tested (TKT-194's hand-maintained inventory,
// TKT-222's plan assertion matching an unrelated node). TestTheAllowlistCannotBeWidened
// below is that test.
//
// Why the claim may do what recovery must not: recovery and checkout replay are
// not ownership operations — they finish work that is already attributed and must
// leave it as they found it. A claim IS the ownership operation. Its predicate,
// which the allowlist requires to be present, is what keeps that narrow: only an
// unattributed order, or one already owned by the caller.
// Two statements now, each bound to its OWN predicates (TKT-225).
//
// Not one relaxed rule covering both: a check that admitted "customer_id = $2 or
// customer_id = NULL" with either statement's predicates would let a detach borrow
// the claim's where-clause and vice versa, which is how a two-entry allowlist
// becomes a blanket one. Each entry names the assignment AND the predicates that
// make that particular write narrow, and neither set authorizes the other.
//
// Why a SECOND exception is sanctioned where a third would not be: a claim is the
// only NULL -> customer transition and a detach is the only customer -> NULL one
// (ADR-052). Together they are the whole of attribution's mutable life. A third
// statement is by construction something else — a repoint, a recovery path, a
// compensation — and is exactly what this guard exists to refuse.
var sanctionedAttributionWrites = []struct {
	assignment string
	predicates []string
}{
	{
		// The claim (TKT-223).
		assignment: "customer_id = $2",
		predicates: []string{
			"guest_order_ref = $1",
			"status = 'completed'",
			"customer_id IS NULL OR customer_id = $2",
		},
	},
	{
		// The detach (TKT-225). `customer_id IS NOT NULL` is load-bearing, not
		// defensive: without it a detach of an unattributed order reports success
		// and writes an audit row for something that did not happen.
		assignment: "customer_id = NULL",
		predicates: []string{
			"id = $1",
			"status = 'completed'",
			"customer_id IS NOT NULL",
		},
	},
}

// allowedAttributionUpdate reports whether a statement's assignment AND its own
// where-clause together match one sanctioned write.
//
// Both halves are checked against the SAME entry. Checking them independently —
// "is this a known assignment" and "are these known predicates" — is the bypass
// that lets a detach's SET ride the claim's WHERE.
func allowedAttributionUpdate(assignment, where string) bool {
	normalized := strings.Join(strings.Fields(assignment), " ")
	for _, sanctioned := range sanctionedAttributionWrites {
		if normalized != sanctioned.assignment {
			continue
		}
		for _, predicate := range sanctioned.predicates {
			if !strings.Contains(where, predicate) {
				return false
			}
		}
		return true
	}
	return false
}

// scanAttributionUpdates is the WHOLE decision — match, recognise, authorize — in
// one place, so the production check and its regression test run the same code.
//
// The regression test used to call the helpers directly, which meant restoring
// the file-scoped bug left it green: it was asserting the helpers, not the wiring
// (ai-review pass 2 [medium]). Third time this run that a test could not fail;
// the fix is always the same shape — make the test call what production calls.
//
// Returns: statements seen, allowlisted attribution updates, and the SET clause of
// every one that is not allowed.
func scanAttributionUpdates(body string) (statements, allowed int, refused []string) {
	for _, match := range updateOrders.FindAllStringSubmatch(body, -1) {
		statements++
		if !strings.Contains(strings.ToLower(match[1]), "customer_id") {
			continue
		}
		if allowedAttributionUpdate(match[1], match[2]) {
			allowed++
			continue
		}
		refused = append(refused, match[1])
	}
	return statements, allowed, refused
}

func TestNoProductionCodeUpdatesOrderAttribution(t *testing.T) {
	sources := commerceProductionSQL(t)

	var statements, allowed int
	for path, body := range sources {
		found, ok, bad := scanAttributionUpdates(body)
		statements += found
		allowed += ok
		for _, assignment := range bad {
			t.Errorf("%s updates orders.customer_id:\n\tUPDATE orders SET %s\n"+
				"Attribution is written once, on the INSERT in claimOrder, and must survive "+
				"completion and recovery untouched — an order can be completed minutes after "+
				"the request that established it is gone. The ONE exception is the claim "+
				"(TKT-223), which must keep its guest_order_ref / completed / NULL-or-same "+
				"predicates.", path, strings.TrimSpace(assignment))
		}
	}
	// Exactly two: the claim (TKT-223) and the detach (TKT-225). A THIRD copy —
	// even a correct-looking one — is how an allowlist stops being a list.
	//
	// The count is asserted separately from the per-statement check because they
	// fail differently: a wrong count means someone added a statement that happens
	// to match a sanctioned shape, which the shape check by definition cannot see.
	if allowed != 2 {
		t.Errorf("allowlisted attribution updates = %d, want exactly 2 "+
			"(the TKT-223 claim and the TKT-225 detach). "+
			"A third is not covered by anything that reviewed these two.", allowed)
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

// The guard on the guard (TKT-223 plan-review F3).
//
// An allowlist whose own bypasses are untested is not a guard. These are the three
// ways this one can be widened without anybody noticing, and each must be rejected
// by the same machinery that permits the real statement.
func TestTheAllowlistCannotBeWidened(t *testing.T) {
	for _, tc := range []struct {
		name       string
		assignment string
		body       string
		want       bool
	}{
		{
			name:       "the real claim statement",
			assignment: "customer_id = $2",
			body:       "guest_order_ref = $1 status = 'completed' customer_id IS NULL OR customer_id = $2",
			want:       true,
		},
		{
			name:       "the claim with its ownership predicate removed",
			assignment: "customer_id = $2",
			body:       "guest_order_ref = $1 status = 'completed'",
			want:       false,
		},
		{
			name:       "the claim with its completed predicate removed",
			assignment: "customer_id = $2",
			body:       "guest_order_ref = $1 customer_id IS NULL OR customer_id = $2",
			want:       false,
		},
		{
			name:       "a recovery-shaped clearing of attribution",
			assignment: "customer_id = NULL, status = 'refunded'",
			body:       "guest_order_ref = $1 status = 'completed' customer_id IS NULL OR customer_id = $2",
			want:       false,
		},
		{
			name:       "an update that sets attribution from a different parameter",
			assignment: "customer_id = $3",
			body:       "guest_order_ref = $1 status = 'completed' customer_id IS NULL OR customer_id = $2",
			want:       false,
		},
		// The second sanctioned statement, and the ways IT can be widened (TKT-225).
		{
			name:       "the real detach statement",
			assignment: "customer_id = NULL",
			body:       "id = $1 status = 'completed' customer_id IS NOT NULL",
			want:       true,
		},
		{
			name: "the detach with its IS NOT NULL predicate removed",
			// Without it, detaching an unattributed order succeeds and writes an
			// audit row for a detachment that did not happen.
			assignment: "customer_id = NULL",
			body:       "id = $1 status = 'completed'",
			want:       false,
		},
		{
			name:       "the detach with its completed predicate removed",
			assignment: "customer_id = NULL",
			body:       "id = $1 customer_id IS NOT NULL",
			want:       false,
		},
		{
			name: "the detach with no order identity — every attributed order at once",
			// The predicate that scopes it to ONE order. Without `id = $1` this is
			// a statement that unattributes the whole table.
			assignment: "customer_id = NULL",
			body:       "status = 'completed' customer_id IS NOT NULL",
			want:       false,
		},
		{
			name: "a detach borrowing the CLAIM's predicates",
			// The bypass a two-entry allowlist actually has: checking "is this a
			// known assignment" and "are these known predicates" independently
			// would admit this, because both halves are individually sanctioned —
			// just not together.
			assignment: "customer_id = NULL",
			body:       "guest_order_ref = $1 status = 'completed' customer_id IS NULL OR customer_id = $2",
			want:       false,
		},
		{
			name:       "a claim borrowing the DETACH's predicates",
			assignment: "customer_id = $2",
			body:       "id = $1 status = 'completed' customer_id IS NOT NULL",
			want:       false,
		},
		{
			name: "a THIRD statement that looks like a transfer",
			// A repoint is not one of the two sanctioned transitions. It is
			// TKT-9/TKT-160's problem and has a different adversary.
			assignment: "customer_id = $2",
			body:       "id = $1 status = 'completed' customer_id IS NOT NULL AND customer_id <> $2",
			want:       false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := allowedAttributionUpdate(tc.assignment, tc.body)
			if got != tc.want {
				t.Fatalf("allowed = %v, want %v — the allowlist %s", got, tc.want,
					map[bool]string{true: "refuses the one statement it exists to permit",
						false: "admits a statement nobody reviewed"}[tc.want])
			}
		})
	}
}

// The bypass the file-scoped version admitted (ai-review [medium]).
//
// The predicate check is only as good as what it is handed. The first version was
// handed the whole FILE, so a statement with no predicates at all passed as long
// as the words appeared *somewhere* — in a comment, or in a different function.
// This drives the real regex over synthetic source and asserts the where-clause it
// extracts belongs to the statement it matched.
func TestPredicatesAreReadFromTheStatementAndNotTheFile(t *testing.T) {
	source := `
// A comment that mentions guest_order_ref = $1 and status = 'completed' and
// customer_id IS NULL OR customer_id = $2 — none of which is in the statement.
const sneaky = ` + "`" + `
	UPDATE orders SET customer_id = $2 WHERE id = $1` + "`" + `
`
	// Drives the PRODUCTION scan, not the helpers underneath it. Calling the
	// predicate check directly would leave this green with the file-scoped bug
	// restored, which is the regression it is named for.
	statements, allowed, refused := scanAttributionUpdates(withoutLineComments(source))
	if statements != 1 {
		t.Fatalf("matched %d statements, want 1 — the regex has stopped seeing this shape", statements)
	}
	if allowed != 0 || len(refused) != 1 {
		t.Fatalf("a statement whose only predicate is `id = $1` was allowed (allowed=%d refused=%v). "+
			"The predicate words are in the FILE, not the statement.", allowed, refused)
	}

	// And the real statement, read the same way, is allowed.
	_, realAllowed, realRefused := scanAttributionUpdates(withoutLineComments("const x = `" + claimGuestOrderStatement + "`"))
	if realAllowed != 1 || len(realRefused) != 0 {
		t.Fatalf("the actual claim statement failed its own check: allowed=%d refused=%v", realAllowed, realRefused)
	}
}

// The forms the scanner must not be blind to (ai-review pass 2 [high]).
//
// `UPDATE ONLY orders` and `UPDATE public.orders` are ordinary PostgreSQL. A
// statement the scanner cannot SEE is its worst failure — the exactly-one count
// is still satisfied by the legitimate claim, so a second real attribution writer
// is invisible rather than merely unapproved.
func TestTheScannerSeesQualifiedAndOnlyForms(t *testing.T) {
	for _, sql := range []string{
		"UPDATE ONLY orders SET customer_id = NULL WHERE id = $1",
		"UPDATE public.orders SET customer_id = NULL WHERE id = $1",
		"UPDATE ONLY public.orders o SET customer_id = NULL WHERE o.id = $1",
	} {
		t.Run(sql, func(t *testing.T) {
			statements, allowed, refused := scanAttributionUpdates(sql)
			if statements != 1 {
				t.Fatalf("the scanner did not see this statement at all — a form it cannot see is "+
					"a writer it cannot guard: %s", sql)
			}
			if allowed != 0 || len(refused) != 1 {
				t.Fatalf("allowed=%d refused=%v, want it refused", allowed, refused)
			}
		})
	}
}
