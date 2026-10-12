# ADR-083: Access reads performance time zones from Catalog

Date: 2026-10-12

## Status

Accepted (TKT-365, decision D1).

## Context

The storefront ticket page lists each ticket's lifecycle history. TKT-365 requires that these
times use the performance time zone. Before this change, the page formatted each time with the
default zone of the server process. The storefront container sets no `TZ`, so that zone is UTC,
and the page showed no zone name.

Catalog owns the zone: `performances.timezone` is `NOT NULL` and is set when the performance is
created. Nothing updates it after that.

The ticket page cannot get the zone itself:

- The page knows only the order reference in its URL. The Access ticket bundle had no
  performance id, no event id and no zone.
- Access stores each ticket's `slot_id` (the performance id), but no event id.
- Catalog has no public read by performance id. The public event read needs an event id, and it
  answers 404 unless the event has a published, priced performance. A ticket for an archived
  event would then have no zone.
- The storefront holds no service token, and the gateway refuses `/internal/` paths.

Catalog already has an internal read for a set of performance ids,
`GET /internal/performances/display-names` (TKT-222). It takes 1 to 20 ids and does not filter on
publication, because it serves past purchases.

## Possible Solutions

- **Option 1: Access reads the zone from Catalog's internal display resolver when it serves the
  bundle, and adds it to each ticket.**
    - Pros:
        - It uses a read that exists. Catalog adds one column to its response.
        - It answers for archived and unpublished performances.
        - The storefront keeps no service token.
    - Cons:
        - Access gets a new synchronous dependency on the page a buyer opens at the gate.
        - Two service contracts change.
- **Option 2: Access sends `slot_id`, and the storefront reads the zone from a new public
  Catalog endpoint by performance id.**
    - Pros:
        - Access does not call Catalog.
    - Cons:
        - It needs a new public endpoint, a gateway route and an Access contract change, so it is
          not smaller.
        - The new endpoint is an unscoped read by id in the public contract.
- **Option 3: Catalog adds the zone to the `performance.published` event, and Access stores it.**
    - Pros:
        - No read at bundle time.
    - Cons:
        - An event payload change (ADR-017), a projection table, and a backfill for the
          performances published before the change.
- **Option 4: Do nothing.** History times stay in the server zone, with no zone name. This does
  not meet the TKT-365 requirement.

## Decision

We adopt Option 1.

1. Catalog's internal display resolver returns `timezone` for each performance it knows.
2. For each bundle it serves, Access asks the resolver for the distinct slot ids in that bundle.
   It sends at most 20 ids in one request, so it makes one request for each 20 distinct
   performances, in sequence, and all of them share one deadline (item 5). It sends the shared
   internal service token and a fixed supported locale, because the resolver also resolves the
   event name, which Access does not read. It maps each zone to its ticket by performance id.
3. The ticket contract has a required, nullable `performance_timezone` on each ticket. The value
   is per ticket because an exchange can put tickets for different performances under one order
   reference.
4. Failure policy: each failure makes a zone `null`, and the bundle still loads. This includes a
   timeout, a non-200 answer, a body that does not decode, and an id that Catalog does not know.
   A zone is presentation. It must not stop a buyer from getting their QR code.
5. Deadline: all the requests of one bundle share one deadline of 1.5 s. This is less than the
   storefront's 5 s deadline for one upstream read, so a slow Catalog alone does not make that read
   time out. It adds up to 1.5 s to every bundle read, and so uses part of the storefront's total
   ticket-read budget (`ISSUANCE_TOTAL_BUDGET_MS`, which also covers the issuance retries). It does
   not guarantee that the whole read finishes within that budget.
6. Access refuses to start without `CATALOG_URL`.
7. The storefront shows each time with its zone name. When the zone is `null`, or when its ICU
   data does not know the zone, it shows the time in UTC with the label "UTC". It never uses the
   zone of the server process.

Premise: the zone does not change after a performance is created. Thus the zone that Access reads
today is also the zone at purchase time. If a later change lets an organizer edit a performance's
zone, this premise is false. Then that change must decide whether a ticket shows the current zone
or the zone at purchase.

## Consequences

- **Positive:**
    - Ticket history times show in the performance zone, also for an archived event and also when
      a buyer opens the ticket link directly.
    - The read is a seam between Access and Catalog for performance data. A later ticket that
      shows the event name or date on the ticket page can use the same read.
    - A Catalog outage cannot stop the ticket page. The page shows labelled UTC times.
- **Negative:**
    - Each bundle read makes more requests, to Catalog: one for each 20 distinct performances in
      the bundle. Most of these occur when doors open. One shared deadline limits the extra time
      for all of them together. There is no cache, because the zone is immutable only by
      the premise above.
    - Access's handler tests use a fake zone reader. Only the smoke suite
      (`smoke/ticket_timezone_test.go`) proves that the real binary is wired to Catalog.

## References *(optional)*

- TKT-365, TKT-222, TKT-36
- [ADR-012](ADR-012-ticket-issuance-and-qr-credentials.md) (the ticket bundle)
- [ADR-017](ADR-017-domain-event-schema-evolution.md) (why Option 3 is larger)
- [ADR-043](ADR-043-where-a-service-auth-guard-lives.md) (the guard on Catalog's internal surface)
- [Configuration: Access and CATALOG_URL](../configuration.md#access-catalog_url-and-ticket-time-zones)
