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
  adapter: node({ mode: 'standalone' }),
  vite: {
    // Bundle every SSR dependency into dist/server so the runtime image ships
    // dist only — no node_modules (see Dockerfile).
    ssr: { noExternal: true },
  },
  integrations: [react()],
});
