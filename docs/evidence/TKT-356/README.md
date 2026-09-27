# TKT-356 rotating barcode evidence

Date: 2026-09-27

## Finding

The current QR credential is a signed v1 payload without `exp`; the 10-minute QR image link is a separate capability. Its verifier accepts the expiry second itself. A rotating signed credential could let a disconnected scanner verify authenticity and a bounded time window using a public key. It cannot establish that the device clock is honest. A slow clock within the assumed five-second bound still accepts the old screenshot until true time `T+40s` in this model.

I recommend that the owner consider Access-signed v2 credentials with short validity windows for tickets explicitly issued in rotating mode. This remains conditional on a separate clock-trust decision and end-to-end implementation. A server-minted opaque code needs an online lookup or a preloaded code map; without that map, an offline gate cannot resolve it. A shared-secret verifier can mint codes too, so it gives scanners more authority than public-key verification.

The spike accepts no rotating-credential policy. ADR-066 already records the owner's separate revocation policy: refuse admission after a scanner's revocation feed exceeds its staleness ceiling, with a recorded operator override. The ceiling and its implementation remain open. The ADR keeps rotating credential, mode, migration, bundle, PDF, clock, expiry response, and reconciliation choices open.

## Source evidence

This section records the current code read for this spike. It is not a report that the listed repository tests or integration paths were run.

| Area | Read in this run | Current behavior observed |
|---|---|---|
| Credential issue and verification | [`token.go`](../../../services/access/internal/ticket/token.go), [`consumer.go`](../../../services/access/internal/consumer/consumer.go) (`issue`, `switchEntitlement`), and `token_test.go` | Both issuance paths sign v1 identity claims. `Verify` validates signature and required identities; it has no expiry check. |
| Bundle and image | [`server.go`](../../../services/access/internal/api/server.go) (`tickets`, `qr`), [`qrlink.go`](../../../services/access/internal/api/qrlink.go), [`postgres.go`](../../../services/access/internal/store/postgres.go) (`Tickets`, `TicketForQR`), [`ticket-api.ts`](../../../web/storefront/src/lib/ticket-api.ts), OpenAPI ticket schemas, and `qrlink_test.go` | Bundle returns stored `qr_payload` and a fresh `qr_url`. The signed image-link predicate accepts `now.Unix() == expiry`. Expiring the URL does not change the stored payload. The storefront requires `qr_payload`. |
| Online scan and reconciliation | [`server.go`](../../../services/access/internal/api/server.go) (`scan`, `reconcile`), [`reconcile.go`](../../../services/access/internal/store/reconcile.go), [`scan.go`](../../../services/access/internal/store/scan.go), `occurrence_smoke_test.go`, and OpenAPI | Both server handlers verify v1 credentials and organizer scope. Reconciliation records admissions. It flags absolute device/server time difference only when it is greater than 24 hours; it does not reject an admission for that flag. Current local refusal value is `revocation_refused`. |
| Scanner | [`App.tsx`](../../../web/scanner/src/App.tsx), [`occurrences.ts`](../../../web/scanner/src/occurrences.ts), [`revocation.ts`](../../../web/scanner/src/revocation.ts), and [`access-contract.ts`](../../../web/scanner/src/access-contract.ts) | Scanner persists occurrence IDs and failed requests. It decodes ticket ID only to refuse a locally revoked ticket. It does not verify the ticket signature or expiry offline. Offline admission follows venue policy. |
| Decisions | [ADR-012](../../../docs/adr/ADR-012-ticket-issuance-and-qr-credentials.md), [ADR-021](../../../docs/adr/ADR-021-ticket-lifecycle-trail-integrity.md), [ADR-025](../../../docs/adr/ADR-025-admission-events-and-offline-reconciliation.md), and [ADR-066](../../../docs/adr/ADR-066-scanner-voided-ticket-feed.md) | Offline occurrence time is claimed, not attested. Reconciliation records physical admission rather than retroactively denying it. ADR-066 accepts refusal past a revocation staleness ceiling and a recorded operator override. The exact ceiling and implementation remain open. |

The ticket-named compatibility tests remain unchanged. No repository test suite ran as part of this evidence packet.

## Standalone model

[`probe.go`](probe.go) uses only the Go standard library. It signs synthetic candidate v2 payloads with Ed25519 and tests an opaque-code lookup. It copies the current image-link and reconciliation boundary predicates for comparison. Its values are model inputs, not selected product settings:

- `T = 2026-09-27T12:00:00Z`.
- Old candidate window: `[T, T+30s)`. Fresh candidate window: `[T+30s, T+60s)`.
- Online model uses server time and `[nbf, exp)`. Offline model uses device time and `[nbf-5s, exp+5s)`.
- Seed, ticket IDs, key ID, code, and occurrence IDs are synthetic and probe-only.

The probe asserts all exact online and offline window endpoints and adjacent seconds, the old screenshot under device offsets `-6, -5, 0, +5, +6s`, and the final old-code expiry point at true `T+40s` with a device five seconds slow. It checks a fresh credential under the same five offsets at true `T+35s`; all five device times fall within `[T+25s, T+65s)`. It also checks unknown and rolled-back clock evidence, signature tampering, unknown key, the image link's Unix-second boundary (including `T+600.5s`), and both signs of the 24-hour reconciliation flag. The model separately signs a synthetic v1 identity containing no expiry and verifies it after the image URL expires at `T+601s`. This isolates the static-payload distinction. It does not run the production QR path or establish production behavior.

The occurrence model derives the local decision and gate-open flag from signature verification, candidate time checks, and an explicit clock-evidence input. The clock evidence is toy input; it does not attest real time. A valid case opens the model gate so an always-refuse implementation fails. An expired credential, missing clock evidence, and rolled-back clock evidence each keep the gate closed. Their decisions persist, sync as refusals, and replay as refusals with the same occurrence IDs. An early signed credential at `T-6s` receives the separate `not_yet_valid` reason, syncs as a refusal, and replays as a refusal. Equality at the offline lower bound `T-5s` remains admissible. The model recognizes only `expired_credential`, `unknown_clock`, and `not_yet_valid` as local refusal reasons; an arbitrary decision is rejected before any ledger state changes. Retention and reconciliation of these refusal reasons are proposed behavior in this model, not shipped behavior. The model also asserts that a genuine `T+29s` admission remains recorded when received at `T+120s`; the same occurrence replays; and a distinct second-gate occurrence conflicts without adding another admission. The ledger's admission and refusal counters are model bookkeeping for this sequence, not factual counts from the current production scanner or server.

The task-local mutations remove the offline upper-bound check, force a refusal evaluation to open the gate, route an unknown decision through the admission path, remove the local clock-trust check, and map the early-credential reason to expiry. Each mutation must fail at its named assertion. Six subsequent restored runs must pass with identical output. The validation runner, captured stdout, commands, and results are recorded in [`validate.py`](validate.py), [`probe.stdout`](probe.stdout), and [`results.txt`](results.txt).

## Limits and future obligations

The probe is a standalone candidate model. It does not prove scanner v2 support, real clock attestation, key distribution, mode enforcement, refusal routing, or production behavior. The time-evidence input is not evidence that a device clock is honest. Six repeats establish only deterministic behavior for this fixed model. No production key was read or used.

The bounded screenshot statement assumes an honest scanner whose time error stays within the chosen five seconds. It does not prevent live relay, replay during a valid window, fresh-code retrieval by a bundle-capability holder, disconnected double admission, scanner compromise, signer compromise, or database rollback. A signed occurrence does not prove its claimed time.

Implementation must still define and test: v2 issuance and all online/offline verifiers; public-key distribution and rotation; trustworthy time evidence and rollback behavior; immutable issued-ticket mode; the full bundle and image flow; static PDF policy; expiry and early-credential reasons in scan and reconciliation; durable local refusal storage and replay; server refusal recording; unknown local decisions refusing without ledger mutation; contract/schema version transition; and exact gate actuation rules. The current bundle requires `qr_payload`; removing it requires a contract change. The current queue and reconciliation API accept only `revocation_refused`; `expired_credential`, `unknown_clock`, and `not_yet_valid` are not supported today. Proposed clock-refusal retention and reconciliation are not shipped. The claimed scan timestamp alone does not prove timely admission. ADR-066's revocation fail-closed policy and recorded override are accepted separately; this spike does not choose its ceiling or implement it.
