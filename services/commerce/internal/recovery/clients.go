package recovery

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Internal-Token", c.Token)
	resp, err := c.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// LookupOperation reads payments' recorded outcome for an idempotency key. Read-only:
// it never binds an operation, so a recovery pass cannot fabricate one for an order
// that never charged.
func (c HTTPClients) LookupOperation(ctx context.Context, org uuid.UUID, key string) (Operation, bool, error) {
	u := fmt.Sprintf("%s/internal/operations?organizer_id=%s&idempotency_key=%s",
		c.PaymentsURL, org, url.QueryEscape(key))
	var body struct {
		Resolved bool   `json:"resolved"`
		Status   string `json:"status"`
	}
	code, err := c.do(ctx, http.MethodGet, u, &body)
	if err != nil {
		return Operation{}, false, err
	}
	switch code {
	case http.StatusOK:
		return Operation{Resolved: body.Resolved, Status: body.Status}, true, nil
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

func (c HTTPClients) Release(ctx context.Context, org, hold uuid.UUID) error {
	u := fmt.Sprintf("%s/holds/%s/release?organizer_id=%s", c.InventoryURL, hold, org)
	code, err := c.do(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	// Idempotent for a repeated target: inventory returns 200 for an already-released
	// claim, which is exactly the "committed but response lost" case that makes an
	// ambiguous release ambiguous. 409/404 mean the claim is already gone — also done.
	switch code {
	case http.StatusOK, http.StatusConflict, http.StatusNotFound:
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
