package api

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	apispec "ticketing/services/payments/api"
	"ticketing/services/payments/internal/psp"
	"ticketing/services/payments/internal/splits"
	"ticketing/services/payments/internal/store"
	"ticketing/shared/contract"
	"ticketing/shared/httpx"
)

type Server struct {
	journal    *store.Journal
	credential string
	psp        psp.PSP
	// statusReplayRetention bounds same-key status replay for ref-less unresolved
	// operations — a property of the configured provider (Stripe retains idempotency
	// keys ~24h; the fake retains forever, expressed as 0 = unbounded). See
	// statusReplayDeadline (psp.go) and ADR-032 §Status/replay amendment (TKT-115).
	statusReplayRetention time.Duration
}

// New wires the payments server. The PSP port decides charge outcomes; New(j, cred)
// defaults it to the fake PSP so existing callers and the fact-only tests are unchanged.
// Callers select a provider with NewWithPSP (main.go picks fake vs Stripe by config).
func New(j *store.Journal, credential string) *Server {
	return NewWithPSP(j, credential, psp.NewFake())
}

// NewWithPSP wires the server against an explicit PSP implementation with no
// status-replay bound (correct for the fake; Stripe callers use NewWithPSPRetention).
func NewWithPSP(j *store.Journal, credential string, provider psp.PSP) *Server {
	return NewWithPSPRetention(j, credential, provider, 0)
}

// NewWithPSPRetention wires the server with the provider's idempotency-key retention,
// which bounds the status-replay contract (0 = unbounded).
func NewWithPSPRetention(j *store.Journal, credential string, provider psp.PSP, retention time.Duration) *Server {
	return &Server{journal: j, credential: credential, psp: provider, statusReplayRetention: retention}
}
func (s *Server) Router(log *slog.Logger, validateResponses bool) http.Handler {
	r := chi.NewRouter()
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=300")
		_, _ = w.Write(apispec.Spec)
	})
	r.Post("/internal/facts", s.fact)
	r.Post("/internal/charges", s.charge)
	r.Get("/internal/operations", s.operation)
	r.Get("/internal/psp/status", s.pspStatus)
	r.Post("/internal/psp/void", s.pspVoid)
	r.Post("/internal/psp/refund", s.pspRefund)
	r.Post("/internal/psp/partial-refund", s.pspPartialRefund)
	validated, err := contract.RequestValidator(apispec.Spec, r, log, validateResponses)
	if err != nil {
		panic(err)
	}
	return validated
}
func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) authorized(r *http.Request) bool {
	return s.credential != "" && r.Header.Get("X-Internal-Token") == s.credential
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if httpx.DecodeJSON(w, r, v, 1<<20) != nil {
		write(w, 400, map[string]string{"error": "invalid body"})
		return false
	}
	return true
}

// operation reports an already-bound payment operation's recorded outcome. Read-only:
// it never binds, so a recovery pass cannot fabricate an operation for an order that
// never charged. 404 means no operation exists — evidence the charge was never
// submitted, which is what lets commerce release the claim rather than guess.
func (s *Server) operation(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		write(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	org, err := uuid.Parse(r.URL.Query().Get("organizer_id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "valid organizer_id required"})
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("idempotency_key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "idempotency_key required"})
		return
	}
	op, found, err := s.journal.LookupOperation(r.Context(), org, key)
	if err != nil {
		write(w, 500, map[string]string{"error": "lookup operation"})
		return
	}
	if !found {
		write(w, 404, map[string]string{"error": "operation not found"})
		return
	}
	// occurred_at is returned for EVERY found operation (TKT-115): it is the durable
	// bind time commerce derives the status-replay deadline from — an unresolved
	// operation is exactly the caller that needs it.
	out := map[string]any{"resolved": op.Resolved, "occurred_at": op.OccurredAt}
	if op.Resolved {
		out["status"] = op.Status
		out["fact_id"] = op.FactID
	}
	if deadline, bounded := statusReplayDeadline(op, s.statusReplayRetention); bounded {
		out["status_replay_deadline_at"] = deadline
	}
	write(w, 200, out)
}

func (s *Server) fact(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		write(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var f store.Fact
	if !decode(w, r, &f) {
		return
	}
	e, replay, err := s.journal.Append(r.Context(), f)
	if err != nil {
		write(w, 400, map[string]string{"error": err.Error()})
		return
	}
	write(w, 200, map[string]any{"sequence": e.Sequence, "hash": store.Hex(e.EntryHash), "replay": replay})
}

type chargeRequest struct {
	OrderID      uuid.UUID `json:"order_id"`
	OrganizerID  uuid.UUID `json:"organizer_id"`
	BuyerID      uuid.UUID `json:"buyer_id"`
	Amount       int64     `json:"amount"`
	Currency     string    `json:"currency"`
	PaymentToken string    `json:"payment_token"`
	// Settlement is how this capture is attributed (TKT-217). Derived by
	// commerce from the fee snapshot it persisted at reserve time.
	Settlement *settlementPlanRequest `json:"settlement,omitempty"`
}

type settlementPlanRequest struct {
	FaceValue   int64  `json:"face_value"`
	PassedOn    int64  `json:"passed_on"`
	Absorbed    int64  `json:"absorbed"`
	TotalAmount int64  `json:"total_amount"`
	Currency    string `json:"currency"`
	Fees        []struct {
		FeeCode   string `json:"fee_code"`
		Incidence string `json:"incidence"`
		Amount    int64  `json:"amount"`
		Currency  string `json:"currency"`
		Parts     []struct {
			PayeeID           uuid.UUID `json:"payee_id"`
			Kind              string    `json:"kind"`
			DisplayName       string    `json:"display_name"`
			ExternalReference *string   `json:"external_reference,omitempty"`
			ShareBps          int32     `json:"share_bps"`
		} `json:"parts"`
	} `json:"fees"`
}

// toStorePlan maps the wire shape onto the store's plan. The mapping is here
// rather than in the store so the store's types stay free of JSON concerns —
// and so an added wire field cannot silently change settlement semantics.
func (p *settlementPlanRequest) toStorePlan() store.SettlementPlan {
	out := store.SettlementPlan{
		FaceValue: p.FaceValue, PassedOn: p.PassedOn, Absorbed: p.Absorbed,
		TotalAmount: p.TotalAmount, Currency: p.Currency,
	}
	for _, f := range p.Fees {
		line := store.FeeLine{FeeCode: f.FeeCode, Incidence: f.Incidence, Amount: f.Amount,
			Currency: f.Currency, Payees: map[uuid.UUID]store.PayeeRef{}}
		for _, part := range f.Parts {
			line.Shares = append(line.Shares, splits.Share{PayeeID: part.PayeeID, ShareBps: part.ShareBps})
			line.Payees[part.PayeeID] = store.PayeeRef{ID: part.PayeeID, Kind: part.Kind,
				DisplayName: part.DisplayName, ExternalReference: part.ExternalReference}
		}
		out.Fees = append(out.Fees, line)
	}
	return out
}

func (s *Server) charge(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		write(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	var in chargeRequest
	if !decode(w, r, &in) {
		return
	}
	if in.OrderID == uuid.Nil || in.OrganizerID == uuid.Nil || in.BuyerID == uuid.Nil || in.Amount < 0 || in.Currency != "EUR" {
		write(w, 400, map[string]string{"error": "invalid charge"})
		return
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d\n%s\n%s", in.OrderID, in.BuyerID, in.Amount, in.Currency, in.PaymentToken))))
	boundStatus, boundID, occurredAt, replay, err := s.journal.BindOperation(r.Context(), in.OrganizerID, key, fingerprint, store.OperationRequest{
		OrderID: in.OrderID, BuyerID: in.BuyerID, Amount: in.Amount, Currency: in.Currency, PaymentMethodRef: in.PaymentToken,
	})
	if err != nil {
		write(w, 409, map[string]string{"error": err.Error()})
		return
	}
	if replay {
		code := 200
		if boundStatus == "declined" {
			code = 402
		}
		if boundStatus == "timeout" {
			code = 408
		}
		write(w, code, map[string]any{"status": boundStatus, "payment_id": boundID, "replay": true})
		return
	}
	result, err := s.psp.Authorize(r.Context(), psp.AuthorizeRequest{
		OrganizerID:    in.OrganizerID.String(),
		OrderID:        in.OrderID.String(),
		BuyerID:        in.BuyerID.String(),
		Amount:         in.Amount,
		Currency:       in.Currency,
		PaymentToken:   in.PaymentToken,
		IdempotencyKey: key,
	})
	if errors.Is(err, psp.ErrInvalidToken) {
		// Port-level message, not fake-specific: a Stripe adapter (S2) maps its own
		// "no such payment method" to ErrInvalidToken, and the caller must not see "fake".
		write(w, 400, map[string]string{"error": "invalid payment token"})
		return
	}
	if err != nil {
		// The charge contract declares no gateway-error status (200/400/401/402/408/409/
		// 500); a provider/transport failure is an internal failure to complete, so it maps
		// to 500 like the journal-append failures below. The Stripe slice, which actually
		// calls out to a provider, introduces explicit upstream-failure statuses in the spec.
		write(w, 500, map[string]string{"error": "payment provider error"})
		return
	}
	// Fail closed on a self-contradictory provider result before writing anything to the
	// money journal (e.g. a captured outcome that did not capture). The fake always produces
	// consistent results; a future adapter might not, and the journal must not record an
	// impossible payment.
	if err := result.Validate(); err != nil {
		write(w, 500, map[string]string{"error": "invalid payment outcome"})
		return
	}
	// Derive the journalled fact type, the operation status string and the HTTP code from
	// the normalized PSP outcome ALONE — a single dispatch point, so the authorize append
	// and the terminal fact can never be decided from divergent fields. This preserves the
	// pre-refactor mapping exactly: a captured success appends payment.authorized first,
	// decline/timeout append a single terminal fact, HTTP codes unchanged (200/402/408).
	var status, factType string
	var code int
	// Provider evidence persisted with the completion: references and the amounts the
	// normalized outcome proves. This is the durable basis the compensation endpoints'
	// state checks read (void needs authorized-uncaptured, refund needs captured money).
	prov := store.ProviderResult{PaymentRef: result.ProviderRef, ChargeRef: result.ProviderChargeRef}
	switch result.Outcome {
	case psp.Captured:
		// Captured is authorize-then-capture: append payment.authorized first, from this
		// case, not a separate boolean guard (Validate has proven Authorized && Captured).
		authorizedID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("payment:"+in.OrganizerID.String()+":"+key+":payment.authorized"))
		if _, _, err := s.journal.Append(r.Context(), store.Fact{ID: authorizedID, OrganizerID: in.OrganizerID, Type: "payment.authorized", OccurredAt: occurredAt, BuyerID: in.BuyerID, Amount: in.Amount, Currency: in.Currency, Payload: map[string]string{"order_id": in.OrderID.String()}}); err != nil {
			write(w, 500, map[string]string{"error": "journal append failed"})
			return
		}
		status, factType, code = "captured", "payment.captured", 200
		prov.State, prov.AuthorizedAmount, prov.CapturedAmount = "captured", in.Amount, in.Amount
	case psp.Declined:
		status, factType, code = "declined", "payment.declined", 402
		prov.State = "declined"
	case psp.Timeout:
		status, factType, code = "timeout", "payment.timeout", 408
		prov.State = "timeout"
	default:
		// Authorized-only and Unknown are structurally valid (Validate accepts them) but the
		// charge path only produces captured/declined/timeout, so they fail closed here.
		// The S2 Stripe adapter satisfies this by CAPTURING INTERNALLY on the charge path
		// (Authorize returns Captured, never Authorized — plan-final F2), and a transport
		// failure surfaces as err != nil above, so this default stays unreachable on the
		// happy path. The bound-but-incomplete operation a 500 leaves behind is exactly the
		// payment_unknown case, now resolvable via GET /internal/psp/status (replay-safe).
		write(w, 500, map[string]string{"error": "unsupported payment outcome"})
		return
	}
	factID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("payment:"+in.OrganizerID.String()+":"+key+":"+factType))
	// Settlement rides the captured fact's transaction (ADR-048). Only a capture
	// settles: a decline or a timeout moved no money, so there is nothing to
	// attribute and the ledger stays silent about it.
	var settlement []store.SettlementEntry
	if factType == "payment.captured" {
		if in.Settlement == nil {
			// Fail closed. The database would refuse this anyway — the deferred
			// trigger requires entries for a captured fact — but refusing here
			// says WHY, and says it before the row lock is taken.
			write(w, 400, map[string]string{"error": "a captured charge must carry a settlement plan"})
			return
		}
		settlement, err = store.BuildSettlementEntries(in.Settlement.toStorePlan(), in.Amount)
		if err != nil {
			// The plan is our own data being wrong, not the caller's request
			// shape — the same disposition catalog gives a misconfigured rule.
			// The reason is deliberately not returned: it names fee codes and
			// payees, and this response reaches commerce, not an operator.
			write(w, 500, map[string]string{"error": "settlement plan unusable"})
			return
		}
	}
	e, replay, err := s.journal.AppendWithSettlement(r.Context(), store.Fact{ID: factID, OrganizerID: in.OrganizerID, Type: factType, OccurredAt: occurredAt, BuyerID: in.BuyerID, Amount: in.Amount, Currency: in.Currency, Payload: map[string]string{"order_id": in.OrderID.String()}}, settlement)
	if err != nil {
		write(w, 500, map[string]string{"error": "journal append failed"})
		return
	}
	if err := s.journal.CompleteOperation(r.Context(), in.OrganizerID, key, status, factID, prov); err != nil {
		write(w, 500, map[string]string{"error": "persist payment result"})
		return
	}
	write(w, code, map[string]any{"status": status, "payment_id": factID, "sequence": e.Sequence, "replay": replay})
}
