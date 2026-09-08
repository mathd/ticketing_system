package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	apispec "ticketing/services/commerce/api"
	"ticketing/shared/contract"
)

// Tests for partner confirm (TKT-277).
// Contract-level checks, idempotency derivation, and commission snapshot validation.

func TestPartnerConfirmDeclaredStatusesAndContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}

	pathItem := doc.Paths.Find("/partners/orders")
	if pathItem == nil || pathItem.Post == nil {
		t.Fatal("POST /partners/orders is not declared in OpenAPI contract")
	}
	op := pathItem.Post

	// Security requirement: PartnerCredential
	if op.Security == nil || len(*op.Security) != 1 || len((*op.Security)[0]) != 1 {
		t.Fatalf("expected 1 security requirement, got %v", op.Security)
	}
	if _, ok := (*op.Security)[0][partnerCredentialScheme]; !ok {
		t.Fatalf("expected security scheme %q, got %v", partnerCredentialScheme, (*op.Security)[0])
	}

	// Request body: Checkout schema (additionalProperties: false)
	if op.RequestBody == nil || op.RequestBody.Value == nil {
		t.Fatal("POST /partners/orders declares no request body")
	}
	content := op.RequestBody.Value.Content.Get("application/json")
	if content == nil || content.Schema == nil {
		t.Fatal("missing application/json schema for request body")
	}
	if content.Schema.Ref != "#/components/schemas/Checkout" {
		t.Fatalf("requestBody schema ref = %q, want #/components/schemas/Checkout", content.Schema.Ref)
	}

	checkoutSchema, ok := doc.Components.Schemas["Checkout"]
	if !ok || checkoutSchema.Value == nil {
		t.Fatal("missing Checkout schema in components")
	}
	if checkoutSchema.Value.AdditionalProperties.Has != nil && *checkoutSchema.Value.AdditionalProperties.Has {
		t.Fatal("Checkout schema must have additionalProperties: false")
	}

	// Idempotency-Key parameter required
	hasIdempotency := false
	for _, p := range op.Parameters {
		if p.Value != nil && p.Value.Name == "Idempotency-Key" && p.Value.In == "header" && p.Value.Required {
			hasIdempotency = true
			break
		}
	}
	if !hasIdempotency {
		t.Fatal("POST /partners/orders must require Idempotency-Key header parameter")
	}

	// Declare EVERY status the handler can return: 200, 202, 400, 401, 402, 404, 408, 409, 429, 500, 503
	expectedStatuses := []string{"200", "202", "400", "401", "402", "404", "408", "409", "429", "500", "503"}
	for _, st := range expectedStatuses {
		resp := op.Responses.Status(mustParseInt(st))
		if resp == nil {
			t.Errorf("status %s is not declared on POST /partners/orders", st)
		}
	}

	// The 409 must support seated_pool_unsupported via anyOf over OrderConflict and Error
	resp409 := op.Responses.Status(409)
	if resp409 == nil || resp409.Value == nil {
		t.Fatal("missing 409 response on POST /partners/orders")
	}
	c409 := resp409.Value.Content.Get("application/json")
	if c409 == nil || c409.Schema == nil || c409.Schema.Value == nil {
		t.Fatal("missing 409 application/json schema")
	}
	s409 := c409.Schema.Value
	if len(s409.AnyOf) != 2 {
		t.Fatalf("409 schema should have anyOf with 2 items, got %d", len(s409.AnyOf))
	}
	refs := []string{s409.AnyOf[0].Ref, s409.AnyOf[1].Ref}
	hasOrderConflict := refs[0] == "#/components/schemas/OrderConflict" || refs[1] == "#/components/schemas/OrderConflict"
	hasError := refs[0] == "#/components/schemas/Error" || refs[1] == "#/components/schemas/Error"
	if !hasOrderConflict || !hasError {
		t.Fatalf("409 anyOf refs = %v, want OrderConflict and Error", refs)
	}
}

func mustParseInt(s string) int {
	var n int
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// COS 4: Confirm body naming a different reseller_id/channel_code is refused by
// the contract validator (additionalProperties: false) before the handler runs.
func TestPartnerConfirmRejectsBodyNamingResellerOrChannel(t *testing.T) {
	for _, field := range []string{"reseller_id", "channel_code", "commission", "payee", "amount"} {
		t.Run(field, func(t *testing.T) {
			body := fmt.Sprintf(`{
				"reservation_id": "00000000-0000-0000-0000-000000000001",
				"name": "Partner Buyer",
				"email": "buyer@example.test",
				"payment_token": "fake-ok",
				%q: "not-allowed"
			}`, field)

			r := chi.NewRouter()
			r.Post("/partners/orders", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			h, err := contract.RequestValidatorWithSecurity(apispec.Spec, r, nil, false, nil,
				func(ctx context.Context, input *openapi3filter.AuthenticationInput) error {
					return nil
				})
			if err != nil {
				t.Fatalf("validator: %v", err)
			}

			req := partnerRequest(http.MethodPost, "/partners/orders", body)
			req.Header.Set("X-Partner-Credential", "dummy-credential")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			// The validator refuses extra fields with 400 Bad Request
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("field %q: status = %d, want 400. Extra fields must be unsubmittable: %s",
					field, rec.Code, rec.Body.String())
			}
		})
	}
}

// Idempotency namespace: derive a bounded internal key from organizer + reseller + raw key.
// Two resellers presenting the same raw key must not collide, and one reseller rotating
// its credential must replay its own order.
func TestPartnerIdempotencyKeyNamespace(t *testing.T) {
	org := uuid.New()
	reseller1 := uuid.New()
	reseller2 := uuid.New()
	rawKey := "order-submit-123"

	key1 := partnerIdempotencyKey(org, reseller1, rawKey)
	key2 := partnerIdempotencyKey(org, reseller2, rawKey)

	if key1 == key2 {
		t.Fatal("two different resellers presenting the same raw key must produce different internal keys")
	}

	// Same reseller, new credential (rotated): produces identical key.
	key1Rotated := partnerIdempotencyKey(org, reseller1, rawKey)
	if key1 != key1Rotated {
		t.Fatal("rotating credential must replay with the same internal key")
	}

	// Must be bounded within 200 characters (payments Idempotency-Key limit).
	longRawKey := strings.Repeat("x", 200)
	longInternalKey := partnerIdempotencyKey(org, reseller1, longRawKey)
	if len(longInternalKey) > 200 {
		t.Fatalf("internal key length = %d, want <= 200", len(longInternalKey))
	}
}

func sampleCommissionSnapshot(channel string, resellerID uuid.UUID, shareBps int32, extRef string) []byte {
	snap := map[string]any{
		"face_value":     int64(5000),
		"passed_on_fees": int64(600),
		"total_amount":   int64(5600),
		"breakdown": []map[string]any{
			{
				"fee_code":  ResellerCommissionFeeCode,
				"incidence": "passed_on",
				"amount":    int64(600),
				"currency":  "EUR",
			},
		},
		"resolution": map[string]any{
			"fees": []map[string]any{
				{
					"fee_code": ResellerCommissionFeeCode,
					"split": map[string]any{
						"mode": "split",
						"winner": map[string]any{
							"channel_code": channel,
							"parts": []map[string]any{
								{
									"payee": map[string]any{
										"payee_id":           uuid.NewString(),
										"kind":               "reseller",
										"display_name":       "Reseller Partner",
										"external_reference": extRef,
									},
									"share_bps": shareBps,
								},
							},
						},
					},
				},
			},
		},
	}
	bytes, _ := json.Marshal(snap)
	return bytes
}

// COS 7: An all-null payee is REFUSED by partner confirm.
func TestPartnerCommissionValidationRejectsAllNullPayee(t *testing.T) {
	channel := "reseller-channel"
	reseller := uuid.New()

	// Snapshot where fee has no split (all-null payee).
	unsplitSnap := []byte(`{
		"face_value": 5000,
		"passed_on_fees": 600,
		"total_amount": 5600,
		"breakdown": [{"fee_code":"reseller_commission","incidence":"passed_on","amount":600,"currency":"EUR"}],
		"resolution": {
			"fees": [{"fee_code":"reseller_commission","split":{"mode":"unsplit","winner":null}}]
		}
	}`)

	err := validatePartnerCommission(unsplitSnap, channel, reseller)
	if err == nil {
		t.Fatal("an all-null payee in fee snapshot must be refused by partner confirm")
	}
}

func TestPartnerCommissionValidation(t *testing.T) {
	channel := "reseller-channel"
	reseller := uuid.New()
	validExtRef := fmt.Sprintf("reseller:%s", reseller)

	t.Run("valid commission passes", func(t *testing.T) {
		snap := sampleCommissionSnapshot(channel, reseller, 10000, validExtRef)
		if err := validatePartnerCommission(snap, channel, reseller); err != nil {
			t.Fatalf("valid snapshot failed: %v", err)
		}
	})

	t.Run("missing reseller_commission fee code", func(t *testing.T) {
		snap := []byte(`{"breakdown":[],"resolution":{"fees":[]}}`)
		if err := validatePartnerCommission(snap, channel, reseller); err == nil {
			t.Fatal("missing reseller_commission must fail")
		}
	})

	t.Run("channel mismatch on winning split", func(t *testing.T) {
		snap := sampleCommissionSnapshot("other-channel", reseller, 10000, validExtRef)
		if err := validatePartnerCommission(snap, channel, reseller); err == nil {
			t.Fatal("channel mismatch must fail")
		}
	})

	t.Run("beneficiary mismatch", func(t *testing.T) {
		otherReseller := uuid.New()
		snap := sampleCommissionSnapshot(channel, otherReseller, 10000, fmt.Sprintf("reseller:%s", otherReseller))
		if err := validatePartnerCommission(snap, channel, reseller); err == nil {
			t.Fatal("beneficiary mismatch must fail")
		}
	})

	t.Run("ambiguous beneficiaries", func(t *testing.T) {
		// Snapshot with two parts both matching the same reseller external_reference
		snap := map[string]any{
			"breakdown": []map[string]any{
				{"fee_code": ResellerCommissionFeeCode, "incidence": "passed_on", "amount": int64(600), "currency": "EUR"},
			},
			"resolution": map[string]any{
				"fees": []map[string]any{
					{
						"fee_code": ResellerCommissionFeeCode,
						"split": map[string]any{
							"winner": map[string]any{
								"channel_code": channel,
								"parts": []map[string]any{
									{
										"payee": map[string]any{
											"payee_id":           uuid.NewString(),
											"kind":               "reseller",
											"display_name":       "Part 1",
											"external_reference": validExtRef,
										},
										"share_bps": int32(5000),
									},
									{
										"payee": map[string]any{
											"payee_id":           uuid.NewString(),
											"kind":               "reseller",
											"display_name":       "Part 2",
											"external_reference": validExtRef,
										},
										"share_bps": int32(5000),
									},
								},
							},
						},
					},
				},
			},
		}
		b, _ := json.Marshal(snap)
		if err := validatePartnerCommission(b, channel, reseller); err == nil {
			t.Fatal("ambiguous beneficiaries matching the reservation reseller must fail")
		}
	})
}

// The Go-tier half of "attribution fields are unsubmittable", asserted against the
// HANDLER rather than the contract.
//
// TestPartnerConfirmRejectsBodyNamingResellerOrChannel above proves the VALIDATOR
// refuses these fields, and it proves nothing about this: it registers a stub
// handler that answers 200, so it stays green with confirmWithScope's decode guard
// deleted entirely. Two guards, two tests, and the one that matters here is the one
// the day the contract is edited -- the same argument reserveWithScope makes for
// overwriting the body's channel rather than trusting it.
//
// So this drives confirmWithScope DIRECTLY, with no validator in front of it. A bare
// json.Decoder accepts an unknown field and ignores it; decode() sets
// DisallowUnknownFields and refuses.
func TestPartnerConfirmHandlerItselfRefusesUnknownFields(t *testing.T) {
	for _, field := range []string{"reseller_id", "channel_code", "payee"} {
		t.Run(field, func(t *testing.T) {
			body := fmt.Sprintf(`{
				"reservation_id": "00000000-0000-0000-0000-000000000001",
				"name": "Partner Buyer",
				"email": "buyer@example.test",
				"payment_token": "fake-ok",
				%q: "not-allowed"
			}`, field)

			req := partnerRequest(http.MethodPost, "/partners/orders", body)
			rec := httptest.NewRecorder()

			// A scope whose reservation cannot exist. The decode must refuse BEFORE any
			// database work, so a nil *sql.DB is never reached: if this panics, the guard
			// ran too late.
			(&Server{}).confirmWithScope(rec, req, partnerScope{
				OrganizerID: uuid.New(),
				ResellerID:  uuid.New(),
				ChannelCode: "reseller-test",
			})

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("field %q: status = %d, want 400 from the handler's own decode. "+
					"A bare json.Decoder would ignore the unknown field and fall through to "+
					"the reservation lookup: %s", field, rec.Code, rec.Body.String())
			}
		})
	}
}

// The idempotency key is bounded at the handler, as it is on every other idempotent
// write in this service (checkout, reserve, exchange all use `len(key) > 200`).
func TestPartnerConfirmBoundsTheIdempotencyKey(t *testing.T) {
	req := partnerRequest(http.MethodPost, "/partners/orders", `{
		"reservation_id": "00000000-0000-0000-0000-000000000001",
		"name": "Partner Buyer",
		"email": "buyer@example.test",
		"payment_token": "fake-ok"
	}`)
	req.Header.Set("Idempotency-Key", strings.Repeat("k", 201))
	rec := httptest.NewRecorder()

	(&Server{}).confirmWithScope(rec, req, partnerScope{
		OrganizerID: uuid.New(),
		ResellerID:  uuid.New(),
		ChannelCode: "reseller-test",
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an over-long Idempotency-Key", rec.Code)
	}
}
