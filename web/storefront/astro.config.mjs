// Storefront shell: Astro 7 SSR + React islands (ADR-006). Cache-Control per
// route class is set in src/middleware.ts; this story's reads are all
// page-layer owned (minutes tier), no islands yet — those arrive in US-003.
import node from '@astrojs/node';
import react from '@astrojs/react';
import { defineConfig } from 'astro/config';

export default defineConfig({
  output: 'server',
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
