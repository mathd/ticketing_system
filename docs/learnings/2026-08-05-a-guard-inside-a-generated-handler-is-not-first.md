# A guard inside a generated handler is not the first thing that runs

**TKT-214.** Catalog's new `/internal/` fee-resolution read checked its service credential as the
first statement of its handler, and the handler comment said so. The claim was false, and an
adversarial review found it.

## What actually happens

`oapi-codegen`'s generated wrapper binds and validates path and query parameters **before** it
applies `HandlerMiddlewares` (`services/catalog/internal/api/openapi_gen.go` — the
`BindStyledParameterWithOptions` / `ErrorHandlerFunc` block precedes the middleware loop). So:

| Request | Answer, with the check inside the handler |
|---|---|
| valid uuid, no credential | 401 |
| **malformed uuid, no credential** | **400, with validation detail** |
| **over-long query parameter, no credential** | **400, with validation detail** |

An unauthenticated caller therefore gets a **schema oracle**: it learns the route exists, that its
parameters are validated, and which one it got wrong. Five of six malformed-request shapes answered
something other than the uniform refusal the handler advertised.

## The fix that does not work

Moving the check into `ChiServerOptions.Middlewares` **also fails**, and for the same reason: those
middlewares are applied by the wrapper, after binding. Ordering them differently changes nothing.

## The fix that works

Guard **outside** the generated handler — wrap the finished handler, so the check runs before chi
routes and before anything binds:

```go
return contract.ResponseValidator(apispec.Spec, guardInternalSurface(s, handler), s.log, validateResponses)
```

A **prefix** guard over `/internal/`, not a per-route one, so a newly declared internal operation is
closed by construction — the same argument TKT-191 made for declaring the staff-write requirement
once at the document level instead of in 26 handlers.

## Two things to check when you do this

- **The guard and the router must read the same path**, or a spelling that reaches a handler is a
  spelling the guard missed. Here both read `r.URL.Path`, so they cannot diverge — and a test pins
  it (`TestTheInternalGuardCannotBeSpelledAround`: leading double slash, dot segments, case, internal
  double slash — each must answer 401 or 404, never 200). That test is what fails if a
  path-normalising middleware is ever added upstream.
- **Name the side effect.** An unknown `/internal/` path now answers 401 instead of chi's 404. That
  is the direction ADR-043 argues for — *"enumerating the internal surface is the caller's problem to
  solve, not ours to help with"* — but it is a behaviour change to routes outside the ticket that
  introduced it, so it belongs in the commit message and the review guide, not in someone's later
  bug report.

## The general shape

**"First" is a claim about a runtime, not about source order.** A check written at the top of a
function is first only within that function; whatever wraps the function ran earlier. Whenever a
comment says a guard runs first, ask what the framework does before it gets there — and if the
answer matters for security, write the test that would fail if it changed.
