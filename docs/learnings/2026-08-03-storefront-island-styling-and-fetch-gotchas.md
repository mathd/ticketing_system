# A component can be correct and the page still wrong

**TKT-174 (PR #148) — 2026-08-03**

## What happened

Forty-seven passing component tests, and free seats still rendered identically to selected ones in
a real browser. The cause was CSS specificity: an existing global rule `.hold-picker button`
(0,1,1) beat the island's own `.seat` (0,1,0). No unit test, DOM assertion or accessible-name check
can see this — only a browser with the real global stylesheet.

Two follow-on traps in the same ticket, both in the async read path:

- **`fetch()` resolves on headers, not on the body.** A deadline cleared when the response arrives
  leaves body consumption unbounded — a stalled body hangs for ever behind a timeout that already
  fired.
- **A hand-built `Response` in a test is not wired to the `AbortController`.** An abort test must
  connect the signal to the stream explicitly, or it proves only that a body can stall.
- **`DOMException` is an `Error`**, so `reason instanceof Error` cannot distinguish a timeout from
  an ordinary abort. Use an explicit closure boolean set by the timeout.

Also worth knowing: **the storefront page is minutes-tier cached**, so a CSS fix can look
ineffective while a stale document is being served. Disable the cache when verifying
(`Network.setCacheDisabled` over CDP).

## What to do

- When an island renders inside an existing styled container, **scope its rules under a container
  class from the start** (`.seat-map .seat`, not `.seat`).
- Do the browser verification **before the first commit**, not after — the styles were written
  blind and then corrected, which is the expensive order.
- See also the repo rule in `AGENTS.md`: a web-UI ticket is not verified until a browser has
  *submitted* its forms.
