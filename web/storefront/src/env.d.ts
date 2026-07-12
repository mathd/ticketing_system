declare namespace App {
  interface Locals {
    /** Set by page reads (src/lib/api.ts); consumed by src/middleware.ts. */
    pageData?: {
      ageSeconds: number;
      maxAgeSeconds: number;
    };
  }
}
