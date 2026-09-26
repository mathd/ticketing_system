# pgx does not wrap context.DeadlineExceeded for an interrupted query

**2026-09-26 — TKT-211**

When a context deadline interrupts a query that is already running in Postgres, pgx returns a
**server-side cancellation**. That error is **not** `errors.Is(err, context.DeadlineExceeded)`.
Code that classifies "the query timed out" from the error alone gets the in-query case wrong.

## How it was established

TKT-211 maps an availability read whose load outran its budget to 503. The handler test blocks the
read INSIDE Postgres: a second transaction holds `LOCK TABLE inventory_pools IN ACCESS EXCLUSIVE MODE`,
so the query is waiting on the server when the 200 ms budget fires.

A probe mutation removed the cache's own classification (`ErrLoadBudgetExceeded`, set from the load
context's `Err()`) and made the handler match `errors.Is(err, context.DeadlineExceeded)` instead.
Result, from `go test -tags smoke -run TestAvailabilityPastItsBudgetAnswers503`:

    load_budget_smoke_test.go:137: status = 500, want 503; body {"error":"internal error"}

So the error that reached the handler did not wrap `context.DeadlineExceeded`. The test with an
injected fake source (`availability/cache_test.go`, `TestASlowLoadFailsRatherThanWedging`) returns
`ctx.Err()` itself, which is why it never showed this.

## The rule

- To know whether YOUR deadline caused a failure, ask the context: `errors.Is(ctx.Err(),
  context.DeadlineExceeded)` after the call. Do not ask the error.
- Exclude domain answers first: an `ErrNotFound` that arrives after the deadline is still a 404.
- Test the classification against a real query blocked in Postgres, not only against a fake that
  returns `ctx.Err()`. The fake encodes the assumption the test should check.
- A test that observes `pg_locks` to prove "this read waited" needs one fixture per case when the
  code under test keeps a load running after the caller returns. Otherwise a previous case's waiter
  satisfies the next case's observer (TKT-211 review pass 4).
