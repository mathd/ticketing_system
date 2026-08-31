package statusaudit

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// The matcher's arms, exercised on synthetic documents because the live specs cannot reach
// them all: no service operation declares `4XX`, `5XX` or `default:`, so those arms would
// ship as unfalsifiable claims if only the real documents tested them. That is the shape
// TKT-142's ai-review found twice on the sibling 400 invariant.
//
// Every document is run through Validate first. The services build their routers from these
// documents and panic on an invalid one, so an invariant "proved" on a document no router
// could serve would be proved about nothing.
func loadOp(t *testing.T, responses string) *openapi3.Operation {
	t.Helper()
	doc := "openapi: 3.0.3\ninfo: {title: t, version: '1'}\npaths:\n  /p:\n    get:\n      operationId: probe\n      responses:\n" + responses
	loader := openapi3.NewLoader()
	d, err := loader.LoadFromData([]byte(doc))
	if err != nil {
		t.Fatalf("load synthetic spec: %v", err)
	}
	if err := d.Validate(loader.Context); err != nil {
		t.Fatalf("synthetic spec is not a valid OpenAPI document, so proving anything on it "+
			"would prove nothing about a contract a router can serve: %v", err)
	}
	return d.Paths.Value("/p").Get
}

func TestDeclaresMirrorsTheValidatorsResolution(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responses string
		status    int
		want      bool
	}{
		// The exact key. The common case, and the only one the live documents exercise.
		{"exact status", "        '200': {description: ok}\n        '500': {description: boom}\n", 500, true},
		// A `5XX` range. ValidateResponse resolves it via Responses.Status(500), so it is
		// contract-correct — a matcher written against Responses.Value("500") would report
		// this operation as missing and send someone to "fix" a correct document.
		{"5XX range covers 500", "        '200': {description: ok}\n        '5XX': {description: boom}\n", 500, true},
		{"4XX range covers 404", "        '200': {description: ok}\n        '4XX': {description: nope}\n", 404, true},
		// `default:` alone. Nothing else reaches the Default() fallback — the range cases
		// above are satisfied by Status() — so deleting that fallback leaves every other
		// case green until this one exists.
		{"default alone covers anything", "        default: {description: whatever}\n", 503, true},
		// THE COMPLEMENT, and it is what stops the acceptance arms above from passing by
		// admitting everything. Without it, making Declares return true unconditionally
		// leaves the whole file green.
		{"no declaration at all", "        '200': {description: ok}\n", 500, false},
		// A 5XX range does NOT cover a 4XX status. The ranges are not a blanket.
		{"5XX does not cover 404", "        '200': {description: ok}\n        '5XX': {description: boom}\n", 404, false},
		// THE CASE THAT MOTIVATES CONCRETE STATUSES. An exact '500' declaration does not
		// cover an emitted 502 — which is precisely what the payments 500-class invariant
		// cannot see, and the reason this audit compares concrete codes rather than classes.
		{"exact 500 does not cover 502", "        '200': {description: ok}\n        '500': {description: boom}\n", 502, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Declares(loadOp(t, tc.responses).Responses, tc.status); got != tc.want {
				t.Fatalf("Declares(%d) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestAuditSplitsTheTwoDirections(t *testing.T) {
	op := loadOp(t, "        '200': {description: ok}\n        '404': {description: nope}\n        '500': {description: boom}\n")

	// An emitted status with no declaration is the failure ADR-028 punishes.
	d := Audit("probe", "GET /p", op, []int{200, 409})
	if len(d.Missing) != 1 || d.Missing[0] != 409 {
		t.Fatalf("an emitted-but-undeclared 409 must be reported missing, got %v", d.Missing)
	}
	if Report([]Diff{d}) == "" {
		t.Fatal("Report must render a diff carrying a missing status; an empty report is how a "+
			"caller decides the audit passed")
	}

	// The reverse direction is reported and must NOT be a failure: the adapters
	// under-approximate on purpose, so an unemitted declaration is at least as likely to be
	// an adapter blind spot as a stale document entry.
	d = Audit("probe", "GET /p", op, []int{200})
	if len(d.Missing) != 0 {
		t.Fatalf("declared-but-unemitted statuses must not be reported missing, got %v", d.Missing)
	}
	if len(d.Unemitted) != 2 || d.Unemitted[0] != 404 || d.Unemitted[1] != 500 {
		t.Fatalf("unemitted declarations must be reported for review, got %v", d.Unemitted)
	}
	if Report([]Diff{d}) != "" {
		t.Fatalf("a diff with no missing status must render an empty report, got %q", Report([]Diff{d}))
	}

	// Unemitted walks LITERAL keys only. A `default:` or `5XX` is not a claim about any one
	// status, so asking "was this declaration emitted?" through Declares would answer yes
	// for every code and the field would be permanently empty — inert, and inert in a way a
	// reader would mistake for "nothing is over-declared".
	ranged := loadOp(t, "        '200': {description: ok}\n        '5XX': {description: boom}\n")
	if d := Audit("probe", "GET /p", ranged, []int{200}); len(d.Unemitted) != 0 {
		t.Fatalf("a patterned declaration is not a claim about one status and must not be "+
			"reported unemitted, got %v", d.Unemitted)
	}
}
