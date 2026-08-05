import { describe, expect, it } from 'vitest';

import { pollDelayFromResponse } from '../src/components/SeatMapPicker';

// TKT-208 / ADR-004 rule 3: "each response's TTL drives the client refresh
// cadence. No polling faster than the endpoint's TTL."
//
// Before this ticket the cadence was `const POLL_MS = 5000`, and the contract
// declares max-age=5. The two matched by coincidence, not by construction —
// nothing kept them in step, and a tier change would have left the client
// polling at the old rate with no test noticing.
//
// So these cases deliberately use TTLs the old literal NEVER had. A test that
// served max-age=5 and observed 5000ms could not tell a derived value from the
// constant it replaced, which is the exact shape of test this epic has now been
// caught by four times.
describe('poll cadence is derived from the response', () => {
  it('follows a TTL the old constant never had', () => {
    expect(pollDelayFromResponse('public, max-age=7, s-maxage=7')).toBe(7000);
    expect(pollDelayFromResponse('public, max-age=9, s-maxage=9')).toBe(9000);
    expect(pollDelayFromResponse('public, max-age=30')).toBe(30000);
  });

  it('still honours the tier the contract actually declares', () => {
    expect(pollDelayFromResponse('public, max-age=5, s-maxage=5')).toBe(5000);
  });

  it('falls back rather than inventing a cadence the contract did not promise', () => {
    // Each of these means "this response told us nothing usable". The safe
    // direction is the load we already had, never a faster one.
    for (const header of [null, '', 'no-store', 'public, max-age=0', 'public, max-age=abc', 'private']) {
      expect(pollDelayFromResponse(header)).toBe(5000);
    }
  });

  it('never polls faster than one second, whatever a response claims', () => {
    // No effect against today's contract; it exists so a parsing surprise or a
    // future tiny positive value cannot turn this into a hot loop against the
    // service the poll exists to serve.
    expect(pollDelayFromResponse('public, max-age=0.4')).toBe(5000); // parses to 0 → fallback
    expect(pollDelayFromResponse('public, max-age=1')).toBe(1000);
  });

  it('will not wait longer than a minute for a seconds-tier read', () => {
    // The ceiling is scoped to THIS endpoint, not to the system's longest tier.
    // A mistaken max-age=300 must not leave a buyer on a five-minute-old seat
    // map mid-on-sale; checkout still refuses the claim, but they would keep
    // picking seats that are already gone.
    expect(pollDelayFromResponse('public, max-age=60')).toBe(60000);
    expect(pollDelayFromResponse('public, max-age=61')).toBe(5000);
    expect(pollDelayFromResponse('public, max-age=300')).toBe(5000);
    // A timer-overflow check alone would have accepted max-age=2147483 — about
    // 24.9 DAYS — while every timer behaved correctly.
    expect(pollDelayFromResponse('public, max-age=2147483')).toBe(5000);
    expect(pollDelayFromResponse('public, max-age=999999999')).toBe(5000);
  });

  it('reads max-age, never s-maxage', () => {
    // Raised in ai-review as a suspected bug and checked rather than assumed:
    // the shared-cache directive is `s-maxage`, with no hyphen between max and
    // age, so /\bmax-age=/ cannot match it. Pinned because the question is a
    // reasonable one to ask twice, and because a future parser rewrite could
    // easily introduce the bug that was suspected here.
    expect(pollDelayFromResponse('public, s-maxage=300, max-age=7')).toBe(7000);
    expect(pollDelayFromResponse('public, max-age=7, s-maxage=300')).toBe(7000);
    expect(pollDelayFromResponse('s-maxage=300')).toBe(5000);
  });
});
