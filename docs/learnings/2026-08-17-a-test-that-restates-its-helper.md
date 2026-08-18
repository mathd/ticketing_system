# A test that restates its helper does not pin the wiring

TKT-248 hit this **three times in one ticket**, in three disguises, each caught by cross-model
review rather than by me. `AGENTS.md` already carries the rule it violates — *"ask which edit your
test catches: breaking the mechanism, or removing it from the place that uses it"* — and knowing
the rule did not stop it. That is why this is written down.

## The three disguises

**1. The test proved the SCHEMA, not the handler.**

A public request naming `channel_code` must be refused. I wrote the test against
`srv.Router(nil, false)`, assuming the bool disabled request validation. It does not — it is
`validateResponses`, and **request** validation is unconditional. So the contract refused the body
and the handler check was never reached. Deleting the handler guard left the test green.

Fixed by calling `srv.reserveWithScope(rec, req, nil)` — which *is* the public path
(`server.go:595`), not a synthetic one — so the handler answers on its own.

**2. The "money" test never observed money.**

It was named for asserting what a buyer is charged. It returned `unit + passedOn/quantity`, where
both values had been assigned by **its own fake catalog**. Commerce never composed anything: the
fakes run with a nil database, so a reserve that got far enough to return a total would have
panicked on the insert, and the 409 that stops it carries no amount. The test would have passed had
commerce added absorbed fees to the buyer's charge — the exact defect the ticket existed to close.

Fixed twice over: renamed to what it actually proves (which query catalog was asked), and the money
assertion moved to the smoke tier, where a real stack composes a real total.

**3. The test restated the helper it was meant to pin.**

The fix for a review finding gated exchange repricing behind `repricingChannel(src)`. The test
called `repricingChannel` directly — so it asserted that function's body back to itself. Reverting
the **call site** to pass `src.ChannelCode` reopened the hole and left the test green. Verified by
executing exactly that revert.

Fixed by fusing the decision and the call into `repriceExchangeTarget` and asserting **the query
catalog actually received**.

## The single shape

All three are the same error: **asserting inside the process instead of at the boundary the value
crosses on its way out.** The boundary is the handler's response, the outbound HTTP query, the row
written — something a caller or a collaborator can observe. A return value, a helper's output, a
schema component in isolation: those are internal, and a test that watches them is watching the
implementation describe itself.

The corollary that makes it actionable: **for every guard, name the edit that would remove it from
the place that uses it — not the edit that breaks it — and check your test catches THAT.** Deleting
a function body is the easy mutation and usually caught. Bypassing the call, repointing an
operation at a different schema, reverting one argument: those are the realistic regressions, and
they are what these three tests all missed.

## Two more from the same ticket

**A fixture built from a safe input cannot show an unsafe one.** The money test's public arm drove a
*channel-free* body — so there was no channel to leak whatever the guard did. It went green with the
guard deleted until the request was changed to the one an attacker would send. If the property is
"X must not happen when the caller supplies Y", the fixture must supply Y.

**Component checks are not wiring checks.** A later version of the seated test asserted that
`ReservationCreate` has no `channel_code` and `PartnerReservationCreate` has no `seat_identities` —
true, and insufficient. Repointing `POST /partners/reservations` at `ReservationCreate` would admit
seated partner sales and leave every assertion green. Assert which schema each **operation**
references, then assert the schema's properties.

## Where the discipline actually fails

Not when writing the planned tests. All three of these were written **while fixing something else** —
two of them while closing review findings, under fix-momentum, where the test is a means to closing
a finding rather than the point of the work. `AGENTS.md` already says a test written mid-fix gets
the same mutation check as one written from the plan. TKT-248 is the evidence for why that sentence
exists.
