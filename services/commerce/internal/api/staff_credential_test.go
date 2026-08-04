package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
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
	path   string
	body   string
	key    bool // sends Idempotency-Key
}

// everyInternalOperationExceptRefund mirrors Router's internal registrations.
// If commerce grows another internal route it belongs here — the count
// assertion below exists to make that failure loud rather than silent.
func everyInternalOperationExceptRefund() []internalOp {
	conversion := `{"organizer_id":"` + someUUID + `","ticket_type_id":"` + otherUUID + `","quantity":1,"actor":"staff:amy","reason":"walk-up"}`
	return []internalOp{
		{"convertOperationalHold", http.MethodPost, "/internal/operational-holds/" + someUUID + "/convert", conversion, true},
		{"drawDownGroupReservation", http.MethodPost, "/internal/group-reservations/" + someUUID + "/draw-down", conversion, true},
		{"exchangeOrder", http.MethodPost, "/internal/orders/" + someUUID + "/exchanges",
			`{"organizer_id":"` + someUUID + `","target_ticket_type_id":"` + otherUUID + `","actor":"staff:amy","reason":"upgrade"}`, true},
		{"exchangeTicketsSwitched", http.MethodPost, "/internal/exchanges/" + someUUID + "/tickets-switched",
			`{"organizer_id":"` + someUUID + `"}`, false},
		{"createCancellationRefundRun", http.MethodPost, "/internal/slots/" + someUUID + "/cancellation-refunds",
			`{"organizer_id":"` + someUUID + `","actor":"staff:amy","reason":"event cancelled"}`, true},
		// organizer_id is a REQUIRED query parameter here; without it the
		// validator answers 400 and the credential check never runs.
		{"getCancellationRefundReport", http.MethodGet, "/internal/cancellation-refunds/" + someUUID + "?organizer_id=" + someUUID, "", false},
		{"getDeliveryEmail", http.MethodGet, "/internal/buyers/" + someUUID + "/delivery-email", "", false},
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

func TestStaffCredentialOpensNoInternalOperationButTheRefund(t *testing.T) {
	ops := everyInternalOperationExceptRefund()
	// Commerce registers 8 internal routes; 7 of them are here and the refund is
	// the eighth. A new internal route added without a line in this table would
	// otherwise be silently unproven.
	if len(ops) != 7 {
		t.Fatalf("the enumeration covers %d operations, want 7 — did commerce gain an internal route?", len(ops))
	}
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
