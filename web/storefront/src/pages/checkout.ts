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

  // Bounded, and the failure is a declared JSON answer rather than Astro's error
  // page (ai-review [medium]). Without this a hung gateway holds this SSR request
  // for ever, and a transport error reaches the island as HTML that it tries to
  // parse as JSON — which it reports to the buyer as "payment status is being
  // checked", i.e. as payment uncertainty about a request that never arrived.
  //
  // 20s is above commerce's own checkout budget, so this fires only when the hop
  // itself is broken, never on a slow-but-working payment.
  let upstream: Response;
  try {
    upstream = await fetch(`${GATEWAY_URL}/api/commerce/orders`, {
      method: 'POST',
      headers,
      body: await request.text(),
      signal: AbortSignal.timeout(20_000),
    });
  } catch {
    // The outcome here is UNKNOWN, and the first version of this comment claimed
    // "nothing was submitted, so a retry is safe" — which is false (ai-review
    // pass 2 [high]). A timeout or a disconnect can land *after* the gateway
    // accepted the request and payments accepted the charge; only the response
    // was lost. Telling a buyer that is safe to retry is telling them to pay
    // twice.
    //
    // What actually protects them is commerce, not this handler, and the mechanism
    // is claimOrder resolving by RESERVATION ID — not by anything about the key.
    // It locks the order row for this reservation, so a retry meets the order the
    // lost attempt already created instead of opening a second one.
    //
    // This used to say the protection was the retry carrying a *fresh* key, which
    // claimOrder would refuse with 409. That stopped being true in TKT-184: the
    // island now sends a key bound to the reservation, so the retry arrives with the
    // SAME key and the same request fingerprint and is a REPLAY — it resolves the
    // existing order and reports its real outcome. That is a better answer than the
    // 409, and it is still not a second charge. A 409 now means the buyer edited
    // their name, email or payment token between attempts, which changes the
    // fingerprint. ADR-016's recovery runner resolves the order either way.
    //
    // So this returns 503 with a body that says UNKNOWN. The island's fallback
    // branch renders exactly that — "payment status is being checked" — which is
    // the honest answer and the one it already gave before this bridge existed.
    return new Response(
      JSON.stringify({ error: 'the payment outcome is unknown; it is being checked' }),
      { status: 503, headers: { 'content-type': 'application/json', 'cache-control': 'no-store' } },
    );
  }

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
