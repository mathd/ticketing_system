package api

// TKT-297: the refund-leg amount is the ONE caller-supplied money value on the payments
// surface, and its contract must say what it accepts.
//
// This asserts a DECLARATION, not a refusal. The bound that protects the ceiling is the
// checked addition in store.BindRefundLeg — a leg is refused because it would take the
// cumulative refunded total past the captured amount, which is a fact about the charge
// that no request-level maximum can express. What the maximum does is tell consumers and
// generated clients the range payments accepts, which is why the assertion is on the
// served document rather than on a response code.
//
// Deliberately NOT tested through the validator. At maximum = MaxInt64 there is no JSON
// value the contract refuses and encoding/json accepts: an amount above MaxInt64 fails to
// unmarshal into the int64 field and is answered 400 by decode() whether or not this
// maximum exists. A "post 9223372036854775808, expect 400" test therefore passes with the
// maximum deleted — green, well-named, and about nothing. Asserting the document's own
// value is the only assertion at this tier that can distinguish.

import (
	"math"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/payments/api"
)

func TestPartialRefundAmountDeclaresItsBounds(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	schema, ok := doc.Components.Schemas["PSPPartialRefund"]
	if !ok {
		t.Fatal("the spec declares no PSPPartialRefund schema")
	}
	amount, ok := schema.Value.Properties["amount"]
	if !ok {
		t.Fatal("PSPPartialRefund declares no amount property")
	}

	if amount.Value.Min == nil {
		t.Fatal("amount declares no minimum; a refund of zero or less is not a refund")
	}
	if got := *amount.Value.Min; got != 1 {
		t.Errorf("amount minimum = %v, want 1", got)
	}

	if amount.Value.Max == nil {
		t.Fatal("amount declares no maximum, so the contract states no upper bound for the " +
			"one caller-supplied money value payments accepts")
	}
	// Compared in float64 space, and deliberately so. kin-openapi stores a schema bound as
	// float64, and MaxInt64 is NOT exactly representable there — it rounds to 2^63, whose
	// conversion back to int64 overflows to MinInt64. So the obvious int64(*Max) comparison
	// is not merely imprecise, it reports the bound as negative. `float64(math.MaxInt64)`
	// performs the identical rounding, which makes this an exact comparison of the two
	// values after the same conversion, and any other declared maximum — the Money cap
	// included, which IS exactly representable — differs from it.
	if got := *amount.Value.Max; got != float64(math.MaxInt64) {
		t.Errorf("amount maximum = %.0f, want %.0f (int64's own bound).\n"+
			"It is deliberately NOT catalog's Money cap of 9007199254740991: commerce composes "+
			"an order total with a checkedAdd that bounds at int64, and pins that a "+
			"large-but-representable total must still sell, so a capture may exceed 2^53-1. A "+
			"refund is a subset of a capture, so the JS-safe cap would refuse honest refunds of "+
			"large orders.", got, float64(math.MaxInt64))
	}
}
