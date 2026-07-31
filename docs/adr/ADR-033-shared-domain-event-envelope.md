# ADR-033: One domain-event envelope in the shared kernel

Date: 2026-07-27

## Status

Accepted

## Context

`docs/architecture.md` describes `shared/go` as a **shared kernel** whose additions **require an
ADR**. The bar is deliberately high: five services that share a type share a deployment constraint,
and a kernel that accumulates domain concepts stops being a kernel. This ADR argues that the
platform domain-event envelope earns its way in, and — just as importantly — draws the line at
exactly where it stops.

ADR-009 §5 fixes the envelope as contract: `id`, `type` (== subject), `occurred_at`, `schema`,
`data`, published to JetStream with acks, with a deterministic id so retried or raced emissions
de-duplicate. ADR-017 then makes two rules load-bearing for anyone who *reads* one:

- **§5b′** — a consumer must **dispatch on `schema` before decoding `data`**. A bump exists
  *because* `data` changed, so judging a future variant against today's struct rejects it as
  malformed and terminates it — dropping precisely what the parking rule exists to preserve.
- **§5b** — the poison/skew line has a **bottom end**: `schema <= 0` is a broken envelope, not the
  future. It terminates, and it **must not touch readiness** — otherwise one malformed message is a
  free denial of service against any consumer that latches readiness on unknown variants.

Until now the envelope was declared **independently at six sites**, and those two rules were
re-derived at each decode site:

| Site | Form |
|---|---|
| `services/catalog/internal/events/events.go` | full `Envelope` struct (`ID string`) |
| `services/commerce/internal/events/events.go` | full `Envelope` struct (`ID uuid.UUID`) |
| `services/access/internal/consumer/consumer.go` | `FailureEvent` (emit) + `envelope` + `completed` (decode) |
| `services/access/internal/consumer/policy.go` | a fourth dispatch ladder over the same `envelope` |
| `services/inventory/internal/consumer/consumer.go` | `envelope` + `publication` |
| `services/access/internal/store/{scan,lifecycle,reconcile}.go` | three inline anonymous structs |

The duplication was visible in the code as three different spellings of one rule — `schema <= 0`
in access's order consumer, `<= 0` plus a separate `< spec.min` in inventory, and
`< publicationSchemaMin` in access's policy projector.

**This is not a tidiness complaint.** `AGENTS.md` records the cost: **TKT-61 shipped the
dispatch-ordering bug twice**, past a mutation-checked test suite and a full adversarial review
pass. TKT-74 then found the same bug again in the second consumer. A rule that every site must
remember independently is a rule that will be forgotten independently.

The hard constraint on any fix: the wire must be **byte-for-byte unchanged**. Five services and a
live JetStream stream depend on it, and ADR-017 names the way such a proof goes wrong — *a fixture
built from the type under test cannot fail*, because it encodes the compatibility it claims to
prove.

## Possible Solutions

- **Option 1: Do nothing — keep the envelope declared per service.**
    - Pros:
        - Zero risk to the wire; no kernel growth; services stay independently deployable in the
          strongest sense.
        - Honest about the fact that `data` genuinely differs per subject.
    - Cons:
        - Leaves the mechanism that produced TKT-61 and TKT-74 fully intact. Both were caught late
          and expensively; nothing structural prevents a third.
        - The next consumer (TKT-127's skeleton, and whatever consumes `seat_map.published`) starts
          by copying a decode ladder, which is how the count went from two to six.

- **Option 2: A shared envelope type only — no decode helper.**
    - Pros:
        - Minimal kernel addition; the struct is genuinely universal.
        - Fixes the "six declarations" half of the finding.
    - Cons:
        - Fixes the half that never caused a bug. The declarations were consistent; the **decode
          ordering** is what shipped wrong twice. A shared struct with `Data any` would not have
          prevented TKT-61 at all.

- **Option 3: A shared envelope + a decode helper that returns `data` raw (chosen).**
    - Pros:
        - Makes §5b′ **structural** rather than remembered: a caller holding the decode view has
          nothing typed to judge, so it *cannot* read `data` before dispatching on `schema`.
        - Puts the one universal judgment (`schema <= 0`) in one place with one test.
        - Leaves per-subject payloads and dispositions where they belong.
    - Cons:
        - `shared/go` gains a direct dependency on `github.com/google/uuid`.
        - Generics are a little more abstract than five concrete structs.

- **Option 4: A shared consumer framework — envelope, dispatch, disposition, readiness.**
    - Pros:
        - Would remove the remaining duplication (the `id`/`type` checks, the run loop).
    - Cons:
        - Wrong scope and wrong shape *today*. Dispositions genuinely differ: inventory quarantines
          and acks (TKT-68); access has no quarantine store and parks outstanding (ADR-017 §5b′,
          TKT-74). A framework would have to encode both, or flatten a difference the services
          depend on.
        - The run-loop half is TKT-127's subject, and it is shared **behaviour**, not shared
          **contract** — a different kind of kernel entry, deserving its own argument.

## Decision

We adopt **Option 3**. `shared/go/domainevent` declares the platform envelope once, as a generic
over the payload type, plus `DecodeEnvelope` and the `ErrInvalidSchema` sentinel:

```go
// emit
type Envelope[T any] struct {
    ID         uuid.UUID `json:"id"`
    Type       string    `json:"type"`
    OccurredAt time.Time `json:"occurred_at"`
    Schema     int       `json:"schema"`
    Data       T         `json:"data"`
}

// decode — one field shorter, deliberately
type Decoded[T any] struct {
    ID     uuid.UUID `json:"id"`
    Type   string    `json:"type"`
    Schema int       `json:"schema"`
    Data   T         `json:"data"`
}

type Raw = Decoded[json.RawMessage]

var ErrInvalidSchema = errors.New("domain event envelope has no usable schema")

func DecodeEnvelope(data []byte) (Raw, error)
```

Each is generic over the payload, which is what lets one declaration serve every subject: emitters
instantiate `Envelope` with their per-subject payload type, consumers instantiate `Decoded` with
`json.RawMessage` for the schema dispatch and with a typed payload for the second pass. Field
declaration order is contract — `encoding/json` emits in declaration order — so those lines are the
wire for every service at once.

**Emit and decode are two types, and the difference is exactly `occurred_at`.** That asymmetry is
the decision, not an oversight, and it was not in the first draft of this ADR — the adversarial
review caught it (see Consequences). **Every field the decode path parses is a field whose *format*
can reject a message before `schema` has been looked at.** `occurred_at` decodes into a `time.Time`,
whose parsing is strict, so a malformed timestamp made the decode fail — and a failed decode is
"unreadable", which terminates. At a schema the binary has never seen, that means **discarding a
well-formed future variant on the authority of the binary that provably cannot read it**: TKT-61
exactly, reintroduced by the very refactor meant to prevent it. No consumer dispatches on
`occurred_at`, so parsing it bought nothing.

ADR-017 §5b′ already named the minimal stable envelope as `{id, schema, data}`. This is why.

`type` stays on the decode side because two consumers check it as a contract precondition and
because **a string field cannot fail to parse the way a timestamp can** — any JSON string decodes.
A *non-string* `type` violates ADR-009 §5's envelope contract itself rather than being a schema
variation, and it terminates. That is a tightening for inventory, which previously ignored the
field; it is pinned by a test rather than left implicit.

**`ID` is `uuid.UUID`, not `string`.** Both marshal to the same JSON, so the choice looks cosmetic;
it is not. A `uuid.UUID` field **rejects a malformed id at decode time**, which is how a garbage
envelope currently terminates. Against a `string` field, `"not-a-uuid"` parses cleanly and survives
the consumers' non-empty check — so flattening to `string` would quietly weaken poison detection
across every consumer. Catalog's exported `EventID`-family helpers keep returning `string`
(unchanged public API); each gained an unexported sibling returning the raw `uuid.UUID` that the
envelope is built from. **No `uuid.MustParse` sits on any publish path** — a panic on an emitter is
not an acceptable price for a type change.

**The line this package does not cross.** It says what the envelope *is*, and what is unreadable by
*anyone*. What to *do* about it stays with the service:

- **Per-subject known ranges stay in the service.** Inventory's registry has
  `archived: {min: 2}` — a schema-1 archived event is not `<= 0`, so the shared rule cannot see it.
  Only the owning service knows a subject's first variant.
- **Disposition stays in the service.** Term, park, quarantine, NAK-with-delay, and whether
  readiness latches are policy, and they legitimately differ between inventory and access.
- **The `id`/`type` checks stay in the service, and they are *not* uniform.** All three consumers
  check `id`. Only access's two check `type` against the subject they expect; **inventory does not
  check `type` at all** — it dispatches on the NATS subject and ignores the field. That predates
  this ADR and is not changed here (tracked as TKT-133), but it must be stated rather than papered
  over: an earlier draft of this section claimed every consumer performs both checks, which was
  simply false, and the adversarial review caught it. Their logging and failure records differ too.
  Unifying them is a candidate for TKT-127, which is where shared *behaviour* belongs.

**Byte-for-byte identity is proven by goldens captured before the refactor.** 13 literals covering
every published subject were captured from the **pre-refactor** emitters and committed in a
baseline commit at which `shared/go/domainevent` **does not exist** — checkable by path
(`git show <baseline>:shared/go/domainevent/envelope.go` must fail), not by prose. They compare the
**complete byte slice**: decoding and comparing fields cannot see a key-order or timestamp-format
change, which is most of what the claim protects. Regenerating them from the new type would satisfy
the test and prove nothing, which is exactly ADR-017's trap. **They must never be regenerated** — a
legitimate future wire change means updating a literal deliberately, in an ADR of its own.

## Consequences

- **Positive:**
    - ADR-017 §5b′ is enforced by the type system rather than by reviewer vigilance. The failure
      mode behind TKT-61 and TKT-74 — decoding `data` before dispatching on `schema` — is no longer
      expressible through `DecodeEnvelope`, because there is nothing typed to decode into.
    - The `schema <= 0` rule has one implementation and one test instead of three spellings.
    - TKT-127 (the shared durable-consumer skeleton) has the type it needs to exist.
    - New subjects start from a declaration instead of a copied struct.
    - The wire has an executable, non-circular contract for the first time — 13 goldens that fail on
      a key reorder, a precision change, or a dropped `omitempty`.

- **Negative:**
    - `shared/go` now depends directly on `github.com/google/uuid`. It was already an indirect
      dependency, so the module graph is unchanged, but the kernel's dependency surface is now
      explicit and one larger.
    - The envelope cannot carry a non-UUID identifier without another contract decision. Accepted:
      every emitter and consumer already treats the id as a UUID, and ADR-009 §5's determinism
      requirement is expressed today as `uuid.NewSHA1` at all six derivation sites.
    - A kernel type couples five services' release trains a little more tightly. Mitigated by how
      narrow it is: the envelope shape is contract already, so this changes *where it is written
      down*, not *what may change independently*.
    - The goldens are deliberately brittle. That friction is the intended contract-review gate, and
      it will occasionally be mistaken for a flaky test by someone who has not read this ADR.
    - **The goldens prove what this repo WRITES, and are structurally incapable of proving what it
      must tolerate READING.** This is worth stating because it already cost a blocking bug: the
      first implementation reused the emit type as the decode view, which added `occurred_at`
      parsing to all three consumers and turned a malformed timestamp into a termination — at known
      schemas it dropped valid events, and at future schemas it dropped recoverable ones. Thirteen
      byte-exact goldens were green throughout, because no fixture built from an emitter can express
      an input an emitter would never produce. The adversarial review found it; the parity tests in
      `envelope_tolerance_test.go` (inventory and access) now pin the disposition for every
      unparseable-metadata shape, at both known and future schemas. **Any future change to the
      decode view's field set must come with the same parity evidence — the goldens will not catch
      it.**
    - **This ADR does not make the envelope tamper-evident.** Naming the adversary as ADR-021
      requires: this is honest-writer consistency only. A writer with database or broker access can
      still author whatever envelope it likes; nothing here constrains one. `DecodeEnvelope` rejects
      *malformed* envelopes, not *dishonest* ones.

## References

- TKT-126 — this ticket; TKT-127 — the consumer skeleton it unblocks
- [ADR-009: Contract-first APIs](ADR-009-contract-first-apis.md) — §5 fixes the envelope shape
- [ADR-017: Domain-event schema evolution](ADR-017-domain-event-schema-evolution.md) — §5b, §5b′, and
  the fixture-built-from-the-type-under-test trap
- [ADR-002: Services from day one](ADR-002-services-from-day-one.md) — the five-services layout
- [ADR-021: Ticket-lifecycle trail integrity](ADR-021-ticket-lifecycle-trail-integrity.md) — name the
  adversary before claiming a guarantee
- [ADR-025: Admission events and offline reconciliation](ADR-025-admission-events-and-offline-reconciliation.md)
  — §D9 constrains the alarm payloads whose envelopes this ticket rewraps
- `docs/architecture.md` — the shared-kernel rule this ADR satisfies
- [The 2026-07-25 architecture review](../reviews/2026-07-25-architecture.md), recommendation R3 —
  the originating finding. Its recommendations also live on the board (R3 → TKT-126, R4 → TKT-128).
