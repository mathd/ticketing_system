# A decoded page is not an answered one

**TKT-116, PR #129.** A fail-closed evidence check asked a provider "does a refund already exist for
this payment?" and treated *any response that unmarshalled without error* as an answer. Go's zero
values made the malformed answer and the safe answer identical.

## What happened

`Stripe.Refund` was changed to resolve before submitting: list `GET /v1/refunds?payment_intent=…`,
adopt our own refund if it is there, and only submit when the listing proves none exists. The rule
written into the design was explicit — *"only a conclusive listing licenses a submit"* — because
submitting on an inconclusive answer is how you refund twice.

The struct was the ordinary thing:

```go
type stripeRefundList struct {
	Data    []stripeRefund `json:"data"`
	HasMore bool           `json:"has_more"`
}
```

A truncated page, a proxy error body, an unrelated JSON object — any of these unmarshal cleanly and
produce `Data == nil, HasMore == false`. That is **bit-for-bit the same value** as a genuine
`{"data": [], "has_more": false}`, which is the one answer that licenses a POST. The fail-closed
rule was stated in a comment, tested in three places, and silently unenforceable in the decoder
underneath it.

The tests did not catch it because every fixture was a *well-formed* list. Absence of a field was
never in the input space — the fixture set proved the parser handled real Stripe payloads, which was
never the risk.

## The rule

**When a decoded value is used as evidence, make absence representable.** Pointer (or
`json.RawMessage`, or a presence flag) for every field whose zero value is meaningful, plus a
positive marker check:

```go
type stripeRefundList struct {
	Object  string          `json:"object"`
	Data    *[]stripeRefund `json:"data"`
	HasMore *bool           `json:"has_more"`
}
func (l stripeRefundList) complete() bool {
	return l.Object == "list" && l.Data != nil && l.HasMore != nil
}
```

The test that finds this is the one whose fixture **omits a field** — not one that supplies a
strange value for it. `TestStripeRefundTreatsIncompleteListPagesAsInconclusive` has four subtests
(`missing has_more`, `missing data`, `not a list`, `more pages but empty`) and each asserts that
**no POST was issued**, not merely that an error came back.

This is the sibling of the fixture rule already in `quality-practices.md` §1: a fixture built from
the type under test cannot express incompatibility. Here the fixture was hand-written and still
could not express the failure, because every fixture was *complete*. Hand-writing the JSON is
necessary and not sufficient — the input space has to include the malformed shapes too.

## The second half: "fails safe" needs a direction

The same review found the mirror-image bug, and this one was a *deliberate* choice defended with an
argument that was half right.

The matcher allowed an absent `payment_intent` or `currency` to corroborate a match:

```go
if rf.PaymentIntent != "" && rf.PaymentIntent != providerRef { return false }
return rf.Currency == "" || rf.Currency == lc(currency)
```

The reasoning recorded at the time: *leniency makes a match more likely, a match means we do not
submit, therefore leniency fails toward not-double-refunding — the safe direction.* Every step of
that is true. It is still wrong, because the risk is **two-sided**: adopting a refund does not just
skip a POST, it appends `payment.refunded` to an append-only journal. Leniency fails safe against
double-refunding and fails *unsafe* against fabricating a money fact.

The fix was to stop forcing a two-valued answer onto a three-valued question:

- **No** — no stamp of ours. Somebody else's refund. Says nothing about ours.
- **Yes** — stamped and corroborated.
- **Inconclusive** — stamped, but the evidence disagrees or is missing. Calling it No would license
  a second refund on top of one that probably is ours; calling it Yes would journal money movement
  on evidence that does not confirm it. Neither is defensible, so it fails closed and a human looks.

**On a two-sided risk, "it fails safe" is not an argument until you say *which* failure it is safe
against.** The binary return type was the tell: a predicate that must answer a question with three
honest answers will quietly assign the third one to whichever branch the author was thinking about.
