// The back office's process entrypoint (TKT-194).
//
// It exists for one reason: to check credential separation BEFORE the server
// listens, and to fail the process if it does not hold.
//
// The obvious placement — a module-scope call in middleware.ts — is not
// startup. Astro's standalone build registers the middleware as
// `manifest.middleware: () => import(...)`, resolved while rendering the first
// request, so the adapter is already listening by the time it runs (ai-review
// pass 2). The container does end up unhealthy, because /admin/healthz
// traverses middleware, but the process stays up and answers request-time
// failures instead of refusing to start. For a credential boundary that decides
// whether one bearer value both authors catalog content and moves money, "the
// healthcheck eventually notices" is the wrong mechanism.
//
// So the check runs here, before the dynamic import, and an exit code is the
// answer.
import { assertCredentialSeparation } from './credentials.mjs';

try {
  assertCredentialSeparation();
} catch (e) {
  console.error(`back office refusing to start: ${e.message}`);
  process.exit(1);
}

await import('./dist/server/entry.mjs');
