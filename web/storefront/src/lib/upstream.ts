export const UPSTREAM_DEADLINE_MS = 5_000;

/**
 * Bound one complete server-side service operation, including response decoding.
 * Keeping the callback inside the deadline prevents a peer that sends headers
 * and then stalls its body from retaining an SSR request indefinitely.
 *
 * `budgetMs` exists for the one operation that legitimately needs longer: a read
 * that RETRIES. The default bounds a single request, so applying it to a loop
 * silently shortens the loop's own budget instead of bounding it — see
 * ticket-api.ts. An operation that retries must pass the budget it actually
 * needs, and it must still pass one.
 */
export async function withUpstreamDeadline<T>(
  operation: (signal: AbortSignal) => Promise<T>,
  budgetMs: number = UPSTREAM_DEADLINE_MS,
): Promise<T> {
  const controller = new AbortController();
  const deadline = setTimeout(() => controller.abort(), budgetMs);
  try {
    return await operation(controller.signal);
  } finally {
    clearTimeout(deadline);
  }
}
