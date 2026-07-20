// Container liveness probe. With `base: '/admin'`, Astro serves this at
// /admin/healthz — which is what the compose healthcheck targets (it hits the
// container directly, pre-gateway). wget-compatible.
import type { APIRoute } from 'astro';

export const GET: APIRoute = () =>
  new Response(JSON.stringify({ status: 'ok', service: 'backoffice' }), {
    headers: { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' },
  });
