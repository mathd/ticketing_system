# A stub that ignores its arguments

**2026-09-25 — TKT-169**

A stub that returns a fixed answer whatever it is asked cannot catch a call made with the wrong
arguments. The tests stay green, and the production call silently asks the wrong question.

## What happened

TKT-169 gated a money-path re-check on payments' evidence that no provider movement had started
(the ADR-067 rule: only a payments 404 proves absence). The API tests replaced that lookup with
`stubMoneyEvidence`, which returned the configured `Absent`, `Present` or `Indeterminate` and
ignored its inputs.

The production call passed `existing.PaymentSourceKey` from the looked-up exchange row, and that
row never carries the key (only `BindOrderExchange` fills it). For every downgrade the evidence
lookup asked payments' refund-leg read with an empty source key. Payments answers that with 400
(`services/payments/internal/api/refund_leg.go:36`), the unwind client reads a 400 as
`Indeterminate`, and an `Indeterminate` answer skips the re-check. So the guard could never fire for
a downgrade, while every test passed.

Found in review pass 2 by reading the call, not by any test. Fixed by asking after the bind, and
by making the stub record `delta` and `sourceKey`. `TestPreProviderDowngradeResumeAsksEvidenceWithTheSourceKey`
asserts the key. The mutation that puts the empty key back fails at that assertion:
`evidence source key = "", want the source order's key "resume-downgrade-before-provider-src"`.

## The rule

When a stub stands in for a lookup whose answer depends on its inputs, **record the inputs and
assert them** in at least one test per input class (here, upgrade and downgrade select different
keys). Otherwise the stub proves only that the caller handles each answer, not that the caller
asks the right question. This is the AGENTS.md tier rule applied to fakes: the mechanism that
decides the answer lives in the lookup's inputs, so the assertion must reach them.
