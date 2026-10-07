package api

import (
	"bytes"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/commerce/internal/refunds"
	"ticketing/shared/organizerassertion"
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
		{"getInternalOrderSeats", http.MethodGet, "/internal/orders/{id}/seats", "/internal/orders/" + someUUID + "/seats?organizer_id=" + someUUID, "", false},
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
	s := staffServer(t)
	req := httptest.NewRequest(op.method, op.path, bytes.NewBufferString(op.body))
	if op.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if op.key {
		req.Header.Set("Idempotency-Key", "tkt-194-enumeration")
	}
	// The staff credential AND a valid organizer assertion, and DELIBERATELY no
	// internal token: this asks whether everything the back office holds opens the
	// operation (TKT-287 added the assertion; it must not widen the set).
	req.Header.Set("X-Commerce-Staff-Write-Token", staffTok)
	req.Header.Set(organizerAssertionHeader, mintTestAssertion(t, testOrgKey(), someUUID, time.Now().Add(time.Hour)))
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
	newTestServer(nil, http.DefaultClient, "", "", "", internalTok).registerRoutes(r)
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

	// The two routes the back office's commerce credential may open. Both are the
	// same staff decision on one order — refund it, or void it when it has no
	// money leg (TKT-171) — and each is proven to ACCEPT that credential
	// (TestStaffCredentialIsAcceptedByThe{Refund,Void}) and to REFUSE a wrong or
	// missing one (TestRefundRefusesAWrongOrMissingStaffCredential,
	// TestVoidRefusesAWrongOrMissingStaffCredential) rather than merely listed here.
	covered := map[string]bool{
		"POST /internal/orders/{id}/refunds": true,
		"POST /internal/orders/{id}/voids":   true,
		// The staff order READ (TKT-201). The first non-write on this list, and the
		// widening is deliberate: the back office cannot show a staff member what an
		// order contains without it.
		"GET /internal/orders/{id}": true,
	}
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

// --- TKT-287: the staff operations require the credential AND a verified organizer
// assertion, and take the organizer from the assertion only. ---

const testOrgKID = "catalog-org/test"

func seededKey(b byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, ed25519.SeedSize))
}

// testOrgKey is the "catalog" key the test server trusts; seededKey(0x99) is a
// stranger's.
func testOrgKey() ed25519.PrivateKey { return seededKey(0x2a) }

// mintTestAssertion signs a v2 assertion with crypto/ed25519 directly — NOT with
// catalog's minter or the shared verifier's package — so these tests cannot pass
// by the code under test agreeing with itself.
func mintTestAssertion(t *testing.T, key ed25519.PrivateKey, organizer string, expiry time.Time) string {
	t.Helper()
	payload := strings.Join([]string{"v2", testOrgKID, otherUUID, organizer, strconv.FormatInt(expiry.Unix(), 10)}, ".")
	return payload + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(payload)))
}

func testOrgVerifier(t *testing.T) *organizerassertion.Verifier {
	t.Helper()
	v, err := organizerassertion.NewVerifier(map[string]ed25519.PublicKey{
		testOrgKID: testOrgKey().Public().(ed25519.PublicKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// staffServer is configured exactly as production: the staff credential and the
// public keyring, no database.
func staffServer(t *testing.T) *Server {
	t.Helper()
	return newTestServer(nil, http.DefaultClient, "", "", "", internalTok).
		WithStaffWriteCredential(staffTok).
		WithOrganizerAssertionVerifier(testOrgVerifier(t))
}

type staffOp struct {
	name, method, path, body string
	key                      bool
}

// Every request below is otherwise VALID — the validator runs first and answers
// 400 for a malformed one, which would refuse without the guard ever running.
func staffOperations() []staffOp {
	return []staffOp{
		{"refund", http.MethodPost, "/internal/orders/" + someUUID + "/refunds", `{"quantity":1,"actor":"staff:amy","reason":"customer called"}`, true},
		{"void", http.MethodPost, "/internal/orders/" + someUUID + "/voids", `{"actor":"staff:amy","reason":"event cancelled"}`, true},
		{"read", http.MethodGet, "/internal/orders/" + someUUID, "", false},
	}
}

func staffRequest(op staffOp, headers map[string]string) *http.Request {
	var body *bytes.Buffer
	if op.body != "" {
		body = bytes.NewBufferString(op.body)
	} else {
		body = &bytes.Buffer{}
	}
	req := httptest.NewRequest(op.method, op.path, body)
	if op.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if op.key {
		req.Header.Set("Idempotency-Key", "tkt-287-"+op.name)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// dbProbe is a TCP listener posing as PostgreSQL. It accepts and closes every
// connection and counts them, so a test can OBSERVE whether a request reached the
// database — the step after the guard — instead of inferring it from a status or a
// panic (TKT-287 ai-review: "anything but 404" also admitted a validator 400 and an
// unrelated panic).
func dbProbe(t *testing.T) (*sql.DB, func() int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connections atomic.Int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			_ = conn.Close()
		}
	}()
	db, err := sql.Open("pgx", "postgres://probe:probe@"+ln.Addr().String()+"/probe?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close(); _ = ln.Close() })
	return db, connections.Load
}

func probedStaffServer(t *testing.T) (*Server, func() int64) {
	t.Helper()
	db, connections := dbProbe(t)
	return newTestServer(db, http.DefaultClient, "", "", "", internalTok).
		WithStaffWriteCredential(staffTok).
		WithOrganizerAssertionVerifier(testOrgVerifier(t)), connections
}

// The pair opens each operation: the request reaches the database. That is the half
// this file owns; the organizer it then acts for is proven at the store and smoke
// tiers, where a real order exists to be another tenant's.
func TestStaffOperationsAcceptTheCredentialAndAssertion(t *testing.T) {
	for _, op := range staffOperations() {
		t.Run(op.name, func(t *testing.T) {
			s, connections := probedStaffServer(t)
			res := httptest.NewRecorder()
			s.Router(nil, true).ServeHTTP(res, staffRequest(op, map[string]string{
				"X-Commerce-Staff-Write-Token": staffTok,
				organizerAssertionHeader:       mintTestAssertion(t, testOrgKey(), someUUID, time.Now().Add(time.Hour)),
			}))
			if connections() == 0 {
				t.Fatalf("the credential and a valid assertion did not reach the database: %d %.200s", res.Code, res.Body.String())
			}
			if res.Code == http.StatusNotFound || res.Code == http.StatusBadRequest {
				t.Fatalf("status %d after reaching the database: %.200s", res.Code, res.Body.String())
			}
		})
	}
}

// One case per predicate, each passing every OTHER predicate, so deleting any one
// check turns exactly its case red (AGENTS.md: a guard with N predicates needs N
// tests). Every refusal is the same 404 (ADR-043, TKT-287 D3).
func TestStaffOperationsRefuseEachPredicate(t *testing.T) {
	valid := func(t *testing.T) string {
		return mintTestAssertion(t, testOrgKey(), someUUID, time.Now().Add(time.Hour))
	}
	for _, op := range staffOperations() {
		for _, tc := range []struct {
			name    string
			server  func(t *testing.T) *Server
			headers func(t *testing.T) map[string]string
		}{
			{"no staff credential", staffServer, func(t *testing.T) map[string]string {
				return map[string]string{organizerAssertionHeader: valid(t)}
			}},
			{"a wrong staff credential", staffServer, func(t *testing.T) map[string]string {
				return map[string]string{"X-Commerce-Staff-Write-Token": "not-the-credential", organizerAssertionHeader: valid(t)}
			}},
			{"a prefix of the staff credential", staffServer, func(t *testing.T) map[string]string {
				return map[string]string{"X-Commerce-Staff-Write-Token": staffTok[:len(staffTok)-1], organizerAssertionHeader: valid(t)}
			}},
			{"the internal token in the staff header", staffServer, func(t *testing.T) map[string]string {
				return map[string]string{"X-Commerce-Staff-Write-Token": internalTok, organizerAssertionHeader: valid(t)}
			}},
			// The shared internal token used to open these operations on its own.
			{"the internal token, with a valid assertion", staffServer, func(t *testing.T) map[string]string {
				return map[string]string{"X-Internal-Token": internalTok, organizerAssertionHeader: valid(t)}
			}},
			{"no assertion", staffServer, func(t *testing.T) map[string]string {
				return map[string]string{"X-Commerce-Staff-Write-Token": staffTok}
			}},
			{"an assertion signed by another key", staffServer, func(t *testing.T) map[string]string {
				return map[string]string{"X-Commerce-Staff-Write-Token": staffTok,
					organizerAssertionHeader: mintTestAssertion(t, seededKey(0x99), someUUID, time.Now().Add(time.Hour))}
			}},
			{"an expired assertion", staffServer, func(t *testing.T) map[string]string {
				return map[string]string{"X-Commerce-Staff-Write-Token": staffTok,
					organizerAssertionHeader: mintTestAssertion(t, testOrgKey(), someUUID, time.Now().Add(-time.Second))}
			}},
			{"a malformed assertion", staffServer, func(t *testing.T) map[string]string {
				return map[string]string{"X-Commerce-Staff-Write-Token": staffTok, organizerAssertionHeader: "v2.garbage"}
			}},
			{"no verifier configured", func(t *testing.T) *Server {
				return newTestServer(nil, http.DefaultClient, "", "", "", internalTok).WithStaffWriteCredential(staffTok)
			}, func(t *testing.T) map[string]string {
				return map[string]string{"X-Commerce-Staff-Write-Token": staffTok, organizerAssertionHeader: valid(t)}
			}},
			{"no staff credential configured", func(t *testing.T) *Server {
				return newTestServer(nil, http.DefaultClient, "", "", "", internalTok).WithOrganizerAssertionVerifier(testOrgVerifier(t))
			}, func(t *testing.T) map[string]string {
				return map[string]string{"X-Commerce-Staff-Write-Token": "", organizerAssertionHeader: valid(t)}
			}},
		} {
			t.Run(op.name+"/"+tc.name, func(t *testing.T) {
				db, connections := dbProbe(t)
				s := tc.server(t)
				s.db = db
				s.refunds = refunds.New(db, s.call, s.paymentsURL, s.accessURL, s.inventoryURL)
				res := httptest.NewRecorder()
				s.Router(nil, true).ServeHTTP(res, staffRequest(op, tc.headers(t)))
				if res.Code != http.StatusNotFound || strings.TrimSpace(res.Body.String()) != `{"error":"not found"}` {
					t.Errorf("status=%d body=%.200s, want 404 {\"error\":\"not found\"}", res.Code, res.Body.String())
				}
				if n := connections(); n != 0 {
					t.Errorf("a refused request reached the database %d time(s)", n)
				}
			})
		}
	}
}

// The organizer is UNSUBMITTABLE on the two writes: a body naming one is refused
// before any handler runs (additionalProperties: false), even with valid
// credentials — it is not compared with the assertion, because there is nothing to
// compare (TKT-287 D2, ADR-058 § 1's rule).
func TestStaffWritesRefuseABodyThatNamesAnOrganizer(t *testing.T) {
	for _, op := range staffOperations()[:2] {
		t.Run(op.name, func(t *testing.T) {
			op.body = strings.Replace(op.body, "{", `{"organizer_id":"`+someUUID+`",`, 1)
			res := httptest.NewRecorder()
			staffServer(t).Router(nil, true).ServeHTTP(res, staffRequest(op, map[string]string{
				"X-Commerce-Staff-Write-Token": staffTok,
				organizerAssertionHeader:       mintTestAssertion(t, testOrgKey(), someUUID, time.Now().Add(time.Hour)),
			}))
			if res.Code != http.StatusBadRequest {
				t.Errorf("a body naming organizer_id answered %d, want 400; body=%.200s", res.Code, res.Body.String())
			}
		})
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
