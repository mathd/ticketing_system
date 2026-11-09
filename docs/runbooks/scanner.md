# Scanner operations and outcomes

Local Compose procedures. Run commands from the repository root. Configuration comes from
[configuration](../configuration.md); safety and approval rules remain in [AGENTS.md](../../AGENTS.md).
Load only the section needed for the operation. Historical examples retain their ticket IDs.

## Scanner device enrolment (ai-review S1)

`POST /api/access/scans` and `/scans/reconciliations` admit only **enrolled gate
devices**. Each device holds its own token; there is no shared scanner secret, because
the scanner is a static SPA and anything baked into it ships to every phone that loads
`/scanner/`.

```
access enrol-scanner <organizer-id> "north gate"   # prints the token ONCE
access list-scanners <organizer-id>                # live + revoked, with last-seen
access revoke-scanner <device-id>                  # the answer to a lost phone
```

Run them in the access container: `docker compose exec access /app enrol-scanner …`.

The token is printed once and stored **hashed** (SHA-256), so it cannot be read back
out of the database. An operator who loses it revokes the device and enrols a new one —
which is the correct workflow anyway, and the reason the credential is per device.

**Pairing.** Open `/scanner/` on the device; an unpaired scanner shows a pairing screen
instead of the scan form. Paste the token. It is kept in that browser's `localStorage`
and survives reloads.

**Revocation list.** A paired scanner pulls the organizer's voided-ticket feed when the scan
screen opens, after pairing, every 15 minutes, and when the device comes online. The operator can
also select **Refresh revocation list**. The scanner checks the local list before every scan. A
listed ticket shows **Not valid for entry** and must not be admitted. The scanner queues that refusal
and syncs it when possible. If the current token has no completed pull, the screen says
**This device holds no revocation list yet** and the existing venue offline policy still applies.
TKT-500 owns the freshness ceiling and override.

**What an operator sees when it goes wrong.** A current revoked or unpaired device gets `401`
and the app returns to the pairing screen with the reason, never the ticket-rejection screen.
A late `401` for a token this tab no longer uses is ignored. The person at the turnstile may have
a perfectly good ticket, and "Rejected" would be the wrong instruction. Offline scans already
queued on the device are **kept**, not discarded, and sync once it is paired again.

**What this does and does not constrain** (ADR-021 — name the adversary). It stops a
caller who does not hold an enrolled device's token from admitting, redeeming or
rewriting scan history. It constrains nobody with write access to the access database,
who can enrol their own device.

### Scanner Entry outcomes and local records

The Entry outcome core in TKT-558 implements the local outcome model in
[ADR-085](../adr/ADR-085-scanner-local-outcome-model.md). Each scan keeps its first local decision.
A failed outcome write does not give the scanner permission to choose another decision.

| Live result | Operator action and stored result |
|---|---|
| Accepted, after the local reservation commits | `Entry accepted. Admit.` The reservation does not prove physical passage. A replay can use this instruction only when this call commits a new local reservation. |
| Already processed on this device | `This scan was already handled on this device. No second entry.` This requires a replay and an existing reservation owned by this page. |
| Access accepted, but the local reservation cannot be confirmed | `Access recorded this scan; the scanner did not open; get an operator.` A missing or foreign record cannot prove that this device already handled the scan. |
| `401` | `This attempt was not authorized. Do not admit from this result.` Pair the device and scan the ticket again. Older queued facts remain available for authenticated sync. |
| `409` or `422` with a readable rejection | Rejected, with the exact raw reason and the instruction below. The result stays terminal even if storage fails. Only `already_redeemed` displays a prior time, and only when Access supplies it. |
| `409` or `422` with an unreadable body | `Access refused this scan; reason unavailable. Do not admit`. It never becomes an offline admission. |
| Invalid accepted `200`, an unexpected status, or an opaque redirect | `Scan response unconfirmed. Scanner did not open. Access may have recorded this scan. Get an operator; do not admit from this result.` |
| `500`, `501`, or `505` through `599` | `Outcome unknown. Do not admit. Contact an operator.` Access can have committed before the error. |
| Exactly `502`, `503`, or `504` | Offline decision, with `Admit per venue offline policy.` This applies on every engine. The supported topology is Docker Compose with the gateway as the only hop before Access. |
| The selected native network rejection | Offline decision, with `No response was received.` and `Admit per venue offline policy.` A lost response does not prove that Access decided nothing. |
| A local setup error or an unrecognized network error | `Local scanner failure. Scanner did not open. Do not admit from this result; get an operator.` |
| A ticket on this device's revocation list | `This ticket is on the device's revocation list. Do not admit.` A typed refusal can sync as a refusal only. It carries no admission event type. |

Host desktop Chrome 155.0.8059.79 produced a native `TypeError`, whose constructor is `TypeError`
and whose message is exactly `Failed to fetch`, on offline, route-abort and complete-POST socket-loss
checks. Only that exact native promise-rejection shape selects the network fallback. There is no
browser-family, user-agent or version gate. Android Chrome, other engines and later versions have
not been tested. They select the fallback only on that same shape; other shapes need an operator.
Expected, not verified on iOS or Firefox: a network-loss scan needs an operator. Re-run the native
browser checks after a Chrome update. The owner selects venue devices.

#### Entry rejection instructions

Every readable Entry refusal has the Rejected heading and rejected styling. It ends with
`Reason: <raw reason>.` The scanner preserves the reason's spelling, case and whitespace and
renders it as text. The same readable body has the same presentation at `409` and `422`.

TKT-552 supplied the two pass instructions. TKT-565 adds the remaining instructions:

| Reason | Lines before the final Reason line |
|---|---|
| `already_redeemed` | `Already redeemed.` If Access supplies `original_scan_at`, this becomes `Already redeemed at <time>.` using the existing local date/time display. The next line is `Do not admit. Get an operator.` |
| `exit_required` | `Do not admit. Access records this pass as inside. Confirm that the holder has left, then record Exit before a new Entry scan.` |
| `entry_limit_reached` | `Do not admit. No entries remain on this pass.` |
| `invalid_credential` | `Do not admit. Access did not accept this credential. Get an operator.` |
| `refunded` | `Do not admit. This ticket was voided by a refund. Get an operator.` |
| `exchanged` | `Do not admit. This ticket was replaced by an exchange. Ask for the replacement ticket and scan it.` |
| `integrity_quarantined` | `Do not admit. Access has blocked another admission after an earlier integrity exception. Get an operator.` |
| `integrity_operator_controlled` | `Do not admit. Access refused this ticket under operator-controlled integrity handling. Get an operator.` |
| `occurrence_required` | `Do not admit. This scan is missing its required occurrence ID. Get an operator to check the scanner.` |
| `invalid_occurrence` | `Do not admit. This scan has invalid occurrence data. Get an operator to check the scanner.` |
| `occurrence_collision` | `Do not admit. This scan's occurrence ID conflicts with an existing record. Get an operator.` |
| `not_inside`, `exit_not_applicable`, `exit_unverified` received for Entry | `Do not admit. Access returned an Exit refusal for this Entry scan. Get an operator.` |
| Any other readable reason | `Do not admit. Access refused this scan. Get an operator.` |

Unknown strings remain denials with their exact Reason line. This includes `revoked`, for which
no current live scan producer was verified. It does not mean the device found a ticket on its
local revocation list. That separate local refusal keeps the wording and refusal-only sync behavior
shown above. An unexpected Exit reason in an Entry reply does not switch direction, record Exit,
start another scan or authorize admission. Tests that inject unusual reason/status combinations
check client compatibility; they do not establish that Access emits those combinations.

Only `already_redeemed` uses the supplied Access time. No device date, occurrence time or stored
record time replaces a missing value, and no new timestamp field is stored. A malformed supplied
time still makes the denial unreadable. The `invalid_credential` reason does not establish that
signature verification ran: Access can also use it for request validation failures. The integrity
instructions describe Access's refusal. They add no integrity or containment guarantee.

Confirming that the holder has left is staff judgement, not a system check. Access cannot tell a missing Exit from a passback. If staff accept a claim of departure, record Exit, and then scan Entry, the presenter can be admitted. The `exit_required` instruction adds no guard and does not establish physical presence. This risk exists without the instruction too.

After Exit is recorded, the Scan direction stays on Exit. Select Entry before the next scan. Otherwise the next scan runs as an Exit. It can record another Exit or show a neutral may-leave result, and it never gives an Entry decision. Pairing, replacing or clearing the device token, or a `401`, returns the selection to Entry, as described under Entry and Exit controls.

#### Storage readiness and repair

`Scanner storage is not ready. No ticket was checked.` disables pairing, scan and camera controls.
Input presented while storage is blocked is not replayed when it becomes ready. Close old scanner
tabs, then wait or use **Retry storage**. Submit the ticket again after readiness returns.

`The outcome write needs repair. Keep this page open and retry storage.` means the page still holds
the decision in memory. **Retry storage** writes that same decision. Replacing a connection does
not replace the page owner. The live page's owner lock protects pending rows even after its advisory
lease expires. A failure to repair needs an operator. Keep the page open.

An old scanner bundle cannot open the upgraded version 3 database. It fails with its storage warning
and does not send new scans or reconciliation. Existing version 3 records wait for the compatible
client. Do not roll back by clearing browser data. If an upgrade cannot read a record, expected but
not verified on a physical damaged device, the upgrade aborts and storage remains unavailable.
Get a technician. **Do not clear site data.** Deleting the local database loses all local scans,
unsynced work, held originals, recovery notices, export receipts, acknowledgement metadata and
revocation data. This describes the cost of deletion, not an operator procedure.

#### Held records, export and acknowledgement

Pre-upgrade pending records and unclassifiable records are held. The count and warning appear on
paired and unpaired pages. Held records never sync. **Export local records** requests a local JSON
download with the original fields, ticket credentials and precise claimed times. It distinguishes
an absent field from an own undefined field. The export can contain credentials. Keep the file private.
The browser cannot confirm that the file was saved.

Select the held records reviewed by the operator. **Acknowledge selected held records** becomes
available only after an export request and its local receipt commit. Acknowledgement lowers the
unresolved count, retains the original rows and never makes them eligible for sync. A failed export,
receipt or acknowledgement leaves them unresolved. Export again after an export failure.

Pre-upgrade queued records remain eligible for sync. Some can record false admissions, and their
original cause cannot be reconstructed. Their exact IDs, claimed times and current states remain in
the export after sync retirement. They do not have an acknowledgement or disposal control. An odd
record that the server always refuses stays queued and retries indefinitely. Its export remains
available even when the held count is zero. Other valid queued records can still sync.

Every compatible page shows all unacknowledged recovery notices, including after another page syncs
the record. Each notice gives the occurrence ID, claimed time, type, uncertain recovery cause and
current sync state. The prior result and physical passage are unknown. Do not admit from a recovery
notice. Acknowledge the notice after operator review. This retains the scan and does not change its
sync result.

#### Queue refresh, carried failures and sync waiting, TKT-512

The queue count and a carried sync failure read the shared queue again. This page does so on window
focus, on a transition to a visible tab, after a committed pairing and after another tab replaces or
removes the pairing token. A hidden transition does not read the queue. A storage event that repeats
the token already in use does not read it either.

A refresh repairs and recovers local rows, reads the queue once and updates the count and the
failure line. It does not start a sync or a revocation pull. It does not change "Last successful
sync". Focus and visible transitions keep the scan result on screen. A pairing commit clears the
scan result, as before.

A committed pairing removes the instruction to pair the device. The carried failure stays visible
with no count until the refresh reads the queue. The count then comes from that read. An empty read
removes the failure. A failed read leaves the count unknown. A removed failure does not return when
the device is paired later.

A local authentication failure, a 401 from this page's own sync or revocation check, does not read the
queue. A read that is already running can still finish and update the count.

If storage stops being ready while a failure count is still being read, the count becomes unknown at
once. A read that finishes later, from the storage that stopped, changes nothing. Retry storage reads
the queue on the new storage, and that read sets the count.

Waiting. When initial storage work takes longer than 1,000 ms, the sync line shows "Waiting for local
storage to finish before this sync can continue." The wait covers repair, recovery and the queue read
before the first request. It describes local storage only. It does not mean a failure, a timeout, a
saved scan, a sent scan or permission to admit. The line clears when that work finishes, when the page
is closed or unmounted, or when the storage used by that attempt stops being ready. The line is
removed from the page before the first request is sent. A pairing change does not clear it while
the initial storage work continues. Clearing the line does not cancel an atomic write that already
started or permit a second sync pass to run alongside it. Application of a batch and the refresh
after a batch do not show this line.

A failed local write of a sync result shows this message: "The last sync result could not be saved on
this device, so that batch was not applied. Scans from that batch that are still queued will be retried
on a later sync." The message does not say that Access rolled back the batch, and it does not say that
storage is still unavailable. It does not offer Retry storage.

When the shared queue refuses a batch as a whole, the page shows no message of its own. The rows keep
the state that the shared queue holds, and the refresh after the batch shows the count from that state.
A refused batch is not a storage outage. The page does not send it again on its own.

The write-result message stays through reads, pairing, readiness notifications and unrelated writes.
The next nonempty paired sync clears it when its first request is sent, even if that request fails. An
empty sync, a read that fails before the request and a sync with no pairing token do not clear it.

A pairing form error reads "Pairing could not be completed on this device. Submit the pairing token
again to retry." It appears when this page cannot compute the token fingerprint or read its local pairing record.
It does not show the storage alert or Retry storage. A committed pairing clears it. A readiness
notification alone does not clear the storage alert. An accepted queue read clears it.

Closing or unmounting the page aborts a sync request that has not finished. A local write that has
already started can still commit. The page then publishes no totals, no last-success time and no
later batch from that sync.

Verification limits. The browser journeys for this behavior run in real Chrome, in one browser
context with two tabs. Only those journeys use a second page. The App tests simulate a cross-tab
pairing in one window, with a localStorage write and a synthetic storage event. Focus uses trusted
focus with focus emulation disabled. Synthetic visibility events prove the page's listener wiring
only. Native visibility transitions are not verified. The
1,000 ms wait tests the page's own storage await. The forced contention journey holds one IndexedDB
transaction. It does not measure how often a stall happens on a venue device. These checks do not
cover suspension, bfcache, physical passage or venue performance.

#### Remaining limits

TKT-559 checks both wall and monotonic elapsed time from submission. Mint and send must finish
before 3 s; an admission decision must finish before 30 s. Equality is late. Invalid readings,
monotonic regression or more than 1000 ms disagreement stop the scan. Timers only wake the
controller; each continuation checks elapsed time even if its timer was delayed. A stalled denial
keeps the refusal with its reason unavailable. A stalled accepted body or unanswered request gives
an unconfirmed result. A late mark never opens; a mark that commits still retains ACTUATED.
An in-time offline instruction is inserted into the DOM synchronously before its outcome write.
The controls become usable while that same decision continues saving. This does not guarantee a
browser paint by a physical deadline. Two clock faults that conceal each other remain undetectable,
and physical-device sleep behavior remains unverified.

A gateway-hidden refusal cannot select the offline fallback at or after 30 s. A classified native
network rejection or a 502/503/504 before that limit can still select it with occurrence uncertainty U.
A deadline abort does not undo an Access write. A result that already appeared cannot change because
its persistence finishes late. Keep the page open and retry storage when a repair warning appears.
Old TKT-558 tabs warn on the new stopped-row tag and exclude those rows from sync. Refresh those
tabs to load the current scanner. This is separate from the version-3 barrier for older databases.

A failed terminal write followed by actual page loss can still recover as a false admission.
Recovery can record an unsent attempt. A local actuation reservation can exist without opening.
A `500` can hide an Access commit. No client result proves physical passage. The Compose-only
fallback retains uncertainty about earlier transparent attempts and has no proxy-provenance switch.
Any intermediary before the gateway needs a new ADR and qualification before deployment.

New scans use strict durability for mint, mark and final outcome transactions. The mint and mark
must commit before online acceptance; offline publication precedes its final write. A denial can
use one or two strict decision commits, including optional reason hardening. Renewal uses default
durability. The host Chrome run records 100 strict mint and 100 strict mark completions after ten
warm-up scans in [the raw measurements](../sprints/S-2/evidence/TKT-559-strict-commits.json).
Run `make browser` to reproduce. These observations do not qualify a venue device or prove power-loss
behavior. The 3 s allowance remains an owner rule. Venue use requires TKT-560 Done and the owner's
selection of the provisional pre-decision bound and acceptable offline-spill rate.

#### Entry and Exit controls, TKT-399

Select **Entry** or **Exit** in **Scan direction** before scanning. The page starts in Entry.
Pairing, replacing or clearing a device token returns it to Entry. This includes a scan, feed or
reconciliation `401`. The selection lasts only for this page. Controls are disabled while a scan
waits or storage is not ready. A mode or pairing change stops the camera and clears stale camera
feedback. A scan keeps its captured direction, credential, id and time through writes and sync.
Changing mode after an offline Exit does not turn the saved Exit into an Entry.

Exit results have a neutral status style. They permit leaving, never entry:

| Exit result | Meaning |
|---|---|
| Exit recorded | Access recorded Exit and this page completed its own local mark. The result includes the server time and any replay marker. This does not prove physical passage. |
| Already processed | This page already owns the mark for this replay. No second Exit action. The holder may leave. |
| Exit not recorded, with a reason | Access refused Exit. The exact reason is shown as text. `invalid_credential` reads `credential not accepted`. Revoked, refunded and exchanged tickets are no longer valid for entry. The holder may leave. |
| Exit not recorded, reason unavailable | A `409` or `422` body could not be read, including at its deadline. This stays terminal. The holder may leave. |
| Offline Exit | A supported no-response failure or exactly `502`/`503`/`504` selected an in-time queue decision. `Exit result unknown; the holder may leave`. A failed write needs repair and does not prove that the queue committed. |
| No request sent | Storage, mint, pre-send time, cancellation or local failure stopped the scan. `Exit not recorded; the holder may leave`. |
| Sent, answer unknown | A sent deadline, cancellation, local failure, invalid accepted body or unconfirmed status can leave an Access fact. `Exit result unknown; the holder may leave`. |
| Access recorded, scanner could not finish | A late accepted answer or failed local mark needs an operator. The holder may leave. A committed late mark stays stored; it never produces a second action. |
| Pair this device | The attempt was not authorized. Pair and scan again. The holder may leave. A retained result keeps its captured Exit direction although the selection resets to Entry. |

A locally listed revoked Exit makes no occurrence, refusal row, live request or reconciliation
request. Its result says Exit was not recorded, the ticket is no longer valid for entry, and the
holder may leave. An absent Exit can make the next Entry return `exit_required` on a requires-exit
pass. A remote refusal follows its normal terminal reason instead.

An offline Exit is initially unknown. A single-policy ticket can later receive `rejected` at sync.
That exact row retires as RESOLVED/rejected, and the page shows a separate per-row warning. It
created no Exit fact. TKT-551 adds separate aggregate counts. For example, a complete pass can say
"Synced 92 offline scan(s). 7 conflicts flagged for the operator. 6 rejected scans were not
recorded." A rejected result never increases the Synced count. One rejection alone says
"Synced 0 offline scan(s). 1 rejected scan was not recorded." With no rejection, the existing
success text is unchanged. TKT-565 adds the readable Entry reason instructions listed above.

After each applied batch, the first reconciliation alert reports completed-batch totals. For
example, "Completed batches: Synced 90 offline scan(s). 6 conflicts flagged for the operator.
4 rejected scans were not recorded." Individual warnings follow it. This progress alert can
appear during a pass that later succeeds. Full completion removes it and shows the final note.
A later transport, authentication, response or storage failure leaves earlier batch totals
visible beside the failure notice. A response whose local application fails adds no published
count. "Last successful sync" means a completed pass, so it can still say "none" beside partial
totals. After authentication or storage failure, the separate queue-count display may still
include retired rows until the next refresh. The stored queue determines what can be sent.

These reports are transient. When the next nonempty paired sync pass starts, before its first
request, it replaces the previous partial report and individual reconciliation warnings.
Reconnection or a queued local refusal can start this pass automatically. A coalesced follow-up
also does so. The new pass can fail without applying any result; the old report is still gone.
An empty queue read keeps the report. Page loss does not preserve a durable report, and there
is no acknowledgement list. In the current page, a pairing-token change keeps reconciliation
reports until the next nonempty paired pass replaces them. The live scan outcome is cleared.
The retained report describes this device's rows, not the new pairing. Counts describe results
received by this page whose application returned applied, including matching rows another page
already retired. Several pages can
therefore report the same rows. These are per-pass counts, not unique device-wide totals or
evidence of physical passage. Legacy records that cannot sync remain queued and keep their
existing warning; those warnings do not count as rejected results.

Actual page loss before a final write commits can erase a terminal decision. Its committed PENDING
row may recover as an uncertain Entry or Exit fact. The recovery notice shows the captured id,
direction, time and current sync state on compatible pages, including after retirement. It is not
a new scan result or permission to pass. Closing only a database connection does not end the page
owner and cannot authorize recovery.

The current client shows each strict-reader warning with the unreadable record key and problem.
The record stays local and will not sync. Current-format missing or corrupt direction never defaults
to Entry or falls back to legacy. Existing version-3 TKT-558 and TKT-559 Entry-only tabs silently
exclude Exit from sync and recovery. Their store warnings are not visible in those old Apps. Refresh
them to recover or sync Exit. The version-2 database barrier remains in force. Do not delete the
database to make an old client work.

Entry and Exit share the 3 s send and 30 s decision limits and the page-loss, device-clock, paint
and mark-before-opening limits above. TKT-560 remains required before venue use.

#### Access response contract (ticket 0, TKT-557)

Access answers the three scanner routes with these exact statuses and bodies. S is a live scan
(`POST /scans`), R is reconciliation (`POST /scans/reconciliations`) and F is the voided feed
(`GET /scans/voided-tickets`).

| Origin | Routes | Status and body |
|---|---|---|
| Missing, blank, unknown or revoked device token; a server with no enrolment port; an unknown auth scheme | S, R, F | `401` `{"error":"scanner device is not enrolled"}` |
| The device lookup could not answer, before any decision | S, R, F | `503` `{"error":"scanner device lookup unavailable"}` |
| Any other device lookup failure | S, R, F | `500` `{"error":"scanner device lookup failed"}` |
| The initial policy read or the transaction acquisition could not answer, before any ticket decision | S | `503` `{"error":"scanner store unavailable"}` |
| No ticket verifier configured (existing) | S, R | `503` `{"error":"scanner unavailable"}` |
| Credential mismatch or occurrence collision (existing) | S | `422` with the existing rejected reason |
| Any later scan store failure, including a statement or commit after the decision began | S | `500` `{"error":"redeem ticket"}` |
| A store failure on one reconciliation row; earlier rows can already be committed | R | `500` `{"error":"reconcile admissions"}` or `{"error":"record scanner refusal"}` |
| A panic before the response headers, on any Access operation | all | `500` `{"error":"request failed"}` |
| A response that violates the contract (response validation on) | all | `500` `{"error":"response violates OpenAPI contract"}` |

**What a `503` proves.** Every Access `503` above comes from a step that runs before any ticket
decision in **this HTTP request**, so this request decided nothing. It does not prove that an earlier
attempt for the same scan decided nothing: a browser can resend a request transparently. The
occurrence stays uncertain (ADR-085 P13).

**Bounded steps.** The device lookup, the initial policy read and the single and pass transaction
acquisitions each stop after **500 ms** and answer `503`. Then the scanner can go offline instead of
waiting. The advisory device Touch after a successful lookup also stops after **500 ms**; it never
changes the answer, and when another session holds the device row it skips the update, so
`last_seen_at` can stay stale. One live scan meets at most four bounds in series (lookup, Touch,
policy read, one acquisition), a nominal 2 s; that is not a total request limit. Both 500 ms values are **PROVISIONAL**: TKT-557 ships them without load proof, and TKT-560
qualifies them. **Venue use after TKT-558 requires TKT-560 Done and the owner's selection of the
bound and of the acceptable offline-spill rate.** Nothing enforces this order.

The cost: a healthy but saturated database, for example a full connection pool at peak, also answers
`503` after the bound. After TKT-558 that is an offline admission without the live duplicate check;
a single-entry duplicate then shows up later as `duplicate_admit` with an alarm. A timer `503` proves
that a step passed its bound, not that the database was down.

**Which database failures are `503`.** Only failures that a real driver test proved, at the step
where it proved them: a connect that fails on DNS, connection refused, a server starting up
(SQLSTATE 57P03) or a full server or role (53300), with every connect attempt in one of these
classes; a terminated backend (57P01) at any of the four steps; and a cut connection at the steps
where the gate produces it. Everything else stays `500`, which after TKT-558 stops the gate: a closed
application pool (until Access has a working pool again), wrong database credentials, an unknown SQL
or configuration error, a socket timeout that fires before the server bound, a reset or end of file
during connection startup, and 57P03 or 53300 from a statement instead of a connect. The 57P03 and
53300 classes need `sslmode=disable` in `DATABASE_URL`, which Access requires (see
`docs/configuration.md` § Access: `DATABASE_URL` must set `sslmode=disable`).

**Panic recovery and lost responses.** A panic before the response headers gives the exact `500`
above on all eight Access operations. That `500` does **not** mean that nothing happened: a handler
can panic after a store commit. A panic after the headers aborts the response and keeps the status
that was already sent. A killed process, a shutdown deadline, a network loss or an expired write can
still lose a response. Both servers refuse an `HTTP_WRITE_TIMEOUT` below 30 s; with equal values the
gateway deadline can still expire first and lose a committed answer. The recovered `500` on the other
Access operations changes no eligibility: Commerce leaves a refund's ticket-voiding obligation
outstanding on any non-`200` and retries it (`services/commerce/internal/refunds/service.go:270-275`),
Commerce answers an exchange with `502` "access admission unavailable" on any non-`200` admission read
(`services/commerce/internal/api/exchanges.go:366-369`), and the back office reports a redelivery
`500` and a transport failure alike as ambiguous (`web/backoffice/src/lib/access.ts:192`, `:213-215`).
These reads do not prove that the later retry succeeds.

**Entry client behavior after TKT-558.** A live lookup or recovered `500` is terminal and needs
an operator. A live `401` clears only its captured current pairing and never queues this attempt.
Exactly `502`/`503`/`504` can select the offline instruction. Sync keeps eligible scans on failure.
A failed feed pull keeps its list and completed-pull time.

**Supported topology.** Docker Compose, with the gateway as the only hop in front of Access. A DNS
failure counts as availability only there, where the name `postgres` is the database service. Any
intermediary in front of the gateway needs a new ADR and qualification before deployment; the scanner
has no switch for it (ADR-085). Staff can force offline mode by cutting the
network. That is a known workaround, not an availability guarantee.

**Delivery map.** The delivery tickets merge in the order 1 → 0 → 2 → 3 → 4. Ticket 0 can merge
together with ticket 2, never after it. Each ticket updates this section when it ships, and removes
only the limitations that it fixes.

| Ticket | Scope | What changes for operators |
|---|---|---|
| 1, TKT-556 (documentation only) | ADR-085, the ADR-025 §3 and ADR-066 §8 amendment notes, this section | Nothing in the scanner. |
| 0, Access and gateway, **shipped (TKT-557)** | Device lookup answers `401` (not enrolled), `503` (lookup unavailable) or `500` (lookup failed) on scans, reconciliation and the voided feed; bounded pre-decision `503` (PROVISIONAL 500 ms, TKT-560 qualifies it); panic recovery with an exact `500`; write-timeout and `sslmode=disable` startup checks | The Entry client now keeps `401` and `500` terminal. Exactly `502`/`503`/`504` can select the offline instruction. Sync keeps eligible scans and the feed keeps its list and completed-pull time on these errors. Venue use after TKT-558 waits for TKT-560 Done and the owner's bound and offline-spill selection. |
| 2, Entry outcome core, shipped in TKT-558 | Terminal `401`, `409`/`422`, `500` and unreadable answers; repair of failed writes; storage version 3 barrier; held legacy scans with a count, a warning, an export and an acknowledgement; recovery notices on every page | Re-pair and rescan after a `401`. The export file contains ticket credentials: keep it safe and do not share it. Scans queued by main before the upgrade still sync, and some of them can be false admissions; their ids are in the export. This untimed core was followed by TKT-559. Its Entry clock now prevents offline fallback at or after 30 s; an earlier classified fallback retains occurrence U. |
| 3, Entry scan time, shipped in TKT-559 | 3 s send limit and 30 s decision limit, two clocks, stopped-scan and slow-answer results | A scan stopped by the limit is never synced. A crash after a failed write can still recover the scan; the next page shows a recovery notice. |
| 4, Pass Exit mode, shipped in TKT-399 | Entry/Exit mode, Exit results with "the holder may leave", revoked-ticket Exit with no record | A revoked ticket's Exit is not recorded, so the next Entry on a requires-exit pass can answer `exit_required`. |
