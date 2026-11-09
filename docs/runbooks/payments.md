# Payment operations and journal keys

Local Compose procedures. Run commands from the repository root. Configuration comes from
[configuration](../configuration.md); safety and approval rules remain in [AGENTS.md](../../AGENTS.md).
Load only the section needed for the operation. Historical examples retain their ticket IDs.

## Event-cancellation bulk refunds (TKT-159)

Refunds every completed, not-fully-refunded order on one slot, with a per-order outcome. Internal
and on demand — it does **not** require the slot to be in a cancelled state, and never reads catalog.

```bash
# start a run (Idempotency-Key is required; the same key replays the same run)
curl -X POST "$COMMERCE_URL/internal/slots/$SLOT_ID/cancellation-refunds" \
  -H "X-Internal-Token: $INTERNAL_SERVICE_TOKEN" -H "Idempotency-Key: cancel-$SLOT_ID-1" \
  -H 'Content-Type: application/json' \
  -d '{"organizer_id":"'"$ORGANIZER_ID"'","actor":"ops@example.test","reason":"event cancelled"}'

# read the report (202 while it is still running, with the counts; 200 when complete, with the rows)
curl "$COMMERCE_URL/internal/cancellation-refunds/$RUN_ID?organizer_id=$ORGANIZER_ID"
```

Reading the outcomes:

| Outcome | Means |
|---|---|
| `refunded` | This run returned the money **and** the tickets are void **and** the seat is back. |
| `already_refunded` | The order was already fully refunded, with every obligation discharged. No provider call was made. |
| `failed` + `reversal_outstanding` | The money is back but a reversal is not done. `money_refunded`/`tickets_voided`/`capacity_returned` say which half. A **seated** order stays here permanently (TKT-164). |
| `failed` + `no_captured_money` | A zero-price (comped) order. It has no money leg — and therefore got **no reversal at all**, so its tickets still admit. |
| `failed` + `refund_refused` / `unavailable` / `not_refundable` / `internal` | The refund did not happen. Failures are terminal **for that run**; retry by starting another run, which is safe. |

`incomplete_at_enumeration` on the report is the count of orders that existed on the slot at the run's
cutoff but were not completed, so this run could not refund them. Non-zero means a later run may owe
somebody money.

Tuning: `CANCELLATION_REFUND_INTERVAL` (default `10s`) and `CANCELLATION_REFUND_BATCH` (default `8`).
The batch bounds one claim and, through it, the lease.

Why a second run cannot double-refund, and why the refund key ignores the run:
[ADR-040](../adr/ADR-040-event-cancellation-bulk-refund-runs.md) §3.

## Journal signing key rotation

The payments money journal is signed with HMAC-SHA256 under a **keyring**: one active key that new
entries are signed with, plus retired keys retained so their era stays verifiable (ADR-016 §Decision
8, ADR-032 §Keyring configuration, rotation and retirement).

```
JOURNAL_KEY_ID           active key id, e.g. local-v2
JOURNAL_SIGNING_KEY      active secret, >=16 bytes, raw (not base64)
JOURNAL_HISTORICAL_KEYS  retired keys: kid=<secret>,kid=<secret> — secrets base64.RawStdEncoding
```

**To rotate**, in one deployment (never two):

1. Encode the **outgoing** secret: `printf %s "$OLD_SECRET" | base64 | tr -d '=\n'`
   (unpadded standard alphabet — `base64.RawStdEncoding`; a padded value is rejected. Keep the
   `\n` in `tr`: GNU `base64` wraps at 76 columns, so a secret over 57 bytes otherwise carries an
   embedded newline. `openssl base64 -A` avoids the wrapping entirely.)
2. Set `JOURNAL_HISTORICAL_KEYS` to `"<old-kid>=<that value>"`, appending to any existing list.
3. Set `JOURNAL_KEY_ID`/`JOURNAL_SIGNING_KEY` to the new key. **`JOURNAL_SIGNING_KEY` is RAW — do
   not base64 it.** The asymmetry is deliberate (historical keys share one delimited variable and
   need an encoding; the active key does not), and it is the one step here that fails *silently*:
   a base64 blob is over 16 bytes and passes every check, so the service boots and signs real money
   facts under a key nobody wrote down. **Startup now rejects this outright when the pasted value is
   the base64 of a key in `JOURNAL_HISTORICAL_KEYS` (below) — it is no longer silent.**
4. Deploy, then run `verify-journal` — it must pass over the now mixed-key chain.

Steps 2 and 3 must land in the **same** deployment: a new active key without the old one in the
ring makes every pre-rotation entry fail verification.

**Startup rejects the classic mis-paste.** If `JOURNAL_SIGNING_KEY` **decodes** to a key listed in
`JOURNAL_HISTORICAL_KEYS` — i.e. you pasted step 1's output into step 3 — payments refuses to start
and names the key it matched. That is the one rotation error the steps above can actually produce,
and it used to be silent.

It compares *decoded bytes*, so it does not matter which encoding your tooling produced: the step-1
pipeline above strips padding, a bare `base64` keeps it, and both are caught.

Three honest bounds — the guarantee is exactly this and no wider:

- **Standard base64 only**, padded or unpadded. ADR-032 fixes the keyring's encoding as standard
  base64, so a URL-safe (`-_`) paste is a different mistake and is deliberately outside this
  guarantee rather than half-covered.
- It catches a key that decodes to a secret **in the ring**. Base64 of some other key, or any other
  wrong-but-plausible secret, is not detectable — nothing in this package can make that detectable.
- It cannot tell the mistake from intent. A raw key that happens to decode to a ring member is
  rejected too. That needs a key both arbitrary and a valid encoding of a secret already in the
  ring; the error names what to change, and there is deliberately no override — an escape hatch on
  a fail-closed check is worth less than the case it would serve.
- It is not a security control. This ring is secret material and every holder can forge under every
  kid in it (ADR-021 §the trust boundary). It catches an honest operator's paste error.

Payments also logs the active key id at startup and before `verify-journal`:

```json
{"msg":"journal signing key loaded","journal_key_id":"local-v2"}
```

The key **id** only, never anything derived from the key. An earlier draft of this feature logged a
truncated HMAC of the key as a "fingerprint" so operators could compare it; that was withdrawn
because a deterministic tag over a fixed public message is an **offline oracle** for guessing a
symmetric secret — anyone who can read logs could test candidate keys — and nothing here requires
`JOURNAL_SIGNING_KEY` to be high-entropy. Do not reintroduce a key-derived value into logs or
metrics. (The readable development default this once warned about is gone: ai-review S5 removed
every checked-in signing-key default, `make up` generates one per clone, and the binary refuses
the retired literal forever.)

**To retire** a key, drop its entry from `JOURNAL_HISTORICAL_KEYS` — but only once **no retained
entry references it**, including backups and archives expected to stay auditable. Retiring a key
that is still referenced deliberately makes that era permanently unverifiable; `verify-journal`
then fails naming the unknown key id. Nothing prevents this at startup (validating the ring's
structure is not scanning the journal's contents), and nothing runs `verify-journal` for you in a
deployed environment — so run it after any retirement.

**What the ring is, said plainly.** These are **secrets**, not public keys. This is the opposite of
the access lifecycle keyring (§Access ticket lifecycle trail operations), where a verifier holds
only public keys and genuinely cannot rewrite history. Here **anyone holding the ring can forge
under every key id in it**. Rotation buys one thing: retiring a key no longer invalidates its
history. It does not produce a verifier that lacks signing power, and `verify-journal` must never be
described to an auditor as if it did. Modification and insertion by a database writer who holds no
key are evident; targeted rollback and current-key compromise are not, and remain TKT-11's.
