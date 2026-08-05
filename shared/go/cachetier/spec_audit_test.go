package cachetier

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// The cross-service audit of ADR-004 tier declarations (TKT-204, COS 3).
//
// TWO THINGS IT DOES NOT PROVE, stated here because the registry it enforces
// could easily be read as broader coverage than it is:
//
//  1. It proves a declared value is a KNOWN tier. It never proves it is the RIGHT
//     tier for that endpoint. Tier assignment is unchanged and unchecked by this
//     ticket — an event list declaring the hours tier would pass.
//  2. It constrains what is DECLARED, not what is EMITTED. An endpoint that emits
//     a Cache-Control and declares nothing stays invisible to it. That is exactly
//     the defect ADR-004's TKT-128 amendment recorded, and it is still open.
//
// It lives here rather than in the smoke package on purpose: the smoke module is
// filtered out of GO_TEST_MODULES (Makefile) and only runs behind a Docker
// Compose stack (scripts/smoke.sh), so a static YAML audit placed there could not
// run without the whole system up. Nothing here imports a service package — the
// spec FILES are read, so the module dependency direction is untouched.

// specGlob finds the committed service contracts from this package's directory.
const specGlob = "../../../services/*/api/openapi.yaml"

// wantSpecs is the number of service contracts at HEAD. Asserted, not assumed: a
// relative path that silently matches nothing is a test that cannot fail. It also
// makes a sixth service an explicit decision — someone has to come here and think
// about its tiers.
const wantSpecs = 5

// freeFormAllowed is TKT-204's bounded legacy exception. Catalog's shared
// `CacheControl` response-header component is declared `type: string` with no
// enum, so it commits no value and there is nothing for this audit to check.
// These five operations are the ones that used it when the audit was introduced;
// all five carry a tier the registry knows (minutes for the four public reads,
// hours for the venue list), verified by their own handler tests rather than here.
//
// A sixth operation reusing a free-form declaration fails. The exception is
// therefore visible and bounded rather than a silent skip — and closing it
// entirely is a spec change (single-valued minutes/hours components), which is
// deliberately not this ticket's.
var freeFormAllowed = map[string]bool{
	"listPublicEvents":  true, // minutes
	"getPublicEvent":    true, // minutes
	"getPublicSeason":   true, // minutes
	"getPublicFestival": true, // minutes
	"listPublicVenues":  true, // hours
}

// specDocPath is the route serving a service's own contract document. Excluded
// categorically by route, not by its current byte: the served OpenAPI document is
// a static asset, and its TTL is not an ADR-004 data tier (ADR-004 § amendment
// TKT-128 says so in as many words). Services disagree on that byte today —
// catalog emits no-store, inventory five minutes — which is precisely why the
// exclusion cannot be keyed on the value.
const specDocPath = "/openapi.yaml"

// auditSpec reports every ADR-004 tier-declaration violation in one parsed
// contract. Taking a document rather than a path is what lets the negative
// fixtures below build synthetic specs in-test instead of mutating a tracked
// openapi.yaml — which would either dirty the tree or fail `make check-generate`.
func auditSpec(doc *openapi3.T) []string {
	var out []string
	if doc.Paths == nil {
		return out
	}
	for path, item := range doc.Paths.Map() {
		if path == specDocPath {
			continue
		}
		for method, op := range item.Operations() {
			if op.Responses == nil {
				continue
			}
			for code, respRef := range op.Responses.Map() {
				if respRef == nil || respRef.Value == nil {
					continue
				}
				h := respRef.Value.Headers["Cache-Control"]
				if h == nil || h.Value == nil {
					continue
				}
				where := fmt.Sprintf("%s %s %s (%s)", method, path, code, op.OperationID)
				var enum []any
				if h.Value.Schema != nil && h.Value.Schema.Value != nil {
					enum = h.Value.Schema.Value.Enum
				}
				if len(enum) == 0 {
					if !freeFormAllowed[op.OperationID] {
						out = append(out, where+": Cache-Control declared without an enum, and the "+
							"operation is not in the bounded legacy allowlist — declare a single ADR-004 tier value")
					}
					continue
				}
				for _, v := range enum {
					s, ok := v.(string)
					if !ok {
						out = append(out, fmt.Sprintf("%s: Cache-Control enum member %v is not a string", where, v))
						continue
					}
					if _, known := FromCacheControl(s); !known {
						out = append(out, fmt.Sprintf("%s: Cache-Control declares %q, which is not a registered ADR-004 tier", where, s))
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func loadDoc(t *testing.T, data []byte, what string) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(data)
	if err != nil {
		t.Fatalf("load %s: %v", what, err)
	}
	return doc
}

// TestDeclaredCacheControlValuesUseRegisteredTiers is the audit itself, over
// every committed service contract.
func TestDeclaredCacheControlValuesUseRegisteredTiers(t *testing.T) {
	specs, err := filepath.Glob(specGlob)
	if err != nil {
		t.Fatalf("glob %s: %v", specGlob, err)
	}
	if len(specs) != wantSpecs {
		t.Fatalf("glob %s matched %d specs (%v), want %d — the audit is only cross-service if it finds every service",
			specGlob, len(specs), specs, wantSpecs)
	}
	for _, path := range specs {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if v := auditSpec(loadDoc(t, data, path)); len(v) > 0 {
			for _, s := range v {
				t.Errorf("%s: %s", path, s)
			}
		}
	}
}

// TestCacheControlAuditRejectsNewFreeFormDeclaration is the negative that makes
// the allowlist bounded: without it the audit would pass any endpoint that
// declares Cache-Control as a bare string, which is the same as not declaring it.
func TestCacheControlAuditRejectsNewFreeFormDeclaration(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: synthetic, version: "1"}
paths:
  /public/things:
    get:
      operationId: listPublicThings
      responses:
        '200':
          description: ok
          headers:
            Cache-Control: {schema: {type: string}}
`
	v := auditSpec(loadDoc(t, []byte(spec), "synthetic free-form"))
	if len(v) != 1 {
		t.Fatalf("a new free-form Cache-Control must be rejected, got %d violations: %v", len(v), v)
	}

	// The same shape under an allowlisted operationId is the exception, and must
	// still pass — otherwise the audit would be rejecting on the wrong signal.
	const allowed = `
openapi: 3.0.3
info: {title: synthetic, version: "1"}
paths:
  /public/events:
    get:
      operationId: listPublicEvents
      responses:
        '200':
          description: ok
          headers:
            Cache-Control: {schema: {type: string}}
`
	if v := auditSpec(loadDoc(t, []byte(allowed), "synthetic allowlisted")); len(v) != 0 {
		t.Fatalf("an allowlisted operation must stay permitted, got %v", v)
	}
}

// TestCacheControlAuditRejectsUnregisteredTier proves the enum branch reports a
// real negative — a fixture whose every value is already legal could not.
func TestCacheControlAuditRejectsUnregisteredTier(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: synthetic, version: "1"}
paths:
  /public/things:
    get:
      operationId: getThing
      responses:
        '200':
          description: ok
          headers:
            Cache-Control:
              schema: {type: string, enum: ['public, max-age=60, s-maxage=60']}
`
	v := auditSpec(loadDoc(t, []byte(spec), "synthetic unknown tier"))
	if len(v) != 1 {
		t.Fatalf("an unregistered tier must be rejected, got %d violations: %v", len(v), v)
	}
}

// TestCacheControlAuditExcludesOpenAPISpecDocument: the static contract-document
// route is excluded by ROUTE. The fixture deliberately declares a value that
// would otherwise be rejected, so the test fails if the exclusion is keyed on the
// value instead of the path.
func TestCacheControlAuditExcludesOpenAPISpecDocument(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: synthetic, version: "1"}
paths:
  /openapi.yaml:
    get:
      operationId: getOpenAPISpec
      responses:
        '200':
          description: ok
          headers:
            Cache-Control:
              schema: {type: string, enum: ['public, max-age=42, s-maxage=42']}
`
	if v := auditSpec(loadDoc(t, []byte(spec), "synthetic spec document")); len(v) != 0 {
		t.Fatalf("the served contract document is not an ADR-004 data tier and must be excluded, got %v", v)
	}
}
