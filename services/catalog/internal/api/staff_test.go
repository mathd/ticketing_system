package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
)

// TKT-190 US-B1. The handler's whole job is to map three store outcomes onto
// three declared responses without leaking which account exists.

func TestAuthenticateStaffReturnsThePrincipal(t *testing.T) {
	e := newEnv(t)
	staffID, org := uuid.New(), uuid.New()
	e.store.staffAccounts["ada@example.test"] = staffAuthResult{
		account: store.StaffAccount{ID: staffID, OrganizerID: org, Identifier: "ada@example.test", Role: "admin"},
		password: "correct horse",
	}

	rec := e.do("POST", "/staff/authenticate", StaffCredentials{
		Identifier: "ada@example.test", Password: "correct horse"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("an authentication response is ADR-004's never tier, got %q", cc)
	}
	got := decode[StaffPrincipal](t, rec)
	if got.StaffId != staffID || got.OrganizerId != org {
		t.Fatalf("principal = %+v, want staff %s organizer %s", got, staffID, org)
	}
	// TKT-197 reverses TKT-190's assertion here. That one required the principal
	// to carry NO role, on the reasoning that shipping one early would let a
	// consumer bind to a vocabulary the owning ticket had not chosen. The owning
	// ticket is this one, the vocabulary is now StaffRole in the contract, and
	// the role is what the back-office matrix gates on — so withholding it is
	// what would now be wrong.
	if got.Role != Admin {
		t.Fatalf("principal role = %q, want %q", got.Role, Admin)
	}
}

// A stored role outside the contract's vocabulary is a DATA problem, not a
// credential problem, and the difference decides where the operator looks.
//
// Answering 401 would say "your password is wrong" about a password that is
// right, and send them to reset it. Answering 200 with the unknown value would
// hand the back office a role its matrix cannot classify — and an unclassifiable
// role that reaches a session is precisely the fail-open this ticket exists to
// prevent. So: refuse, generically, and log for the operator.
func TestAuthenticateStaffRefusesAnUnrecognisedStoredRole(t *testing.T) {
	e := newEnv(t)
	e.store.staffAccounts["ada@example.test"] = staffAuthResult{
		account: store.StaffAccount{ID: uuid.New(), OrganizerID: uuid.New(),
			Identifier: "ada@example.test", Role: "superuser"},
		password: "correct horse",
	}

	rec := e.do("POST", "/staff/authenticate", StaffCredentials{
		Identifier: "ada@example.test", Password: "correct horse"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 — an unrecognised stored role must not authenticate: %s",
			rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "superuser") {
		t.Fatalf("the refusal echoes the stored role: %s", body)
	}
}

// COS-4 at the HTTP layer: byte-identical refusals. The store proves the KDF
// side of the same property (staff_test.go counts comparisons); this proves the
// caller-visible side, which is the half an enumerator reads first.
func TestAuthenticateStaffRefusesUnknownAndWrongIdentically(t *testing.T) {
	e := newEnv(t)
	e.store.staffAccounts["ada@example.test"] = staffAuthResult{
		account:  store.StaffAccount{ID: uuid.New(), OrganizerID: uuid.New(), Identifier: "ada@example.test"},
		password: "correct horse",
	}

	unknown := e.do("POST", "/staff/authenticate", StaffCredentials{
		Identifier: "nobody@example.test", Password: "correct horse"})
	wrong := e.do("POST", "/staff/authenticate", StaffCredentials{
		Identifier: "ada@example.test", Password: "wrong"})

	if unknown.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized {
		t.Fatalf("statuses: unknown=%d wrong=%d, want 401 both", unknown.Code, wrong.Code)
	}
	if unknown.Body.String() != wrong.Body.String() {
		t.Fatalf("bodies differ and so leak which accounts exist:\n unknown=%s\n wrong=%s",
			unknown.Body.String(), wrong.Body.String())
	}
	for _, rec := range []struct {
		name string
		body string
	}{{"unknown", unknown.Body.String()}, {"wrong", wrong.Body.String()}} {
		// Echoing the identifier back turns the error page into a reflection
		// surface and hands a log-reader the identifiers being probed.
		if strings.Contains(rec.body, "@example.test") {
			t.Fatalf("%s refusal echoes the submitted identifier: %s", rec.name, rec.body)
		}
	}
	if cc := unknown.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("refusals must be no-store too, got %q", cc)
	}
}

// A store failure is not a credential failure. Reporting 401 for a dead database
// would tell a caller their password is wrong when it may well be right, and
// would hide an outage behind a plausible answer.
func TestAuthenticateStaffReportsStoreFailureAsInternal(t *testing.T) {
	e := newEnv(t)
	e.store.staffAuthErr = errStaffStoreBroken

	rec := e.do("POST", "/staff/authenticate", StaffCredentials{
		Identifier: "ada@example.test", Password: "correct horse"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), errStaffStoreBroken.Error()) {
		t.Fatalf("the internal error text must not reach the caller: %s", rec.Body.String())
	}
}

// The spec middleware, not handler code, is what bounds the submitted password
// at bcrypt's 72-byte input limit — so an unauthenticated caller cannot make the
// service hash something it would silently truncate.
func TestAuthenticateStaffRejectsOverlongPasswordAtTheContract(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/staff/authenticate", StaffCredentials{
		Identifier: "ada@example.test", Password: strings.Repeat("a", 73)})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if e.store.staffAuthCalls != 0 {
		t.Fatalf("an over-long password reached the store %d times; the contract must refuse it first",
			e.store.staffAuthCalls)
	}
}
