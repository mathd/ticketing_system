package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TKT-203 / ADR-068. The back office gets an access credential so a box-office agent
// can re-send a completed order's tickets, and the whole risk of this ticket is that
// the credential opens more than that.
//
// Access's internal surface also carries the refund route, which VOIDS an order's
// tickets. A test that only shows redelivery working proves nothing about that one, so
// this file enumerates it: every internal operation except redelivery must refuse the
// staff credential.
//
// The enumeration exists because a route-level test alone cannot catch the failure it
// is for. ADR-053 §Decision records the precedent: there, widening a supposedly narrow
// allowance to a whole prefix killed NO test, because the hand-mounted handlers carried
// their own checks — the guard stopped being narrow while every route-level test stayed
// green. Hence the router walk below.
//
// Each request is otherwise VALID — real uuids, the required body, the required
// Idempotency-Key — because the OpenAPI request validator runs BEFORE the handler and
// answers 400. A fixture the validator rejects would be refused without the credential
// check ever running, and would pass just as happily with the credential wired to every
// route. Commerce's equivalent needed three attempts to get this right.
//
// The expected refusal is 404, which is ACCESS's answer on its internal surface, not
// the 401 inventory gives. Copying inventory's expectation here would assert nothing:
// this handler never returns 401, so the assertion would pass against a wide-open
// credential. ADR-043 records why the two services differ.
const (
	staffTok    = "access-staff-write-credential"
	internalTok = "internal-service-credential"
	orderUUID   = "00000000-0000-0000-0000-000000000001"
	orgUUID     = "00000000-0000-0000-0000-000000000002"
	refundUUID  = "00000000-0000-0000-0000-000000000003"
)

type internalOp struct {
	name string
	verb string
	// routeTemplate is chi's pattern, compared against the real router.
	routeTemplate string
	path          string
	body          string
	key           bool // sends Idempotency-Key
}

// everyOtherInternalOperation mirrors registerRoutes' internal registrations minus the
// one above. If access grows another internal route it belongs here — the router walk
// below exists to make that omission loud rather than silent.
func everyOtherInternalOperation() []internalOp {
	refund := `{"organizer_id":"` + orgUUID + `","refund_id":"` + refundUUID + `","quantity":1}`
	return []internalOp{
		{"refundTickets", http.MethodPost, "/internal/orders/{id}/refunds",
			"/internal/orders/" + orderUUID + "/refunds", refund, false},
	}
}

func serveWith(t *testing.T, op internalOp, header, value string) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Buffer
	if op.body != "" {
		body = bytes.NewBufferString(op.body)
	} else {
		body = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(op.verb, op.path, body)
	req.Header.Set(header, value)
	if op.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if op.key {
		req.Header.Set("Idempotency-Key", "staff-credential-probe")
	}
	res := httptest.NewRecorder()
	credentialServer(t).Router(nil, true).ServeHTTP(res, req)
	return res
}

// credentialServer builds the Server with BOTH credentials configured and distinct.
func credentialServer(t *testing.T) *Server {
	t.Helper()
	return newTestServer(nil, nil, internalTok).WithStaffWriteCredential(staffTok)
}

// internalRoutesFromRouter walks the REAL chi router. A hand-maintained list cannot
// detect the drift it exists to catch.
func internalRoutesFromRouter(t *testing.T) []string {
	t.Helper()
	r := chi.NewRouter()
	credentialServer(t).registerRoutes(r)
	var found []string
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/internal/") {
			found = append(found, method+" "+route)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk router: %v", err)
	}
	sort.Strings(found)
	return found
}

// The enumeration must cover every internal route the router actually serves. This is
// the test that makes narrowness checkable rather than asserted: an internal route
// added later and not listed here would never be probed with the staff credential.
func TestTheEnumerationCoversEveryInternalRouteAccessServes(t *testing.T) {
	onRouter := internalRoutesFromRouter(t)

	covered := map[string]bool{}
	for route := range staffWriteOperations {
		covered[route] = true
	}
	for _, op := range everyOtherInternalOperation() {
		covered[op.verb+" "+op.routeTemplate] = true
	}

	var unproven, stale []string
	for _, route := range onRouter {
		if !covered[route] {
			unproven = append(unproven, route)
		}
		delete(covered, route)
	}
	for route := range covered {
		stale = append(stale, route)
	}
	sort.Strings(stale)
	sort.Strings(unproven)

	if len(unproven) > 0 {
		t.Errorf("these internal routes are not covered by the credential enumeration — "+
			"the staff credential could open them and nothing here would notice: %v", unproven)
	}
	if len(stale) > 0 {
		t.Errorf("these enumeration entries name routes access no longer serves, so their "+
			"probes prove nothing: %v", stale)
	}
	// A floor: if the walk itself breaks and returns nothing, every other assertion
	// in this file passes vacuously.
	if len(onRouter) < 2 {
		t.Errorf("the walk found %d internal routes, which is fewer than access has — "+
			"the walk itself is broken: %v", len(onRouter), onRouter)
	}
}

// The narrowness assertion. ADR-068 grants exactly one operation; the staff credential
// must satisfy the guard on that one and on nothing else access serves.
//
// THIS DRIVES THE REAL ROUTER and asks the GUARD what it decided, rather than reading
// the response status. Both internal routes refuse with 404, and refundTickets carries
// its own inline check against the shared token — so a staff credential granted the
// refund route leaves it answering the same 404 for the second reason while the first
// has silently widened. A status assertion cannot see that: verified by mutation, which
// is how this version of the test came to exist, and it is ADR-053's recorded failure
// exactly.
func TestStaffCredentialSatisfiesTheGuardOnRedeliveryAlone(t *testing.T) {
	for _, op := range everyOtherInternalOperation() {
		t.Run(op.name, func(t *testing.T) {
			if grantedTo(t, op, staffWriteHeader, staffTok) {
				t.Fatalf("the staff credential satisfies the guard on %s %s: it is granted "+
					"for redelivery alone. That %s also refuses on its own check is not a "+
					"defence — the grant is what widened, and the inner check is what "+
					"hides it", op.verb, op.routeTemplate, op.name)
			}
		})
	}
}

// grantedTo reports what the credential guard answers for one operation, asked from
// the position production asks it: INSIDE the handler, after chi has matched.
//
// Position is load-bearing and cost a debugging cycle. The guard's staff arm is keyed
// on the matched route pattern, and chi populates that context during routing — so a
// probe wrapped as MIDDLEWARE sees an empty pattern and the guard answers false for
// every route, passing vacuously against a credential granted everything. The handler
// registered here stands exactly where the real handler stands.
//
// It re-registers the routes rather than calling registerRoutes, because the question
// is what the guard says for a given pattern, and the real handlers would run their own
// logic against a nil store on the way to answering it.
func grantedTo(t *testing.T, op internalOp, header, value string) bool {
	t.Helper()
	s := credentialServer(t)
	var granted bool
	r := chi.NewRouter()
	r.MethodFunc(op.verb, op.routeTemplate, func(_ http.ResponseWriter, req *http.Request) {
		granted = s.staffOrInternal(req)
	})

	var body *bytes.Buffer
	if op.body != "" {
		body = bytes.NewBufferString(op.body)
	} else {
		body = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(op.verb, op.path, body)
	req.Header.Set(header, value)
	if op.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if op.key {
		req.Header.Set("Idempotency-Key", "staff-credential-probe")
	}
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)
	if res.Code == http.StatusNotFound && !granted {
		// The probe never ran: the fixture did not match the route it names, so the
		// answer below would be "not granted" for a reason that has nothing to do
		// with the credential. A silent vacuous pass is the failure mode this whole
		// file exists to avoid.
		t.Fatalf("the probe fixture for %s %s did not reach a handler (path %s)",
			op.verb, op.routeTemplate, op.path)
	}
	return granted
}

// The other direction, and it is a separate predicate rather than the same one read
// backwards: the credential must actually OPEN the operation it is for. A guard that
// refuses everything would pass the narrowness test above and ship a dead feature.
//
// 404 is the refusal, so "not 404" is what proves the credential was accepted. The
// request then fails further in (no store), which is fine — what is being asserted is
// that it got PAST the guard, not what it did afterwards.
func TestStaffCredentialOpensRedelivery(t *testing.T) {
	op := internalOp{
		name: "redeliverOrderTickets", verb: http.MethodPost,
		routeTemplate: "/internal/orders/{id}/redeliveries",
		path:          "/internal/orders/" + orderUUID + "/redeliveries",
		body:          `{"organizer_id":"` + orgUUID + `"}`, key: true,
	}
	if res := serveWith(t, op, staffWriteHeader, staffTok); res.Code == http.StatusNotFound {
		t.Fatal("the staff credential was refused on the one operation ADR-068 grants it")
	}
	// And the shared internal token still works: ADR-068 adds a credential, it does
	// not replace the one every service-to-service caller already presents.
	if res := serveWith(t, op, "X-Internal-Token", internalTok); res.Code == http.StatusNotFound {
		t.Fatal("the shared internal token was refused on the redelivery route")
	}
}

// Each predicate of the guard, separately. An earlier refusal short-circuits the rest,
// so a single "it was refused" case cannot tell which predicate did the refusing —
// each of these fails for its own reason and each must be mutated on its own.
func TestRedeliveryRefusesEveryWrongCredential(t *testing.T) {
	op := internalOp{
		name: "redeliverOrderTickets", verb: http.MethodPost,
		routeTemplate: "/internal/orders/{id}/redeliveries",
		path:          "/internal/orders/" + orderUUID + "/redeliveries",
		body:          `{"organizer_id":"` + orgUUID + `"}`, key: true,
	}
	cases := []struct{ name, header, value string }{
		{"no credential at all", "X-Unrelated", "nothing"},
		{"the staff credential in the wrong header", "X-Internal-Token", staffTok},
		{"the internal token in the staff header", staffWriteHeader, internalTok},
		{"a wrong value in the staff header", staffWriteHeader, "not-the-credential"},
		{"an empty staff credential", staffWriteHeader, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if res := serveWith(t, op, c.header, c.value); res.Code != http.StatusNotFound {
				t.Fatalf("answered %d, want 404", res.Code)
			}
		})
	}
}

// A server with NO staff credential configured must refuse the staff header, not admit
// everyone presenting an empty one. This is the fail-closed case, and it is the one
// that only shows up in a deployment whose env is missing — the configuration nobody
// exercises being the one that admits everybody.
func TestUnconfiguredStaffCredentialRefusesRatherThanAdmits(t *testing.T) {
	r := chi.NewRouter()
	unconfigured := newTestServer(nil, nil, internalTok) // no WithStaffWriteCredential
	unconfigured.registerRoutes(r)

	for _, presented := range []string{"", "anything", staffTok} {
		req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+orderUUID+"/redeliveries",
			bytes.NewBufferString(`{"organizer_id":"`+orgUUID+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "unconfigured-probe")
		req.Header.Set(staffWriteHeader, presented)
		res := httptest.NewRecorder()
		unconfigured.Router(nil, true).ServeHTTP(res, req)
		if res.Code != http.StatusNotFound {
			t.Fatalf("a server with no staff credential answered %d to %q, want 404", res.Code, presented)
		}
	}
}
