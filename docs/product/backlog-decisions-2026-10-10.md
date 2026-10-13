# Backlog decisions, 2026-10-10

The owner authorized this pass to resolve ticket questions and make decisions. These are planning requirements for future tickets, not claims about shipped behavior. No implementation, merge, dependency override, or priority change is authorized by this record.

The board was initially read-only because its mirror had lost the vault connection. Restarting it with the existing configuration and authentication restored live sync. The live ticket data matched the initial snapshot. Decisions below are accepted shaping requirements under the owner's delegated authority. The board remains the source of ticket status.

## Waitlist policy

Applies to TKT-342, TKT-343, TKT-344, TKT-345, TKT-346, TKT-349, TKT-350, TKT-351, TKT-352 and TKT-353.

- Require a signed-in account for waitlist enrollment and purchase. Commerce derives identity from its verified customer assertion. Each entry names the account, performance, ticket type and quantity. Allow one active entry per account/performance/ticket-type combination. Capture remains per pool, which can serve several performances. The first GA slice permits only public offerings without a channel or unlock-code requirement and inside their sales window. A closed performance or closed window cannot receive a new offer. Joining does not itself hold stock.
- Use a durable server-assigned sequence across entries sharing a pool. Among currently eligible entries whose full quantity fits protected headroom, offer in that order. Show the exact current position as a snapshot, with no promise about timing or stock. Leaving and joining again gets a new sequence.
- Offer the full requested quantity. Do not split a party. Skip a party that does not fit and retain its sequence. If no eligible full party fits, Commerce may acknowledge public fallback for the unused quantity. End an entry with a fixed limit-reached reason when completed purchases leave less permanent allowance than its full request. Skip without ending it when any unreleased quota reservation makes it temporarily ineligible, including provisional issue, live offer, claimed checkout or uncertain issue/termination. Commerce reserves account/event quota under its account/event lock, keyed by offer identity, before asking Inventory to issue the offer. Release it idempotently on every definite end before checkout claim, according to the transition table below. An uncertain issue result or unresolved checkout retains it until reconciliation. Expose the offer to the buyer only after quota and offer state are both durable. The cross-service recovery protocol remains TKT-343/TKT-346 design work.
- At issue time, set the offer deadline to the earliest of 15 minutes by Inventory database time, the sales-window end and the performance close, when those cutoffs exist. Persist and display that deadline. Performance cancellation, pool closure, or a sales-window edit that makes the offering ineligible terminates an unclaimed live offer. The displayed deadline is an upper bound, subject to those changes. A claimed checkout follows its recovery protocol and keeps uncertain quota. The source and transport of scheduled cutoffs are not implemented; TKT-343 must design them rather than assume Inventory already has them. A pending capture is not yet an issued offer. Its retry lease must not be presented as the buyer's deadline. No renewal on refresh or retry. Checkout must claim the offer before its deadline; a charge already in progress follows the existing recovery protocol rather than releasing uncertain money or capacity at the deadline.
- Decline and lapse end the entry. An explicit new join goes to the tail. Preserve the terminated entry and offer links for audit.
- Take waitlist priority over newly public GA headroom after acknowledged activation, as described by the proposed ADR-075. Sources include refunds, explicit buyer or operational release, buyer/group/converted-child expiry, capacity increases, and released unused channel allocations. Active channel reservations remain protected. Channel-specific and seated waitlists are outside the first GA design.
- Snapshot current resolved public face and fees when issuing the offer. Joining locks no price. Checkout uses that immutable offer quote; a catalog edit does not silently reprice a live offer. Do not add promotion codes to the first offer checkout.
- Commerce keeps the private, no-store buyer view. Inventory owns capacity capture and transfer under the pool lock. Use durable pseudonymous entry, capture, offer, claim, reservation and order links. Do not use an Inventory domain publisher.
- Public fallback is allowed only after Commerce acknowledges no eligible demand for the current generation and Inventory applies that terminal transition under the pool lock. An unavailable Commerce service does not establish an empty queue.

Offer, entry and quota transitions must follow one contract:

| Transition | Entry action | Quota action |
|---|---|---|
| Definite issue refusal, offering still eligible | Return to queue with original sequence | Release provisional reservation idempotently |
| Definite issue refusal because offering closed or became ineligible | End entry with reason | Release provisional reservation idempotently |
| Decline or lapse before checkout claim | End entry | Release offer quota idempotently |
| Cancellation, closure or ineligibility before checkout claim | End entry with reason | Release offer quota idempotently |
| Checkout claim | Remove entry from active queue and bind it to checkout | Transfer ownership to checkout without reserving twice |
| Claimed checkout completes | End as purchased | Convert reserved quota to permanently consumed quota |
| Claimed checkout ends definitively without completion | End as failed; rejoin is explicit at the tail | Release checkout quota idempotently |
| Issue, termination or checkout outcome uncertain | Keep linked reconciliation state; expose no new offer | Retain quota until the authoritative outcome is known |

A cancellation after completed purchase follows the refund policy and does not restore consumed purchase quota. This table selects required behavior; TKT-343/TKT-346 still own the cross-service recovery design.

ADR-075 remains a technical proposal. These product choices do not prove its accounting or activation design. TKT-343's approach stays open until its design covers each capacity reader and writer, quiet-pool expiry, business refusal commits, stale generations, cascade, authoritative cutoff sources and offer termination after closure or eligibility edits. TKT-342's approach remains open until the design supplies authoritative pool identity for each performance/ticket-type entry. TKT-346 still needs the authoritative TKT-473 quota contract. The source stories remain blocked by their implementations. TKT-353 must read the final persisted contracts before its approach can close. TKT-351 and TKT-352 cannot close their approaches until amendments to ADR-023, ADR-024 and ADR-027 are accepted. TKT-345 cascade and TKT-351 expiry criteria mean the next eligible entry whose full quantity fits, not an arbitrary next queue row. A nonempty queue alone does not establish eligible demand.

## Static PDF scope

TKT-357 downloads only static-mode tickets. A rotating ticket must never get a permanent admission credential through a PDF. Apply that restriction at whatever mode granularity the rotating design later chooses. A rotating-ticket instruction may contain a refresh link without a permanent admission QR. Do not make static PDF delivery wait for the rotating credential design. TKT-355's common validity verdict remains a dependency.

## Delivery locale

TKT-358 validates the buyer locale before creating the reservation hold and records it in Commerce delivery state associated with that reservation/order. Use the platform supported-locale set, currently English and French. The buyer id is derived from the reservation id and the internal delivery read is keyed by that buyer. A buyer_pii delivery-locale field can therefore preserve the order locale; the plan must verify every producer, including partner and legacy paths. A missing locale on a legacy or partner order means English. Staff re-send uses the stored locale, not the staff interface locale. An unsupported submitted value is refused rather than stored. Extend the internal delivery read; do not add locale to a domain event just to avoid that read.

The current TicketLink builder always uses /en/tickets/. The current AddressBook returns email only. This is future work, not an existing locale guarantee.

## Mail port and real sender

TKT-359 builds a provider-neutral composed message and a local capture transport. Provider choice is not a blocker for that ticket. Automatic delivery and a given staff re-send each use their established stable per-ticket message identity. A new authorized re-send gets its own identity. Transport acceptance does not prove mailbox receipt.

Select Resend for TKT-360's first optional real adapter. Local Compose and gates keep the capture transport. The adapter must send the stable identity as Idempotency-Key. Resend documents a 24-hour deduplication window, not indefinite deduplication. Retain a keyed HMAC digest of the attempted recipient, locale, template version and link beside the stable message identity. This digest is pseudonymous personal data in Access and must be included in TKT-465's data map. Resolve the recipient from Commerce at every attempt and regenerate with the stored template version. Retry under that identity only when the digest matches; changed content requires reconciliation. Access must not persist the recipient or composed message body. Use a dedicated configured digest key and record its version. This prevents honest-writer retry drift and protects low-entropy recipient guesses from a database reader without that key. It is not protection against an Access database writer or a compromised service; no anonymization claim is made. Resolve concurrent resumes with a durable local claim as part of TKT-293. Use a conservative automatic retry deadline of 23 hours from the first recorded send attempt, inside the documented 24-hour provider window. If an attempted message remains unresolved after that deadline, stop automatic re-sends and expose an operator reconciliation state. A later deliberate re-send is a new operation with an explicit possible-duplicate warning.

This choice creates no provider account, domain setup, external send or production rollout. TKT-360 still needs the port, configuration validation, and hermetic provider tests. TKT-293 must verify the concurrency and retry behavior; choosing this provider does not close that ticket.

Sources checked on 2026-10-10: [Resend send API](https://resend.com/docs/api-reference/emails/send-email) and [idempotency keys](https://resend.com/docs/dashboard/emails/idempotency-keys).

## Guest re-send

TKT-364 permits a bearer of the guest order reference to request mail only to the Commerce-recorded recipient. No caller-supplied replacement address or buyer identity is accepted. Keep the staff route staff-only.

Valid, unknown, voided and per-reference throttled requests return the same generic 202 body while the service is healthy. Add a shared/go/ratelimit public-source limiter in Access before lookup; it is per replica and its 429 does not depend on order existence. Coalesce requests within a server-side per-reference cooldown of ten minutes. Also enforce a durable guest budget of three distinct accepted operations per order per rolling 24 hours, separate from the staff budget of five. Count bound operations, including uncertain sends, so they cannot create extra mail work. A duplicate key consumes no extra budget. Exhausting the guest budget returns the same generic 202. Store a stable operation identity before sending and retain it across retries. Failure to accept durable work must remain visible as a service error; do not claim timing or partial-storage-failure enumeration resistance. Keep TKT-359 as a dependency. The implementation must prove same-key concurrent submission and replay do not start independent deliveries.

## Published localized copy

TKT-367 allows the organizer to edit a published event's names and descriptions. English and French names remain required. Optional descriptions fall back to English, matching the current public resolver; no English description means an empty description. A French-only edit preserves the English fields. Copy edits do not change slot identity, schedule, price, seat-map version or issued ticket data. Preserve the existing minutes cache contract and disclose its delay in the editor. The write must enforce organizer scope and invalidate the affected in-process public read cache.

## Promotion price and fee basis

TKT-381 applies one promotion after resolving the existing catalog/channel price winner. Do not stack multiple codes. Compute paid face as floor(unit face * (10000 - reduction basis points) / 10000), per ticket with checked integer arithmetic. The reduction is original face minus paid face. This rounds in the buyer's favor, consistent with ADR-046's stated policy for fees. A fixed reduction is per ticket, clamped at the resolved unit face. Persist resolved face, reduction, paid face, code identity and fee snapshot. reservations.unit_amount is the paid face per ticket. reservations.face_value_amount is the aggregate paid face, quantity multiplied by that unit amount. Keep the pre-discount face in separate snapshot fields. The exchange source loader reads the aggregate; it must not receive a per-ticket value.

Percentage fees use paid discounted face and retain ADR-046's per-ticket floor. Fixed fees retain their configured amounts and incidence. A code never changes channel eligibility or presale entitlement. Example: unit face 1501, 10% reduction gives paid face 1350 and reduction 151. A 333 bps fee gives 44 per ticket. Two tickets have face 2700 and that fee 88. TKT-380 still must settle promotion storage, cap locking and crash recovery.

## Promotion cap and corrections

TKT-382 counts discounted ticket units, not orders. Live or finalizing holds reserve units; completed sales consume them. Expired or released unconfirmed holds return their units through one idempotent terminal transition. A settled sale keeps its redemption after a refund, preventing refund/rebuy reuse of a capped promotion.

TKT-383 refunds the original paid discounted face. An exchange credits that stored source face and prices the target through its current catalog/channel rule. Do not carry or apply a new promotion to the target in this first exchange slice. Source redemption remains consumed. Exchanging into the same undiscounted offering can therefore require an upgrade charge equal to the original reduction; show that quote before confirmation. Ordinary refunds continue returning face only, as the existing refund store does. Any new fee-refund or payee-reversal policy must be designed in TKT-407, not inferred from the discount.

## Comps

TKT-384 is staff-only, organizer-scoped and admin-authorized. box_office and finance are excluded. Its first COS is an explicitly authorized zero-face, zero-fee staff comp, not a buyer-entered 100% promotion. The existing zero-total fulfillment and staff fee-free paths are prerequisites; a promotion implementation is not. A comp operation explicitly waives passed-on fees; no fee remains payable by the buyer. Its amount is zero and it never creates a fake capture. Do not treat a buyer's arbitrary 100% promo as staff authorization. A 100% buyer promotion with fixed passed-on fees remains a paid order. A zero-money order is voided through the existing correction path rather than a PSP refund. This first staff-comp slice applies no fee rules and records no fee snapshot or absorbed fee obligation, following the existing fee-free staff pattern. Fee-bearing zero-gross orders remain outside this slice under TKT-502; this decision does not settle them.

## Bundle allocation

TKT-388 allocates package face to components in proportion to their resolved standalone faces at the package quote. Use integer largest-remainder allocation and a stable component-id tie break. Persist the allocation; never recompute it at refund time. Refuse a package with positive face and zero total reference face. A zero-face package allocates zero.

A component is independently refundable only when its Catalog bundle definition allows it. Default is no independent component refund. When allowed, refund its stored paid share; do not reprice remaining components or claw back the package reduction. Percentage and per-ticket fees use allocated face and existing rounding. Resolve each per-order fee once for the package. Ordinary component refunds return face only until TKT-407 defines a fee reversal. Multi-line claims and pricing remain TKT-385/TKT-387 dependencies.

## Admission conflict access

TKT-400 exposes an organizer-scoped, read-only conflict list to admin and box_office roles. Finance and scanner device credentials cannot use this back-office list. Gate devices cannot resolve or rewrite a conflict. A late exit may change the projection through the existing event rule. A separate manual resolution feature needs its own scope and audit rule. Check all role and organizer predicates independently.

## Device attribution

TKT-401 uses the already authenticated enrollment identity from the scanner token. Online and offline sync handlers pass that identity to persistence; no body field may substitute a different device. Bind each new occurrence to its submitting enrolled device and make same-occurrence retries preserve that binding. A second device cannot replay it as its own success. Store attribution outside the signed canonical lifecycle form and preserve its version and golden bytes. New occurrences require a nonzero authenticated device id; historical unbound occurrences stay explicitly unknown. This is honest-writer attribution of the submitting enrollment credential, not cryptographic proof, trusted occurrence time or tamper-evidence. A second device receives a distinguishable non-actuating refusal and cannot rebind the occurrence. A same-device lost-response retry preserves the existing durable-pending and mark-before-open rule. Amend ADR-025 before implementing the cross-device replay rule. No new HMAC signing secret is needed for this first slice. Read ADR-025's same-device actuation rule before changing replay behavior.

## Camping occupancy

TKT-422's first multi-night product sells one pitch for each local-date night, independent of person count. A party occupies one pitch; person-based pricing and safety occupancy limits need separate rules. Use the site's configured IANA timezone and the date interval including check-in and excluding check-out. Each night is one dated resource claim. Select local check-in at 15:00 and check-out at 11:00 for this testbed product, and persist these on the quote. Never implement local dates as repeated 24-hour UTC increments across daylight-saving changes. TKT-419 still owns resource identity and atomic multi-pool claim order.

## Account purchase quota

TKT-473 counts completed purchased units plus every unreleased quota reservation in the transition table: provisional issue, live offer, claimed checkout and uncertain issue, termination or checkout. A reservation ownership transfer counts the same units once; it never creates a second reservation or a gap in the count. Every terminal non-completed checkout releases reserved quota, including recovery-refunded failed checkout. payment_unknown, reconciliation_required and parked orders retain it until their outcome is known. A post-purchase refund of a completed order does not release consumed quota. A completed purchase consumes quota permanently for that event, including refunded, transferred or resold tickets. Exchanges within the same event move the consumed quota; they do not count a second sale or restore allowance. An exchange into another strict event must obtain target allowance before committing its purchase. This blocks refund/rebuy and transfer/rebuy limit laundering. It is an anti-purchase limit, not a measure of current tickets owned. TKT-346 uses this same authority rather than creating a separate waitlist counter. TKT-472 and a store-tested account/event locking contract remain dependencies.

## Queue expiry and sellout messaging

TKT-478 puts late arrivals at the durable tail. Queue ETA is an estimate and always states that admission does not guarantee tickets. Do not invent a sellout probability from queue length. An observed sellout uses the existing coded inventory refusal and its observation time; do not call an upstream outage a sellout.

An expired queue session requires re-entry at the current tail. It must not cancel an already claimed checkout or recycle quota whose payment outcome is unknown. Keep any in-flight purchase reachable through the account until its outcome resolves. The same expiry policy applies to TKT-477. Its controller-slot recovery must remain distinct from account quota and authoritative checkout outcomes. TKT-477 still needs measured checkout-health signals, admission limits and a safe drain controller. A number selected without the TKT-82 evidence would not close that technical question.

## Structured offer price

TKT-482's advertised Offer price is the public buyer total including unavoidable passed-on charges for the advertised purchase quantity. Never publish face as a complete fee-inclusive price. For the first one-ticket offer, include per-order fixed fees once, preserve integer-to-decimal conversion, and show the same quantity and amount in visible page copy.

The current cached Catalog page payload does not establish that complete quote. If the required projection cannot fit the one-read budget and cache freshness, omit Offer price until it can; keep Event metadata and record price coverage as incomplete. A full TKT-482 COS still needs the quote projection and rendered-page proof. Do not declare it complete based on omission. Google recommends the lowest available price including charges and fees. [Event structured-data guidance](https://developers.google.com/search/docs/appearance/structured-data/event).

## Decisions left open

- TKT-160 retains the owner's explicit deferral of controlled resale. Binding issued tickets to accounts is the narrower engineering prerequisite. Do not decide a resale business model merely to clear a question badge.
- TKT-361, TKT-362 and TKT-363 still need an accepted rotating-mode and clock-trust design. The static-PDF restriction above settles only the PDF bypass question. No test has established a safe offline clock bound.
- TKT-339 has a local-only delay and threshold policy below; the live proof still needs TKT-338. TKT-340 needs the shipped queue contract. TKT-489 needs the accepted TKT-492 measurement profile. TKT-354 remains outside the GA decision; seated capture and exact returned-seat sources need their own checked design.
- TKT-407 and TKT-454 need an explicit compensating-ledger design. ADR-048 binds settlement to captures. Do not invent negative capture facts or pretend its balance trigger covers refunds or equal exchanges.
- TKT-391 has a bounded event-pickup policy below, but multi-line stock and tax contracts remain dependencies. TKT-392 needs a liability and provider model before protection can be sold.
- TKT-470 has an order-specific mailbox proof policy below. TKT-465 still must map and validate the exact records and disclosure scope; this record supplies no legal retention or compliance conclusion.
- TKT-485 still needs a customer-bound payment handoff and provider evidence. ADR-069's opaque token rule does not establish that an agent grant authorizes a charge.
- TKT-279, TKT-289, TKT-313, TKT-329 and TKT-492 retain their recorded blockers. TKT-504 has a selected repair direction below; its high finding remains unresolved until the fix is implemented and reviewed. This pass does not override a high finding, retry denied filesystem access, or authorize merge. TKT-286 and TKT-336 also retain their unfinished implementation/review state.

## Local latency alert

TKT-339 uses a local Commerce checkout p99 threshold of one second for two minutes, with at least 20 real checkout observations in the five-minute input window. No traffic remains No Data. The proof uses a test-only forwarding proxy configured through Commerce's existing PAYMENTS_URL. Delay the /internal/charges response by two seconds and leave other calls unchanged. Use an input series that identifies checkout requests and excludes health and readiness probes. Verify the available label set, histogram units and bucket resolution before fixing the rule, and read live Grafana alert state for fire and resolution. This is a demonstration threshold, not a production paging recommendation. TKT-338 remains a dependency. TKT-339's approach stays open until the measured series can distinguish checkout from probes; the current shared histogram alone does not prove that.

## Merchandise first slice

TKT-391 uses Catalog item definitions and event associations, Commerce order lines, and Inventory-owned stock. The first fulfillment method is event pickup. Shipping is outside this slice. A monetary refund alone does not restore stock; staff must record physical return first. Multi-line stock and tax contracts remain dependencies. This decision does not establish the missing stock implementation.

## Guest export proof

TKT-470 requires the named order reference plus fresh mailbox control. Send a one-use 256-bit proof token only to that order's Commerce-recorded recipient, with a separate purpose namespace, stored hash and twenty-minute expiry. An order reference alone is insufficient. Bind redemption to the named guest order and export only its mapped data. Do not authorize every order sharing an email address. Use generic request responses, source limiting and no-store private export. Add a ten-minute per-order proof-mail cooldown and a durable budget of three distinct accepted proof-mail operations per order per rolling 24 hours, separate from ticket re-send. A new proof token cancels all prior open tokens for that order and purpose. A replay retains its token and original expiry and consumes no new budget. Accept durable mail work before returning the generic healthy-service response; expose a service error when durable acceptance fails. These rules do not claim timing or partial-storage-failure enumeration resistance. TKT-465 must still validate the data map, proof threat model and disclosure scope. Mailbox custody is the engineering trust assumption; this record makes no legal retention or compliance finding.

## Persisted image cache

TKT-504 disables stale-if-error reuse for persisted Astro remote-image entries. The inspected astro@7.3.5 patch has separate network-failure and 5xx fallback paths; remove stale reuse from both. If updated policy cannot be written and both old files cannot be deleted, a new process cannot discover the revoked permission from that directory. Keep valid fresh reuse and successful revalidation subject to the actual policy; failure to revalidate must not copy retained bytes to output. Cover both failed matching-304 and replacement-200 invalidation, followed by a new build/process and 503. Include old on-disk entries and both frontends. This selects a repair direction without adding a separate revocation store. The accepted high finding stays open until the implementation and review prove the repair. No merge or review override is granted.

## Folded pass decision and stale reseller blocker

TKT-281's owner-selected pass rule was folded into TKT-169, now Done. Current code refuses an admitted pass before money and retains the row-locked switch check. The existing TestAdmissionCheckCoversBothVocabularies and TKT-169's merged-code validation name the two cases. Close TKT-281 as delivered by TKT-169, carrying that evidence rather than claiming a new gate run. The late-scan-to-switch race remains TKT-498; it did not disappear when the policy was chosen.

TKT-23's old uncaptured-partner blocker describes agency terms. TKT-277 delivered merchant-of-record partner confirm, and TKT-294 records that agency and net-rate terms are unsupported. Append that correction without deleting the historical blocker or treating reports and reversal as complete.

## Architecture records

[ADR-078](../adr/ADR-078-optional-resend-and-bounded-delivery-retries.md) records the optional transport and retry policy. [ADR-079](../adr/ADR-079-persisted-image-cache-without-stale-if-error.md) records the TKT-504 repair requirement. Both state their unimplemented scope. ADR-075 remains proposed.

## Applying the record

Before each write, re-read the live ticket and its latest decisions. Append one sequential kind=decision entry per settled material choice, with a reference to this file. Record remaining implementation and design dependencies separately. Update authoring readiness through the supported shaping path; POST /ticket ignores readiness and context, so a successful comment write alone cannot clear the DoR badge. Read back each written note and the board before reporting an unblocked ticket.

Keep unimplemented Backlog tickets in Backlog; close only the explicitly folded TKT-281 against TKT-169's carried evidence. This pass does not exercise Gate 1 priority or the implementation flow. Do not mark an implementation or design spike Done based only on these policy choices.


## Verified batch result

The live board and vault readbacks agree for all 35 changed tickets. Each has one new decision entry. Existing comments, labels, assignments and PR fields were preserved. TKT-281 alone changed status, to Done as delivered by TKT-169. All other ticket statuses stayed unchanged.

Human-owned open readiness items fell from 24 to 6. TKT-358, TKT-367, TKT-384 and TKT-400 pass all eight readiness items and have no unfinished blocked-by target. They remain Backlog for owner prioritization. The remaining human-owned questions are TKT-160, TKT-392, TKT-407, TKT-454, TKT-477 and TKT-485. They concern explicit resale deferral, protection liability, settlement/VAT reversal, measured admission control and agent payment handoff.

Claude Opus 5.5 reviewed the choices and their corrections in four read-only passes. The last pass found a quota-definition mismatch; it was corrected to count every unreleased holding state in the transition table. No final clean-review claim is made. Documentation link checks, unique ADR numbers, proposal consistency checks and exact board/vault readbacks passed. No application tests or gate were run for this documentation and board pass. TKT-281 carries TKT-169's existing validation and its recorded review limitations. No code was implemented, committed, pushed, merged or deployed.
