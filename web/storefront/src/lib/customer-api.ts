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
import { withUpstreamDeadline } from './upstream';

export type CustomerPrincipalResponse = components['schemas']['CustomerPrincipal'];

const GATEWAY_URL = process.env.GATEWAY_URL ?? 'http://localhost:8080';

/** What the caller needs to know, without leaking commerce's wire shape upward. */
export type CustomerResult =
  | { ok: true; principal: CustomerPrincipalResponse }
  | { ok: false; reason: 'invalid' | 'taken' | 'unavailable' | 'throttled' };

/**
 * Commerce rate-limits the public customer surface (TKT-224, ADR-051).
 *
 * A 429 is its own reason and must not collapse into `unavailable`: that word
 * tells a buyer something is broken and to escalate, when in fact they only have
 * to wait. It must not collapse into a credential verdict either — that would be
 * false, and it would leak, because the limiter refuses BEFORE commerce looks the
 * address up and so answers identically for an address that exists and one that
 * does not. Preserving that indistinguishability up here is what stops the UI
 * reopening the oracle the handler closed.
 */
const THROTTLED = 429;

async function post(path: string, body: unknown, signal: AbortSignal): Promise<Response> {
  return fetch(`${GATEWAY_URL}/api/commerce${path}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify(body),
    signal,
  });
}

export async function registerCustomer(email: string, password: string): Promise<CustomerResult> {
  try {
    return await withUpstreamDeadline(async (signal) => {
      const response = await post('/customers', { email, password }, signal);
      if (response.status === 201) {
        return { ok: true, principal: (await response.json()) as CustomerPrincipalResponse };
      }
      if (response.status === THROTTLED) return { ok: false, reason: 'throttled' };
      if (response.status === 409) return { ok: false, reason: 'taken' };
      // 400 lands here too. The contract has already bounded the input, so a
      // 400 means the form and the schema disagree.
      if (response.status === 400) return { ok: false, reason: 'invalid' };
      return { ok: false, reason: 'unavailable' };
    });
  } catch {
    return { ok: false, reason: 'unavailable' };
  }
}

export async function authenticateCustomer(
  email: string,
  password: string,
): Promise<CustomerResult> {
  try {
    return await withUpstreamDeadline(async (signal) => {
      const response = await post('/customers/authenticate', { email, password }, signal);
      if (response.status === 200) {
        return { ok: true, principal: (await response.json()) as CustomerPrincipalResponse };
      }
      // 401 and 400 collapse to one answer on purpose. Commerce does not reveal
      // whether the account or password was wrong, and this layer must not infer it.
      if (response.status === THROTTLED) return { ok: false, reason: 'throttled' };
      if (response.status === 401 || response.status === 400) {
        return { ok: false, reason: 'invalid' };
      }
      return { ok: false, reason: 'unavailable' };
    });
  } catch {
    return { ok: false, reason: 'unavailable' };
  }
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
    return await withUpstreamDeadline(async (signal) => {
      const response = await fetch(
        `${GATEWAY_URL}/api/commerce/customers/${encodeURIComponent(customerId)}/orders?${query}`,
        {
          headers: { Accept: 'application/json', 'X-Customer-Assertion': assertion },
          signal,
        },
      );
      if (response.status !== 200) return undefined;
      return (await response.json()) as CustomerOrderPage;
    });
  } catch {
    return undefined;
  }
}

// --- claiming a past guest order (TKT-223) ---

export type ClaimResult =
  | { ok: true; orderId: string }
  | { ok: false; reason: 'refused' | 'unavailable' | 'throttled' };

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
    return await withUpstreamDeadline(async (signal) => {
      const response = await fetch(`${GATEWAY_URL}/api/commerce/orders/claim`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Accept: 'application/json',
          'X-Customer-Assertion': assertion,
        },
        body: JSON.stringify({ guest_order_ref: guestOrderRef }),
        signal,
      });
      if (response.status === 200) {
        const body = (await response.json()) as { order_id: string };
        return { ok: true, orderId: body.order_id };
      }
      if (response.status === THROTTLED) return { ok: false, reason: 'throttled' };
      if (response.status === 404 || response.status === 400) {
        return { ok: false, reason: 'refused' };
      }
      return { ok: false, reason: 'unavailable' };
    });
  } catch {
    return { ok: false, reason: 'unavailable' };
  }
}

/**
 * Ask commerce to mail a reset link (TKT-226).
 *
 * ONE result, deliberately not a discriminated union: commerce answers 202 whether
 * or not the address holds an account, so this layer has nothing to discriminate on
 * and must not invent something. A `reason: 'unknown'` here would be a distinction
 * the page could render — the enumeration oracle the endpoint is shaped to avoid.
 *
 * `ok: false` is a genuine outage and is the only thing a caller may branch on.
 */
export async function requestPasswordReset(
  email: string,
): Promise<{ ok: boolean; throttled?: boolean }> {
  try {
    return await withUpstreamDeadline(async (signal) => {
      const response = await post('/customers/password-reset', { email }, signal);
      // This branch is safe because commerce decides it before looking up the
      // address, so it says nothing about whether an account exists.
      if (response.status === THROTTLED) return { ok: false, throttled: true };
      return { ok: response.status === 202 };
    });
  } catch {
    return { ok: false };
  }
}

export type ResetCompletionResult =
  | { ok: true; customerId: string }
  | { ok: false; reason: 'refused' | 'invalid' | 'unavailable' | 'throttled' };

/**
 * Redeem a mailed token and set a new password (TKT-226).
 *
 * `refused` is unknown, expired and already-used together, because commerce answers
 * all three identically and this layer must not invent a distinction it was not
 * given — the rule claimGuestOrder follows for the same reason.
 *
 * `customerId` is returned so the caller can destroy that customer's sessions. It is
 * NOT a credential, and completing a reset does not sign anyone in: the buyer signs
 * in afterwards with the password they just chose.
 */
export async function completePasswordReset(
  token: string,
  password: string,
): Promise<ResetCompletionResult> {
  try {
    return await withUpstreamDeadline(async (signal) => {
      const response = await post('/customers/password-reset/complete', { token, password }, signal);
      if (response.status === 200) {
        const body = (await response.json()) as { customer_id: string };
        return { ok: true, customerId: body.customer_id };
      }
      if (response.status === THROTTLED) return { ok: false, reason: 'throttled' };
      if (response.status === 400) {
        // Commerce distinguishes a dead link from a rejected password. A dead
        // link sends the buyer back for another, while a rejected password keeps
        // the still-valid token on this form.
        const body = (await response.json().catch(() => ({}))) as { error?: string };
        return { ok: false, reason: body.error === 'invalid request' ? 'invalid' : 'refused' };
      }
      return { ok: false, reason: 'unavailable' };
    });
  } catch {
    return { ok: false, reason: 'unavailable' };
  }
}
