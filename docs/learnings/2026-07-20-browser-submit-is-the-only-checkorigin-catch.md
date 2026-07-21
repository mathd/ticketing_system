# API smoke can't see the SSR layer — a browser-submit test is the only checkOrigin catch

**TKT-105, PR #82.** The back-office seat-map editor's form POST was rejected with HTTP 403
*"Cross-site POST form submissions are forbidden"* — and so was **every** existing back-office
write form (TKT-102's draft-authoring forms had been silently broken the same way since they
shipped).

## What happened

Astro's SSR CSRF guard (`security.checkOrigin`, **on by default** for `output: 'server'`)
validates that the request's `Origin` header matches the app's own `Host`. The back-office is
served exclusively behind the gateway reverse proxy (`SetXForwarded`), so the browser sends
`Origin: http://localhost:8080` while the Astro app sees its own container `Host` — they never
match, and the guard 403s **every POST**.

The fix (TKT-105): `security: { checkOrigin: false }` in `web/backoffice/astro.config.mjs` — the
gateway is the trust boundary for this internal, single-organizer staff tool (no auth yet). A
second trap: an **absolute** form `action` URL also trips the check even same-origin, so back-office
forms must post *relative* to the current page.

## Why nothing caught it for two tickets

The gateway smoke suite exercises the **catalog API directly** (`GET /api/catalog/...`,
`POST /api/catalog/...`) and renders `GET /admin/` — but it never **submits an Astro form**. The
entire class of "the SSR framework rejects/mangles the request before the handler runs" is
invisible to API-level smoke, because the SSR layer is exactly what API smoke bypasses.

## The rule

**A back-office / web-UI ticket is not verified until a real browser has submitted its forms.**
API smoke proves the backend contract; it says nothing about whether the SSR framework will accept
the request the browser actually sends (CSRF/origin checks, base-path rewrites, redirects,
CSP). Drive the real stack (`make up`) in a browser and submit the form — the write path, not just
the render. TKT-105 added this ad hoc; there is no Playwright/e2e harness in-repo yet.

See `AGENTS.md` → Quality gates (web-UI browser-submit expectation).
