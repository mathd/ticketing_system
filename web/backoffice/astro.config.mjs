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
  // this internal, single-organizer staff tool; disable the check here.
  //
  // TKT-190 is the "gains per-user auth" revisit this comment used to defer to.
  // The check STAYS off, because re-enabling it does not work: Astro's Node
  // adapter builds its request URL from the container's own Host, so the
  // comparison is wrong through the proxy no matter which side you adjust, and
  // it would still be wrong behind TLS termination. What replaces it is a
  // proxy-aware origin check in src/lib/gate.ts, run by src/middleware.ts on
  // every unsafe method: it compares Origin against the PUBLIC origin the
  // gateway reports via X-Forwarded-Proto/Host (which Go's SetXForwarded
  // overwrites, so a client cannot forge them through the gateway). Session
  // cookies are additionally SameSite=Lax. See ADR-042.
  security: { checkOrigin: false },
  adapter: node({ mode: 'standalone' }),
  vite: {
    // Bundle every SSR dependency into dist/server so the runtime image ships
    // dist only — no node_modules (see Dockerfile).
    ssr: { noExternal: true },
  },
  integrations: [react()],
});
