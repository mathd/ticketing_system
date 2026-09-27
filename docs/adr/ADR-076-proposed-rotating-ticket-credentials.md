# ADR-076: Proposed rotating ticket credentials and offline clock trust

Date: 2026-09-27

## Status

Proposed. This ADR does not accept a rotating-credential policy. D1 through D7 and D11 through D12 remain owner decisions, subject to the previously accepted revocation policy in ADR-066.

## Context

Access issues signed v1 QR payloads with ticket identity claims and no expiry check. Bundle retrieval returns the stored payload. The QR image URL expires after ten minutes, but its expiry does not change the credential in the image. The scanner does not verify credentials offline. It can refuse a ticket found in its local revocation list, then reconcile persisted occurrences after reconnect.

ADR-025 treats device occurrence time as a claim, not attested time. Reconciliation records offline admissions and flags device/server time differences only outside 24 hours. ADR-066 accepts refusal when a scanner's revocation feed exceeds a staleness ceiling, with a recorded operator override. The exact ceiling and implementation remain open. Neither decision establishes trusted time for a rotating credential.

TKT-356's standalone model shows the main trade-off. A signed time-bounded credential can be checked offline with a public key, but a slow device clock extends acceptance of an old screenshot. An opaque server-minted code needs an online lookup or a preloaded mapping. The model and its fixed values are in [TKT-356 evidence](../evidence/TKT-356/README.md). They are not integration evidence.

## Options

### Keep static signed payloads

This preserves current issuance, bundle, and scan behavior. An old screenshot remains valid as a credential because v1 has no expiry.

### Use server-minted opaque short codes

The service can bind a code to a ticket and expiry. A disconnected scanner cannot resolve a code unless it has a preloaded mapping. That adds distribution, retention, and refresh rules.

### Use Access-signed time-bounded credentials

The scanner can verify a signature offline with a public key and evaluate a bounded time window. Clock trust and revocation freshness remain separate problems. A verifier holding an HMAC secret can also mint codes.

## Proposed direction

Consider Access-signed v2 credentials for tickets issued in rotating mode, subject to owner decisions D1 through D7 and D11 through D12, and implementation evidence at the real scanner and reconciliation boundaries. Do not treat this proposal as a policy decision or as proof that a five-second device-clock bound can be enforced.

### D1. Credential and issuance

Owner-open. Candidate v2 fields are version, existing ticket/order/organizer/slot identities, `iat`, `nbf`, `exp`, and explicit key ID. Keep signing private keys in Access. Decide whether wallets refresh online or receive a bounded prefetched set. Public-key distribution, overlap during rotation, retirement, and unknown-key behavior need an implementation contract.

### D2. Mode

Owner-open. Choose organizer, event/slot, or ticket-type scope. Prefer recording the effective mode on each ticket at issuance. The browser must not choose it. A later scope change should affect new issuance only. If the owner requires existing tickets to rotate, invalidate and reissue them. Do not accept both their static v1 and rotating credential as valid. These are recommendations, not accepted policy.

Trace the choice through issuance, wallet refresh, bundle, image endpoint, online verification, and offline mode/key evidence. A rotating ticket must not pass because its v1 signature is valid. V1 tickets kept static remain outside the rotating screenshot promise.

### D3. Existing tickets

Owner-open. Prefer grandfathering issued v1 tickets as static. The alternative is explicit invalidation and reissue. A rotating ticket must not retain a valid static v1 bypass.

### D4. Bundle and PDF

Owner-open. Prefer no enduring static `qr_payload` and no static admission PDF for a rotating ticket. Decide whether its bundle returns a current expiring payload or a refresh reference. Static mode can keep static PDF delivery. The current bundle schema requires `qr_payload`; omitting it requires a contract and storefront change. The current QR image route reads the stored payload, so rotation also needs an explicit image-generation or refresh path.

### D5. Offline trust

Owner-open for rotating-credential clock trust. Decide tolerated skew, age and source of clock evidence, rollback handling, and whether clock uncertainty has an operator override. Prefer refusing automatic rotating-code admission when clock trust is missing or rolled back. The candidate model proposes retaining `unknown_clock` with the local occurrence ID, syncing it as a refusal, and replaying it as the same refusal. This retention and reconciliation behavior is proposed, not shipped. Enrollment does not attest time, and the current 24-hour reconciliation flag does not attest time. Separately, ADR-066 already accepts refusal past the scanner's revocation staleness ceiling and a recorded operator override for that refusal. Its exact ceiling and implementation remain open.

### D6. Expiry response

Owner-open. Proposed online response is HTTP 422 with `decision: "rejected"` and `reason: "expired_credential"`, after signature and organizer checks. A credential presented before its offline lower bound is not expired. The candidate model proposes the separate `not_yet_valid` reason for that case. The reason and response mapping remain owner-open. Decide how retries for an existing occurrence return its recorded result without opening the gate again.

### D7. Offline reconciliation

Owner-open. Preserve recorded physical admissions and occurrence idempotency. Assess credential expiry separately from reconciliation outcomes such as `recorded`, `conflict`, and `synced`. Local refusals must never become admissions. Candidate model choice D11 recognizes only `expired_credential`, `unknown_clock`, and `not_yet_valid` as persistable local refusals. It proposes syncing and replaying each refusal with the same occurrence ID. An arbitrary unknown decision remains rejected before ledger mutation. This refusal retention and reconciliation behavior is proposed, not shipped. The current API supports none of these three reasons. A future change must version the contract and update queue persistence, refusal storage, server handling, and unknown-value behavior.

The model derives local decision and gate state from signature verification, the candidate time rule, and synthetic clock-evidence input. It includes a positive valid case, expired, unknown-clock, and early-credential refusals that persist and replay through sync, and an arbitrary unknown decision rejected before ledger mutation. Equality at the offline lower bound is admitted. The clock input does not attest real time. The sequence also records a delayed genuine admission even though the credential has expired by receipt. A repeated occurrence remains a replay. A distinct second-gate occurrence conflicts. The ledger counters are model bookkeeping, not counts of current production admissions or refusals. These are candidate assertions, not current scanner or server behavior.

### D11. Persisted local refusal reasons

Owner-open. The model proposes persisting and syncing `expired_credential`, `unknown_clock`, and `not_yet_valid` only as refusals with same-ID replay. Reject arbitrary unknown decisions before any state mutation. This is a candidate contract choice, not shipped behavior.

### D12. Credential presented before its validity window

Owner-open. The model proposes `not_yet_valid` as a separate refusal reason. Do not report an early credential as expired. Keep the offline lower-bound equality case admissible. The reason, client mapping, and reconciliation contract need owner acceptance and implementation evidence.

## Consequences if accepted

- Access needs v2 issuance, key lifecycle rules, and separate verification for online scans and offline scanners.
- The scanner needs trusted clock evidence, rollback handling, expiry checks, and explicit gate behavior when evidence is missing.
- Mode and legacy migration affect issuance, bundle, image, online scan, offline verification, and PDF delivery.
- Expired, early, and clock-trust refusals need durable local occurrence handling and a versioned reconciliation contract if D5, D11, and D12 are accepted.
- Reconciliation must preserve a reported physical admission even when the code is expired at receipt, while reporting credential assessment separately.
- The proposed five-second window is only a model input. It is not a security guarantee until the system can enforce and test the clock bound.

## Security scope

The bounded claim under consideration concerns a holder of an old screenshot and an honest scanner whose clock error stays within the selected bound. It does not prevent live relay, replay during a valid window, retrieval of a fresh code by someone who can access the bundle, disconnected double admission, scanner compromise, signing-key compromise, or database rollback. A signed occurrence does not prove its claimed time.

## References

- [TKT-356 evidence and probe](../evidence/TKT-356/README.md)
- [ADR-012: Ticket issuance and QR credentials](ADR-012-ticket-issuance-and-qr-credentials.md)
- [ADR-021: Ticket lifecycle trail integrity](ADR-021-ticket-lifecycle-trail-integrity.md)
- [ADR-025: Admission events and offline reconciliation](ADR-025-admission-events-and-offline-reconciliation.md)
- [ADR-066: Scanner voided-ticket feed](ADR-066-scanner-voided-ticket-feed.md)
