// The checkout bridge (TKT-221 / US-A2). See ADR-049 § TKT-221.
//
// Why this exists at all: the checkout is a `fetch` from a React island
// (`HoldPicker`, `client:load`), so it runs in the BROWSER — and the session
// cookie is `httpOnly`, so the island cannot read it, and commerce cannot resolve
// it either (the session map lives in this process). Something server-side has to
// stand between them, and this is it.
//
// It is a bridge, not a handler. It adds exactly one header and forwards
// everything else untouched, because "guest checkout is unchanged" has to mean the
// bytes, not just the intent.
//
// Deliberately NOT under `/api/`: in this system `/api/<svc>/` means "the gateway
// proxies this to a service" (gateway/cmd/gateway/main.go), and a storefront route
// inside that namespace reads as a service call to anyone debugging the route
// table while depending on the `/` catch-all rather than a registration.
import type { APIRoute } from 'astro';

import { SESSION_COOKIE, lookupSession } from '../lib/session';

const GATEWAY_URL = process.env.GATEWAY_URL ?? 'http://localhost:8080';

/**
 * Headers forwarded to commerce, and nothing else.
 *
 * An allowlist rather than "copy everything minus cookie": a denylist over
 * browser-controlled headers is a thing that goes wrong quietly the next time a
 * header matters. The cookie in particular must never leave — commerce has no use
 * for it and it is a live credential.
 */
const FORWARDED = ['content-type', 'idempotency-key'];

export const POST: APIRoute = async ({ request, cookies }) => {
  const headers = new Headers();
  for (const name of FORWARDED) {
    const value = request.headers.get(name);
    if (value) headers.set(name, value);
  }

  // Signed in? Attach the proof the buyer earned by authenticating. Signed out —
  // or an expired session — attaches nothing, and that is a GUEST checkout, which
  // is the default this system was built around and not a failure.
  //
  // An expired session is therefore not refused here: refusing would break a guest
  // checkout for the sin of having once been signed in, and the buyer would have
  // no idea why. They get the guest path, which still works and still delivers
  // their tickets by order reference.
  const principal = lookupSession(cookies.get(SESSION_COOKIE)?.value ?? '');
  if (principal) headers.set('X-Customer-Assertion', principal.assertion);

  const upstream = await fetch(`${GATEWAY_URL}/api/commerce/orders`, {
    method: 'POST',
    headers,
    body: await request.text(),
  });

  // Commerce's status and body, verbatim. The island already understands every
  // status the checkout can answer with (200, 202, 400, 402, 408, 409, 500, 503),
  // and re-interpreting them here would put a second opinion about payment
  // outcomes in a process that knows nothing about payments.
  return new Response(await upstream.text(), {
    status: upstream.status,
    headers: {
      'content-type': upstream.headers.get('content-type') ?? 'application/json',
      // Never cache an order outcome, at any layer.
      'cache-control': 'no-store',
    },
  });
};
