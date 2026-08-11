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

// TKT-194. The back office gets a commerce credential so it can refund, and the
// whole risk of this ticket is that the credential opens more than that.
//
// Commerce's internal surface also carries exchanges, cancellation-refund runs,
// operational-hold conversion, group draw-down and the buyer delivery-email
// read. A test that only shows the refund working proves nothing about those,
// so this one ENUMERATES them: every internal operation except the refund must
// refuse the staff credential.
//
// Each request below is otherwise VALID — real uuids, the required body, the
// required Idempotency-Key — because the request validator runs before the
// handler and answers 400. A fixture the validator rejects would be refused
// without the credential check ever running, and would pass just as happily
// with the credential wired to every route. TKT-191 needed three attempts to
// get exactly this right.
const (
	staffTok    = "commerce-staff-credential"
	internalTok = "internal-service-credential"
	someUUID    = "00000000-0000-0000-0000-000000000001"
	otherUUID   = "00000000-0000-0000-0000-000000000002"
)

type internalOp struct {
	name   string
	method string
	// routeTemplate is chi's pattern, compared against the real router.
	routeTemplate string
	path          string
	body          string
	key           bool // sends Idempotency-Key
}

// everyInternalOperationExceptRefund mirrors Router's internal registrations.
// If commerce grows another internal route it belongs here — the count
// assertion below exists to make that failure loud rather than silent.
func everyInternalOperationExceptRefund() []internalOp {
	conversion := `{"organizer_id":"` + someUUID + `","ticket_type_id":"` + otherUUID + `","quantity":1,"actor":"staff:amy","reason":"walk-up"}`
	return []internalOp{
		{"convertOperationalHold", http.MethodPost, "/internal/operational-holds/{id}/convert", "/internal/operational-holds/" + someUUID + "/convert", conversion, true},
		{"drawDownGroupReservation", http.MethodPost, "/internal/group-reservations/{id}/draw-down", "/internal/group-reservations/" + someUUID + "/draw-down", conversion, true},
		{"exchangeOrder", http.MethodPost, "/internal/orders/{id}/exchanges", "/internal/orders/" + someUUID + "/exchanges",
			`{"organizer_id":"` + someUUID + `","target_ticket_type_id":"` + otherUUID + `","actor":"staff:amy","reason":"upgrade"}`, true},
		{"exchangeTicketsSwitched", http.MethodPost, "/internal/exchanges/{id}/tickets-switched", "/internal/exchanges/" + someUUID + "/tickets-switched",
			`{"organizer_id":"` + someUUID + `"}`, false},
		{"createCancellationRefundRun", http.MethodPost, "/internal/slots/{id}/cancellation-refunds", "/internal/slots/" + someUUID + "/cancellation-refunds",
			`{"organizer_id":"` + someUUID + `","actor":"staff:amy","reason":"event cancelled"}`, true},
		// organizer_id is a REQUIRED query parameter here; without it the
		// validator answers 400 and the credential check never runs.
		{"getCancellationRefundReport", http.MethodGet, "/internal/cancellation-refunds/{id}", "/internal/cancellation-refunds/" + someUUID + "?organizer_id=" + someUUID, "", false},
		{"getDeliveryEmail", http.MethodGet, "/internal/buyers/{id}/delivery-email", "/internal/buyers/" + someUUID + "/delivery-email", "", false},
		// The un-claim (TKT-225). Deliberately on THIS side of the list: it is a
		// support action on someone else's purchase, and this slice ships no
		// back-office surface to reach it from, so the staff credential must not
		// open it. Whoever adds that surface moves this line and argues for it —
		// which is the whole reason this enumeration exists.
		{"unclaimOrder", http.MethodPost, "/internal/orders/{id}/unclaim", "/internal/orders/" + someUUID + "/unclaim",
			`{"actor":"staff:amy","reason":"claimed by the wrong account"}`, true},
	}
}

func serveWithStaffCredential(t *testing.T, op internalOp) *httptest.ResponseRecorder {
	t.Helper()
	s := New(nil, http.DefaultClient, "", "", "", internalTok).WithStaffWriteCredential(staffTok)
	req := httptest.NewRequest(op.method, op.path, bytes.NewBufferString(op.body))
	if op.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if op.key {
		req.Header.Set("Idempotency-Key", "tkt-194-enumeration")
	}
	// The staff credential, and DELIBERATELY no internal token: this asks
	// whether the staff credential alone opens the operation.
	req.Header.Set("X-Commerce-Staff-Write-Token", staffTok)
	res := httptest.NewRecorder()
	s.Router(nil, true).ServeHTTP(res, req)
	return res
}

// internalRoutesFromRouter walks the REAL router rather than trusting a count.
//
// A hand-maintained number cannot detect the drift it exists to catch: add a
// ninth internal route — including one mistakenly guarded by staffOrInternal —
// and a `len(ops) != 7` assertion stays green while the staff credential opens
// something new (ai-review pass 1). The router is the only thing that knows
// what commerce actually serves.
func internalRoutesFromRouter(t *testing.T) []string {
	t.Helper()
	r := chi.NewRouter()
	New(nil, http.DefaultClient, "", "", "", internalTok).registerRoutes(r)
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

// The fixture table and the router must describe the same surface, in BOTH
// directions: a route the table misses is unproven, and a table entry naming a
// route that no longer exists is a probe hitting a 404 for the wrong reason.
func TestTheEnumerationCoversEveryInternalRouteCommerceServes(t *testing.T) {
	onRouter := internalRoutesFromRouter(t)

	covered := map[string]bool{"POST /internal/orders/{id}/refunds": true}
	for _, op := range everyInternalOperationExceptRefund() {
		covered[op.method+" "+op.routeTemplate] = true
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

	if len(unproven) > 0 {
		t.Errorf("these internal routes are not covered by the credential enumeration — "+
			"the staff credential could open them and nothing here would notice: %v", unproven)
	}
	if len(stale) > 0 {
		t.Errorf("these enumeration entries name routes commerce no longer serves, so their "+
			"probes prove nothing: %v", stale)
	}
	if len(onRouter) < 8 {
		t.Errorf("the walk found %d internal routes, which is fewer than commerce has — "+
			"the walk itself is broken: %v", len(onRouter), onRouter)
	}
}

func TestStaffCredentialOpensNoInternalOperationButTheRefund(t *testing.T) {
	ops := everyInternalOperationExceptRefund()
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			res := serveWithStaffCredential(t, op)
			// 404, the same answer a wrong internal token gets: it does not
			// confirm the route exists. A 400 here would mean the validator
			// refused the fixture and the credential check never ran.
			if res.Code != http.StatusNotFound {
				t.Errorf("staff credential got %d from %s; want 404. Body: %.200s",
					res.Code, op.name, res.Body.String())
			}
		})
	}
}

// The complement: the refund is not refused for want of a credential. It cannot
// complete here — New(nil, …) has no database — so this asserts only that the
// request gets PAST the credential check, which is the half this file owns.
// That it then refunds correctly is proven end to end in the smoke suite.
func TestStaffCredentialIsAcceptedByTheRefund(t *testing.T) {
	body := `{"organizer_id":"` + someUUID + `","quantity":1,"actor":"staff:amy","reason":"customer called"}`
	s := New(nil, http.DefaultClient, "", "", "", internalTok).WithStaffWriteCredential(staffTok)
	req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+someUUID+"/refunds", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "tkt-194-accepted")
	req.Header.Set("X-Commerce-Staff-Write-Token", staffTok)
	res := httptest.NewRecorder()

	defer func() {
		// Reaching the database with no database is the proof: the credential
		// check let it through. Recovered so the panic is a PASS signal rather
		// than a failing test.
		_ = recover()
	}()
	s.Router(nil, true).ServeHTTP(res, req)
	if res.Code == http.StatusNotFound {
		t.Errorf("the staff credential was refused by the refund it exists for; body=%.200s", res.Body.String())
	}
}

func TestRefundRefusesAWrongOrMissingStaffCredential(t *testing.T) {
	body := `{"organizer_id":"` + someUUID + `","quantity":1,"actor":"staff:amy","reason":"customer called"}`
	for name, hdr := range map[string]string{
		"no credential at all": "",
		"a wrong value":        "not-the-credential",
		// One character short: a prefix comparison would admit it.
		"a prefix of the credential": staffTok[:len(staffTok)-1],
		// The internal token is NOT the staff credential for this header.
		"the internal token in the staff header": internalTok,
	} {
		t.Run(name, func(t *testing.T) {
			s := New(nil, http.DefaultClient, "", "", "", internalTok).WithStaffWriteCredential(staffTok)
			req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+someUUID+"/refunds", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "tkt-194-refused")
			if hdr != "" {
				req.Header.Set("X-Commerce-Staff-Write-Token", hdr)
			}
			res := httptest.NewRecorder()
			s.Router(nil, true).ServeHTTP(res, req)
			if res.Code != http.StatusNotFound {
				t.Errorf("status=%d want 404; body=%.200s", res.Code, res.Body.String())
			}
		})
	}
}

// An unconfigured staff credential must fail closed, never open — the same rule
// the internal token already follows.
func TestAnUnconfiguredStaffCredentialOpensNothing(t *testing.T) {
	body := `{"organizer_id":"` + someUUID + `","quantity":1,"actor":"staff:amy","reason":"customer called"}`
	s := New(nil, http.DefaultClient, "", "", "", internalTok) // no WithStaffWriteCredential
	req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+someUUID+"/refunds", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "tkt-194-unconfigured")
	req.Header.Set("X-Commerce-Staff-Write-Token", "")
	res := httptest.NewRecorder()
	s.Router(nil, true).ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Errorf("an empty configured credential admitted an empty header: status=%d", res.Code)
	}
}

// ai-review S8. `call` reaches catalog, inventory AND payments, and since the
// money surface took its own credential the right one has to be picked per
// destination. It is picked from the URL, once, rather than at each of the
// seventeen call sites — so this asserts the rule, not a call site.
//
// The inventory direction matters as much as the payments one: leaking the
// payments credential to catalog or inventory would hand the money key to
// services that have no business holding it, which is the exact coupling the
// split removed.
func TestInternalTokenIsChosenByDestination(t *testing.T) {
	s := &Server{
		catalogURL:   "http://catalog:8080",
		inventoryURL: "http://inventory:8080",
		paymentsURL:  "http://payments:8080",
		token:        "shared",
	}
	s.WithPaymentsToken("payments-only")

	for url, want := range map[string]string{
		"http://payments:8080/internal/charges":       "payments-only",
		"http://payments:8080/internal/psp/refund":    "payments-only",
		"http://catalog:8080/internal/ticket-types/1": "shared",
		"http://inventory:8080/internal/holds":        "shared",
		// A host that merely starts with the same letters is not payments.
		"http://payments-lookalike:8080/internal/x": "shared",
	} {
		if got := s.internalTokenFor(url); got != want {
			t.Errorf("internalTokenFor(%q) = %q, want %q", url, got, want)
		}
	}

	// Unconfigured falls back rather than sending an empty credential: payments
	// fails closed on empty, so the failure would read as an outage instead of a
	// missing environment variable.
	unsplit := &Server{paymentsURL: "http://payments:8080", token: "shared"}
	if got := unsplit.internalTokenFor("http://payments:8080/internal/charges"); got != "shared" {
		t.Errorf("unsplit server sent %q, want the shared credential", got)
	}
}
