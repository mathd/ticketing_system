declare namespace App {
  interface Locals {
    /** Set by page reads (src/lib/api.ts); consumed by src/middleware.ts. */
    pageData?: {
      ageSeconds: number;
      maxAgeSeconds: number;
    };
    /**
     * Set by the gate (src/lib/gate.ts via src/middleware.ts) on an authenticated
     * account page, and only there. Its absence on a public page is not a bug:
     * the storefront is guest-first and a public page never consults the session
     * (TKT-220).
     */
    customer?: import('./lib/session').CustomerPrincipal;
  }
}
