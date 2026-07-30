# `format: uuid` is unenforced, and catalog validates its own rejections

**TKT-110, PR #127.** Two facts that together made nine catalog operations return **500 for a
malformed UUID in production** — each fact individually harmless, and the ticket that filed the bug
named the wrong mechanism for it.

## 1. `format: uuid` rejects nothing

kin-openapi validates string `format` only when format validation is explicitly turned on
(`openapi3.EnableFormatValidation`, or a `DefineStringFormat` registration). **Neither call exists
anywhere in this repo.** So a path or query parameter declared `{type: string, format: uuid}` is, to
the request validator, an unconstrained string: drive `not-a-uuid` through
`contract.RequestValidator` and it reaches the handler with **200**.

Consequence, and the reason this is worth writing down: **the five services do not agree on what
rejects a malformed UUID.**

- **Catalog** is codegen'd. The generated wrapper binds the param into a `uuid.UUID`
  (`runtime.BindStyledParameterWithOptions(..., Format: "uuid")`) and calls `ChiServerOptions`'
  `ErrorHandlerFunc` — a **400** — when that fails. The rejection is real, but it comes from the
  **binder**, not from the request validator.
- **Inventory, commerce, payments, access** hand-mount their routes and parse UUIDs themselves
  (`parseUUID` → `write(w, 400, …)`). Also a 400, also not from the validator.

So "the request validator rejects bad UUIDs" is false everywhere, and any reasoning that starts there
lands on the wrong file. If you ever *do* enable format validation, it changes request handling on
every operation in all five services and can turn currently-succeeding requests into 400s — which is
why TKT-110 explicitly declined to (see TKT-142 for the sequencing).

## 2. Catalog's response validator is the **outermost** wrap, so it checks the binder's 400s

[ADR-028](../adr/ADR-028-response-drift-fail-closed.md)'s Consequences said statuses written by
request-rejection short-circuits *"sit outside the response wrap and are not checked."* That is true
for the four hand-mounted services — `shared/go/contract.requestValidator` builds
`inner = responseValidated(next)` and then `validator(inner)`, so a rejection short-circuits **above**
the response wrap and never reaches it.

**Catalog inverts the order.** `NewRouter` returns
`contract.ResponseValidator(apispec.Spec, handler, …)` where `handler` is the entire
`HandlerWithOptions(...)` result — request validator included. Response validation is therefore
catalog's outermost layer, and **every** status written beneath it is checked: the request validator's,
*and* the binder's.

With `IncludeResponseStatus: true`, an undeclared status is drift. Nine lifecycle operations
(publish/archive for performances, series and festivals; close/reopen for slots; publish for seat maps)
declared `200/404/409/500` and no `'400'`. So the binder's perfectly correct 400 was laundered into
`500 {"error":"response violates OpenAPI contract"}` plus an ERROR log — telling a caller who sent a
bad UUID that the *server* had failed and it should retry.

**The rule for catalog is therefore stricter than ADR-028 originally implied: any status the binder or
the request validator can write must be declared.** ADR-028 now says so, scoped to catalog.

## 3. The ticket described the symptom correctly and the mechanism wrongly

TKT-110 was filed from a review finding as *a latent test trap*: the catalog handler-test validator
"WOULD reject such a 400 **if** a test ever drives malformed input." Both halves of that were wrong —
it was the binder, not the request validator, and it was **live in production**, not latent in tests.
The ticket even offered "codify the carve-out in the test helper" as a candidate fix, which cannot
repair production and would have suppressed the only signal.

The correction cost one throwaway probe at **claim** time: drive the real `newEnv(t).handler` at each
of the nine, print the status. Ten lines, deleted afterwards, and it changed the ticket's severity,
its option set, and its scope (nine operations, not the four the ticket named — and `editSeatMap`,
which it named, already declared `'400'` and served as the control).

**Probe the stated mechanism, not just the stated symptom, before planning.** A finding can be real,
confirmed against HEAD, and still wrong about which layer produces it — and the plan inherits the
wrong layer.

## 4. Declaring a response header makes it enforced, in both directions

kin-openapi **does** validate *declared* response headers (v0.142.0, measured): an enum mismatch and
a missing `required` header each fail closed. An *undeclared* header is invisible to it — which is
exactly why inventory's availability read could emit ADR-004's seconds tier for months with no
declaration and no complaint.

So `required: true` is what carries the guarantee. Without it, a handler that stops emitting the
header still validates clean and the declaration quietly degrades into a comment. With it — plus a
single-value enum — the handler constant and the contract can only move together.

And the cheap test that pins it is **not** the one that asserts a wrong value is rejected: that test
hardcodes its own wrong value and passes whether the production constant is right, wrong, or absent.
Feed the test **the constant itself** and assert it satisfies the declaration. That is the case that
fails when the two drift, and it needs no running stack.

## 5. A fixture that fails for the wrong reason proves nothing

The first draft of the inventory test stubbed an `Availability` body with `"sold"` where the schema has
`"confirmed"`. `additionalProperties: false` rejected it, so **both** cases "worked" for the wrong
reason — the wrong-tier case returned its expected 500 from a *body* violation while the header was
never examined. It would have looked green forever. Make the property under test the only variable,
then re-observe the red.
