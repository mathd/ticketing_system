import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

// TKT-165 / ADR-071. The ordering assumption must reach the GENERATED CLIENT, which is the
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
// EACH ASSERTION IS SCOPED TO ITS OWN OPERATION'S BLOCK, and that is the whole point of the
// slicing below. The first version of this file called `toContain` on the WHOLE generated
// file for every case, which made the operation name decorative: each phrase was found
// wherever it lived. Its comment claimed "a description accidentally copied from a sibling
// operation fails rather than passing on the shared marker alone" — and that sentence was
// FALSE when it was written. ai-review ran the mutation: exchange `confirmHold`'s and
// `releaseHold`'s descriptions in openapi.yaml, regenerate, and both vitest and `go test`
// stayed green while `confirm` told operators to check the entitlement and `release` told
// them to capture payment. AGENTS.md names this exact shape — an assertion written BECAUSE a
// hazard was identified and unable to see it.
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

/**
 * The generated text for one path's entry, and nothing after it.
 *
 * openapi-typescript keys the `paths` object by PATH, not by operationId, so the path
 * literal is the only handle the generated file offers. The slice runs to the next
 * top-level path key, which is what makes a per-operation assertion possible at all — an
 * assertion against the whole file cannot tell one operation's prose from its neighbour's.
 *
 * Throws rather than returning empty on a miss: a renamed path must fail loudly here, not
 * silently reduce every assertion below to a search of an empty string, which would pass
 * nothing and fail nothing.
 */
function blockForPath(path: string): string {
  const key = `    "${path}": {`;
  const start = generated.indexOf(key);
  if (start === -1) {
    throw new Error(
      `${path} is not in the generated client. ADR-071 §4 enumerates it, so either the ` +
        `enumeration is stale or the route was renamed.`,
    );
  }
  const after = generated.indexOf('\n    "/', start + key.length);
  return after === -1 ? generated.slice(start) : generated.slice(start, after);
}

describe('the generated inventory client carries the ordering assumptions', () => {
  // ADR-071 §4's three inventory operations, each with the phrase that makes its own
  // assumption identifiable. The phrase is asserted against THAT OPERATION'S BLOCK, so a
  // description copied or swapped from a sibling fails — see the header note.
  it.each([
    ['/internal/holds/{id}/confirm', 'confirmHold', 'must have captured the payment'],
    ['/internal/holds/{id}/release', 'releaseHold', 'can no longer admit before releasing'],
    [
      '/internal/holds/{id}/refund-capacity',
      'returnRefundedCapacity',
      'AFTER the tickets are voided',
    ],
  ])('%s (%s) declares its own assumption', (path, operation, ownPhrase) => {
    const block = blockForPath(path);
    expect(block, `${operation}: no ordering marker in its own generated block`).toContain(
      ORDERING_MARKER,
    );
    expect(block, `${operation}: its own assumption is missing from its generated block`).toContain(
      ownPhrase,
    );
  });

  // The standing mutation, pinned as a test rather than left as a note. Two operations must
  // not carry each other's assumption: this is what the whole-file version could not see.
  it('does not let one operation carry a sibling operation assumption', () => {
    const confirm = blockForPath('/internal/holds/{id}/confirm');
    const release = blockForPath('/internal/holds/{id}/release');
    expect(confirm, 'confirmHold carries releaseHold assumption').not.toContain(
      'can no longer admit before releasing',
    );
    expect(release, 'releaseHold carries confirmHold assumption').not.toContain(
      'must have captured the payment',
    );
  });

  it('emits the marker once per ordering-dependent operation, not once overall', () => {
    // Guards the failure this file exists for: a generator that emitted only the first
    // description, or collapsed them, would satisfy a bare `toContain` three times over.
    const occurrences = generated.split(ORDERING_MARKER).length - 1;
    expect(occurrences).toBe(3);
  });
});
