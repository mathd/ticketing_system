# Batch endpoints with per-item results must not carry per-item format constraints in the request schema

**Ticket:** TKT-85 (ai-review pass 1 R3 + pass 2 R4). **Date:** 2026-07-17.

## The trap

A batch endpoint that promises *per-item* results (`recorded`/`rejected`/… per
occurrence) while its OpenAPI item schema carries `format: uuid`,
`format: date-time`, or `minLength` constraints does not deliver that promise:
the contract validator rejects the **whole request** (422) on the first
malformed item. One bad row in a gate's overnight queue blocked the sync of
every good row — the exact failure the per-item results existed to prevent.

Corollary on the response side: echo the client's correlation key **verbatim**
(plain string, not `format: uuid`). A normalized or zeroed key on a rejected
item can never be matched to the sender's queue entry, which then retries
forever; and the runtime response validator turns a schema-violating verbatim
echo into a 500.

## The rule

If the endpoint's error unit is the item, the schema's validation unit must be
the item too: envelope-level constraints only (`maxItems`, required fields,
`additionalProperties: false`); item fields as plain strings, validated
server-side into per-item rejections. State the choice in a schema comment so
a reviewer doesn't "fix" the missing formats back in.

## Where it applies here

`services/access/api/openapi.yaml` `ReconcileOccurrence`/`ReconcileResult`
(the schema comments carry the rule). Any future batch-sync surface (TKT-19
hardware gates, POS offline orders in TKT-16) inherits the same shape.
