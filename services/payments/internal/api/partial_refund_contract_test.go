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
	"bytes"
	"errors"
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
	// The VALUE is asserted against the spec's own bytes, not against the parsed schema.
	//
	// kin-openapi stores a bound as float64, and MaxInt64 is not representable there: it,
	// MaxInt64-1 and MaxInt64+1 all round to the same 2^63. So a float64 comparison passes
	// for three different declarations, two of them wrong — MaxInt64-1 refuses a legitimate
	// full refund at the bound, and MaxInt64+1 promises a value the int64 decoder rejects
	// with a 400. Converting back to int64 is worse still: 2^63 overflows and the bound
	// reports as MinInt64. The parsed schema simply cannot answer this question, so the
	// question is asked of the source text, where the integer is exact.
	//
	// Found by the TKT-297 ai-review, over two rounds. The first version compared in float64
	// space and carried a comment claiming every other maximum differed from it — green,
	// well-named, and blessing two wrong contracts. The second scanned the whole document
	// with bytes.Contains, which a mutant defeats by declaring the wrong maximum here and
	// putting the right literal in a comment elsewhere.
	//
	// So the text is SCOPED to this property before it is matched: the maximum must be the
	// one declared inside PSPPartialRefund's own `amount` block, not merely present
	// somewhere in the file.
	const wantMax = "9223372036854775807"
	block := partialRefundAmountBlock(t)
	if got := declaredMaximumIn(block); got != wantMax {
		t.Errorf("PSPPartialRefund.amount declares maximum %q, want %q. The block reads:\n%s\n\n"+
			"It is deliberately NOT catalog's Money cap of 9007199254740991: commerce composes "+
			"an order total with a checkedAdd that bounds at int64, and pins that a "+
			"large-but-representable total must still sell, so a capture may exceed 2^53-1. A "+
			"refund is a subset of a capture, so the JS-safe cap would refuse honest refunds of "+
			"large orders.", got, wantMax, block)
	}
	// And the bound the parser hands every consumer is the one the text declares — the two
	// assertions together are what make this about the contract rather than about a string
	// that happens to appear somewhere in the document.
	if got := *amount.Value.Max; got != float64(math.MaxInt64) {
		t.Errorf("parsed amount maximum = %.0f, want the int64 bound", got)
	}
}

// partialRefundAmountBlock returns the source text of PSPPartialRefund's `amount` property
// and nothing else: from its `amount:` key to the next key at the same indentation.
//
// Hand-cut rather than parsed, because the value this exists to protect is precisely the
// one a YAML parser cannot represent — MaxInt64 does not survive float64, which is what
// sent the previous two versions of this assertion wrong. Scanning the raw text is the
// point; scoping it to one property is what the ai-review's mutant defeated, and what this
// restores. The scoping is itself tested below, so it cannot silently widen.
func partialRefundAmountBlock(t *testing.T) []byte {
	t.Helper()
	block, err := amountBlockIn(apispec.Spec)
	if err != nil {
		t.Fatalf("locate PSPPartialRefund.amount in the spec: %v", err)
	}
	return block
}

func amountBlockIn(spec []byte) ([]byte, error) {
	schema := []byte("\n    PSPPartialRefund:\n")
	start := bytes.Index(spec, schema)
	if start < 0 {
		return nil, errors.New("no PSPPartialRefund schema")
	}
	rest := spec[start+len(schema):]
	// The next schema at the same indentation ends this one. Schemas are indented four
	// spaces under `components: schemas:`, so a line starting with exactly four spaces and
	// a non-space is the next sibling.
	if end := nextSiblingAt(rest, 4); end >= 0 {
		rest = rest[:end]
	}
	key := []byte("\n        amount:")
	ai := bytes.Index(rest, key)
	if ai < 0 {
		return nil, errors.New("PSPPartialRefund declares no amount property")
	}
	amount := rest[ai+1:]
	if end := nextSiblingAt(amount[1:], 8); end >= 0 {
		amount = amount[:end+1]
	}
	return amount, nil
}

// declaredMaximumIn returns the value of the block's own `maximum:` key, or "" if it
// declares none.
//
// Comments are stripped first, and the key is matched as a key at the start of a line
// rather than as a substring. Both matter, and the ai-review found them by naming the
// mutant that beat the earlier version: the property declares the wrong bound while the
// right literal sits in a COMMENT inside the very same block, so a substring search over
// the block text is satisfied. Scoping to the property was necessary and not sufficient;
// what the assertion needs is the declared value, not the presence of a string.
func declaredMaximumIn(block []byte) string {
	for _, line := range bytes.Split(block, []byte("\n")) {
		if i := bytes.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = bytes.TrimSpace(line)
		const key = "maximum:"
		if !bytes.HasPrefix(line, []byte(key)) {
			continue
		}
		return string(bytes.TrimSpace(line[len(key):]))
	}
	return ""
}

// nextSiblingAt reports the offset of the next line indented exactly n spaces followed by a
// non-space, or -1. That line opens the next key at this level, so it ends the current one.
func nextSiblingAt(b []byte, n int) int {
	for i := 0; i < len(b); {
		nl := bytes.IndexByte(b[i:], '\n')
		if nl < 0 {
			return -1
		}
		i += nl + 1
		j := i
		for j < len(b) && b[j] == ' ' {
			j++
		}
		if j-i == n && j < len(b) && b[j] != '\n' {
			return i
		}
	}
	return -1
}

// The scoping is load-bearing, so it is tested rather than assumed: a maximum declared
// somewhere else in the document must NOT satisfy the assertion above.
//
// This is the mutant the ai-review named — the wrong bound on the property, the right
// literal in a comment elsewhere. Without this case, a later "simplification" of
// amountBlockIn back to a document-wide search leaves the suite green.
func TestPartialRefundAmountBlockIsScopedToTheProperty(t *testing.T) {
	block, err := amountBlockIn(apispec.Spec)
	if err != nil {
		t.Fatalf("locate the amount block: %v", err)
	}
	for _, unwanted := range []string{"PSPPartialRefundResult", "refund_key:", "currency:", "organizer_id:"} {
		if bytes.Contains(block, []byte(unwanted)) {
			t.Errorf("the amount block leaked into neighbouring text: it contains %q.\nblock:\n%s",
				unwanted, block)
		}
	}

	// The named mutant, applied to a synthetic document: the property declares the WRONG
	// maximum while the right literal appears in a comment further down. A document-wide
	// search passes this; a scoped one must not.
	mutant := []byte(`components:
  schemas:
    PSPPartialRefund:
      type: object
      properties:
        amount:
          type: integer
          minimum: 1
          maximum: 9223372036854775806
        currency: {type: string}
    PSPPartialRefundResult:
      type: object
      properties:
        # the bound is maximum: 9223372036854775807 for the request
        amount: {type: integer, format: int64}
`)
	got, err := amountBlockIn(mutant)
	if err != nil {
		t.Fatalf("locate the amount block in the mutant: %v", err)
	}
	if v := declaredMaximumIn(got); v != "9223372036854775806" {
		t.Errorf("declared maximum = %q, want the property's OWN wrong bound "+
			"9223372036854775806 — a maximum declared elsewhere in the document was picked "+
			"up instead.\nblock:\n%s", v, got)
	}

	// The decoy the ai-review named after the scoping landed: the property declares the
	// wrong bound while the RIGHT literal sits in a comment inside the very same block. The
	// block scoping cannot help here, so the assertion must read the declared value rather
	// than search the text.
	inBlockDecoy := []byte(`components:
  schemas:
    PSPPartialRefund:
      type: object
      properties:
        amount:
          type: integer
          # the intended bound is maximum: 9223372036854775807
          maximum: 9223372036854775806
        currency: {type: string}
`)
	got, err = amountBlockIn(inBlockDecoy)
	if err != nil {
		t.Fatalf("locate the amount block in the in-block decoy: %v", err)
	}
	if v := declaredMaximumIn(got); v != "9223372036854775806" {
		t.Errorf("declared maximum = %q, want 9223372036854775806 — a literal in a COMMENT "+
			"inside the block was read as the declaration, so the wrong-bound mutant "+
			"survives.\nblock:\n%s", v, got)
	}

	// And the honest case still reads correctly, so the comment-stripping cannot pass by
	// simply finding nothing.
	if v := declaredMaximumIn(partialRefundAmountBlock(t)); v != "9223372036854775807" {
		t.Errorf("the real spec's declared maximum read as %q", v)
	}
}
