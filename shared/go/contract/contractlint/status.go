// Package contractlint checks source-derived HTTP behavior against OpenAPI declarations.
// Service packages provide only routing mode, response helpers, and explicit policy rules;
// parsing, reachability, coverage, and response matching live here.
package contractlint

import (
	"sort"

	"github.com/getkin/kin-openapi/openapi3"
)

// declares reports whether responses cover status the way openapi3filter.ValidateResponse
// resolves it: the exact key, then the patterned range (`4XX`/`5XX`), then `default`.
//
// Responses.Status already handles the exact-and-range pair, which is why this is not a
// string comparison — a matcher written against Responses.Value("500") would report a
// contract-correct `5XX` operation as missing, and one written against the range alone would
// accept a `500` declaration for an emitted 502. Both spellings are legal and mean different
// things, so the audit has to resolve them exactly as the validator does.
func declares(responses *openapi3.Responses, status int) bool {
	if responses == nil {
		return false
	}
	return responses.Status(status) != nil || responses.Default() != nil
}

// diff is one operation's source-derived statuses and missing declarations.
type diff struct {
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
	// Declared is the operation's response keys verbatim, including `default` and the
	// patterned ranges. Carried so a failure message can show what the document actually
	// says rather than making the reader open it.
	Declared []string
}

// audit diffs one operation. emitted may repeat and need not be sorted.
func audit(operationID, route string, op *openapi3.Operation, emitted []int) diff {
	d := diff{OperationID: operationID, Route: route, Emitted: dedupe(emitted)}
	if op.Responses != nil {
		for k := range op.Responses.Map() {
			d.Declared = append(d.Declared, k)
		}
		sort.Strings(d.Declared)
	}
	for _, s := range d.Emitted {
		if !declares(op.Responses, s) {
			d.Missing = append(d.Missing, s)
		}
	}
	return d
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
