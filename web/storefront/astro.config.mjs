// Storefront shell: Astro 7 SSR + React islands (ADR-006). Cache-Control per
// route class is set in src/middleware.ts; this story's reads are all
// page-layer owned (minutes tier), no islands yet — those arrive in US-003.
import node from '@astrojs/node';
import react from '@astrojs/react';
import { defineConfig } from 'astro/config';

export default defineConfig({
  output: 'server',
  // Astro's default SSR CSRF guard (security.checkOrigin, which defaults to TRUE)
  // compares the browser's Origin against this app's own Host — behind the
  // gateway that is the container name, so it never matches and 403s EVERY POST
  // ("Cross-site POST form submissions are forbidden"). TKT-105 found this in the
  // back office; the storefront had no form until TKT-220, so the default sat
  // here latent and nothing could have noticed — `make check`'s smoke suite
  // renders pages, it never submits one.
  //
  // Re-enabling it is not an option: the Node adapter builds its request URL from
  // the container Host, so the comparison is wrong through the proxy whichever
  // side you adjust, and it would still be wrong behind TLS termination. What
  // replaces it is a proxy-aware origin check in src/lib/gate.ts, run by
  // src/middleware.ts on every unsafe method: it compares Origin against the
  // PUBLIC origin the gateway reports via X-Forwarded-Proto/Host (which Go's
  // SetXForwarded overwrites, so a client cannot forge them through the gateway).
  // Session cookies are additionally SameSite=Lax. See ADR-049.
  security: { checkOrigin: false },
  adapter: node({ mode: 'standalone' }),
  vite: {
    // Bundle every SSR dependency (react included) into dist/server so the
    // runtime images ship dist only — no node_modules (see Dockerfile*).
    ssr: { noExternal: true },
  },
  // "/" lands on the default-locale events list ("/en/" is covered by
  // i18n.redirectToDefaultLocale; "/en/" -> events by pages/[locale]/index).
  redirects: { '/': '/en/events' },
  i18n: {
    locales: ['en', 'fr'],
    defaultLocale: 'en',
    routing: {
      prefixDefaultLocale: true,
      redirectToDefaultLocale: true,
    },
  },
  integrations: [react()],
});
