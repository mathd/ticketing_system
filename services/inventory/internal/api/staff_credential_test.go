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

// TKT-244 / ADR-057. The back office gets an inventory credential so an operator can
// edit a slot's channel allocations, and the whole risk of this ticket is that the
// credential opens more than that.
//
// Inventory's internal surface also carries holds and their transitions, operational
// holds, group reservations and draw-down, capacity adjustments and the availability
// cache kill-switch. A test that only shows the allocation editor working proves nothing
// about those, so this file ENUMERATES them: every internal operation except the two the
// editor needs must refuse the staff credential.
//
// The enumeration exists because a route-level test alone cannot catch the failure it is
// for. ADR-053 §Decision records the precedent: there, widening a supposedly narrow
// allowance to a whole prefix killed NO test, because the hand-mounted handlers carried
// their own credential checks — the guard stopped being narrow while every route-level
// test stayed green. Hence the router walk below.
//
// Each request is otherwise VALID — real uuids, the required body, the required
// Idempotency-Key — because the OpenAPI request validator runs BEFORE the handler and
// answers 400. A fixture the validator rejects would be refused without the credential
// check ever running, and would pass just as happily with the credential wired to every
// route. Commerce's equivalent (TKT-191/TKT-194) needed three attempts to get this right.
const (
	staffTok     = "inventory-staff-credential"
	internalTok  = "internal-service-credential"
	slotUUID     = "00000000-0000-0000-0000-000000000001"
	orgUUID      = "00000000-0000-0000-0000-000000000002"
	otherUUID    = "00000000-0000-0000-0000-000000000003"
	ttypeUUID    = "00000000-0000-0000-0000-000000000004"
	resellerUUID = "00000000-0000-0000-0000-000000000005"
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

// theTwoOperationsTheEditorNeeds is the allowlist ADR-057 grants the staff credential:
// the page cannot function with the write alone, because showing CURRENT CONSUMPTION is
// a condition of success and only the staff availability read reports it.
var theTwoOperationsTheEditorNeeds = map[string]bool{
	"GET /internal/slots/{id}/availability":          true,
	"PUT /internal/slots/{id}/channel-allocations":   true,
}

// everyOtherInternalOperation mirrors Router's internal registrations minus the two
// above. If inventory grows another internal route it belongs here — the router walk
// below exists to make that failure loud rather than silent.
func everyOtherInternalOperation() []internalOp {
	opHold := `{"organizer_id":"` + orgUUID + `","slot_id":"` + slotUUID + `","quantity":1,` +
		`"purpose":"house","label":"front row","actor":"staff:amy","reason":"hold back"}`
	opRelease := `{"organizer_id":"` + orgUUID + `","quantity":1,"actor":"staff:amy","reason":"done"}`
	conversion := `{"organizer_id":"` + orgUUID + `","slot_id":"` + slotUUID + `","quantity":1,` +
		`"ticket_type_id":"` + ttypeUUID + `","unit_amount":100,"currency":"EUR",` +
		`"actor":"staff:amy","reason":"walk-up"}`
	groupReservation := `{"organizer_id":"` + orgUUID + `","slot_id":"` + slotUUID + `","quantity":1,` +
		`"counterparty":"Acme Tours","expires_at":"2099-01-01T00:00:00Z",` +
		`"actor":"staff:amy","reason":"group booking"}`
	refundCapacity := `{"organizer_id":"` + orgUUID + `","refund_id":"` + otherUUID + `","quantity":1}`
	return []internalOp{
		{"createInternalHold", http.MethodPost, "/internal/holds", "/internal/holds",
			`{"organizer_id":"` + orgUUID + `","slot_id":"` + slotUUID + `","quantity":1,"unit_amount":100,"reseller_id":"` + resellerUUID + `"}`, true},
		{"confirmHold", http.MethodPost, "/internal/holds/{id}/confirm", "/internal/holds/" + slotUUID + "/confirm?organizer_id=" + orgUUID, "", false},
		{"finalizeHold", http.MethodPost, "/internal/holds/{id}/finalize", "/internal/holds/" + slotUUID + "/finalize?organizer_id=" + orgUUID, "", false},
		{"releaseHold", http.MethodPost, "/internal/holds/{id}/release", "/internal/holds/" + slotUUID + "/release?organizer_id=" + orgUUID, "", false},
		{"refundCapacity", http.MethodPost, "/internal/holds/{id}/refund-capacity", "/internal/holds/" + slotUUID + "/refund-capacity", refundCapacity, true},
		{"holdSeating", http.MethodGet, "/internal/holds/{id}/seating", "/internal/holds/" + slotUUID + "/seating?organizer_id=" + orgUUID, "", false},
		{"placeOperationalHold", http.MethodPost, "/internal/operational-holds", "/internal/operational-holds", opHold, true},
		{"releaseOperationalHold", http.MethodPost, "/internal/operational-holds/{id}/release", "/internal/operational-holds/" + slotUUID + "/release", opRelease, true},
		{"convertOperationalHold", http.MethodPost, "/internal/operational-holds/{id}/convert", "/internal/operational-holds/" + slotUUID + "/convert", conversion, true},
		{"operationalHoldHistory", http.MethodGet, "/internal/operational-holds/{id}/history", "/internal/operational-holds/" + slotUUID + "/history?organizer_id=" + orgUUID, "", false},
		{"placeGroupReservation", http.MethodPost, "/internal/group-reservations", "/internal/group-reservations", groupReservation, true},
		{"drawDownGroupReservation", http.MethodPost, "/internal/group-reservations/{id}/draw-down", "/internal/group-reservations/" + slotUUID + "/draw-down", conversion, true},
		{"groupReservationHistory", http.MethodGet, "/internal/group-reservations/{id}/history", "/internal/group-reservations/" + slotUUID + "/history?organizer_id=" + orgUUID, "", false},
		{"adjustCapacity", http.MethodPost, "/internal/slots/{id}/capacity-adjustments", "/internal/slots/" + slotUUID + "/capacity-adjustments",
			`{"organizer_id":"` + orgUUID + `","capacity":10,"actor":"staff:amy","reason":"more room"}`, true},
		{"capacityHistory", http.MethodGet, "/internal/slots/{id}/capacity-adjustments", "/internal/slots/" + slotUUID + "/capacity-adjustments?organizer_id=" + orgUUID, "", false},
		{"getCacheControl", http.MethodGet, "/internal/cache-control", "/internal/cache-control", "", false},
		{"putCacheControl", http.MethodPut, "/internal/cache-control", "/internal/cache-control", `{"enabled":true}`, false},
	}
}

// serveWithStaffCredential drives one operation presenting ONLY the staff credential.
func serveWithStaffCredential(t *testing.T, op internalOp) *httptest.ResponseRecorder {
	t.Helper()
	return serveWith(t, op, staffWriteHeader, staffTok)
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
	server(t).Router(nil, true).ServeHTTP(res, req)
	return res
}

// server builds the Server under test with BOTH credentials configured and distinct.
func server(t *testing.T) *Server {
	t.Helper()
	return New(nil, internalTok, nil).WithStaffWriteCredential(staffTok)
}

// internalRoutesFromRouter walks the REAL chi router. A hand-maintained list cannot
// detect the drift it exists to catch.
func internalRoutesFromRouter(t *testing.T) []string {
	t.Helper()
	r := chi.NewRouter()
	server(t).registerRoutes(r)
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
// the test that makes narrowness checkable rather than asserted: an internal route added
// later and not listed here would never be probed with the staff credential.
func TestTheEnumerationCoversEveryInternalRouteInventoryServes(t *testing.T) {
	onRouter := internalRoutesFromRouter(t)

	covered := map[string]bool{}
	for route := range theTwoOperationsTheEditorNeeds {
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
		t.Errorf("these enumeration entries name routes inventory no longer serves, so their "+
			"probes prove nothing: %v", stale)
	}
	// A floor: if the walk itself breaks and returns nothing, every other assertion in
	// this file passes vacuously.
	if len(onRouter) < 15 {
		t.Errorf("the walk found %d internal routes, which is fewer than inventory has — "+
			"the walk itself is broken: %v", len(onRouter), onRouter)
	}
}

// The narrowness assertion. ADR-057 grants exactly two operations; everything else on
// inventory's internal surface must refuse this credential.
func TestStaffCredentialOpensNoInternalOperationButTheEditorsTwo(t *testing.T) {
	for _, op := range everyOtherInternalOperation() {
		t.Run(op.name, func(t *testing.T) {
			res := serveWithStaffCredential(t, op)
			// 401, the same answer a wrong internal token gets (ADR-043: inventory
			// refuses its internal surface with 401, where commerce uses 404 — do not
			// copy commerce's expectation here).
			//
			// A 400 would mean the request validator refused the fixture and the
			// credential check never ran, so the probe proved nothing.
			if res.Code != http.StatusUnauthorized {
				t.Errorf("staff credential got %d from %s; want 401. Body: %.200s",
					res.Code, op.name, res.Body.String())
			}
		})
	}
}

// theGrantedOperations are the two ADR-057 opens. Both are driven with a body the
// validator ACCEPTS but a slot id no store would resolve, so reaching the handler is
// observable without a database.
func theGrantedOperations() []internalOp {
	return []internalOp{
		{"getStaffAvailability", http.MethodGet, "/internal/slots/{id}/availability",
			"/internal/slots/" + slotUUID + "/availability?organizer_id=" + orgUUID, "", false},
		{"replaceChannelAllocations", http.MethodPut, "/internal/slots/{id}/channel-allocations",
			"/internal/slots/" + slotUUID + "/channel-allocations",
			`{"organizer_id":"` + orgUUID + `","allocations":[{"channel":"reseller-acme","cap":5}]}`, false},
	}
}

// pastTheGuard reports whether a request reached the handler.
//
// New(nil, …) has no database, so a handler that runs panics on the nil store; chi's
// Recoverer is not mounted here, so the panic surfaces as a 500 through httptest only if
// recovered. Rather than depend on that, this drives the guard DIRECTLY: it asserts the
// wrapper's decision, which is the half this file owns. That the operation then behaves
// correctly is proven end to end in the smoke suite.
func pastTheGuard(t *testing.T, op internalOp, header, value string) bool {
	t.Helper()
	reached := false
	s := server(t)
	h := s.staffOrInternal(func(http.ResponseWriter, *http.Request) { reached = true })
	req := httptest.NewRequest(op.verb, op.path, strings.NewReader(op.body))
	req.Header.Set(header, value)
	h(httptest.NewRecorder(), req)
	return reached
}

// The complement to the narrowness test: the two operations the editor needs are NOT
// refused for want of a credential.
func TestStaffCredentialIsAcceptedByTheEditorsTwoOperations(t *testing.T) {
	for _, op := range theGrantedOperations() {
		t.Run(op.name, func(t *testing.T) {
			if !pastTheGuard(t, op, staffWriteHeader, staffTok) {
				t.Errorf("staff credential was refused by %s, which ADR-057 grants it", op.name)
			}
		})
	}
}

// X-Internal-Token must keep working on those same two operations. Four smoke files
// (eight call sites) present it; this is an ADDITIONAL accepted credential, not a
// replacement.
//
// The count was "five smoke drivers and every service-to-service caller" until TKT-250
// checked it: there is NO service-to-service caller of the allocation route, and there
// are four smoke files, not five. The wrong number is what made TKT-250 look like it had
// to keep the revision optional for compatibility.
func TestInternalTokenStillOpensTheEditorsTwoOperations(t *testing.T) {
	for _, op := range theGrantedOperations() {
		t.Run(op.name, func(t *testing.T) {
			if !pastTheGuard(t, op, "X-Internal-Token", internalTok) {
				t.Errorf("the shared internal token was refused by %s", op.name)
			}
		})
	}
}

// A wrong or absent staff credential is refused on the granted operations too — through
// the FULL router, so the refusal is proven where a caller meets it. The store is never
// reached, so a nil one is safe here.
func TestTheGrantedOperationsRefuseAWrongOrMissingStaffCredential(t *testing.T) {
	for _, op := range theGrantedOperations() {
		for _, token := range []string{"", "wrong", internalTok + "x"} {
			res := serveWith(t, op, staffWriteHeader, token)
			if res.Code != http.StatusUnauthorized {
				t.Errorf("%s with staff token %q: status=%d want=401", op.name, token, res.Code)
			}
		}
	}
}

// An UNCONFIGURED staff credential opens nothing — a service started without one must
// refuse everyone rather than admit anyone presenting nothing. Both arms fail closed:
// the empty presented value and a non-empty one against an empty configured value.
func TestAnUnconfiguredStaffCredentialOpensNothing(t *testing.T) {
	unconfigured := New(nil, internalTok, nil) // staff credential deliberately unset
	for _, op := range theGrantedOperations() {
		for _, token := range []string{"", "anything"} {
			reached := false
			h := unconfigured.staffOrInternal(func(http.ResponseWriter, *http.Request) { reached = true })
			req := httptest.NewRequest(op.verb, op.path, strings.NewReader(op.body))
			req.Header.Set(staffWriteHeader, token)
			res := httptest.NewRecorder()
			h(res, req)
			if reached || res.Code != http.StatusUnauthorized {
				t.Errorf("%s: unconfigured staff credential admitted token %q (reached=%v status=%d)",
					op.name, token, reached, res.Code)
			}
		}
	}
}
