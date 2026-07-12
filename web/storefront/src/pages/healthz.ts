// Container liveness probe (compose healthcheck; wget-compatible).
import type { APIRoute } from 'astro';

export const GET: APIRoute = () =>
  new Response(JSON.stringify({ status: 'ok', service: 'storefront' }), {
    headers: { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' },
  });
