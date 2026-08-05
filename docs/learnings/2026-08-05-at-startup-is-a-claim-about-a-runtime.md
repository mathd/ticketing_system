# "At startup" is a claim about a runtime, not about where you put the call

**TKT-194, ai-review pass 2.** Filed 2026-08-05.

## What happened

The back office holds two staff credentials with deliberately different blast radii: one authors
catalog content, one moves money. If a deployment sets both to the same value that separation
silently evaporates, and no Go service can notice — catalog compares its own against
`INTERNAL_SERVICE_TOKEN`, commerce compares its own, and neither is ever given the other's. The back
office is the only process holding both.

The first attempt checked it inside the refund call. A reviewer pointed out that a deployment with
collapsed credentials would then start, pass its healthcheck, serve every page, and only object if
someone tried to refund — and an attacker who compromised the process would use the value directly
and never call the helper.

The second attempt moved it to **module scope in `src/middleware.ts`**, with a comment stating it now
ran at startup. That comment was wrong.

Astro's standalone build registers middleware as `manifest.middleware: () => import(...)`, resolved
by `pipeline.getMiddleware()` **while rendering a request**. The adapter binds its port and starts
listening before that module is ever loaded. With both credentials set to the same value the process
started normally; the first request threw. The container did end up unhealthy — `/admin/healthz`
traverses middleware — so Compose's `service_healthy` dependency still caught it, which is why
nothing looked broken.

## Why the comment was confident and wrong

"Module scope runs once, at load" is true. The mistaken step is *which* load, and when the runtime
performs it. In a plain Node program, module scope is startup. Under a framework that code-splits and
lazily imports its own hooks, module scope is "whenever the framework gets around to it" — which for
middleware is, by design, the first request.

None of the unit tests could have caught this. They imported the function and asserted it throws; all
of them pass whether the module-scope call exists or not.

## The fix

A real entrypoint, before the server exists:

```js
// start.mjs — the container CMD
import { assertCredentialSeparation } from './credentials.mjs';
try { assertCredentialSeparation(); }
catch (e) { console.error(`back office refusing to start: ${e.message}`); process.exit(1); }
await import('./dist/server/entry.mjs');
```

with the rule in one plain `.mjs` both the entrypoint and the middleware import — a security rule
written twice will disagree with itself — and the middleware call relabelled **defence in depth**
rather than the mechanism.

The test that proves it runs the real file as a subprocess and requires exit 1. That is the only kind
of test that can: the property is about the process, so it has to observe a process.

## The rule

**If you claim a check happens "at startup", name the runtime event you mean and say how you would
observe it.** For a plain binary that is `main`. For a framework, find out when it loads the thing
you put the call in — the answer is often "lazily, on first use", and a healthcheck that eventually
notices is not the same control as a process that refuses to start.

And: **a property about a process cannot be tested inside that process.** Unit tests of the assertion
function survive deleting every call to it.

## See also

- ADR-042 § *TKT-194 amendment* — the boundary this protects and what remains open.
- `services/catalog/cmd/catalog/main.go` — the Go equivalent, which is genuinely fail-fast because
  `run()` is the process, with a comment noting that moving it after the NATS connect turns a failing
  test into a hanging one.
