package api

// The provider-neutral PSP recovery surface (TKT-114/S2, ADR-032): status resolution and
// the void/refund compensation endpoints. Requests and responses carry ONLY the
// organizer-scoped operation identity — no Stripe identifiers, no payment-method
// reference, no secret material. Payments resolves provider references, validates that
// the requested compensation matches the stored durable evidence, and journals the
// compensating fact; commerce (S3) consumes this without ever learning Stripe's dialect.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"ticketing/services/payments/internal/psp"
	"ticketing/services/payments/internal/store"
)

// operationParams parses the shared organizer/idempotency-key query identity.
func operationParams(w http.ResponseWriter, r *http.Request) (uuid.UUID, string, bool) {
	org, err := uuid.Parse(r.URL.Query().Get("organizer_id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "valid organizer_id required"})
		return uuid.Nil, "", false
	}
	key := strings.TrimSpace(r.URL.Query().Get("idempotency_key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "idempotency_key required"})
		return uuid.Nil, "", false
	}
	return org, key, true
}

// statusBody is the provider-neutral status answer: the normalized outcome plus the
// amounts that evidence proves. It never contains provider references.
func statusBody(result psp.Result, authorized, captured int64, currency string) map[string]any {
	return map[string]any{
		"outcome":                 string(result.Outcome),
		"terminal_no_side_effect": result.TerminalNoSideEffect,
		"captured":                result.Captured,
		"authorized":              result.Authorized,
		"authorized_amount":       authorized,
		"captured_amount":         captured,
		"currency":                currency,
	}
}

// resolvedResult reconstructs the normalized result from a locally-resolved operation's
// terminal status. Local durable evidence IS the answer for a completed operation — no
// provider call, so the endpoint works identically for the fake and Stripe.
func resolvedResult(op store.Operation) (psp.Result, bool) {
	switch op.Status {
	case "captured":
		return psp.Result{Outcome: psp.Captured, Captured: true, Authorized: true}, true
	case "declined":
		return psp.Result{Outcome: psp.Declined, TerminalNoSideEffect: true}, true
	case "timeout":
		return psp.Result{Outcome: psp.Timeout, TerminalNoSideEffect: true}, true
	default:
		return psp.Result{}, false
	}
}

// pspStatus resolves an operation's provider state. A resolved operation answers from
// its recorded terminal status; an unresolved one asks the provider — retrieve when the
// provider reference was persisted, replay under the SAME original idempotency key when
// it was not (ADR-032 §Status/replay) — and persists what it learned as durable evidence
// for the void/refund state checks.
func (s *Server) pspStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		write(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	org, key, ok := operationParams(w, r)
	if !ok {
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
	// A completed compensation supersedes the operation's terminal status (ai-review B6):
	// after a refund/void, reporting "captured"/"authorized" with live amounts would tell
	// the caller money is still held that has already been returned or released.
	if comp, found, err := s.journal.LookupCompensation(r.Context(), org, key, "refund"); err == nil && found && comp.Completed {
		write(w, 200, statusBody(psp.Result{Outcome: psp.Refunded}, 0, 0, op.RequestCurrency))
		return
	} else if err != nil {
		write(w, 500, map[string]string{"error": "lookup compensation"})
		return
	}
	if comp, found, err := s.journal.LookupCompensation(r.Context(), org, key, "void"); err == nil && found && comp.Completed {
		write(w, 200, statusBody(psp.Result{Outcome: psp.Voided, TerminalNoSideEffect: true}, 0, 0, op.RequestCurrency))
		return
	} else if err != nil {
		write(w, 500, map[string]string{"error": "lookup compensation"})
		return
	}
	if result, ok := resolvedResult(op); ok {
		write(w, 200, statusBody(result, op.AuthorizedAmount, op.CapturedAmount, op.RequestCurrency))
		return
	}
	result, err := s.psp.Status(r.Context(), psp.StatusRequest{
		ProviderRef:    op.ProviderPaymentRef,
		IdempotencyKey: key,
		Amount:         op.RequestAmount,
		Currency:       op.RequestCurrency,
		PaymentToken:   op.PaymentMethodRef,
	})
	if err != nil {
		// Still ambiguous (transport failure, unsettled provider state): 502, and the
		// operation stays exactly as recoverable as before. Never invented as terminal.
		write(w, 502, map[string]string{"error": "provider status unresolved"})
		return
	}
	if err := result.Validate(); err != nil {
		write(w, 500, map[string]string{"error": "invalid provider status"})
		return
	}
	// Translate the outcome into the amounts it proves for THIS operation, then persist
	// the learned evidence (refs + state + amounts) without touching the terminal status.
	var authorized, captured int64
	var state string
	switch result.Outcome {
	case psp.Authorized:
		state, authorized = "authorized", op.RequestAmount
	case psp.Captured:
		state, authorized, captured = "captured", op.RequestAmount, op.RequestAmount
	case psp.Voided:
		state = "voided"
	case psp.Declined:
		state = "declined"
	case psp.Timeout:
		state = "timeout"
	default:
		// Unknown, Refunded (only reachable through a re_ ref this endpoint never holds),
		// or any future outcome: nothing this operation's evidence columns can safely
		// record — answer honestly, persist nothing (fail-safe, ai-review A3).
		write(w, 200, statusBody(result, 0, 0, op.RequestCurrency))
		return
	}
	recorded, err := s.journal.RecordProviderState(r.Context(), org, key, store.ProviderResult{
		PaymentRef: result.ProviderRef, ChargeRef: result.ProviderChargeRef,
		State: state, AuthorizedAmount: authorized, CapturedAmount: captured,
	})
	if err != nil {
		write(w, 500, map[string]string{"error": "persist provider state"})
		return
	}
	if !recorded {
		// The monotonic guard blocked a STALE observation (second-pass P2-2): the store
		// holds stronger evidence than what the provider just answered — report the
		// stored truth, not the answer we refused to record.
		fresh, found, err := s.journal.LookupOperation(r.Context(), org, key)
		if err != nil || !found {
			write(w, 500, map[string]string{"error": "lookup operation"})
			return
		}
		storedResult, storedOK := providerStateResult(fresh)
		if !storedOK {
			// The guard blocked the write but the stored state is not reconstructable
			// (a state this switch does not know): reporting the refused observation
			// would present blocked-as-stale data as truth — fail loud instead
			// (third-pass P3-1).
			write(w, 500, map[string]string{"error": "provider evidence inconsistent"})
			return
		}
		write(w, 200, statusBody(storedResult, fresh.AuthorizedAmount, fresh.CapturedAmount, fresh.RequestCurrency))
		return
	}
	write(w, 200, statusBody(result, authorized, captured, op.RequestCurrency))
}

// providerStateResult reconstructs the normalized result from the operation's recorded
// provider evidence — the answer of record when a fresher observation was refused by the
// monotonic guard.
func providerStateResult(op store.Operation) (psp.Result, bool) {
	switch op.ProviderState {
	case "authorized":
		return psp.Result{Outcome: psp.Authorized, Authorized: true}, true
	case "captured":
		return psp.Result{Outcome: psp.Captured, Captured: true, Authorized: true}, true
	case "declined":
		return psp.Result{Outcome: psp.Declined, TerminalNoSideEffect: true}, true
	case "timeout":
		return psp.Result{Outcome: psp.Timeout, TerminalNoSideEffect: true}, true
	case "voided":
		return psp.Result{Outcome: psp.Voided, TerminalNoSideEffect: true}, true
	default:
		return psp.Result{}, false
	}
}

// compensationAllowed validates that the requested compensation is the correct one for
// the stored durable evidence (plan §void-vs-refund): void releases an authorized,
// uncaptured hold; refund returns captured money. The caller's claimed state is never
// trusted, and a row without evidence (bound before migration 0002, or never resolved
// against the provider) supports no compensation at all.
func compensationAllowed(op store.Operation, kind string) error {
	if op.OrderID == uuid.Nil || op.BuyerID == uuid.Nil || op.RequestCurrency == "" {
		return errors.New("operation carries no compensation evidence")
	}
	switch kind {
	case "void":
		if op.ProviderState != "authorized" || op.CapturedAmount != 0 || op.AuthorizedAmount <= 0 {
			return errors.New("void requires an authorized, uncaptured operation")
		}
	case "refund":
		if op.ProviderState != "captured" || op.CapturedAmount <= 0 {
			return errors.New("refund requires captured money")
		}
	default:
		return errors.New("unknown compensation kind")
	}
	return nil
}

// compensationBasis is the money the compensation acts on, from the STORED row only: a
// void releases the authorized amount, a refund returns the captured amount (plan-final
// A5). Caller-supplied amounts do not exist in this API.
func compensationBasis(op store.Operation, kind string) (int64, string) {
	if kind == "void" {
		return op.AuthorizedAmount, op.RequestCurrency
	}
	return op.CapturedAmount, op.RequestCurrency
}

// compensationFactID derives the deterministic compensating-fact ID (plan-final A3),
// mirroring the charge handler's derivation: exactly-once across the append/complete
// crash boundary rests on the journal's fact-id replay dedupe, which needs the SAME ID
// on every retry.
func compensationFactID(org uuid.UUID, sourceKey, factType string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("payment:"+org.String()+":"+sourceKey+":"+factType))
}

type compensationRequest struct {
	OrganizerID    uuid.UUID `json:"organizer_id"`
	IdempotencyKey string    `json:"idempotency_key"`
}

func (s *Server) pspVoid(w http.ResponseWriter, r *http.Request) { s.compensate(w, r, "void") }

func (s *Server) pspRefund(w http.ResponseWriter, r *http.Request) { s.compensate(w, r, "refund") }

// compensate drives the durable compensation sequence: bind (deterministic provider key)
// → replay short-circuit → provider call → journal the compensating fact → complete. A
// provider failure or an unsettled result (refund pending — plan-final A7) leaves the
// compensation BOUND and answers 502: recoverable, never terminal, no fact appended
// (plan-final A1). A crash between provider success and completion is absorbed by the
// deterministic provider idempotency key + the deterministic fact ID.
func (s *Server) compensate(w http.ResponseWriter, r *http.Request, kind string) {
	if !s.authorized(r) {
		write(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var in compensationRequest
	if !decode(w, r, &in) {
		return
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if in.OrganizerID == uuid.Nil || key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "organizer_id and idempotency_key required"})
		return
	}
	op, found, err := s.journal.LookupOperation(r.Context(), in.OrganizerID, key)
	if err != nil {
		write(w, 500, map[string]string{"error": "lookup operation"})
		return
	}
	if !found {
		write(w, 404, map[string]string{"error": "operation not found"})
		return
	}
	// Replay/resume check FIRST, read-only (ai-review B5, second-pass P2-1): a completed
	// compensation answers as a replay, and a BOUND-but-incomplete one RESUMES on the
	// basis recorded at bind time. Eligibility is derived once, when the compensation is
	// first bound — re-deriving it on a resume would 409 the compensation's own progress
	// (a crash between the provider call and completion legitimately moves the evidence
	// to voided/refunded, which no longer looks eligible), wedging it permanently.
	existing, resuming, err := s.journal.LookupCompensation(r.Context(), in.OrganizerID, key, kind)
	if err != nil {
		write(w, 500, map[string]string{"error": "lookup compensation"})
		return
	}
	if resuming && existing.Completed {
		write(w, 200, map[string]any{"status": existing.Status, "fact_id": existing.FactID, "replay": true})
		return
	}
	var amount int64
	var currency string
	if resuming {
		// The bind recorded the money basis durably; evidence columns may since have been
		// zeroed by the very progress we are resuming (e.g. voided => authorized_amount 0).
		amount, currency = existing.Amount, existing.Currency
	} else {
		if err := compensationAllowed(op, kind); err != nil {
			write(w, 409, map[string]string{"error": err.Error()})
			return
		}
		amount, currency = compensationBasis(op, kind)
	}
	comp, err := s.journal.BindCompensation(r.Context(), in.OrganizerID, key, kind, amount, currency)
	if err != nil {
		write(w, 500, map[string]string{"error": "bind compensation"})
		return
	}
	if comp.Completed {
		write(w, 200, map[string]any{"status": comp.Status, "fact_id": comp.FactID, "replay": true})
		return
	}
	var result psp.Result
	var wantOutcome psp.Outcome
	var status, factType string
	switch {
	case kind == "void":
		wantOutcome, status, factType = psp.Voided, "voided", "payment.voided"
		result, err = s.psp.Void(r.Context(), op.ProviderPaymentRef, comp.ProviderKey)
	case comp.ProviderRef != "":
		// A refund the provider accepted but left pending on an earlier attempt: RESOLVE
		// the recorded re_ reference — re-submitting under the same idempotency key would
		// replay the original "pending" snapshot forever (ai-review B3).
		wantOutcome, status, factType = psp.Refunded, "refunded", "payment.refunded"
		result, err = s.psp.Status(r.Context(), psp.StatusRequest{ProviderRef: comp.ProviderRef})
	default:
		wantOutcome, status, factType = psp.Refunded, "refunded", "payment.refunded"
		result, err = s.psp.Refund(r.Context(), op.ProviderPaymentRef, comp.ProviderKey, amount, currency)
	}
	if err != nil {
		// A pending refund is recoverable, but its provider reference is durable progress:
		// persist it so the next attempt resolves instead of re-submitting (ai-review B3).
		if errors.Is(err, psp.ErrRefundPending) && result.ProviderRef != "" && comp.ProviderRef == "" {
			if recErr := s.journal.RecordCompensationProviderRef(r.Context(), in.OrganizerID, key, kind, result.ProviderRef); recErr != nil {
				write(w, 500, map[string]string{"error": "persist compensation reference"})
				return
			}
		}
		write(w, 502, map[string]string{"error": "provider compensation unresolved"})
		return
	}
	if err := result.Validate(); err != nil {
		write(w, 500, map[string]string{"error": "invalid provider result"})
		return
	}
	if result.Outcome != wantOutcome {
		// A structurally valid but unexpected outcome (e.g. Unknown without error) is
		// still not proof the compensation happened: stay bound, stay recoverable.
		write(w, 502, map[string]string{"error": "provider compensation unresolved"})
		return
	}
	factID := compensationFactID(in.OrganizerID, key, factType)
	// OccurredAt is the compensation row's stable bound_at, NEVER the clock: the fact ID is
	// deterministic and the journal's replay dedupe compares the full canonical fact, so a
	// retry across the append/complete crash boundary must rebuild byte-identical content —
	// a fresh timestamp would fail "fact id reused with different content" and wedge the
	// compensation permanently (ai-review B1).
	if _, _, err := s.journal.Append(r.Context(), store.Fact{
		ID: factID, OrganizerID: in.OrganizerID, Type: factType, OccurredAt: comp.BoundAt,
		BuyerID: op.BuyerID, Amount: amount, Currency: currency,
		Payload: map[string]string{"order_id": op.OrderID.String()},
	}); err != nil {
		write(w, 500, map[string]string{"error": "journal append failed"})
		return
	}
	if err := s.journal.CompleteCompensation(r.Context(), in.OrganizerID, key, kind, status, result.ProviderRef, factID); err != nil {
		write(w, 500, map[string]string{"error": "persist compensation result"})
		return
	}
	write(w, 200, map[string]any{"status": status, "fact_id": factID, "replay": false})
}
