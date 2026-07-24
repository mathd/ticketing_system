package recovery

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
)

// HTTPClients adapts the recovery runner's ports to the internal service APIs. The
// runner is background work with no request on the stack, so it owns its own client
// rather than borrowing the API server's.
//
// It implements the Payments and Inventory ports only. Completer is a pure database
// transaction and Journal composes a database write with an HTTP post, so neither
// belongs on the type whose whole job is talking HTTP to another service.
type HTTPClients struct {
	Client                    *http.Client
	InventoryURL, PaymentsURL string
	Token                     string
}

func (c HTTPClients) do(ctx context.Context, method, url string, out any) (int, error) {
	return c.doBody(ctx, method, url, nil, out)
}

// doBody is do with an optional JSON request body — the compensation POSTs carry the
// operation identity in the body, everything else stays body-less.
func (c HTTPClients) doBody(ctx context.Context, method, url string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Internal-Token", c.Token)
	resp, err := c.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil && resp.StatusCode == http.StatusOK {
		// Decode stops at the first complete JSON value, so `{"status":"refunded"}garbage`
		// and two concatenated bodies both "succeed" — and the callers treat a decoded 200
		// as proof enough to release seats and mark orders refunded. A body we cannot read
		// in full is not proof: require exactly one value (ai-review pass 2, F1).
		dec := json.NewDecoder(resp.Body)
		if err := dec.Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
		// Not dec.More(): it reports whether another element follows in the CURRENT array
		// or object, so it answers false on a `}` or `]` trailer — `{"status":"refunded"}}`
		// and even `{"status":"refunded"}} {"status":"voided"}` slipped through and were
		// accepted as proof (ai-review pass 3, P3-3). Only a Token() that reports EOF
		// proves the body held exactly one value; trailing whitespace still reads as EOF.
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return resp.StatusCode, fmt.Errorf("decode response: trailing content after JSON value")
		}
	}
	return resp.StatusCode, nil
}

// Sentinels for the PSP recovery surface (TKT-115). Each carries a distinct decision:
// ErrWrongCompensation re-derives, ErrProviderUnresolved retries later (the compensation
// stays bound in payments — never terminal), ErrReplayWindowExpired parks in ONE pass
// (retrying cannot help: the provider forgot the idempotency key, and a replay would
// mint a second PaymentIntent), ErrOperationNotFound parks as inconsistent durable
// state (an order in a PSP-recovery status must have a bound operation).
var (
	ErrWrongCompensation   = errors.New("compensation does not match the stored operation evidence")
	ErrProviderUnresolved  = errors.New("provider state unresolved; compensation stays bound")
	ErrReplayWindowExpired = errors.New("status replay window expired; manual reconciliation required")
	ErrOperationNotFound   = errors.New("payment operation not found")
)

// Status resolves an operation's provider state via payments' provider-neutral
// GET /internal/psp/status. Only a decoded 200 is evidence; every other answer maps to
// a sentinel naming the decision it forces.
func (c HTTPClients) Status(ctx context.Context, org uuid.UUID, key string) (PSPStatus, error) {
	u := fmt.Sprintf("%s/internal/psp/status?organizer_id=%s&idempotency_key=%s",
		c.PaymentsURL, org, url.QueryEscape(key))
	var body PSPStatus
	code, err := c.do(ctx, http.MethodGet, u, &body)
	if err != nil {
		return PSPStatus{}, err
	}
	switch code {
	case http.StatusOK:
		// A 200 is proof only when the outcome and the money flags AGREE. actOnProviderStatus
		// dispatches on Outcome alone, so a contradictory body (version skew, an upstream bug)
		// such as outcome=declined with captured=true would reach recordAndRelease and free
		// the seats against live money — exactly what ADR-016 §D3 forbids. Fail closed as
		// unresolved: the compensation stays bound and the next pass re-derives from fresh
		// evidence, never a terminal decision on a body we do not trust (ai-review pass 2, F2).
		if !consistentStatus(body) {
			return PSPStatus{}, fmt.Errorf("psp status: outcome %q contradicts captured=%t authorized=%t terminal_no_side_effect=%t: %w",
				body.Outcome, body.Captured, body.Authorized, body.TerminalNoSideEffect, ErrProviderUnresolved)
		}
		return body, nil
	case http.StatusNotFound:
		return PSPStatus{}, ErrOperationNotFound
	case http.StatusConflict:
		return PSPStatus{}, ErrReplayWindowExpired
	case http.StatusBadGateway:
		return PSPStatus{}, ErrProviderUnresolved
	default:
		return PSPStatus{}, fmt.Errorf("psp status: unexpected status %d", code)
	}
}

// consistentStatus reports whether a status body's money flags agree with its outcome, as
// payments actually emits them (services/payments/internal/api/psp.go statusBody):
// captured carries the authorization it captured; a refund's money already came back, so
// `refunded` carries NO live money and is NOT terminal-no-side-effect (ADR-032 amendment).
// An unrecognized outcome needs no check — actOnProviderStatus already proves nothing from
// it and retries. This is a boundary check, not a trust decision: it constrains an honest
// payments that has skewed or regressed, never an adversary with journal access (ADR-021).
func consistentStatus(s PSPStatus) bool {
	switch s.Outcome {
	case "captured":
		return s.Captured && !s.TerminalNoSideEffect
	case "authorized":
		return s.Authorized && !s.Captured && !s.TerminalNoSideEffect
	case "declined", "timeout", "voided":
		return s.TerminalNoSideEffect && !s.Captured && !s.Authorized
	case "refunded":
		return !s.TerminalNoSideEffect && !s.Captured && !s.Authorized
	}
	return true
}

// Void cancels an authorized, uncaptured operation via POST /internal/psp/void.
func (c HTTPClients) Void(ctx context.Context, org uuid.UUID, key string) (CompensationResult, error) {
	return c.compensate(ctx, "void", org, key)
}

// Refund returns captured money via POST /internal/psp/refund. Amounts come from
// payments' stored evidence; the request carries only the operation identity.
func (c HTTPClients) Refund(ctx context.Context, org uuid.UUID, key string) (CompensationResult, error) {
	return c.compensate(ctx, "refund", org, key)
}

func (c HTTPClients) compensate(ctx context.Context, kind string, org uuid.UUID, key string) (CompensationResult, error) {
	var body CompensationResult
	code, err := c.doBody(ctx, http.MethodPost, c.PaymentsURL+"/internal/psp/"+kind,
		map[string]any{"organizer_id": org, "idempotency_key": key}, &body)
	if err != nil {
		return CompensationResult{}, err
	}
	switch code {
	case http.StatusOK:
		// A 200 is proof only when it SAYS the compensation completed. An empty or
		// unexpected body (version skew, a bug upstream) must read as unresolved — the
		// callers release seats and mark orders refunded on this answer, so it fails
		// closed like every other ambiguous provider signal (ai-review B1).
		if want := kind + "ed"; body.Status != want {
			return CompensationResult{}, fmt.Errorf("psp %s: 200 with status %q (want %q): %w",
				kind, body.Status, want, ErrProviderUnresolved)
		}
		return body, nil
	case http.StatusNotFound:
		return CompensationResult{}, ErrOperationNotFound
	case http.StatusConflict:
		return CompensationResult{}, ErrWrongCompensation
	case http.StatusBadGateway:
		return CompensationResult{}, ErrProviderUnresolved
	default:
		return CompensationResult{}, fmt.Errorf("psp %s: unexpected status %d", kind, code)
	}
}

// LookupOperation reads payments' recorded outcome for an idempotency key. Read-only:
// it never binds an operation, so a recovery pass cannot fabricate one for an order
// that never charged.
func (c HTTPClients) LookupOperation(ctx context.Context, org uuid.UUID, key string) (Operation, bool, error) {
	u := fmt.Sprintf("%s/internal/operations?organizer_id=%s&idempotency_key=%s",
		c.PaymentsURL, org, url.QueryEscape(key))
	var body struct {
		Resolved               bool       `json:"resolved"`
		Status                 string     `json:"status"`
		OccurredAt             time.Time  `json:"occurred_at"`
		StatusReplayDeadlineAt *time.Time `json:"status_replay_deadline_at"`
	}
	code, err := c.do(ctx, http.MethodGet, u, &body)
	if err != nil {
		return Operation{}, false, err
	}
	switch code {
	case http.StatusOK:
		return Operation{Resolved: body.Resolved, Status: body.Status,
			OccurredAt: body.OccurredAt, StatusReplayDeadlineAt: body.StatusReplayDeadlineAt}, true, nil
	case http.StatusNotFound:
		// No operation: payments never bound a charge for this key.
		return Operation{}, false, nil
	default:
		return Operation{}, false, fmt.Errorf("lookup operation: unexpected status %d", code)
	}
}

func (c HTTPClients) Confirm(ctx context.Context, org, hold uuid.UUID) error {
	u := fmt.Sprintf("%s/holds/%s/confirm?organizer_id=%s", c.InventoryURL, hold, org)
	code, err := c.do(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusOK:
		return nil
	case http.StatusConflict, http.StatusNotFound:
		// The claim is terminally unconfirmable — released, expired, or gone. Distinct
		// from a transport failure: this one never succeeds on retry, and the caller
		// must treat captured money against it as needing compensation, not another go.
		return ErrClaimGone
	default:
		return fmt.Errorf("confirm claim: status %d", code)
	}
}

// ErrClaimNotReleasable reports that a release was refused because the claim reached a
// terminal state that is not `released` — in practice `confirmed`. The seat is sold and
// the order must not be journalled as failed against it.
var ErrClaimNotReleasable = errors.New("inventory claim cannot be released")

func (c HTTPClients) Release(ctx context.Context, org, hold uuid.UUID) error {
	u := fmt.Sprintf("%s/holds/%s/release?organizer_id=%s", c.InventoryURL, hold, org)
	code, err := c.do(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusOK:
		// Inventory returns 200 when the claim is already `released` (status == target),
		// which is exactly the "committed but response lost" case that makes an ambiguous
		// release ambiguous. Genuinely idempotent for a repeated target.
		return nil
	case http.StatusConflict:
		// NOT "already gone". Inventory answers 200 for an already-released claim AND
		// for an expired one (expiry already freed the seats — TKT-115), so a 409 here
		// means a terminal state that is neither — `confirmed`, i.e. the seat is sold.
		// Treating it as success would journal order.failed for an order inventory
		// counts as confirmed, and permanently strand the seat as sold against a failed
		// order. Surface it: the runner parks it for a human.
		return ErrClaimNotReleasable
	case http.StatusNotFound:
		// No such claim for this organizer. Nothing is holding the seat, so there is no
		// obligation left to discharge and the release is vacuously satisfied.
		return nil
	default:
		return fmt.Errorf("release claim: status %d", code)
	}
}

// StoreCompleter completes an order through the same store transaction the checkout
// path uses. No HTTP: completion is commerce's own state.
type StoreCompleter struct {
	DB *sql.DB
}

// Complete finishes a stuck order whose claim is confirmed. The completion transaction
// owes the event via the outbox (ADR-016 §Decision 6), so the recovery runner does not
// publish anything itself — the drainer already mounted in this service picks the row
// up. That is why StuckOrder carries the projection: recovery reuses the checkout
// path's completion rather than a parallel one that could drift from it.
//
// The guest reference is a fresh candidate, not a stored value: CompleteOrder returns
// the persisted canonical ref and only commits a candidate for an order that had none.
// A re-drive of an already-completed order therefore short-circuits and keeps the
// reference the buyer was originally shown.
func (c StoreCompleter) Complete(ctx context.Context, s store.StuckOrder) error {
	_, err := store.CompleteOrder(ctx, c.DB, s.Completion(), uuid.New())
	return err
}

// JournalFact submits the order.failed fact through payments, mirroring the checkout
// path's fact submission.
type JournalFact struct {
	Client      *http.Client
	PaymentsURL string
	Token       string
	DB          FactDB
}

// FactDB is the commerce-side fact table write the journal submission is derived from.
type FactDB interface {
	RecordOrderFact(ctx context.Context, s store.StuckOrder, factType string) (uuid.UUID, time.Time, error)
}

// StoreFactDB binds FactDB to the commerce store.
type StoreFactDB struct {
	DB *sql.DB
}

func (f StoreFactDB) RecordOrderFact(ctx context.Context, s store.StuckOrder, factType string) (uuid.UUID, time.Time, error) {
	return store.RecordOrderFact(ctx, f.DB, s, factType)
}

func (j JournalFact) OrderFailed(ctx context.Context, s store.StuckOrder) error {
	factID, occurred, err := j.DB.RecordOrderFact(ctx, s, "order.failed")
	if err != nil {
		return fmt.Errorf("record order fact: %w", err)
	}
	payload := map[string]any{
		"fact_id": factID, "organizer_id": s.OrganizerID, "fact_type": "order.failed",
		"buyer_id": s.BuyerID, "amount": s.Amount, "currency": s.Currency,
		"occurred_at": occurred, "payload": map[string]string{"order_id": s.OrderID.String()},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.PaymentsURL+"/internal/facts", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", j.Token)
	resp, err := j.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("journal order.failed: status %d", resp.StatusCode)
	}
	return nil
}
