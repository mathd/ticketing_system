# A test-side response validator downstream of the production wrap sees laundered statuses, not real ones

**Ticket:** TKT-108, PR #84 (ai-review finding, gpt-5.6-luna:high@claudex arm)

## The trap

The catalog handler tests drive requests through the full production stack (`NewRouter`
mounts `contract.ResponseValidator`), then re-validate the recorded response in a test helper.
Setting `IncludeResponseStatus: true` in that helper looks like it makes undocumented statuses
fail tests, mirroring production (ADR-028).

It only half does. The production validator runs **first**: an undocumented status (say a 418)
is rewritten to the generic 500 (`{"error":"response violates OpenAPI contract"}`) — which,
once 500 is declared in the spec, is a *documented* status. The helper then validates the
laundered response and passes. Only an undocumented **500** stays visible (the mask preserves
the status), which is why the TKT-108 red run worked while the general guarantee didn't hold.

## The rule

A test validator placed downstream of the production fail-closed wrap cannot re-check what the
wrap already rewrote. Don't chase the raw status — **detect the mask itself**: the generic
contract-violation body is written by exactly one place (`shared/go/contract/http.go`), so any
test response containing it means a handler drifted from the spec, whatever the original status
was. The catalog helper now fails hard on that body (`server_test.go` `validateResponse`).

## Transfer

Any service that adopts per-op response validation in handler tests (inventory, commerce,
payments, access run it at smoke level instead) should copy the mask-detection check, not just
`IncludeResponseStatus`. More generally: when production middleware rewrites failures into
well-formed successes-of-another-kind, test-side re-validation must assert the rewrite did NOT
happen, not merely that the final artifact is valid.
