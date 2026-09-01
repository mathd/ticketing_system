import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

// TKT-165 / ADR-070. The ordering assumption must reach the GENERATED CLIENT, which is the
// artifact an integrator actually reads.
//
// WHY THIS EXISTS ALONGSIDE THE GO TESTS. The three Go declaration tests
// (services/{inventory,payments,commerce}/internal/api/internal_ordering_contract_test.go)
// assert the SERVED document: they load the embedded spec through the OpenAPI loader, where
// a `#` comment cannot survive. That is the contract. This asserts one step further along —
// that `openapi-typescript` carries operation prose through into the client — which is a
// property of the GENERATOR rather than of our spec, and is the last link in the chain the
// ticket cares about ("the assumption is visible where people integrate").
//
// ONE FILE, NOT TWO. The storefront's commerce client would exercise the same generator
// property with a smaller sample (one operation against inventory's three), so a second copy
// would be a fixture that proves nothing the first does not already prove — the
// seeds-two-mechanisms shape from AGENTS.md. Inventory is the sample because it carries the
// most ordering-dependent operations of any single client.
//
// WHAT IT CANNOT CATCH, so a green run is not over-read: `check-generate` already fails if
// this file drifts from the spec, so the realistic failure this catches is not drift but a
// GENERATOR CHANGE — an openapi-typescript upgrade that stopped emitting `@description`
// would leave the Go tests green, the spec correct, and every integrator's client silent.
// That is the input class this file, and only this file, rejects.

const generated = readFileSync(
  fileURLToPath(new URL('../src/lib/inventory-api-types.gen.ts', import.meta.url)),
  'utf8',
);

const ORDERING_MARKER = 'ORDERING ASSUMED, NOT VERIFIED';

describe('the generated inventory client carries the ordering assumptions', () => {
  // ADR-070 §4's three inventory operations, each with the phrase that makes its own
  // assumption identifiable — so a description accidentally copied from a sibling operation
  // fails rather than passing on the shared marker alone.
  it.each([
    ['confirmHold', 'must have captured the payment'],
    ['releaseHold', 'can no longer admit before releasing'],
    ['returnRefundedCapacity', 'AFTER the tickets are voided'],
  ])('%s declares its own assumption', (operation, ownPhrase) => {
    // The generated file keys operations by path, not by operationId, so assert on the
    // distinctive prose rather than trying to re-derive the generator's key shape.
    expect(generated, `${operation}: no ordering marker anywhere in the generated client`)
      .toContain(ORDERING_MARKER);
    expect(generated, `${operation}: its own assumption is missing from the generated client`)
      .toContain(ownPhrase);
  });

  it('emits the marker once per ordering-dependent operation, not once overall', () => {
    // Guards the failure this file exists for: a generator that emitted only the first
    // description, or collapsed them, would satisfy a bare `toContain` three times over.
    const occurrences = generated.split(ORDERING_MARKER).length - 1;
    expect(occurrences).toBe(3);
  });
});
