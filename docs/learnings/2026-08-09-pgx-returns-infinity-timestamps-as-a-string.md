# pgx returns `infinity` timestamps as a string

**TKT-233**, flagged by the ai-review pass, verified against the driver.

PostgreSQL's `timestamptz` accepts the special values `infinity` and `-infinity`. Go's `time.Time`
has no representation for them, so pgx's `database/sql` layer hands them back as a **string** rather
than converting. Scanning one into a `time.Time` or `*time.Time` fails:

```
sql: Scan error on column index 0, name "timestamptz":
unsupported Scan, storing driver.Value type string into type *time.Time
```

Confirmed by direct probe against `pgx/v5/stdlib`, both for `time.Time` and `*time.Time`.

## Why it matters

`'infinity'::timestamptz` is otherwise an excellent fixture value for **pinning a row live**
unconditionally. `TestExpiredChannelHoldFreesItsCap` uses it exactly that way: it satisfies
`liveClaims` (`expires_at > now()`) and the `claims_kind_shape` CHECK (buyer expiry must be
non-NULL), so a hold pinned to it cannot expire out from under the next statement no matter how long
the process is delayed. That property is the whole point — it is what removes a wall-clock race
rather than merely widening its window.

The catch is that the row is now unreadable through any path that scans its `expires_at` into a Go
`time.Time`. In the inventory store that means `Transition` (which loads the claim before mutating
it) is a trap, while `History` is safe — it selects `claim_history` columns only and never touches
`expires_at` (`operational.go:346-347`). The distinction is not guessable from the method name; it
has to be read.

## The rule

`infinity` is fine to **write** and fine for SQL predicates to **compare against**. It is not fine to
**scan into Go**. If a fixture pins a row with it, keep the pin short-lived — restore a finite
timestamp before any code path reads the row — and say so in a comment where the pin happens, because
the failure surfaces as a confusing driver-level type error a long way from the cause.

The alternative, a finite "far future" timestamp, avoids the scan problem but reintroduces a
magnitude judgement ("how far is far enough?") — which is precisely the class of decision a
determinism fix exists to remove. Prefer `infinity` plus a comment.
