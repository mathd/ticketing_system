# ADR-079: Disable persisted Astro image stale-if-error reuse

Date: 2026-10-10

## Status

Accepted as the repair requirement for TKT-504. The repair is not implemented or verified. Its high review finding remains blocking.

## Context

The reviewed TKT-504 patch has separate network-failure and 5xx fallback paths for persisted remote-image entries. After a policy change, failed writes and failed deletion can leave the old policy and image on disk. A new process has no signal in that directory that the old reuse permission was revoked.

## Possible solutions

Add a separate durable revocation mechanism, or disable stale-if-error reuse for persisted remote-image entries. Retrying deletion alone cannot establish that a later process will refuse the retained entry.

## Decision

Disable persisted-entry stale-if-error reuse in both fallback paths. Keep valid fresh reuse and successful revalidation subject to the actual cache policy. A revalidation failure must not copy retained image bytes to output. Apply this rule to both Astro frontends, including entries written before the patch. Do not add a separate revocation store in this repair.

## Consequences

A build can fail when revalidation is unavailable, even if old image bytes remain on disk. This trades fallback availability for refusal of stale bytes whose policy could not be safely updated.

Verification must cover failed matching-304 and replacement-200 invalidation, followed by a new build or process receiving 503. It must also cover network failure, old persisted entries, and both frontend dependency resolutions. The selected direction does not close the high finding, authorize merge, or establish that the rest of TKT-504 passes its checks.

## References

- [Repair requirement](../product/backlog-decisions-2026-10-10.md#persisted-image-cache)
- TKT-504 and its accepted persisted-cache review finding
