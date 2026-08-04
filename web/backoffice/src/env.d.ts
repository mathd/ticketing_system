import type { StaffPrincipal } from './lib/session';

declare global {
  namespace App {
    interface Locals {
      /**
       * Set by src/middleware.ts on every gated request (TKT-190). Optional in
       * the type because the anonymous paths — login, healthz, static assets —
       * run without one; a page that reads it must be behind the gate.
       */
      staff?: StaffPrincipal;
    }
  }
}

export {};
