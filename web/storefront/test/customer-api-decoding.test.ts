import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  authenticateCustomer,
  claimGuestOrder,
  completePasswordReset,
  listCustomerOrders,
  registerCustomer,
} from '../src/lib/customer-api';

const CUSTOMER = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1';
const ORDER = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb2';
const GUEST_ORDER_REF = 'cccccccc-cccc-4ccc-8ccc-ccccccccccc3';
const OTHER_CUSTOMER = 'dddddddd-dddd-4ddd-8ddd-ddddddddddd4';

// Encoded independently of the production decoder. The storefront cannot verify
// the MAC, but it must require the issuer's four-field shape before persisting it.
function assertionFor(
  customerId: string,
  fields: { version?: string; expiry?: string; mac?: string } = {},
): string {
  return `${fields.version ?? 'v1'}.${customerId}.${fields.expiry ?? '2000000000'}.${fields.mac ?? 'A'.repeat(43)}`;
}

function respond(status: number, body: unknown): void {
  vi.stubGlobal('fetch', async () =>
    new Response(JSON.stringify(body), {
      status,
      headers: { 'content-type': 'application/json' },
    }));
}

const principal = {
  customer_id: CUSTOMER,
  email: 'buyer@example.test',
  customer_assertion: assertionFor(CUSTOMER),
};

const orderPage = {
  orders: [{
    order_id: ORDER,
    guest_order_ref: GUEST_ORDER_REF,
    purchased_at: '2026-08-03T12:00:00Z',
    quantity: 2,
    total_amount: 2500,
    currency: 'EUR',
    event_name: 'Opening night',
    starts_at: '2026-09-01T18:00:00Z',
  }],
  next_cursor: null,
};

afterEach(() => vi.unstubAllGlobals());

describe('customer response decoding', () => {
  it.each([
    ['absent identity', { email: principal.email, customer_assertion: principal.customer_assertion }],
    ['wrong field type', { ...principal, email: 42 }],
    ['malformed identity', { ...principal, customer_id: 'customer-1' }],
    ['a nil identity', {
      ...principal,
      customer_id: '00000000-0000-0000-0000-000000000000',
      customer_assertion: assertionFor('00000000-0000-0000-0000-000000000000'),
    }],
  ])('fails a successful registration closed when the principal has %s', async (_name, body) => {
    respond(201, body);
    await expect(registerCustomer('buyer@example.test', 'correct horse battery')).resolves.toEqual({
      ok: false,
      reason: 'unavailable',
    });
  });

  it('returns a fully decoded principal for a valid authentication response', async () => {
    respond(200, principal);
    await expect(authenticateCustomer('buyer@example.test', 'correct horse battery')).resolves.toEqual({
      ok: true,
      principal,
    });
  });

  it('binds the assertion subject to the principal as a canonical, case-insensitive UUID', async () => {
    const body = {
      ...principal,
      customer_id: CUSTOMER.toUpperCase(),
      customer_assertion: assertionFor(CUSTOMER),
    };
    respond(200, body);

    await expect(authenticateCustomer('buyer@example.test', 'correct horse battery')).resolves.toEqual({
      ok: true,
      principal,
    });
  });

  it.each([
    ['a non-string assertion', 42],
    ['an unsupported assertion version', assertionFor(CUSTOMER, { version: 'v2' })],
    ['a missing assertion subject', `v1..2000000000.${'A'.repeat(43)}`],
    ['a malformed assertion subject', assertionFor('customer-1')],
    ['a nil assertion subject', assertionFor('00000000-0000-0000-0000-000000000000')],
    ['a non-integer assertion expiry', assertionFor(CUSTOMER, { expiry: '2e9' })],
    ['a non-positive assertion expiry', assertionFor(CUSTOMER, { expiry: '0' })],
    ['an unsafe assertion expiry', assertionFor(CUSTOMER, { expiry: '9007199254740992' })],
    ['a missing assertion MAC', assertionFor(CUSTOMER, { mac: '' })],
    ['another customer in the assertion', assertionFor(OTHER_CUSTOMER)],
  ])('fails a successful authentication closed with %s', async (_name, customerAssertion) => {
    respond(200, { ...principal, customer_assertion: customerAssertion });
    await expect(authenticateCustomer('buyer@example.test', 'correct horse battery')).resolves.toEqual({
      ok: false,
      reason: 'unavailable',
    });
  });

  it.each([
    ['an absent required array', { next_cursor: null }],
    ['a non-array orders field', { orders: {}, next_cursor: null }],
    ['a malformed order identity', {
      ...orderPage,
      orders: [{ ...orderPage.orders[0], order_id: 'order-1' }],
    }],
    ['an impossible quantity', {
      ...orderPage,
      orders: [{ ...orderPage.orders[0], quantity: 0 }],
    }],
    ['a fractional money amount', {
      ...orderPage,
      orders: [{ ...orderPage.orders[0], total_amount: 12.5 }],
    }],
    ['a negative money amount', {
      ...orderPage,
      orders: [{ ...orderPage.orders[0], total_amount: -1 }],
    }],
    ['a quantity above the per-order limit', {
      ...orderPage,
      orders: [{ ...orderPage.orders[0], quantity: 51 }],
    }],
    ['an unsafe money amount', {
      ...orderPage,
      orders: [{ ...orderPage.orders[0], total_amount: Number.MAX_SAFE_INTEGER + 1 }],
    }],
    ['a non-ISO currency', {
      ...orderPage,
      orders: [{ ...orderPage.orders[0], currency: 'eur' }],
    }],
    ['a three-character non-currency', {
      ...orderPage,
      orders: [{ ...orderPage.orders[0], currency: 'EU1' }],
    }],
    ['an invalid date', {
      ...orderPage,
      orders: [{ ...orderPage.orders[0], purchased_at: 'tomorrow' }],
    }],
  ])('does not turn a malformed wallet body with %s into an order page', async (_name, body) => {
    respond(200, body);
    await expect(listCustomerOrders(CUSTOMER, 'assertion', 'en')).resolves.toBeUndefined();
  });

  it('accepts the exact per-order quantity and JavaScript-safe money ceilings', async () => {
    const body = {
      ...orderPage,
      orders: [{
        ...orderPage.orders[0],
        quantity: 50,
        total_amount: Number.MAX_SAFE_INTEGER,
      }],
    };
    respond(200, body);

    await expect(listCustomerOrders(CUSTOMER, 'assertion', 'en')).resolves.toEqual(body);
  });

  it.each([
    ['a malformed order id', {
      order_id: 'order-1',
      guest_order_ref: GUEST_ORDER_REF,
      customer_id: CUSTOMER,
    }],
    ['another customer', {
      order_id: ORDER,
      guest_order_ref: GUEST_ORDER_REF,
      customer_id: OTHER_CUSTOMER,
    }],
  ])('does not claim success when the claim body identifies %s', async (_name, body) => {
    respond(200, body);
    await expect(claimGuestOrder(GUEST_ORDER_REF, CUSTOMER, 'assertion')).resolves.toEqual({
      ok: false,
      reason: 'unavailable',
    });
  });

  it('accepts claim identities that differ from the request only by UUID case', async () => {
    respond(200, {
      order_id: ORDER,
      guest_order_ref: GUEST_ORDER_REF,
      customer_id: CUSTOMER,
    });
    await expect(claimGuestOrder(
      GUEST_ORDER_REF.toUpperCase(),
      CUSTOMER.toUpperCase(),
      'assertion',
    )).resolves.toEqual({
      ok: true,
      orderId: ORDER,
    });
  });

  it('does not accept a reset response without a customer identity', async () => {
    respond(200, {});
    await expect(completePasswordReset('reset-token', 'a new password')).resolves.toEqual({
      ok: false,
      reason: 'unavailable',
    });
  });
});
