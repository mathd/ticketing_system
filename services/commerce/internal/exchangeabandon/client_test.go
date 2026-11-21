package exchangeabandon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
)

// TKT-498. The HTTP client's requests and its strict decoding, with literal wire fixtures. The
// service judges what a decoded answer means; these tests pin what counts as an answer at all.

type seenRequest struct {
	method, path string
	query        map[string]string
	token        string
	body         map[string]any
}

// wire serves one literal answer and records the request.
func wire(t *testing.T, status int, body string) (HTTP, *seenRequest) {
	t.Helper()
	seen := &seenRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen.method, seen.path, seen.token = r.Method, r.URL.Path, r.Header.Get("X-Internal-Token")
		seen.query = map[string]string{}
		for k := range r.URL.Query() {
			seen.query[k] = r.URL.Query().Get(k)
		}
		_ = json.Unmarshal(raw, &seen.body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewHTTP(srv.URL+"/access/", srv.URL+"/payments", srv.URL+"/inventory", "internal-token", "payments-token", 5*time.Second), seen
}

func TestAdmissionUsesTheSharedReaderWithTheInternalToken(t *testing.T) {
	org, order := uuid.New(), uuid.New()
	h, seen := wire(t, 200, `{"admitted":true,"issued_count":2}`)
	got, err := h.Admission(context.Background(), org, order)
	if err != nil || !got.Admitted || got.IssuedCount != 2 {
		t.Fatalf("admission = %+v, %v", got, err)
	}
	if seen.method != http.MethodGet || seen.path != "/access/internal/orders/"+order.String()+"/admission" ||
		seen.query["organizer_id"] != org.String() || seen.token != "internal-token" {
		t.Fatalf("request = %+v", seen)
	}
}

func TestChargeOperationReadsTheScopedKey(t *testing.T) {
	org := uuid.New()
	const full = `{"resolved":true,"occurred_at":"2026-11-21T10:00:00Z","status":"succeeded","fact_id":"` +
		`7f1c1f3e-8f53-4b0c-9a51-0f8f0b2f5e11","captured_amount":1000,"currency":"EUR",` +
		`"confirmed_captured_amount":1000,"confirmed_currency":"EUR","status_replay_deadline_at":"2026-12-21T10:00:00Z"}`
	h, seen := wire(t, 200, full)
	got, err := h.ChargeOperation(context.Background(), org, "exchange-charge:x")
	if err != nil || !got.Found || !got.Resolved || *got.CapturedAmount != 1000 || *got.Currency != "EUR" ||
		*got.ConfirmedAmount != 1000 || *got.ConfirmedCurrency != "EUR" {
		t.Fatalf("evidence = %+v, %v", got, err)
	}
	if seen.method != http.MethodGet || seen.path != "/payments/internal/operations" || seen.token != "payments-token" ||
		seen.query["organizer_id"] != org.String() || seen.query["idempotency_key"] != "exchange-charge:x" {
		t.Fatalf("request = %+v", seen)
	}

	legacy, _ := wire(t, 200, `{"resolved":true,"occurred_at":"2026-11-21T10:00:00Z","status":"succeeded","fact_id":"7f1c1f3e-8f53-4b0c-9a51-0f8f0b2f5e11","captured_amount":1000,"currency":"EUR"}`)
	if got, err := legacy.ChargeOperation(context.Background(), org, "k"); err != nil || got.ConfirmedAmount != nil || got.ConfirmedCurrency != nil {
		t.Fatalf("legacy evidence = %+v, %v; want absent confirmation", got, err)
	}
	absent, _ := wire(t, 404, `{"error":"operation not found"}`)
	if got, err := absent.ChargeOperation(context.Background(), org, "k"); err != nil || got.Found {
		t.Fatalf("404 = %+v, %v; want not found", got, err)
	}
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"500", `{}`, 500},
		{"409", `{}`, 409},
		{"no resolved", `{"occurred_at":"x"}`, 200},
		{"unknown field", `{"resolved":true,"extra":1}`, 200},
		{"wrong type", `{"resolved":true,"captured_amount":"1000"}`, 200},
		{"trailing", `{"resolved":true}{}`, 200},
		{"route-level 404", "404 page not found\n", 404},
		{"404 with another message", `{"error":"not found"}`, 404},
		{"404 with an extra field", `{"error":"operation not found","x":1}`, 404},
		{"404 with no body", ``, 404},
	} {
		h, _ := wire(t, tc.status, tc.body)
		if _, err := h.ChargeOperation(context.Background(), org, "k"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: err = %v, want ErrUnavailable", tc.name, err)
		}
	}
}

func TestRefundLegReadsTheScopedLeg(t *testing.T) {
	org := uuid.New()
	h, seen := wire(t, 200, `{"completed":true,"amount":700,"currency":"EUR"}`)
	got, err := h.RefundLeg(context.Background(), org, "src-key", "exchange-refund:x")
	if err != nil || !got.Found || !got.Completed || got.Amount != 700 || got.Currency != "EUR" || got.ConfirmedAmount != nil {
		t.Fatalf("leg = %+v, %v", got, err)
	}
	if seen.method != http.MethodGet || seen.path != "/payments/internal/refund-legs" || seen.token != "payments-token" ||
		seen.query["organizer_id"] != org.String() || seen.query["source_idempotency_key"] != "src-key" ||
		seen.query["refund_idempotency_key"] != "exchange-refund:x" {
		t.Fatalf("request = %+v", seen)
	}
	absent, _ := wire(t, 404, `{"error":"refund leg not found"}`)
	if got, err := absent.RefundLeg(context.Background(), org, "a", "b"); err != nil || got.Found {
		t.Fatalf("404 = %+v, %v", got, err)
	}
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"503", `{}`, 503},
		{"missing completed", `{"amount":700,"currency":"EUR"}`, 200},
		{"missing amount", `{"completed":true,"currency":"EUR"}`, 200},
		{"null currency", `{"completed":true,"amount":700,"currency":null}`, 200},
		{"unknown field", `{"completed":true,"amount":700,"currency":"EUR","abandoned":true}`, 200},
		{"not JSON", `leg`, 200},
		{"route-level 404", "404 page not found\n", 404},
		{"404 with another message", `{"error":"operation not found"}`, 404},
		{"404 with an extra field", `{"error":"refund leg not found","x":1}`, 404},
		{"404 with no body", ``, 404},
	} {
		h, _ := wire(t, tc.status, tc.body)
		if _, err := h.RefundLeg(context.Background(), org, "a", "b"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: err = %v, want ErrUnavailable", tc.name, err)
		}
	}
}

func TestPartialRefundClassifiesEveryAnswer(t *testing.T) {
	org := uuid.New()
	fact := uuid.New()
	req := RefundRequest{OrganizerID: org, ChargeKey: "exchange-charge:x", RefundKey: "rk", Amount: 1000, Currency: "EUR"}
	h, seen := wire(t, 200, `{"status":"refunded","fact_id":"`+fact.String()+`","amount":1000,"currency":"EUR","replay":false}`)
	got, err := h.PartialRefund(context.Background(), req)
	if err != nil || got.FactID != fact || got.Amount != 1000 || got.Currency != "EUR" {
		t.Fatalf("answer = %+v, %v", got, err)
	}
	if seen.method != http.MethodPost || seen.path != "/payments/internal/psp/partial-refund" || seen.token != "payments-token" ||
		seen.body["organizer_id"] != org.String() || seen.body["idempotency_key"] != "exchange-charge:x" ||
		seen.body["refund_key"] != "rk" || seen.body["amount"] != float64(1000) || seen.body["currency"] != "EUR" {
		t.Fatalf("request = %+v", seen)
	}

	failed, _ := wire(t, 422, `{"error":"provider refund failed","code":"provider_refund_failed","provider_ref":"re_123"}`)
	_, err = failed.PartialRefund(context.Background(), req)
	var typed *ProviderRefundFailedError
	if !errors.As(err, &typed) || typed.ProviderRef != "re_123" || !errors.Is(err, ErrProviderRefundFailed) {
		t.Fatalf("typed 422: err = %v", err)
	}

	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"422 without code", 422, `{"error":"x","provider_ref":"re_1"}`, ErrUnavailable},
		{"422 wrong code", 422, `{"error":"x","code":"other","provider_ref":"re_1"}`, ErrUnavailable},
		{"422 bare re_", 422, `{"error":"x","code":"provider_refund_failed","provider_ref":"re_"}`, ErrUnavailable},
		{"422 not re_", 422, `{"error":"x","code":"provider_refund_failed","provider_ref":"ch_1"}`, ErrUnavailable},
		{"409 exhausted ceiling", 409, `{"error":"refund legs would exceed the captured amount"}`, ErrRefused},
		{"404 no operation", 404, `{"error":"operation not found"}`, ErrRefused},
		{"502 unresolved", 502, `{"error":"provider refund unresolved"}`, ErrUnavailable},
		{"200 wrong status", 200, `{"status":"pending","fact_id":"` + fact.String() + `","amount":1000,"currency":"EUR","replay":false}`, ErrUnavailable},
		{"200 nil fact", 200, `{"status":"refunded","fact_id":"00000000-0000-0000-0000-000000000000","amount":1000,"currency":"EUR","replay":false}`, ErrUnavailable},
		{"200 missing fact", 200, `{"status":"refunded","amount":1000,"currency":"EUR","replay":false}`, ErrUnavailable},
		{"200 missing amount", 200, `{"status":"refunded","fact_id":"` + fact.String() + `","currency":"EUR","replay":false}`, ErrUnavailable},
		{"200 unknown field", 200, `{"status":"refunded","fact_id":"` + fact.String() + `","amount":1000,"currency":"EUR","replay":false,"x":1}`, ErrUnavailable},
		{"200 trailing", 200, `{"status":"refunded","fact_id":"` + fact.String() + `","amount":1000,"currency":"EUR","replay":false} x`, ErrUnavailable},
	} {
		h, _ := wire(t, tc.status, tc.body)
		if _, err := h.PartialRefund(context.Background(), req); !errors.Is(err, tc.want) || errors.Is(err, ErrProviderRefundFailed) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestAppendFactSendsTheCanonicalEnvelopeAndRequiresAnAcknowledgment(t *testing.T) {
	f := store.ExchangeAbandonedFact{
		ID: uuid.New(), OrganizerID: uuid.New(), OrderID: uuid.New(), BuyerID: uuid.New(), ExchangeID: uuid.New(),
		Amount: 3200, Currency: "EUR", OccurredAt: time.Date(2026, 11, 21, 10, 0, 0, 123000, time.UTC),
		SourceGross: 2200, DeltaAmount: 1000, RefundFactID: uuid.New(),
	}
	h, seen := wire(t, 200, `{"sequence":42,"hash":"ab12","replay":false}`)
	if err := h.AppendFact(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	payload, _ := seen.body["payload"].(map[string]any)
	if seen.method != http.MethodPost || seen.path != "/payments/internal/facts" || seen.token != "payments-token" ||
		seen.body["fact_id"] != f.ID.String() || seen.body["organizer_id"] != f.OrganizerID.String() ||
		seen.body["fact_type"] != "order.exchange.abandoned" || seen.body["buyer_id"] != f.BuyerID.String() ||
		seen.body["amount"] != float64(3200) || seen.body["currency"] != "EUR" ||
		seen.body["occurred_at"] != "2026-11-21T10:00:00.000123Z" ||
		payload["order_id"] != f.OrderID.String() || payload["exchange_id"] != f.ExchangeID.String() ||
		payload["source_gross"] != "2200" || payload["delta_amount"] != "1000" ||
		payload["refund_fact_id"] != f.RefundFactID.String() || len(payload) != 5 {
		t.Fatalf("request = %+v", seen)
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"400 invalid payload", 400, `{"error":"invalid fact"}`, ErrRefused},
		{"409", 409, `{"error":"fact id reused with different content"}`, ErrRefused},
		{"503", 503, `{}`, ErrUnavailable},
		{"200 zero sequence", 200, `{"sequence":0,"hash":"ab","replay":false}`, ErrUnavailable},
		{"200 empty hash", 200, `{"sequence":1,"hash":"","replay":false}`, ErrUnavailable},
		{"200 missing replay", 200, `{"sequence":1,"hash":"ab"}`, ErrUnavailable},
		{"200 not JSON", 200, `ok`, ErrUnavailable},
	} {
		h, _ := wire(t, tc.status, tc.body)
		if err := h.AppendFact(context.Background(), f); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestReturnCapacityPostsTheReceiptAndDecodesStrictly(t *testing.T) {
	org, hold, receipt := uuid.New(), uuid.New(), uuid.New()
	h, seen := wire(t, 200, `{"hold_id":"`+hold.String()+`","quantity":2,"unreturned_quantity":0,"replay":false}`)
	got, err := h.ReturnCapacity(context.Background(), org, hold, receipt, 2)
	if err != nil || got.HoldID != hold || got.Quantity != 2 || got.Unreturned != 0 {
		t.Fatalf("answer = %+v, %v", got, err)
	}
	if seen.method != http.MethodPost || seen.path != "/inventory/internal/holds/"+hold.String()+"/refund-capacity" ||
		seen.token != "internal-token" || seen.body["organizer_id"] != org.String() ||
		seen.body["refund_id"] != receipt.String() || seen.body["quantity"] != float64(2) {
		t.Fatalf("request = %+v", seen)
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"409 not confirmed", 409, `{"error":"only a confirmed buyer claim can return capacity"}`, ErrRefused},
		{"404", 404, `{"error":"not found"}`, ErrRefused},
		{"500", 500, `{}`, ErrUnavailable},
		{"missing remainder", 200, `{"hold_id":"` + hold.String() + `","quantity":2,"replay":false}`, ErrUnavailable},
		{"unknown field", 200, `{"hold_id":"` + hold.String() + `","quantity":2,"unreturned_quantity":0,"replay":false,"x":1}`, ErrUnavailable},
	} {
		h, _ := wire(t, tc.status, tc.body)
		if _, err := h.ReturnCapacity(context.Background(), org, hold, receipt, 2); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// R1-4. Each dependency call names its own credential. The configured URLs share a prefix with
// the payments URL, so a credential chosen from the URL would send the payments token to Access
// and inventory.
func TestEachDependencyCallSendsItsOwnCredential(t *testing.T) {
	tokens := map[string]string{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens[r.URL.Path] = r.Header.Get("X-Internal-Token")
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	base := srv.URL + "/svc"
	h := NewHTTP(base+"-access", base, base+"-inventory", "internal-token", "payments-token", 5*time.Second)
	ctx, org, order, hold := context.Background(), uuid.New(), uuid.New(), uuid.New()
	_, _ = h.Admission(ctx, org, order)
	_, _ = h.ReturnCapacity(ctx, org, hold, uuid.New(), 1)
	_, _ = h.ChargeOperation(ctx, org, "k")
	_, _ = h.RefundLeg(ctx, org, "a", "b")
	_, _ = h.PartialRefund(ctx, RefundRequest{OrganizerID: org, ChargeKey: "k", RefundKey: "r", Amount: 1, Currency: "EUR"})
	_ = h.AppendFact(ctx, store.ExchangeAbandonedFact{ID: uuid.New(), OrganizerID: org})
	want := map[string]string{
		"/svc-access/internal/orders/" + order.String() + "/admission":        "internal-token",
		"/svc-inventory/internal/holds/" + hold.String() + "/refund-capacity": "internal-token",
		"/svc/internal/operations":         "payments-token",
		"/svc/internal/refund-legs":        "payments-token",
		"/svc/internal/psp/partial-refund": "payments-token",
		"/svc/internal/facts":              "payments-token",
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != len(want) {
		t.Fatalf("requests = %v, want %v", tokens, want)
	}
	for path, token := range want {
		if tokens[path] != token {
			t.Errorf("%s sent %q, want %q", path, tokens[path], token)
		}
	}
}

// RA1-5. A redirect is not an answer. The client does not follow it, so the redirect target
// receives no request and no credential, and every call stays unresolved.
func TestARedirectIsUnresolvedAndIsNotFollowed(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(target.Close)
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+r.URL.Path, code)
		}))
		h := NewHTTP(origin.URL, origin.URL+"/payments", origin.URL+"/inventory", "internal-token", "payments-token", 5*time.Second)
		ctx, org := context.Background(), uuid.New()
		calls := map[string]error{}
		_, calls["admission"] = h.Admission(ctx, org, uuid.New())
		_, calls["operation"] = h.ChargeOperation(ctx, org, "k")
		_, calls["refund leg"] = h.RefundLeg(ctx, org, "a", "b")
		_, calls["partial refund"] = h.PartialRefund(ctx, RefundRequest{OrganizerID: org, ChargeKey: "k", RefundKey: "r", Amount: 1, Currency: "EUR"})
		calls["fact"] = h.AppendFact(ctx, store.ExchangeAbandonedFact{ID: uuid.New(), OrganizerID: org})
		_, calls["capacity"] = h.ReturnCapacity(ctx, org, uuid.New(), uuid.New(), 1)
		origin.Close()
		for name, err := range calls {
			if err == nil || errors.Is(err, ErrRefused) {
				t.Errorf("%d %s: err = %v, want an unresolved answer", code, name, err)
			}
		}
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("the redirect target received %d request(s), want 0", n)
	}
}
