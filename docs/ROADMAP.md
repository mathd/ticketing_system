# Roadmap

This is the capability-level view. Ticket state is not in this repo: it lives as one note per
ticket in a Fast Note Sync vault, served by the board in `~/sources/sdlc-board`, and that board is
the source for ticket status and the next Gate 1 choice. Unchecked acceptance boxes in the PRD
preserve the planning baseline; they are not a second completion tracker.

## Completed

- **M1 walking skeleton** (TKT-1, US-001 through US-006). Five services, the gateway, storefront,
  and scanner run under Compose. One GA ticket travels from event publication through a contended
  reservation, fake-PSP checkout, QR delivery, and gate scan.
- **Catalog and venue authoring** (TKT-2, TKT-3). Events, typed dated slots, series, seasons,
  festivals, GA venues, and immutable published seat-map versions are implemented.
- **Inventory and reservation core** (TKT-4). Capacity adjustment, operational holds, channel
  allocations, group reservations, seated claims, orphan prevention, best-available selection,
  and the sustained no-oversell proof are implemented.
- **Pricing, fees, and settlement foundations** (TKT-5, TKT-6). Rule-resolved integer prices,
  fee incidence, payees, split schedules, and the settlement ledger are implemented.
- **Buyer and post-purchase flows** (TKT-9, TKT-21, TKT-35). The storefront supports customer
  accounts, guest-order claims, wallets, password recovery, interactive seating, ticket bundles,
  refunds, and exchanges within the limits recorded by their ADRs.
- **Staff operations** (TKT-22). The role-gated back office supports authoring, venue and seat-map
  administration, order lookup and refunds, channel management, and allocation editing.
- **Hot read paths** (TKT-31). Storefront page data, Catalog public reads, and Inventory
  availability use bounded in-process caches. The Catalog and Inventory caches invalidate after
  writes and have operator kill switches; the storefront cache expires from upstream freshness.

## Next

- Select the next incomplete capability epic from the board at Gate 1. Do not use this page as a
  second ticket tracker; update this summary when an epic becomes true in the running system.

## Later

- Live PSP operation beyond the fake and Stripe test-mode adapters, multi-tenant onboarding UX,
  cloud deployment, and the NF525 compliance profile for the French market remain outside the
  current testbed scope. See the [product brief](./product/brief.md) for the v1 non-goals.
