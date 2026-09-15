# TKT-336: Runtime bounds and service dependencies acceptance specification

Date: 2026-09-14

## Context

The validated findings R3, R4, R5, and R7 concern CLI dispatch, gateway proxy bounds, proxy trace propagation, and payments broker dependencies. This specification defines the acceptance criteria and boundaries for all four fixes.

## Acceptance criteria

### 1. Strict CLI dispatch (R3)

- `cmdline.Dispatch(args, registry, serve)`:
  - An empty `args` slice invokes `serve` and returns its result with an empty command name.
  - An unknown command name (`args[0]` not in `registry`) returns a nonzero exit status and a descriptive usage error without invoking `serve` or any callback.
  - Commands registered with `WithoutArgs` or `ExitStatus` reject any trailing arguments (`len(args) > 1`) before invoking their callbacks, returning a nonzero exit status and usage error.
  - Commands registered with `WithArgs` receive their tail (`args[1:]`) byte-for-byte unchanged.
- Gateway adoption:
  - `gateway/cmd/gateway/main.go` registers `healthcheck` via `cmdline.Registry` and dispatches CLI arguments through `cmdline.Dispatch`.
- Service tests:
  - All five service CLI dispatch test suites (`access`, `catalog`, `commerce`, `inventory`, `payments`) pass tails only to `WithArgs` commands and verify that `WithArgs` forwarding occurs without regressions.

### 2. Bounded gateway proxying (R4)

- Configuration:
  - `shared/go/runtimecfg` provides `GatewayProxyTimeoutFromEnv(writeTimeout time.Duration) (time.Duration, error)`.
  - Default value is 25s, strictly below the server's default `HTTP_WRITE_TIMEOUT` of 30s.
  - Rejects non-positive durations, empty strings, and values greater than or equal to `HTTP_WRITE_TIMEOUT`.
  - Added to `compose.yaml` gateway service environment: `GATEWAY_PROXY_TIMEOUT: ${GATEWAY_PROXY_TIMEOUT:-25s}`.
- Proxy execution:
  - Upstream request execution is bounded by a handler-level `context.WithTimeout` using the configured proxy timeout with deferred cancellation, limiting the lifetime of stalled upstream I/O. This bounds the duration a hung upstream can occupy gateway proxy goroutines and pooled connections, without making the edge server immune to socket exhaustion under high concurrency.
  - Pre-header stalls: if upstream headers are not received before the deadline, `ReverseProxy.ErrorHandler` returns HTTP 504 Gateway Timeout.
  - Post-header stalls: once headers have been sent to the client, a body timeout aborts and truncates the response stream rather than attempting an impossible 504.
  - Connection reuse: retains standard idle connection pool ceilings (`MaxIdleConns: 500`, `MaxIdleConnsPerHost: 100`).
  - No application-level retries or request-body replays on write methods.
  - Upgrades (HTTP 101 Switching Protocols): bidirectional byte streaming is supported, and the connection is terminated upon context expiration.

### 3. Proxy trace propagation and export-time redaction (R5)

- Transport observability:
  - `shared/go/obs` exposes `WrapTransport(base http.RoundTripper) http.RoundTripper` using package-level W3C propagators.
  - `shared/go/obs` exposes `WrapTransportWithTracerProvider(base http.RoundTripper, tp trace.TracerProvider) http.RoundTripper` for isolated test environments.
  - `obs.Middleware` and `obs.MiddlewareWithTracerProvider` use explicit propagators.
  - Gateway's pooled upstream transport is wrapped with `obs.WrapTransport`.
- Export-time URL sanitization:
  - `obs.CapabilitySpanProcessor` implements `OnEnd(s sdktrace.ReadOnlySpan)` via a `ReadOnlySpan` wrapper overriding `Attributes()`.
  - `url.path` attributes are sanitized with `obs.SanitizedPath`.
  - `url.full` attributes have query parameters, fragments, and userinfo removed, the decoded path sanitized via `SanitizedPath`, stale `RawPath` cleared, and are rendered via `u.String()`.
  - Unparseable or opaque URLs (e.g. `scheme:opaque_payload`) fail closed by clearing the value.
  - Client spans exported through the production provider path contain no raw capability references or query secrets.

### 4. Payments broker decoupling (R7)

- Process independence:
  - `services/payments/cmd/payments/main.go` removes direct NATS imports, connection logic, and broker healthcheck probes.
  - Health endpoints `/healthz` and `/readyz` probe only the PostgreSQL database. Database failure yields HTTP 503 with only `"db"` in the `checks` object. This decouples payments process boot and health monitoring from NATS; it does not eliminate dependencies on the external PSP for charges, nor does it imply the end-to-end checkout flow functions without NATS.
- Deployment dependencies:
  - `compose.yaml` removes `NATS_URL` from the payments service environment.
  - `compose.yaml` overrides `payments.depends_on` to depend exclusively on `postgres` (service_healthy) and `payments-migrate` (service_completed_successfully), removing dependencies on `nats` and `nats-init`.
  - `ADR-072` is amended to clarify that payments NATS ACL definitions and `smoke/nats_acl_test.go` negative tests exist for historical compatibility and negative verification, not runtime operational dependency.
  - Unused direct NATS dependency in `services/payments/go.mod` is removed.
