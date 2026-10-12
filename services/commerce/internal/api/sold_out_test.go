package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TKT-341 / ADR-082. Commerce forwards inventory's `sold_out` on a public GA reserve,
// and ONLY there. Each predicate of the allowlist has its own case below, and each
// case passes every other predicate, so deleting one predicate turns exactly its own
// case red.
//
// The public cases go through Router(nil, true): the response validator is ON, so a
// missing `sold_out` in commerce's Error.code enum turns the forwarded body into a 500
// here, at the commerce tier.

const (
	inventorySoldOut  = `{"error":"insufficient capacity: sold out","code":"sold_out"}`
	commerceSoldOut   = `{"code":"sold_out","error":"sold out"}`
	commerceCodeless  = `{"error":"inventory unavailable"}`
	soldOutTestPrefix = "sold-out-"
)

// soldOutStack is a catalog that prices the GA ticket type and an inventory that
// answers every hold with invStatus and invBody.
func soldOutStack(t *testing.T, invStatus int, invBody string) *Server {
	t.Helper()
	return soldOutStackFor(t, nil, invStatus, invBody)
}

// soldOutStackFor is soldOutStack for a channel: catalog echoes the channel it was
// asked about, which commerce validates (TKT-215, TKT-237).
func soldOutStackFor(t *testing.T, channel *string, invStatus int, invBody string) *Server {
	t.Helper()
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasSuffix(r.URL.Path, "/fee-resolution") {
			_, _ = w.Write([]byte(emptyFeeResolutionBody(channel)))
			return
		}
		_, _ = w.Write([]byte(resolutionBodyFor(2500, false, channel)))
	}))
	t.Cleanup(catalog.Close)
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(invStatus)
		_, _ = w.Write([]byte(invBody))
	}))
	t.Cleanup(inventory.Close)
	return newTestServer(nil, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
}

func reservePublicGA(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"organizer_id":"` + pricingOrg + `","ticket_type_id":"` + pricingTT + `","quantity":2}`
	req := httptest.NewRequest(http.MethodPost, "/reservations", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", soldOutTestPrefix+t.Name())
	res := httptest.NewRecorder()
	s.Router(nil, true).ServeHTTP(res, req)
	return res
}

func assertReserveAnswer(t *testing.T, res *httptest.ResponseRecorder, wantStatus int, wantBody string) {
	t.Helper()
	if got := strings.TrimSpace(res.Body.String()); res.Code != wantStatus || got != wantBody {
		t.Fatalf("reserve answered %d %s, want %d %s", res.Code, got, wantStatus, wantBody)
	}
}

// The positive case: every predicate holds. The body is rebuilt, not forwarded, so
// inventory's message never reaches a public caller.
func TestAPublicGASelloutIsForwardedAsSoldOut(t *testing.T) {
	res := reservePublicGA(t, soldOutStack(t, http.StatusConflict, inventorySoldOut))
	assertReserveAnswer(t, res, http.StatusConflict, commerceSoldOut)
}

// Status predicate: the code counts only on inventory's 409.
func TestASoldOutBodyOnAnotherStatusIsNotForwarded(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		res := reservePublicGA(t, soldOutStack(t, status, inventorySoldOut))
		assertReserveAnswer(t, res, http.StatusConflict, commerceCodeless)
	}
}

// Code predicate: exact `sold_out`. Every other inventory 409 keeps the code-less
// answer, including the coded refusals that must never look like a sellout.
func TestEveryOtherInventoryRefusalStaysCodeless(t *testing.T) {
	for name, body := range map[string]string{
		"code-less capacity refusal": `{"error":"insufficient capacity"}`,
		"slot closed":                `{"error":"slot closed","code":"slot_closed"}`,
		"channel window closed":      `{"error":"channel sales window closed","code":"channel_window_closed"}`,
		"presale code invalid":       `{"error":"invalid presale code","code":"presale_code_invalid"}`,
		"code in another case":       `{"error":"insufficient capacity: sold out","code":"SOLD_OUT"}`,
		"code with padding":          `{"error":"insufficient capacity: sold out","code":"sold_out "}`,
		"code of another type":       `{"error":"insufficient capacity: sold out","code":1}`,
		"code null":                  `{"error":"insufficient capacity: sold out","code":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			res := reservePublicGA(t, soldOutStack(t, http.StatusConflict, body))
			assertReserveAnswer(t, res, http.StatusConflict, commerceCodeless)
		})
	}
}

// Body predicate: the whole body must decode, with a string `error`. A body that is
// malformed, or that carries the code without the shape inventory's Error has, is
// not evidence of a sellout.
func TestAMalformedSoldOutBodyIsNotForwarded(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":              `sold_out`,
		"truncated":             `{"error":"insufficient capacity: sold out","code":"sold_out"`,
		"error absent":          `{"code":"sold_out"}`,
		"error of another type": `{"error":7,"code":"sold_out"}`,
		"empty body":            ``,
		// The input class that separates the decode check from the field checks
		// (TKT-305): json.Unmarshal fills `error` and the first `code` and THEN
		// reports the type error on the second `code`.
		"repeated code of another type": `{"error":"insufficient capacity: sold out","code":"sold_out","code":5}`,
	} {
		t.Run(name, func(t *testing.T) {
			res := reservePublicGA(t, soldOutStack(t, http.StatusConflict, body))
			assertReserveAnswer(t, res, http.StatusConflict, commerceCodeless)
		})
	}
}

// A transport failure is not a sellout. Note on the `err` predicate: call() returns a
// nil body with any error (httpx.ReadResponseBody), and status 0 on a transport
// error, so the status and body predicates already refuse every errored call. The
// `err` check is kept as a defence for a future call() that returns a partial body,
// and no test can discriminate it today.
func TestAnUnreachableInventoryIsNotASellout(t *testing.T) {
	s := soldOutStack(t, http.StatusConflict, inventorySoldOut)
	s.inventoryURL = "http://127.0.0.1:1"
	res := reservePublicGA(t, s)
	assertReserveAnswer(t, res, http.StatusConflict, commerceCodeless)
}

// Scope predicate: a partner reserve keeps the uniform refusal its contract promises
// (/partners/reservations). Inventory never sends `sold_out` for a channelled hold,
// so only a fake can reach this; the predicate is commerce's own guard.
func TestAPartnerReserveNeverForwardsSoldOut(t *testing.T) {
	s := soldOutStackFor(t, ptr("reseller-acme"), http.StatusConflict, inventorySoldOut)
	body := `{"organizer_id":"` + pricingOrg + `","ticket_type_id":"` + pricingTT + `","quantity":2}`
	req := httptest.NewRequest(http.MethodPost, "/reservations", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", soldOutTestPrefix+t.Name())
	res := httptest.NewRecorder()
	s.reserveWithScope(res, req, &partnerScope{
		OrganizerID: uuid.MustParse(pricingOrg),
		ChannelCode: "reseller-acme",
		ResellerID:  uuid.MustParse(pricingReseller),
	})
	assertReserveAnswer(t, res, http.StatusConflict, commerceCodeless)
}

// GA predicate: a seated reserve keeps its own refusal mapping. The story is GA only.
func TestASeatedReserveNeverForwardsSoldOut(t *testing.T) {
	s, _, _, done := seatedStack(t, http.StatusConflict, inventorySoldOut)
	defer done()
	res := reserveSeats(t, s, soldOutTestPrefix+"seated", `["A/1/1"]`)
	if strings.Contains(res.Body.String(), "sold_out") {
		t.Fatalf("a seated reserve answered %d %s, want no sold_out", res.Code, res.Body.String())
	}
}
