# ADR-073: Runtime bounds and service dependencies

Date: 2026-09-14

## Status

Accepted, TKT-336.

## Context

The validated review identified four runtime issues:

- The gateway's pooled transport had no deadline covering upstream headers and response bodies. A server write deadline limits client writes; it does not itself interrupt a blocked upstream read.
- The proxy did not create outbound client spans. Adding OpenTelemetry transport instrumentation introduces `url.full`, which includes the query and is set after span creation. The existing span processor sanitized only `url.path` in `OnStart`.
- Unknown CLI commands started the server, and commands registered without arguments ignored trailing input.
- Payments connected to NATS and included it in health checks despite having no publishers or subscribers. Compose also required the broker and stream initializer before starting payments.

## Decision

### Gateway deadline

Add `GATEWAY_PROXY_TIMEOUT`, defaulting to `25s`, and require it to be positive and strictly below `HTTP_WRITE_TIMEOUT`, whose default is `30s`. Reject an explicitly empty value when reading the process environment.

Wrap `ReverseProxy.ServeHTTP` with `context.WithTimeout` and defer cancellation. The standard transport observes this context during upstream I/O. Before final response headers, a deadline error maps to 504. After headers, the proxy aborts a stalled response body. ReverseProxy also closes upgraded connections when the request context expires.

Keep the existing connection pools and add no application retry. A response-header timeout alone cannot cover a stalled body. Custom body wrappers would duplicate cancellation and upgrade handling already provided by `net/http`.

### Proxy tracing and URL sanitization

Wrap the pooled gateway transport with `obs.WrapTransport`. Use the shared W3C propagator explicitly in transport and handler instrumentation. Production continues to use the provider installed by `obs.Setup`.

Retain early `url.path` sanitization in `OnStart`. In `OnEnd`, pass the next processor a read-only span wrapper with sanitized attributes. Sanitize `url.path` again and sanitize the decoded path in `url.full`. Remove query, fragment, userinfo, and stale `RawPath`; clear unparseable or opaque URLs. This covers client URL attributes added after span creation without changing the outgoing request.

Verify exported client spans through the production setup path, and verify parentage through the actual gateway transport. Tests must observe the client span as well as downstream trace continuity.

### CLI dispatch

An empty argument list starts the server. Unknown command names return a nonzero status and an error. `WithoutArgs` and `ExitStatus` reject trailing arguments before invoking their callbacks. `WithArgs` forwards the tail unchanged. The gateway adopts the shared dispatcher used by the five services.

### Payments dependencies

Remove the payments runtime NATS connection and health probe. Both `/healthz` and `/readyz` check PostgreSQL. Remove the supplied `NATS_URL` and override Compose dependencies to retain only healthy PostgreSQL and the completed payments migration.

Retain the broker's deny-all payments principal and its negative ACL tests for compatibility. ADR-072 records this distinction.

## Consequences

- Stalled upstream reads are canceled at the proxy deadline. This does not guarantee delivery of a 504 to a disconnected or blocked client, or limit the number of concurrent requests.
- Streams and upgraded connections exceeding the deadline close. Longer-lived streams require coordinated proxy and server timeout settings.
- Gateway requests create client spans and propagate their context downstream. URL attributes pass through export-time sanitization; this is not a guarantee about arbitrary application-defined span attributes or events.
- CLI typos fail before starting a server or running a command.
- Payments startup and database health no longer depend on NATS. Charges still require the PSP, and the wider checkout workflow still uses NATS.

## References

- [Acceptance specification](../product/tkt-336-runtime-bounds-and-service-dependencies.md)
- [ADR-002: Services from day one](ADR-002-services-from-day-one.md)
- [ADR-007: PostgreSQL and NATS](ADR-007-postgres-nats.md)
- [ADR-012: Ticket issuance and QR credentials](ADR-012-ticket-issuance-and-qr-credentials.md)
- [ADR-022: Out-of-band service migrations](ADR-022-out-of-band-service-migrations.md)
- [ADR-072: NATS publisher ACLs](ADR-072-nats-publisher-acls.md)
