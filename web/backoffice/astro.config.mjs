// Back-office shell: Astro 7 SSR + React (ADR-006), reusing the storefront's
// real scaffolding (@astrojs/node standalone, ssr.noExternal) minus i18n — a
// single-locale staff tool. It serves under /admin/ (base), because the gateway
// proxies /admin/ to this app WITHOUT stripping the prefix (web-shell rule,
// gateway/cmd/gateway/main.go); a missing base would 404 every asset behind the
// proxy. The healthz probe therefore lives at /admin/healthz (see compose).
import node from '@astrojs/node';
import react from '@astrojs/react';
import { defineConfig } from 'astro/config';

export default defineConfig({
  output: 'server',
  base: '/admin',
  // The back-office is served exclusively behind the gateway reverse proxy
  // (SetXForwarded; gateway/cmd/gateway/main.go). Astro's default SSR CSRF guard
  // (security.checkOrigin) compares the browser Origin against the app's own
  // Host, which never matches through the proxy — so it 403s EVERY POST
  // ("Cross-site POST form submissions are forbidden"), breaking all staff write
  // forms (found in TKT-105 browser verification; TKT-102's authoring forms were
  // latently broken the same way, undetected because smoke tests hit the catalog
  // API directly, not the Astro SSR layer). The gateway is the trust boundary for
  // this internal, single-organizer staff tool (no auth yet); disable the check
  // here. Revisit if the back-office is ever exposed off the gateway or gains
  // per-user auth — then origin/host must be reconciled at the proxy instead.
  security: { checkOrigin: false },
  adapter: node({ mode: 'standalone' }),
  vite: {
    // Bundle every SSR dependency into dist/server so the runtime image ships
    // dist only — no node_modules (see Dockerfile).
    ssr: { noExternal: true },
  },
  integrations: [react()],
});
