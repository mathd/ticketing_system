package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	qrcode "github.com/skip2/go-qrcode"

	apispec "ticketing/services/access/api"
	"ticketing/services/access/internal/delivery"
	"ticketing/services/access/internal/store"
	"ticketing/services/access/internal/ticket"
	"ticketing/shared/contract"
	"ticketing/shared/httpx"
)

// scannerDeviceStore is the enrolment-check port, kept narrow on purpose: the
// scan path needs to resolve a token to a live device and to note that the device
// was seen, and nothing else. Enrolment and revocation are the operator CLI's, so
// they are deliberately absent — a handler that cannot enrol a device cannot be
// talked into enrolling one.
//
// It is an interface so the contract tests can drive the scan routes without a
// database. The interface is NOT where the guarantee lives: a fake enforces
// revocation in Go while the shipped predicate is a SQL WHERE clause, so the
// revocation and enrolment assertions belong in a smoke test against real
// PostgreSQL (scanner_devices_smoke_test.go) — AGENTS.md, "a test must live at
// the tier its mechanism does".
type scannerDeviceStore interface {
	AuthenticateScannerDevice(ctx context.Context, token string) (store.ScannerDevice, error)
	TouchScannerDevice(ctx context.Context, id uuid.UUID)
}

type Server struct {
	st *store.Postgres
	// devices resolves scanner enrolment (ai-review S1). Nil means "cannot
	// check", which the authentication func treats as REFUSE — a server that
	// cannot verify enrolment must not admit everyone.
	devices scannerDeviceStore

	// telemetry records authenticated polls of the deliberately-unlimited feed
	// surface (TKT-272). Nil is a working server that simply emits nothing —
	// observability must never be able to refuse a scan.
	telemetry *scannerTelemetry
	verifier  *ticket.Verifier
	// token authenticates service-to-service callers. Access had no inbound internal
	// surface before TKT-157 — it only ever used this token outbound, from its
	// consumer — so the whole auth path here is new.
	token string
	// cursors authenticates voided-feed pagination positions (ai-review [high]).
	// Its own key: see feedCursorSigner for why this claim is not the QR link's.
	cursors feedCursorSigner
	// qrLinks signs the short-lived image URLs the bundle hands out (ai-review
	// S2). See qrlink.go for what that bounds and what it deliberately does not.
	qrLinks qrLinkSigner
	// now is the clock seam. Tests inject one; production leaves it nil and gets
	// time.Now. Without it every expiry test is a sleep.
	now func() time.Time
	// staffWriteToken is the back office's access credential (TKT-203, ADR-068).
	// Empty means unconfigured, which the guard treats as REFUSE — see
	// staffOrInternal in redeliveries.go.
	staffWriteToken string
	// The staff redelivery route's two outbound ports. Nil means "cannot send",
	// which redeliver treats as refuse: a server that cannot reach the transport
	// must not report success for mail nobody sent.
	addresses delivery.AddressBook
	mailer    delivery.Mailer
	// publicURL is the origin the guest capability link is built on. Held here so
	// the resend builds the SAME link issuance does, through delivery.TicketLink.
	publicURL string
	// redeliveries is the resend's store port (see redeliveries.go). Nil means the
	// route refuses, like every other unwired dependency here.
	redeliveries redeliveryStore
}

// WithRedelivery supplies the staff redelivery route's outbound ports: the buyer
// address lookup, the transport, and the origin the capability link is built on.
//
// One option for all three, deliberately: a server holding two of them cannot send,
// and three separate setters would let a call site configure a partial path that
// looks wired and refuses at request time.
func (s *Server) WithRedelivery(addresses delivery.AddressBook, mailer delivery.Mailer, publicURL string) *Server {
	s.addresses, s.mailer, s.publicURL = addresses, mailer, publicURL
	return s
}

// WithQRLinkKey supplies the HMAC key for signed QR image links. An option rather
// than a New parameter: New's variadic token argument is already carrying one
// optional value, and a second positional string next to it is exactly the kind
// of call site that gets the two the wrong way round.
func (s *Server) WithQRLinkKey(key string) *Server {
	s.qrLinks = qrLinkSigner{key: []byte(key)}
	return s
}

// WithFeedCursorKey supplies the HMAC key that authenticates voided-feed
// cursors. Same shape and same reasoning as WithQRLinkKey, and a DIFFERENT key:
// one key making both claims spends a leak of the cheaper one at the price of
// the dearer.
func (s *Server) WithFeedCursorKey(key string) *Server {
	s.cursors = feedCursorSigner{key: []byte(key)}
	return s
}

// WithClock replaces the link signer's time source. Tests only.
func (s *Server) WithClock(now func() time.Time) *Server {
	s.now = now
	return s
}

func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func New(st *store.Postgres, verifier *ticket.Verifier, token ...string) *Server {
	s := &Server{st: st, verifier: verifier}
	// A typed nil in an interface is not nil, so the assignment is guarded rather
	// than unconditional: `Server{devices: (*store.Postgres)(nil)}` would pass a
	// `!= nil` check and then panic inside the auth path, which is the worst of
	// both — a guard that looks present and fails open on the way to failing hard.
	if st != nil {
		s.devices = st
		// Same typed-nil guard as devices above, and for the same reason: a typed nil
		// in an interface is not nil, so an unguarded assignment would pass a != nil
		// check and then panic inside the handler.
		s.redeliveries = st
	}
	if len(token) > 0 {
		s.token = token[0]
	}
	return s
}

// WithScannerDevices injects the enrolment-check port. Tests only: production
// passes the store to New and gets it from there.
func (s *Server) WithScannerDevices(devices scannerDeviceStore) *Server {
	s.devices = devices
	return s
}

// WithScannerTelemetry injects the abuse-telemetry emitter for the polling
// surface (TKT-272). Production wires it in main so the counter reaches the
// real meter; a Server without it serves identically and emits nothing.
func (s *Server) WithScannerTelemetry(t *scannerTelemetry) *Server {
	s.telemetry = t
	return s
}

// registerRoutes mounts every operation on a bare router.
//
// Extracted from Router so the credential enumeration in staff_credential_test.go can
// walk the ROUTER ITSELF rather than a hand-maintained list — a list cannot detect the
// drift it exists to catch (ADR-057, following commerce and inventory). Router is the
// only production caller.
func (s *Server) registerRoutes(r chi.Router) {
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=300")
		_, _ = w.Write(apispec.Spec)
	})
	r.Get("/orders/{ref}/tickets", s.tickets)
	r.Get("/orders/{ref}/tickets/{ticket}/qr.png", s.qr)
	r.Post("/scans", s.scan)
	r.Post("/scans/reconciliations", s.reconcile)
	r.Get("/scans/voided-tickets", s.voidedTickets)
	r.Post("/internal/orders/{id}/refunds", s.refundTickets)
	r.Post("/internal/orders/{id}/redeliveries", s.redeliverOrderTickets)
}

func (s *Server) Router(log *slog.Logger, validateResponses bool) http.Handler {
	r := chi.NewRouter()
	s.registerRoutes(r)
	validated, err := contract.RequestValidatorWithSecurity(apispec.Spec, r, log, validateResponses, func(w http.ResponseWriter, req *http.Request, _ string, status int) {
		// A refused scanner device (ai-review S1). It arrives here rather than from
		// a handler because the contract DECLARES the requirement and the validator
		// enforces it — which is what stops a newly added scan-shaped operation
		// inheriting the declaration without the check.
		//
		// Its own status and its own body: a gate app has to tell "this phone is
		// not paired" from "turn this person away", and the scan-shaped 422 below
		// would say the second about the first.
		if status == http.StatusUnauthorized {
			write(w, http.StatusUnauthorized, map[string]string{"error": "scanner device is not enrolled"})
			return
		}
		// The scan-shaped 422 is the gate's established representation and stays that
		// way — but it is not this whole service's, and applying it to the internal
		// refund route emitted a status that route does not declare (ai-review F4).
		// An undeclared status is exactly the drift ADR-028's response validator exists
		// to turn into a 500, and here it slipped past because the REQUEST validator
		// answers before the response validator can see it. Internal routes get the
		// Error shape they declare.
		// The voided feed declares the Error shape, not the scan shape, and the
		// difference is not cosmetic: the scan-shaped 422 below says "turn this
		// person away", which is a wrong and alarming answer to a malformed query
		// parameter on a background sync. This is the same drift F4 found on the
		// internal route — a new operation inheriting a representation that was
		// only ever the gate's — caught here before it shipped rather than after.
		if strings.HasPrefix(req.URL.Path, "/scans/voided-tickets") {
			write(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		if strings.HasPrefix(req.URL.Path, "/internal/") {
			// 404 covers BOTH an unknown path and a wrong method: the middleware
			// reports every route-lookup failure as 404, and it is not worth
			// distinguishing here (ai-review pass 3). This route already answers 404
			// to any caller without the internal token, so answering 404 to a wrong
			// method is the same deliberate silence rather than a new one — and 405
			// is not a status this operation declares, so returning it would be the
			// undeclared-status drift this handler exists to stop.
			if status == http.StatusNotFound {
				write(w, http.StatusNotFound, map[string]string{"error": "not found"})
				return
			}
			write(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "invalid_credential"})
	}, s.authenticateScannerDevice)
	if err != nil {
		panic(err)
	}
	// The slot the authentication func fills (see scannerOrganizerKey). It is
	// installed outside the validator because the validator runs before any
	// middleware the chi router could carry.
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		validated.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), scannerOrganizerKey{}, new(scannerIdentity))))
	})
}
func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func parseRef(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	ref, err := uuid.Parse(chi.URLParam(r, "ref"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid order reference"})
		return uuid.Nil, false
	}
	return ref, true
}
func (s *Server) tickets(w http.ResponseWriter, r *http.Request) {
	ref, ok := parseRef(w, r)
	if !ok {
		return
	}
	ts, err := s.st.Tickets(r.Context(), ref)
	if err != nil {
		write(w, 500, map[string]string{"error": "load tickets"})
		return
	}
	if len(ts) == 0 {
		write(w, 404, map[string]string{"error": "not found"})
		return
	}
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		history, err := s.st.History(r.Context(), t.ID)
		if err != nil {
			write(w, 500, map[string]string{"error": "load ticket history"})
			return
		}
		// Minted fresh on every load, and short-lived (ai-review S2). The bundle
		// page is the renewal mechanism, so a buyer never meets the expiry — only a
		// link that outlived its page does, which is the link that leaked.
		qrURL := "/api/access/orders/" + ref.String() + "/tickets/" + t.ID.String() + "/qr.png" +
			s.qrLinks.mint(ref, t.ID, s.clock())
		out = append(out, map[string]any{"ticket_id": t.ID, "qr_payload": t.Payload, "issued_at": t.IssuedAt, "history": history, "qr_url": qrURL})
	}
	write(w, 200, map[string]any{"order_ref": ref, "tickets": out})
}
func (s *Server) qr(w http.ResponseWriter, r *http.Request) {
	ref, ok := parseRef(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "ticket"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid ticket"})
		return
	}
	// A live signature from this service, or nothing (ai-review S2). 404 rather
	// than 401/403 and identical to the not-found answer on purpose: a distinct
	// status would tell a caller holding a dead link that the ticket behind it is
	// real, which is the one fact the link was protecting.
	if !s.qrLinks.verify(r, ref, id, s.clock()) {
		write(w, 404, map[string]string{"error": "not found"})
		return
	}
	payload, err := s.st.TicketForQR(r.Context(), ref, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			write(w, 404, map[string]string{"error": "not found"})
			return
		}
		write(w, 500, map[string]string{"error": "load QR"})
		return
	}
	image, err := qrcode.Encode(payload, qrcode.Medium, 256)
	if err != nil {
		write(w, 500, map[string]string{"error": "encode QR"})
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(image)
}

// parseOccurrence enforces the pairing the contract cannot express: occurred_at
// is required iff occurrence_id is present, and the id must be a UUIDv4
// (ADR-025 §D3). An empty occurrence_id returns Nil — old-scanner semantics.
func parseOccurrence(occurrenceID, occurredAt string) (uuid.UUID, time.Time, error) {
	if occurrenceID == "" {
		return uuid.Nil, time.Time{}, nil
	}
	occ, err := uuid.Parse(occurrenceID)
	if err != nil || occ == uuid.Nil || occ.Version() != 4 || occ.Variant() != uuid.RFC4122 {
		return uuid.Nil, time.Time{}, errors.New("occurrence_id must be a UUIDv4")
	}
	at, err := time.Parse(time.RFC3339, occurredAt)
	if err != nil {
		// RFC 3339 permits lowercase t/z; Go's layout is strict about them.
		// The contract says date-time, so accept what it advertises.
		at, err = time.Parse(time.RFC3339, strings.ToUpper(occurredAt))
	}
	if err != nil {
		return uuid.Nil, time.Time{}, errors.New("occurred_at (RFC 3339) is required with occurrence_id")
	}
	return occ, at, nil
}

// scannerDeviceHeader carries an enrolled gate device's token (ai-review S1).
//
// Distinct from X-Internal-Token, which every service holds and which the
// scanner — a static SPA served to phones — must never be given. Distinct from
// the staff write tokens too: this credential admits and reconciles at a door,
// which is nobody else's authority.
const scannerDeviceHeader = "X-Scanner-Token"

// scannerDeviceScheme is the name in the contract; scannerDeviceHeader is the
// header it declares. A scheme naming a header nobody reads is documentation, not
// a guard, so both are read from one place.
const scannerDeviceScheme = "ScannerDeviceToken"

// authenticateScannerDevice is the contract's ScannerDeviceToken scheme.
//
// It runs inside the request validator, so it answers BEFORE the body is decoded
// and long before a ticket is touched: an unenrolled caller learns nothing about
// a payload it submits, not even whether it parsed.
//
// Fail closed on a server with no enrolment port (a construction that skipped
// New, or a store-less unit test) rather
// than open: a Server that cannot check enrolment must refuse scans, because the
// alternative is that the one configuration nobody exercises is the one that
// admits everybody.
//
// The device is touched on a best-effort basis so an operator can tell a live
// gate from a forgotten enrolment. That write is never part of a scan's success
// (see TouchScannerDevice) — a turnstile must not refuse a valid ticket because
// a bookkeeping UPDATE lost a race.
func (s *Server) authenticateScannerDevice(ctx context.Context, input *openapi3filter.AuthenticationInput) error {
	if input.SecuritySchemeName != scannerDeviceScheme {
		// An unknown scheme must not be silently treated as satisfied: that is how
		// a renamed or mistyped scheme becomes an open door.
		return errors.New("unauthorized")
	}
	if s.devices == nil {
		return errors.New("unauthorized")
	}
	device, err := s.devices.AuthenticateScannerDevice(ctx, input.RequestValidationInput.Request.Header.Get(scannerDeviceHeader))
	if err != nil {
		return errors.New("unauthorized")
	}
	s.devices.TouchScannerDevice(ctx, device.ID)
	// Hand the device's organizer to the handler. Resolving a device and then
	// discarding what it is enrolled FOR is what made enrolment platform-wide.
	if slot, ok := input.RequestValidationInput.Request.Context().Value(scannerOrganizerKey{}).(*scannerIdentity); ok && slot != nil {
		// Both, together. The device id used to be resolved here and discarded,
		// which left the only input to `access revoke-scanner` unreachable from
		// a handler (TKT-272). One struct rather than a second context key: two
		// keys can drift out of step, and these two facts come from one row.
		*slot = scannerIdentity{OrganizerID: device.OrganizerID, DeviceID: device.ID}
	}
	return nil
}

// scannerIdentity is what the request validator resolved from the device token:
// the organizer the device is enrolled for, and the device's own id.
//
// The device id is here because revocation is the control on the polling
// surface (ADR-066, TKT-272) and it takes a device id, so telemetry that cannot
// name the device cannot feed the only control there is. It is the AUTHENTICATED
// id — nothing client-submitted ever reaches this struct.
type scannerIdentity struct {
	OrganizerID uuid.UUID
	DeviceID    uuid.UUID
}

// scannerOrganizerKey carries the authenticated device's identity from the
// request validator to the handler.
//
// The value behind it is a POINTER to a slot rather than the organizer itself,
// because an AuthenticationFunc is handed the request, not the chain: a context
// is immutable, and the func has no way to replace the request the handler will
// later see. So Router installs an empty slot on the way in and the auth func
// fills it — one slot per request, so two gates scanning at once cannot read
// each other's device.
type scannerOrganizerKey struct{}

// scannerScopeAllows answers whether the authenticated device may act on a
// ticket belonging to organizer.
//
// Enrolment is per-organizer — scanner_devices carries organizer_id, the CLI
// demands one, and an operator's device list is scoped by it — but until this
// comparison existed that was true only on paper: every enrolled device could
// redeem, and so BURN, any organizer's ticket at its own door, and the victim
// had no device to revoke because the offending one was never enrolled under
// them.
//
// Fails CLOSED when no device organizer is present. A request that reached a
// scan handler without the validator resolving a device is a route that lost
// its `security:` declaration, and the safe reading of "no device" is "not this
// organizer's device", not "everyone's".
func scannerScopeAllows(ctx context.Context, organizer uuid.UUID) bool {
	slot, ok := scannerOrganizer(ctx)
	if !ok {
		return false
	}
	return slot == organizer
}

// scannerOrganizer returns the authenticated device's organizer.
//
// scannerScopeAllows answers "may this device act on THAT organizer's ticket?",
// which is the question every scan path asks because the organizer arrives on the
// credential being scanned. The voided feed has no such input — it is a read OF
// the device's own organizer — so it needs the identity itself.
//
// Same fail-closed rule, one implementation: absent, nil or zero means no
// authenticated device, and for a read whose entire scope is that identity the
// only safe answer is to refuse. Returning uuid.Nil with no `ok` would invite a
// caller to pass it straight to a query, which is a whole-table read of every
// organizer's revocations.
func scannerOrganizer(ctx context.Context) (uuid.UUID, bool) {
	id, ok := scannerIdentityFrom(ctx)
	if !ok {
		return uuid.Nil, false
	}
	return id.OrganizerID, true
}

// scannerIdentityFrom returns the authenticated device's identity.
//
// Fail-closed on the ORGANIZER, matching what scannerOrganizer has always
// promised: a zero organizer means the validator never resolved a device, and
// every caller treats that as "refuse". The device id is not part of that
// condition — it is telemetry, and a missing id must never turn a valid scan
// into a refusal.
func scannerIdentityFrom(ctx context.Context) (scannerIdentity, bool) {
	slot, ok := ctx.Value(scannerOrganizerKey{}).(*scannerIdentity)
	if !ok || slot == nil || slot.OrganizerID == uuid.Nil {
		return scannerIdentity{}, false
	}
	return *slot, true
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	if s.verifier == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "scanner unavailable"})
		return
	}
	var input struct {
		QRPayload    string `json:"qr_payload"`
		OccurrenceID string `json:"occurrence_id"`
		OccurredAt   string `json:"occurred_at"`
		Direction    string `json:"direction"`
	}
	if err := httpx.DecodeJSON(w, r, &input, 8<<10); err != nil || input.QRPayload == "" {
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "invalid_credential"})
		return
	}
	// The contract enum already refused anything else; the default is entry so
	// an old scanner's request means exactly what it always meant (§D10).
	direction := store.AdmissionEntry
	if input.Direction == string(store.AdmissionExit) {
		direction = store.AdmissionExit
	}
	occ, occurredAt, err := parseOccurrence(input.OccurrenceID, input.OccurredAt)
	if err != nil {
		// Its own reason: a scanner must be able to tell a broken occurrence
		// envelope (fix the request) from a bad credential (deny the holder).
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "invalid_occurrence"})
		return
	}
	claims, err := s.verifier.Verify(input.QRPayload)
	if err != nil {
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "invalid_credential"})
		return
	}
	// The device is enrolled, but not necessarily for THIS organizer. Rejected
	// with the same reason and status as an unverifiable payload, deliberately:
	// a distinct answer would tell a device pointed at someone else's event that
	// the ticket it just read is real. And NOT a 401 — the phone is validly
	// paired, and the scanner unpairs itself on 401, which would turn one
	// misdirected scan into a gate device that has to be re-enrolled.
	if !scannerScopeAllows(r.Context(), claims.OrganizerID) {
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "invalid_credential"})
		return
	}
	result, err := s.st.Scan(r.Context(), store.ScanInput{
		RedeemInput: store.RedeemInput{
			TicketID: claims.TicketID, OrderID: claims.OrderID, OrganizerID: claims.OrganizerID, SlotID: claims.SlotID,
			OccurrenceID: occ, OccurredAt: occurredAt,
		},
		Direction: direction,
	})
	if errors.Is(err, store.ErrTicketCredential) {
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "invalid_credential"})
		return
	}
	if errors.Is(err, store.ErrOccurrenceCollision) {
		write(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected", "reason": "occurrence_collision"})
		return
	}
	if err != nil {
		write(w, http.StatusInternalServerError, map[string]string{"error": "redeem ticket"})
		return
	}
	// A degraded admission (ADR-021 §D6) is indistinguishable from a clean one to
	// the scanner, deliberately: the door opened either way, and the person at
	// the turnstile is not who the alarm is for. The operator learns about it
	// through the alarm route, not through the gate's screen.
	if !result.Accepted {
		reason := "already_redeemed"
		switch result.Decision {
		case store.DecisionIntegrityQuarantined:
			reason = "integrity_quarantined"
		case store.DecisionIntegrityOperatorControlled:
			reason = "integrity_operator_controlled"
		// Pass-policy denials (TKT-87): the Decision values are already the
		// wire reasons — distinguishable, and none of them appended anything.
		case store.DecisionEntryLimitReached, store.DecisionExitRequired,
			store.DecisionNotInside, store.DecisionExitNotApplicable,
			store.DecisionOccurrenceRequired, store.DecisionExitUnverified,
			// TKT-157: the Decision value is already the wire reason, and
			// ScanRejected.reason is an unconstrained string by design — no
			// contract change is needed to carry it.
			store.DecisionRefunded,
			// TKT-166: likewise, and deliberately a different reason string —
			// "refunded" would send an exchanging buyer looking for money back.
			store.DecisionExchanged:
			reason = string(result.Decision)
		}
		// No cryptographic detail leaves the gate: which field failed to verify
		// is exactly what an attacker probing the trail would want back.
		rejection := map[string]any{"decision": "rejected", "reason": reason}
		switch result.Decision {
		case store.DecisionAlreadyRedeemed, store.DecisionIntegrityQuarantined:
			// Only these denials have an original admission whose time is real
			// (ai-review G8): a policy denial has no prior scan to point at,
			// and fabricating one misleads any client that trusts the field.
			rejection["original_scan_at"] = result.OccurredAt
		}
		write(w, http.StatusConflict, rejection)
		return
	}
	response := map[string]any{"decision": "accepted", "scanned_at": result.OccurredAt}
	if result.Replayed {
		// Present-and-true only on replays (ADR-025 §D3: never a bare accepted)
		// — absent on first-time results so old scanners parse them unchanged.
		response["replay"] = true
	}
	write(w, http.StatusOK, response)
}

// reconcile syncs offline occurrences (ADR-025 §D6). Recording, not deciding:
// each occurrence gets its own result in request order, and one bad occurrence
// never fails the batch — a gate syncing a night's queue must learn the fate
// of every entry.
func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	if s.verifier == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "scanner unavailable"})
		return
	}
	var input struct {
		Occurrences []struct {
			QRPayload    string `json:"qr_payload"`
			OccurrenceID string `json:"occurrence_id"`
			OccurredAt   string `json:"occurred_at"`
			EventType    string `json:"event_type"`
		} `json:"occurrences"`
	}
	if err := httpx.DecodeJSON(w, r, &input, 256<<10); err != nil || len(input.Occurrences) == 0 {
		write(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid reconciliation request"})
		return
	}
	results := make([]map[string]any, 0, len(input.Occurrences))
	// The id is echoed VERBATIM, even when malformed: the scanner correlates
	// results to its queue by this value, and a normalized or zeroed id would
	// strand the queue entry in retry-forever (ai-review R3).
	rejected := func(occurrenceID string) map[string]any {
		return map[string]any{"occurrence_id": occurrenceID, "result": "rejected"}
	}
	for _, entry := range input.Occurrences {
		occ, occurredAt, err := parseOccurrence(entry.OccurrenceID, entry.OccurredAt)
		if err != nil || occ == uuid.Nil {
			results = append(results, rejected(entry.OccurrenceID))
			continue
		}
		// event_type follows the batch posture: an unknown value is ONE bad
		// row with its own rejected result, never a 422 for the night's queue.
		eventType := store.AdmissionEntry
		switch entry.EventType {
		case "", string(store.AdmissionEntry):
		case string(store.AdmissionExit):
			eventType = store.AdmissionExit
		default:
			results = append(results, rejected(entry.OccurrenceID))
			continue
		}
		claims, err := s.verifier.Verify(entry.QRPayload)
		if err != nil {
			results = append(results, rejected(entry.OccurrenceID))
			continue
		}
		// Same scope check as a live scan, and the same reason it matters more
		// here: reconcile REWRITES a night's admission history, so an unscoped
		// batch lets one organizer's device rewrite another's. Per-occurrence,
		// following the batch posture — one out-of-scope entry is one rejected
		// result, not a failure for the whole queue.
		if !scannerScopeAllows(r.Context(), claims.OrganizerID) {
			results = append(results, rejected(entry.OccurrenceID))
			continue
		}
		result, err := s.st.ReconcileAdmission(r.Context(), store.ReconcileOccurrence{
			TicketID: claims.TicketID, OrderID: claims.OrderID, OrganizerID: claims.OrganizerID, SlotID: claims.SlotID,
			OccurrenceID: occ, OccurredAt: occurredAt, Type: eventType,
		})
		if errors.Is(err, store.ErrTicketCredential) || errors.Is(err, store.ErrOccurrenceCollision) || errors.Is(err, store.ErrExitNotApplicable) {
			results = append(results, rejected(entry.OccurrenceID))
			continue
		}
		if err != nil {
			// Infrastructure failure, not a per-occurrence verdict: the whole
			// batch fails so the scanner keeps its queue and retries.
			write(w, http.StatusInternalServerError, map[string]string{"error": "reconcile admissions"})
			return
		}
		entryResult := map[string]any{"occurrence_id": result.OccurrenceID, "result": string(result.Outcome), "occurred_at": result.OccurredAt}
		if result.SkewFlagged {
			entryResult["skew_flagged"] = true
		}
		results = append(results, entryResult)
	}
	write(w, http.StatusOK, map[string]any{"results": results})
}
