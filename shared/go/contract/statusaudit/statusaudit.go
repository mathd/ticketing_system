// Package statusaudit compares the statuses a handler can WRITE against the ones its
// OpenAPI operation DECLARES (TKT-278).
//
// Why the direction matters. Under ADR-028 the services validate their own responses with
// `IncludeResponseStatus: true`, so a status the document does not declare is failed closed:
// the caller gets a generic 500 carrying "response violates OpenAPI contract" instead of the
// status the handler meant, and the real cause is obscured exactly while someone is
// debugging an outage. Emitting an undeclared status is the defect; declaring one that is
// never emitted costs nothing, so `Missing` fails a caller and `Unemitted` is only reported.
//
// WHY THIS PACKAGE HOLDS ONLY THE MATCHER. Deriving what a handler can emit is a per-service
// problem — catalog routes through generated `ServerInterface` comments while commerce,
// inventory and access hand-mount on chi, and each service writes responses through its own
// helper (`writeJSON`, `write`, `problem`). Those adapters live in each service's own test.
// What is genuinely common is this: how a declared response set is matched against a
// concrete status, which must mirror kin-openapi's own resolution or the audit will disagree
// with the validator it exists to predict.
package statusaudit

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Declares reports whether responses cover status the way openapi3filter.ValidateResponse
// resolves it: the exact key, then the patterned range (`4XX`/`5XX`), then `default`.
//
// Responses.Status already handles the exact-and-range pair, which is why this is not a
// string comparison — a matcher written against Responses.Value("500") would report a
// contract-correct `5XX` operation as missing, and one written against the range alone would
// accept a `500` declaration for an emitted 502. Both spellings are legal and mean different
// things, so the audit has to resolve them exactly as the validator does.
func Declares(responses *openapi3.Responses, status int) bool {
	if responses == nil {
		return false
	}
	return responses.Status(status) != nil || responses.Default() != nil
}

// Diff is one operation's audit: what it can write, what it declares, and the two directions
// of disagreement.
type Diff struct {
	// OperationID and Route identify the operation in failure output. Route is
	// "METHOD /path"; a reader debugging a failure needs the route more than the id.
	OperationID string
	Route       string
	// Emitted is the set of concrete statuses the service's adapter derived. Concrete, not
	// classes: a 502 is not covered by an exact 500 declaration.
	Emitted []int
	// Missing is Emitted minus what the document covers. NON-EMPTY IS A FAILURE — each
	// entry is a status ADR-028's validator would rewrite into a contract 500.
	Missing []int
	// Unemitted is declared-but-not-derived. REPORTED, NEVER FAILED. Two reasons: the
	// adapters deliberately under-approximate (see each service's blind-spot note), so an
	// entry here is at least as likely to be an adapter gap as a stale declaration; and
	// ADR-028 does not punish an unexercised declaration.
	Unemitted []int
	// Declared is the operation's response keys verbatim, including `default` and the
	// patterned ranges. Carried so a failure message can show what the document actually
	// says rather than making the reader open it.
	Declared []string
}

// Audit diffs one operation. `emitted` may repeat and need not be sorted.
func Audit(operationID, route string, op *openapi3.Operation, emitted []int) Diff {
	d := Diff{OperationID: operationID, Route: route, Emitted: dedupe(emitted)}
	if op.Responses != nil {
		for k := range op.Responses.Map() {
			d.Declared = append(d.Declared, k)
		}
		sort.Strings(d.Declared)
	}
	for _, s := range d.Emitted {
		if !Declares(op.Responses, s) {
			d.Missing = append(d.Missing, s)
		}
	}
	// The reverse direction, and it is deliberately not symmetrical: `Declares` resolves a
	// `default:` or `5XX` for EVERY status, so asking "is this declaration emitted?" through
	// it would answer yes for every code. Walk the literal keys instead, and skip the
	// patterned ones — a `5XX` declaration is not a claim about any particular status.
	if op.Responses != nil {
		emittedSet := map[int]bool{}
		for _, s := range d.Emitted {
			emittedSet[s] = true
		}
		var keys []string
		for k := range op.Responses.Map() {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			// strconv.Atoi, not Sscanf("%d"): Sscanf stops at the first non-digit and
			// happily reads "5XX" as 5, so a patterned declaration would be reported
			// unemitted as status 5. Caught by the range case in this package's own test.
			status, err := strconv.Atoi(k)
			if err != nil {
				continue // `default`, `4XX`, `5XX`: not a claim about one status
			}
			if !emittedSet[status] {
				d.Unemitted = append(d.Unemitted, status)
			}
		}
	}
	return d
}

// Report renders the failing half of a set of diffs for a t.Fatalf. Empty when nothing is
// missing, so a caller can use it as the whole failure condition.
func Report(diffs []Diff) string {
	var lines []string
	for _, d := range diffs {
		if len(d.Missing) == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s (%s) can write %v; declared: %v; UNDECLARED: %v",
			d.Route, d.OperationID, d.Emitted, d.Declared, d.Missing))
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func dedupe(in []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}
