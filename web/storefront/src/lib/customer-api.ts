// Customer account calls to commerce, through the gateway (TKT-220 / ADR-049).
//
// These are the storefront's first WRITES. They carry no credential and need
// none: `registerCustomer` and `authenticateCustomer` are public operations in
// commerce's contract, which is what lets this container keep holding exactly one
// environment variable (GATEWAY_URL) and no service token. Handing a
// public-facing SSR process INTERNAL_SERVICE_TOKEN — one value that also opens
// commerce's refunds and inventory's operational holds — is what that buys us out
// of. See compose.yaml's back-office block for the same argument, and ADR-043 for
// the rule.
//
// Types come from commerce's OpenAPI document (ADR-009); regenerate with
// `make generate`. The generated file is in check-generate's diff list, so a
// contract change that is not regenerated fails the gate.
import type { components } from './commerce-api-types.gen';

export type CustomerPrincipalResponse = components['schemas']['CustomerPrincipal'];

const GATEWAY_URL = process.env.GATEWAY_URL ?? 'http://localhost:8080';

/** What the caller needs to know, without leaking commerce's wire shape upward. */
export type CustomerResult =
  | { ok: true; principal: CustomerPrincipalResponse }
  | { ok: false; reason: 'invalid' | 'taken' | 'unavailable' };

async function post(path: string, body: unknown): Promise<Response> {
  return fetch(`${GATEWAY_URL}/api/commerce${path}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify(body),
  });
}

export async function registerCustomer(email: string, password: string): Promise<CustomerResult> {
  const response = await post('/customers', { email, password });
  if (response.status === 201) {
    return { ok: true, principal: (await response.json()) as CustomerPrincipalResponse };
  }
  if (response.status === 409) return { ok: false, reason: 'taken' };
  // 400 lands here too. The contract has already bounded the input, so a 400
  // means the form and the schema disagree — an outage-shaped answer is the
  // honest one, and it is never a claim about the credential.
  if (response.status === 400) return { ok: false, reason: 'invalid' };
  return { ok: false, reason: 'unavailable' };
}

export async function authenticateCustomer(
  email: string,
  password: string,
): Promise<CustomerResult> {
  const response = await post('/customers/authenticate', { email, password });
  if (response.status === 200) {
    return { ok: true, principal: (await response.json()) as CustomerPrincipalResponse };
  }
  // 401 and 400 collapse to ONE answer on purpose. A form that says "no such
  // account" for one and "check your password" for the other reopens, in the UI,
  // exactly the enumeration the store and the handler go to some trouble to
  // close. Anything else is an outage, which is NOT a credential verdict: telling
  // a buyer their correct password is wrong sends them to reset it — which this
  // system cannot do — while the real fault goes unreported.
  if (response.status === 401 || response.status === 400) return { ok: false, reason: 'invalid' };
  return { ok: false, reason: 'unavailable' };
}

// --- the wallet (TKT-222) ---

export type CustomerOrderPage = components['schemas']['CustomerOrderPage'];
export type CustomerOrderSummary = components['schemas']['CustomerOrderSummary'];

/**
 * One page of the signed-in customer's purchases.
 *
 * The assertion is read from the session and sent as a header, server-side. It
 * must never be handed to the browser — an XSS that could read it would be able
 * to attribute purchases to this customer for the rest of the session.
 *
 * A failure returns undefined rather than throwing: the account page renders a
 * "temporarily unavailable" state, which is a better answer for a buyer than a
 * 500, and the distinction between "you have nothing" and "we could not look" is
 * one the page must be able to make.
 */
export async function listCustomerOrders(
  customerId: string,
  assertion: string,
  locale: string,
  after?: string,
): Promise<CustomerOrderPage | undefined> {
  const query = new URLSearchParams({ locale });
  if (after) query.set('after', after);
  // Every failure becomes `undefined`, including a rejected fetch and an
  // undecodable body (ai-review [medium]). Without the catch a gateway reset
  // escapes Astro's frontmatter and the buyer gets a 500 instead of the
  // "temporarily unavailable" state the page already knows how to render — and a
  // 500 is the one answer that tells them nothing and offers nothing.
  try {
    const response = await fetch(
      `${GATEWAY_URL}/api/commerce/customers/${encodeURIComponent(customerId)}/orders?${query}`,
      {
        headers: { Accept: 'application/json', 'X-Customer-Assertion': assertion },
        // A wallet is a page render, not a background job: it must not hold the
        // SSR request for the global client timeout while commerce is stuck.
        signal: AbortSignal.timeout(5_000),
      },
    );
    if (response.status !== 200) return undefined;
    return (await response.json()) as CustomerOrderPage;
  } catch {
    return undefined;
  }
}

// --- claiming a past guest order (TKT-223) ---

export type ClaimResult = { ok: true; orderId: string } | { ok: false; reason: 'refused' | 'unavailable' };

/**
 * Attach a completed guest order to the signed-in customer.
 *
 * The assertion travels as a header, server-side; the order reference is a bearer
 * credential (ADR-012) and must not be logged.
 *
 * `refused` covers every case commerce refuses — no such order, not completed,
 * claimed by somebody else — because commerce deliberately answers all three
 * identically and this layer must not invent a distinction it was not given.
 */
export async function claimGuestOrder(
  guestOrderRef: string,
  assertion: string,
): Promise<ClaimResult> {
  try {
    const response = await fetch(`${GATEWAY_URL}/api/commerce/orders/claim`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Accept: 'application/json',
        'X-Customer-Assertion': assertion,
      },
      body: JSON.stringify({ guest_order_ref: guestOrderRef }),
      signal: AbortSignal.timeout(5_000),
    });
    if (response.status === 200) {
      const body = (await response.json()) as { order_id: string };
      return { ok: true, orderId: body.order_id };
    }
    if (response.status === 404 || response.status === 400) return { ok: false, reason: 'refused' };
    return { ok: false, reason: 'unavailable' };
  } catch {
    return { ok: false, reason: 'unavailable' };
  }
}
