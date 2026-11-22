// Two loopback HTTP listeners for redirect tests (TKT-602, ADR-088).
//
// The FIRST HOP stands in for a configured service URL. It records every request
// and answers either directly or with a redirect to the TARGET, a second listener
// on a different origin. The target counts and records every request it receives.
// A target request after a redirect means that native fetch followed the redirect
// and sent the request, with its credentials, past the configured first hop.
//
// Native fetch and real sockets, never a fetch stub: a stub cannot show whether
// fetch made a second HTTP request. All values are synthetic.
import { createServer, type IncomingHttpHeaders, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';

export type Hit = {
  method: string;
  /** Path and query string, exactly as received. */
  path: string;
  headers: IncomingHttpHeaders;
  body: string;
};

export type Answer = {
  status: number;
  headers?: Record<string, string>;
  body?: string;
};

export type Listener = { origin: string; hits: Hit[] };

export type RedirectPair = {
  firstHop: Listener;
  target: Listener;
  close(): Promise<void>;
};

export function jsonAnswer(body: unknown, status = 200): Answer {
  return { status, headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) };
}

/** A redirect to `path` on the target. 307 keeps the method and the body. */
export function redirectAnswer(targetOrigin: string, path: string, status = 307): Answer {
  return {
    status,
    headers: { location: `${targetOrigin}${path}`, 'content-type': 'application/json' },
    body: JSON.stringify({ error: 'moved' }),
  };
}

async function listen(answer: (hit: Hit) => Answer): Promise<{ listener: Listener; server: Server }> {
  const hits: Hit[] = [];
  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => chunks.push(chunk));
    req.on('end', () => {
      const hit: Hit = {
        method: req.method ?? '',
        path: req.url ?? '',
        headers: req.headers,
        body: Buffer.concat(chunks).toString('utf8'),
      };
      hits.push(hit);
      let reply: Answer;
      try {
        reply = answer(hit);
      } catch (cause) {
        reply = { status: 599, body: `fixture handler failed: ${String(cause)}` };
      }
      res.writeHead(reply.status, reply.headers ?? {});
      res.end(reply.body ?? '');
    });
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  return { listener: { origin: `http://127.0.0.1:${port}`, hits }, server };
}

async function stop(server: Server): Promise<void> {
  // Fetch keeps connections alive. Close them, or close() waits for them.
  server.closeAllConnections();
  await new Promise<void>((resolve) => server.close(() => resolve()));
}

/**
 * Start the target, then the first hop. `firstHop` receives the target origin so
 * that it can build the redirect. A request the target does not expect gets a
 * finite 418, so a test never waits for a timeout.
 */
export async function startRedirectPair(handlers: {
  firstHop: (hit: Hit, targetOrigin: string) => Answer;
  target?: (hit: Hit) => Answer;
}): Promise<RedirectPair> {
  const target = await listen(handlers.target ?? (() => ({ status: 418 })));
  const firstHop = await listen((hit) => handlers.firstHop(hit, target.listener.origin));
  return {
    firstHop: firstHop.listener,
    target: target.listener,
    async close() {
      await Promise.all([stop(firstHop.server), stop(target.server)]);
    },
  };
}

/** The base-URL settings of the Back Office service clients. */
export const BASE_URL_SETTINGS = [
  'GATEWAY_URL',
  'CATALOG_URL',
  'COMMERCE_URL',
  'INVENTORY_URL',
  'ACCESS_URL',
] as const;

/** Point every client base URL at `origin`. Call before the clients are imported. */
export function pointClientsAt(origin: string): void {
  for (const name of BASE_URL_SETTINGS) process.env[name] = origin;
}

export function clearClientUrls(): void {
  for (const name of BASE_URL_SETTINGS) delete process.env[name];
}
