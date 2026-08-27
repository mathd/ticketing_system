package cachetier

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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

// wantDeclarations is every SUCCESS response that declares a Cache-Control at
// HEAD, as `<service>/<operationId> <status>`.
//
// Without it the audit is only half a check: it constrains the values that ARE
// declared and says nothing about declarations that stop existing. Delete every
// Cache-Control from all five specs and a value-only audit reports zero
// violations while staying green — coverage silently reaching zero is exactly the
// emitted-versus-declared drift ADR-004's TKT-128 amendment recorded, so the
// audit must not be able to shrink quietly.
//
// **Every 2xx, plus every INLINE non-2xx, plus every shared response component
// that declares one.** The narrowing that is deliberately NOT made is by status:
// a `no-store` on an authentication 401 is a real tier decision — the contract
// requires the header there, and losing it could let a credential-validity answer
// become cacheable — so excluding non-2xx wholesale would let exactly that
// disappear unnoticed.
//
// What is excluded is repetition, not risk. Catalog attaches `NeverCacheControl`
// to a shared `StaffWriteUnauthorized` response which twenty-odd staff-write
// operations `$ref`; pinning each inheritance would mean editing this list for
// changes that make no tier decision at all, and a list nobody can read stops
// being read. So the shared response is pinned ONCE, by name (`<service>#<name>`),
// and its inheritors are not — deleting the header from the component still
// fails, which is the case that matters. An inline non-2xx (`authenticateStaff`
// 401) is nobody's inheritance and is pinned directly.
//
// The cost is that adding or removing a tier declaration means editing this list.
// That is the point: tier coverage becomes a decision someone states, rather than
// a side effect nothing observes.
var wantDeclarations = []string{
	"catalog#StaffWriteUnauthorized",
	"catalog/authenticateStaff 200",
	"catalog/authenticateStaff 401",
	// The staff-login rate limit (TKT-195, ai-review S4). Never tier: a 429 is
	// computed from a per-identifier budget, so a shared cache holding one would
	// refuse a staff member whose budget has since refilled — and serve the
	// refusal to everyone behind it.
	"catalog/authenticateStaff 429",
	"catalog/getPublicEvent 200",
	"catalog/getPublicFestival 200",
	"catalog/getPublicSeatMapGeometry 200",
	"catalog/getPublicSeason 200",
	"catalog/listPublicEvents 200",
	"catalog/listPublicVenues 200",
	"catalog/listSeatMapVersions 200",
	"catalog/listVenueSeatMaps 200",
	// TKT-214: fee resolution takes the same never tier as price resolution, and
	// for the same reason — it feeds a money decision whose correctness expires
	// at a known instant once effective windows are in play. It is an /internal/
	// operation rather than a public read (ADR-046 §6), which changes who may ask
	// but not how long the answer stays true.
	// TKT-235, the sales-channel registry. Two decisions, not one:
	//
	// listPublicChannels takes the MINUTES tier, like the four aggregated
	// storefront reads — a channel list is slow-moving organizer configuration
	// and a buyer seeing a retired channel for five minutes costs a rejected
	// hold, not a wrong price. It declares the new single-valued
	// MinutesCacheControl component, which since TKT-209 is how every catalog
	// public read declares its tier — the free-form alternative no longer exists.
	//
	// updateChannel takes NEVER, like every other write's response.
	"catalog/listPublicChannels 200",
	"catalog/resolveTicketTypeFees 200",
	"catalog/resolveTicketTypePrice 200",
	"catalog/updateChannel 200",
	"inventory/getAvailability 200",
	// The operator kill-switch (TKT-210). A never-tier declaration on a control
	// surface rather than a data read: a cached answer about whether a cache is
	// on is the wrong thing to hand an operator mid-incident.
	"inventory/getCacheControl 200",
	"inventory/putCacheControl 200",
	"inventory/getSeatOccupancy 200",
}

// specDocPath is the route serving a service's own contract document. Excluded
// categorically by route, not by its current byte: the served OpenAPI document is
// a static asset, and its TTL is not an ADR-004 data tier (ADR-004 § amendment
// TKT-128 says so in as many words). Services disagree on that byte today —
// catalog emits no-store, inventory five minutes — which is precisely why the
// exclusion cannot be keyed on the value.
const specDocPath = "/openapi.yaml"

// auditSpec reports every ADR-004 tier-declaration violation in one parsed
// contract, and — separately — every declaration it saw, as
// `<service>/<operationId> <status>`. The second return is what makes deletion
// detectable: violations alone go to zero when the declarations do.
//
// Taking a document rather than a path is what lets the negative fixtures below
// build synthetic specs in-test instead of mutating a tracked openapi.yaml —
// which would either dirty the tree or fail `make check-generate`.
func auditSpec(doc *openapi3.T, service string) (violations, declared []string) {
	var out []string
	if doc.Paths == nil {
		return out, declared
	}
	// Webhooks are a 3.1 construct and cannot appear in these 3.0.3 documents;
	// callbacks can, and this walk does not descend into them. So rather than
	// carry traversal code no committed spec exercises — untested machinery
	// guarding a case that does not exist — the audit asserts the absence. The day
	// someone adds a callback, this fails and they extend the walk with a real
	// document to test it against, instead of discovering years later that a
	// declaration was never covered.
	if len(doc.Webhooks) > 0 {
		out = append(out, "document declares webhooks, which this audit does not traverse — extend auditSpec")
	}
	for path, item := range doc.Paths.Map() {
		if path == specDocPath {
			continue
		}
		for method, op := range item.Operations() {
			if len(op.Callbacks) > 0 {
				out = append(out, fmt.Sprintf("%s %s (%s) declares callbacks, which this audit does not traverse — extend auditSpec",
					method, path, op.OperationID))
			}
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
				// A response reached through $ref is an inheritance, not a decision:
				// its declaration is pinned once at the component below. Everything
				// else — every success, and every inline non-2xx — is pinned here.
				if strings.HasPrefix(code, "2") || respRef.Ref == "" {
					declared = append(declared, fmt.Sprintf("%s/%s %s", service, op.OperationID, code))
				}
				var enum []any
				if h.Value.Schema != nil && h.Value.Schema.Value != nil {
					enum = h.Value.Schema.Value.Enum
				}
				// TKT-209 deleted TKT-204's bounded legacy exception, so this is now
				// unconditional: catalog's free-form `CacheControl` component is gone
				// and every declaration commits a single tier. The `declared` append
				// above stays HERE rather than moving into auditHeaderValue with the
				// value rules — it is the coverage mechanism wantDeclarations rests
				// on, and folding it into the shared helper is how it goes missing.
				if len(enum) == 0 {
					out = append(out, where+": Cache-Control declared without an enum — "+
						"declare a single ADR-004 tier value")
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
	// Shared response components, audited and pinned by name. This is what makes
	// deleting the Cache-Control from StaffWriteUnauthorized fail, without pinning
	// each of the twenty-odd operations that inherit it.
	if doc.Components != nil {
		for name, respRef := range doc.Components.Responses {
			if respRef == nil || respRef.Value == nil {
				continue
			}
			h := respRef.Value.Headers["Cache-Control"]
			if h == nil || h.Value == nil {
				continue
			}
			declared = append(declared, fmt.Sprintf("%s#%s", service, name))
			out = append(out, auditHeaderValue(h, fmt.Sprintf("components.responses.%s", name))...)
		}
	}
	sort.Strings(out)
	sort.Strings(declared)
	return out, declared
}

// auditHeaderValue is the value check, shared by the operation walk and the
// component walk so a shared response cannot declare a tier the registry does not
// know just by being reached from a different direction.
func auditHeaderValue(h *openapi3.HeaderRef, where string) []string {
	var out []string
	var enum []any
	if h.Value.Schema != nil && h.Value.Schema.Value != nil {
		enum = h.Value.Schema.Value.Enum
	}
	if len(enum) == 0 {
		return []string{where + ": Cache-Control declared without an enum — declare a single ADR-004 tier value"}
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
	var declared []string
	for _, path := range specs {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// services/<service>/api/openapi.yaml — the service name scopes the coverage
		// list, because an operationId is unique within one contract and nothing
		// more: `convertOperationalHold` and `drawDownGroupReservation` each appear
		// in two specs at HEAD. (It scoped TKT-204's allowlist too, until TKT-209
		// deleted that exception.)
		service := filepath.Base(filepath.Dir(filepath.Dir(path)))
		v, d := auditSpec(loadDoc(t, data, path), service)
		for _, s := range v {
			t.Errorf("%s: %s", path, s)
		}
		declared = append(declared, d...)
	}

	sort.Strings(declared)
	// Sorted here rather than relying on the literal being in order — a future
	// editor adding an entry should not be able to fail the audit by putting it on
	// the wrong line.
	want := slices.Sorted(slices.Values(wantDeclarations))
	if !slices.Equal(declared, want) {
		t.Errorf("declared ADR-004 tiers drifted.\n got: %v\nwant: %v\n"+
			"Adding or removing a tier declaration is a decision — state it in wantDeclarations.",
			declared, want)
	}
}

// TestCacheControlAuditRejectsFreeFormDeclaration is the negative that makes the
// value rule real: without it the audit would pass any endpoint declaring
// Cache-Control as a bare string, which is the same as not declaring one.
//
// TKT-209 made the rule SERVICE-INDEPENDENT. TKT-204's bounded legacy exception
// (freeFormAllowed) let catalog's five public reads declare a free-form header;
// that component is gone and so is the allowlist, so a free-form declaration is
// now a violation wherever it appears.
//
// The two services below are therefore NOT a scoping test — the service argument
// can no longer change the outcome, which is exactly the point. They are a pin
// that the exception has not come back: reintroduce an allowlist keyed on
// catalog and the first case goes green while the second stays red, which no
// single-service fixture could distinguish.
func TestCacheControlAuditRejectsFreeFormDeclaration(t *testing.T) {
	const spec = `
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
	// listPublicEvents under catalog is deliberately the fixture: it is the exact
	// service/operation pair that WAS allowlisted, so this fails the moment the
	// exception returns in any form.
	doc := loadDoc(t, []byte(spec), "synthetic free-form")
	for _, service := range []string{"catalog", "payments"} {
		v, _ := auditSpec(doc, service)
		if len(v) != 1 {
			t.Fatalf("%s: a free-form Cache-Control must be rejected, got %d violations: %v", service, len(v), v)
		}
	}
}

// TestCacheControlAuditReportsDeclarationCoverage is the mechanism the coverage
// assertion in TestDeclaredCacheControlValuesUseRegisteredTiers rests on. Without
// it the audit would be only half a check: deleting every declaration from every
// spec drives the violation count to zero, so a value-only audit stays green
// while auditing nothing.
//
// It also pins the exclusion rule, which is about REPETITION, not status: an
// inline non-2xx (an authentication 401 saying no-store) is a real tier decision
// and is pinned; a non-2xx reached through $ref is an inheritance, pinned once at
// its shared component instead of once per inheriting operation.
func TestCacheControlAuditReportsDeclarationCoverage(t *testing.T) {
	const declaring = `
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
            Cache-Control: {schema: {type: string, enum: ['no-store']}}
        '401':
          description: inline, so a decision of its own
          headers:
            Cache-Control: {schema: {type: string, enum: ['no-store']}}
        '403':
          $ref: '#/components/responses/Shared'
components:
  responses:
    Shared:
      description: inherited by anyone who refs it
      headers:
        Cache-Control: {schema: {type: string, enum: ['no-store']}}
`
	_, d := auditSpec(loadDoc(t, []byte(declaring), "synthetic declaring"), "catalog")
	want := []string{"catalog#Shared", "catalog/getThing 200", "catalog/getThing 401"}
	if !slices.Equal(d, want) {
		t.Fatalf("coverage must pin the success, the INLINE 401 and the shared component once —\n got: %v\nwant: %v", d, want)
	}

	// The same operation with the declaration removed must vanish from coverage —
	// this is what makes a deletion fail the audit rather than pass it silently.
	const deleted = `
openapi: 3.0.3
info: {title: synthetic, version: "1"}
paths:
  /public/things:
    get:
      operationId: getThing
      responses:
        '200': {description: ok}
`
	v, d := auditSpec(loadDoc(t, []byte(deleted), "synthetic deleted"), "catalog")
	if len(v) != 0 {
		t.Fatalf("a removed declaration is not a VALUE violation, got %v", v)
	}
	if len(d) != 0 {
		t.Fatalf("a removed declaration must disappear from coverage, got %v", d)
	}
}

// TestCacheControlAuditRefusesUntraversedLocations: a Cache-Control can also be
// declared inside a callback, which this audit does not walk. No committed spec
// has one (and webhooks are a 3.1 construct these 3.0.3 documents cannot express
// at all), so the audit refuses rather than pretending to cover them — the gap
// becomes a loud failure at the moment it is first reachable, instead of a
// declaration that was silently never pinned.
func TestCacheControlAuditRefusesUntraversedLocations(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: synthetic, version: "1"}
paths:
  /things:
    post:
      operationId: createThing
      responses:
        '201': {description: ok}
      callbacks:
        onDone:
          '{$request.body#/url}':
            post:
              operationId: onDone
              responses:
                '401':
                  description: a tier declaration this walk would never see
                  headers:
                    Cache-Control: {schema: {type: string, enum: ['no-store']}}
`
	v, d := auditSpec(loadDoc(t, []byte(spec), "synthetic callback"), "catalog")
	if len(v) != 1 {
		t.Fatalf("a callback must be refused, not silently skipped, got %d violations: %v", len(v), v)
	}
	if len(d) != 0 {
		t.Fatalf("the callback's declaration must not enter coverage — it is not traversed: %v", d)
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
	v, _ := auditSpec(loadDoc(t, []byte(spec), "synthetic unknown tier"), "inventory")
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
	if v, d := auditSpec(loadDoc(t, []byte(spec), "synthetic spec document"), "catalog"); len(v) != 0 || len(d) != 0 {
		t.Fatalf("the served contract document is not an ADR-004 data tier and must be excluded, got %v", v)
	}
}
