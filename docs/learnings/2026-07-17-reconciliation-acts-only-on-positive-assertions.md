# Reconciliation acts only on positive assertions — absence is never death

**Context (TKT-90, PR #67).** Inventory's startup reconciliation pass converges pool offering
state against catalog for pools whose archive/closure events fell outside stream retention.

**The trap.** The obvious read — the existing published-only per-id lookup
(`GET /internal/performances/{id}`) — makes two very different things indistinguishable:

- an **archived solo slot** (404: filtered out by the published-only query), and
- a **live festival group pool** (404: its pool id is a *capacity-group* id, not a performance id).

`inventory_pools` carries no kind marker (`slot_id` is either, by construction of the schema-3
publication path), so the reconciler has nothing to dispatch on. A reconciliation that treats
404 as "the slot is dead" archives every live festival pool it visits — terminal, on live
sellable inventory. The plan draft shipped this; it survived until an adversarial plan review
verified what the 404 actually covers.

**The rule.** A reconciliation write requires a *positive* assertion about the id from the
authoritative source — "this is a performance and it is archived" — never the absence of an
answer. TKT-90 added `GET /internal/pools/{id}/offer-state`, which answers per-id for a
performance in **any** lifecycle (archived is 200, not 404) or a festival (`kind":"festival"`,
which the reconciler skips); a real 404 means "touch nothing and say so loudly".

**Generalization.** Any converge-against-authority pass over ids whose namespace is a union of
entity kinds must either (a) read an endpoint that answers for the whole union, or (b) prove the
id space is single-kind. A lookup scoped to one kind + one lifecycle turns every out-of-scope id
into a false death certificate.
