# Time-window fixtures must be relative to now — a fixed date is a time bomb

**Seen:** TKT-93's gate run, 2026-07-18. `make check` failed on
`services/access/internal/store` occurrence tests with no code change on main.

**Root cause:** the TKT-85 fixture pinned device time to a calendar literal
(`time.Date(2026, July, 17, 9, 0, 0, 0, UTC)`) while the code under test judges that
time against `now()` with a 24-hour bound (`AdmissionSkewBound`). The suite was green at
merge time and failed exactly 24 hours later. Full CI, a mutation-checked suite, and an
adversarial review pass all missed it, because at review time the fixture was inside the
window — the defect only exists later.

**Rule:** a fixture that feeds any now-relative window (skew bounds, expiry, retention,
"recent" filters) must be constructed relative to `time.Now()`, offset to sit deliberately
inside or outside the window. Pin it **once** (package var) when tests compare it across
calls, and truncate to microseconds if it round-trips through `timestamptz`.

**Review cue:** any `time.Date(...)` literal in a test near code that calls `time.Now()`
deserves the question "what happens when the wall clock passes this date plus the bound?"

**Fix:** commit `44226f2` (carried on PR #71): base pinned once at init as `now-1h`,
microsecond-truncated; deliberately-skewed cases already used `now-48h`.
