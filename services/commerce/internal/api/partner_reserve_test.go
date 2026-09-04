package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The positive half of the channel seam (TKT-246).
//
// channel_seam_test.go pins that the UNAUTHENTICATED route forwards no channel.
// This file pins the other side: the AUTHENTICATED partner route forwards its
// credential's channel and reseller, and takes neither from the request body.
//
// Together they are the ticket's claim, and neither alone is: "the channel reaches
// inventory" without "only when authorized" is the defect TKT-240 was reverted for,
// and "only when authorized" without "the channel reaches inventory" is the seam
// still being open.

// reserveAsPartner drives one partner reserve with an injected authenticated scope
// and returns what commerce sent to inventory.
//
// The scope is injected rather than authenticated because authentication needs a
// database (AuthenticateResellerCredential) and what is under test here is what the
// handler does with a scope it already has. The credential path itself is pinned by
// partner_test.go and by the contract's `security:` declaration, which
// TestEveryPartnerOperationDeclaresTheCredential enforces for this new operation
// automatically -- that test derives its list from the document, so this route was
// covered by it the moment it was added.
func reserveAsPartner(t *testing.T, scope partnerScope, requestBody string) capturedHold {
	t.Helper()
	var got capturedHold
	channel := scope.ChannelCode
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasSuffix(r.URL.Path, "/fee-resolution") {
			_, _ = w.Write([]byte(emptyFeeResolutionBody(&channel)))
			return
		}
		_, _ = w.Write([]byte(resolutionBodyFor(2500, false, &channel)))
	}))
	defer catalog.Close()

	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/holds") {
			raw, _ := io.ReadAll(r.Body)
			got.path = r.URL.Path
			got.body = map[string]any{}
			_ = json.Unmarshal(raw, &got.body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409) // stop after the hold; this test is about the request
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
	}))
	defer inventory.Close()

	srv := newTestServer(nil, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
	req := httptest.NewRequest(http.MethodPost, "/partners/reservations", bytes.NewBufferString(requestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "partner-"+t.Name())
	// The slot the validator would have filled after authenticating.
	slot := new(partnerScope)
	*slot = scope
	req = req.WithContext(context.WithValue(req.Context(), partnerScopeKey{}, slot))
	srv.partnerReserve(httptest.NewRecorder(), req)
	return got
}

// An authenticated partner GA sale forwards its channel AND its reseller.
//
// This is the seam closing. Both fields must be present: the channel is what
// channel_allocations caps consumption against, and the reseller is what the
// allocation's sold_by is compared to. A forward carrying only the channel is the
// TKT-240 shape -- it consumes the right allocation but proves nothing about who is
// consuming it.
func TestAPartnerGASaleForwardsItsChannelAndReseller(t *testing.T) {
	reseller := uuid.New()
	scope := partnerScope{
		CredentialID: uuid.New(),
		ResellerID:   reseller,
		OrganizerID:  uuid.MustParse(pricingOrg),
		ChannelCode:  "reseller-acme",
	}
	got := reserveAsPartner(t, scope, `{"organizer_id":"`+pricingOrg+`","ticket_type_id":"`+pricingTT+`","quantity":2}`)

	if got.body == nil {
		t.Fatal("the partner reserve never called inventory's hold endpoint")
	}
	if strings.Contains(got.path, "/holds/seats") {
		t.Fatalf("a quantity sale took the seated hold path (%s)", got.path)
	}
	if got.body["channel"] != "reseller-acme" {
		t.Fatalf("the partner hold carries channel=%v, want %q from the credential — without it "+
			"the sale consumes PUBLIC stock while pricing under the channel's fees, which is the "+
			"whole defect TKT-246 closes", got.body["channel"], "reseller-acme")
	}
	if got.body["reseller_id"] != reseller.String() {
		t.Fatalf("the partner hold carries reseller_id=%v, want %s from the credential — inventory "+
			"compares this against the allocation's sold_by, so without it a bound allocation is "+
			"unreachable by the partner it belongs to", got.body["reseller_id"], reseller)
	}
}

// The partner route takes channel and reseller from the CREDENTIAL, never the body.
//
// The load-bearing test of this ticket, and it pins BOTH layers of the defence,
// because they refuse differently:
//
//   - `reseller_id` is not a field of the request type at all, so the strict decoder
//     (DecodeJSON rejects unknown fields) refuses the whole request with a 400. A
//     partner cannot even ask.
//   - `channel_code` IS a field -- the public route's -- so it decodes, and the
//     handler must then OVERWRITE it from the scope rather than honour it.
//
// The second is the one that would silently matter: a body-supplied channel that
// survived to inventory is the TKT-240 bypass with a credential attached, letting an
// authenticated partner consume a DIFFERENT partner's allocation.
func TestThePartnerRouteIgnoresChannelAndResellerInTheBody(t *testing.T) {
	credentialled := uuid.New()
	scope := partnerScope{
		CredentialID: uuid.New(),
		ResellerID:   credentialled,
		OrganizerID:  uuid.MustParse(pricingOrg),
		ChannelCode:  "reseller-acme",
	}

	// A body-supplied channel_code decodes, and is overwritten by the credential's.
	got := reserveAsPartner(t, scope, `{"organizer_id":"`+pricingOrg+`","ticket_type_id":"`+pricingTT+
		`","quantity":2,"channel_code":"reseller-globex"}`)
	if got.body == nil {
		t.Fatal("the partner reserve never called inventory's hold endpoint")
	}
	if got.body["channel"] != "reseller-acme" {
		t.Fatalf("a body-supplied channel_code reached inventory as %v — the credential said "+
			"reseller-acme. A partner that can name another partner's channel is the TKT-240 "+
			"bypass with a credential attached", got.body["channel"])
	}
	if got.body["reseller_id"] != credentialled.String() {
		t.Fatalf("the hold carries reseller_id=%v, want the credential's %s",
			got.body["reseller_id"], credentialled)
	}

	// A body-supplied reseller_id is refused outright by the strict decoder.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/partners/reservations",
		bytes.NewBufferString(`{"organizer_id":"`+pricingOrg+`","ticket_type_id":"`+pricingTT+
			`","quantity":2,"reseller_id":"`+uuid.New().String()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "partner-body-reseller")
	slot := new(partnerScope)
	*slot = scope
	req = req.WithContext(context.WithValue(req.Context(), partnerScopeKey{}, slot))
	newTestServer(nil, http.DefaultClient, "", "", "", "secret").partnerReserve(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a body carrying reseller_id answered %d, want 400 — the field must not exist on "+
			"the request type, so that attributing a sale to another reseller is unaskable rather "+
			"than merely ignored", rec.Code)
	}
}

// Two resellers on ONE organizer sharing an idempotency key do not collide.
//
// The reservation id was derived from organizer + key alone, so two partners of the
// same organizer that happened to choose the same key derived the SAME id -- and the
// second one would have received the first one's reservation: another reseller's
// buyer, seats and money, attributed to them. Idempotency keys are caller-chosen and
// frequently sequential ("1", "order-1"), so this is a collision waiting rather than
// an attack.
//
// Asserted on the DERIVATION rather than by driving two reserves through a database,
// because the derivation IS the mechanism -- and pinning it here also pins the
// compatibility half in the same breath: the public basis must stay byte-identical or
// every in-flight retry in production derives a new id, misses its persisted row and
// places a second hold.
func TestTwoResellersSharingAnIdempotencyKeyDeriveDifferentReservations(t *testing.T) {
	org := uuid.New()
	acme, globex := uuid.New(), uuid.New()
	const key = "1" // the kind of key a partner actually sends

	// The REAL derivation, not a copy of it. A test that recomputes the rule it is
	// checking agrees with the code by construction and cannot fail (AGENTS.md).
	id := func(reseller *uuid.UUID) uuid.UUID {
		if reseller == nil {
			return reservationID(org, key, nil)
		}
		return reservationID(org, key, &partnerScope{ResellerID: *reseller})
	}

	if id(&acme) == id(&globex) {
		t.Fatal("two resellers on one organizer derived the SAME reservation id from a shared " +
			"idempotency key — the second would receive the first's reservation")
	}
	if id(&acme) == id(nil) || id(&globex) == id(nil) {
		t.Fatal("a partner reservation collided with the public derivation for the same key")
	}
	// The public derivation is unchanged, byte for byte, and the expected value is
	// written out INDEPENDENTLY here rather than taken from a run: every reservation
	// that already exists was written under this exact basis, so this is a
	// compatibility constant, not an observation of current behaviour.
	if got := reservationID(org, key, nil); got != uuid.NewSHA1(uuid.NameSpaceOID,
		[]byte("reservation:"+org.String()+":"+key)) {
		t.Fatalf("the public reservation id derivation changed (got %s) — that re-identifies "+
			"every reservation that already exists, so an in-flight retry misses its row and "+
			"places a second hold", got)
	}
}

// A partner may not reserve for an organizer other than its own.
//
// The credential is scoped to one organizer (ADR-056). organizer_id stays in the body
// because the shared reserve path needs it, so it must be COMPARED against the scope
// rather than trusted -- the same reasoning as the channel, one tenant up.
func TestAPartnerCannotReserveForAnotherOrganizer(t *testing.T) {
	scope := partnerScope{
		CredentialID: uuid.New(),
		ResellerID:   uuid.New(),
		OrganizerID:  uuid.New(), // NOT pricingOrg
		ChannelCode:  "reseller-acme",
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/partners/reservations",
		bytes.NewBufferString(`{"organizer_id":"`+pricingOrg+`","ticket_type_id":"`+pricingTT+`","quantity":2}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "partner-cross-org")
	slot := new(partnerScope)
	*slot = scope
	req = req.WithContext(context.WithValue(req.Context(), partnerScopeKey{}, slot))

	srv := newTestServer(nil, http.DefaultClient, "", "", "", "secret")
	srv.partnerReserve(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-organizer partner reserve answered %d, want 403 — a credential scoped to "+
			"one organizer must not sell another's inventory", rec.Code)
	}
}

// A partner handler with no scope refuses, rather than running as nobody.
//
// requirePartnerScope's fail-closed contract, pinned for the write path. A
// zero-value scope names organizer uuid.Nil, reseller uuid.Nil and channel "" --
// and channel "" is the PUBLIC channel, so a handler that carried on would forward
// no channel at all and consume public stock while looking like a partner sale.
func TestAPartnerReserveWithoutAScopeIsRefused(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/partners/reservations",
		bytes.NewBufferString(`{"organizer_id":"`+pricingOrg+`","ticket_type_id":"`+pricingTT+`","quantity":2}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "partner-no-scope")

	srv := newTestServer(nil, http.DefaultClient, "", "", "", "secret")
	srv.partnerReserve(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a partner reserve with no authenticated scope answered %d, want 401", rec.Code)
	}
}
