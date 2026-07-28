// Package domainevent declares the platform domain-event envelope (ADR-009 §5)
// and the one decode rule ADR-017 makes universal.
//
// Before TKT-126 the envelope was declared independently at six sites — two
// publisher structs, four consumer decode structs, three inline anonymous
// structs — and ADR-017's load-bearing rules were re-derived at each one. That
// is not a tidiness complaint: TKT-61 shipped the dispatch-ordering bug TWICE,
// past a mutation-checked suite and a full adversarial review pass, because
// each decode site had to remember the rule on its own. One declaration is how
// the rule stops being something to remember.
//
// # What lives here, and what deliberately does not
//
// Here: the envelope's shape, and the bottom end of the poison/skew line
// (`schema <= 0`) — the one judgment that is identical for every subject and
// every consumer, because it follows from schema numbering alone.
//
// NOT here, on purpose:
//
//   - Per-subject known ranges. Inventory's registry has `archived: {min 2}` —
//     a schema-1 archived event is not `<= 0`, so this package cannot see it;
//     only the owning service knows the subject's first variant.
//   - The disposition. Term, park, quarantine, NAK-with-delay and whether
//     readiness latches are service policy, and they differ: inventory
//     quarantines and acks (TKT-68), access parks outstanding (it has no
//     quarantine store). A library that returned a disposition would be
//     inventing policy for services whose constraints it does not know.
//   - The `id`/`type` checks. Every consumer makes them, but their logging and
//     failure records differ; folding them in would flatten distinctions the
//     services depend on. They are a candidate for the consumer skeleton
//     (TKT-127), which is where shared *behaviour* belongs — this package is
//     shared *contract*.
//
// The dividing line: this package says what the envelope IS and what is
// unreadable by anyone. What to DO about it stays with the service.
package domainevent

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Envelope is the platform domain-event envelope (ADR-009 §5): a minimal
// identifying payload with a versioned shape, where type == subject and the id
// is deterministic so retried or raced emissions de-duplicate.
//
// Field order is CONTRACT, not style. encoding/json emits struct fields in
// declaration order, so reordering these five lines changes the bytes on the
// wire for every service at once. The per-service golden tests exist to make
// that unshippable.
//
// Data is generic so one declaration serves both directions: emitters
// instantiate it with their per-subject payload type (which stays in the owning
// service — this package never learns what an order or a performance is), and
// consumers instantiate it with json.RawMessage so `data` stays undecoded until
// `schema` has been dispatched on.
type Envelope[T any] struct {
	ID         uuid.UUID `json:"id"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurred_at"`
	Schema     int       `json:"schema"`
	Data       T         `json:"data"`
}

// Raw is the decode-side view. It is deliberately NOT Envelope[json.RawMessage]:
// it is a strict subset, and the subset is load-bearing.
//
// The raw Data is the first half of the point. A consumer holding a Raw
// physically cannot judge `data` before it has looked at `schema`, because there
// is nothing typed to judge yet. ADR-017 §5b′ states the rule; this type is the
// rule made structural.
//
// The ABSENCE of OccurredAt is the second half, and it is the less obvious one.
// ADR-017 §5b′ names the minimal stable envelope as `{id, schema, data}` for a
// reason: every field the first pass decodes is a field whose FORMAT can reject
// the message before `schema` has been looked at. `occurred_at` decodes into a
// time.Time, and time.Time parsing is strict — so an envelope carrying a
// malformed timestamp would fail the first-pass decode and be terminated as
// unreadable, even at a schema this binary has never seen. That is precisely the
// TKT-61 failure: discarding a well-formed future variant on the authority of
// the one binary that provably cannot read it. No consumer dispatches on
// `occurred_at`, so decoding it here buys nothing and risks exactly that.
//
// Type stays because two consumers check it as a contract precondition, and
// because a string field cannot fail to parse the way a timestamp can: any JSON
// string decodes. A NON-string `type` is a violation of ADR-009 §5's envelope
// contract itself rather than a schema variation, and it is terminated — see
// TestDecodeEnvelopeRejectsNonStringType, which pins that consequence.
type Raw = Decoded[json.RawMessage]

// Decoded is the decode-side envelope, generic over the payload so the SAME
// declaration serves both passes: `Raw` (data still bytes) for the schema
// dispatch, and `Decoded[SomePayload]` for the second pass once an arm is known.
//
// It is a separate declaration from Envelope, and the difference is exactly one
// field: Decoded has no OccurredAt. That asymmetry is the design, not an
// oversight — emitters MUST write `occurred_at` (ADR-009 §5 requires it), and
// consumers must NOT parse it, because parsing it is what lets a malformed
// timestamp reject a message that no consumer's disposition depends on.
//
// The second pass needs this as much as the first. Access and inventory both
// decode the full message into a typed envelope once the variant is known, so a
// Decoded carrying OccurredAt would terminate an otherwise valid, known-schema
// event over a field nothing reads.
type Decoded[T any] struct {
	ID     uuid.UUID `json:"id"`
	Type   string    `json:"type"`
	Schema int       `json:"schema"`
	Data   T         `json:"data"`
}

// ErrInvalidSchema reports an envelope with no usable schema.
//
// It is deliberately NOT the same as a JSON decode failure: several consumers
// give the two different dispositions and different failure records, so callers
// branch with errors.Is rather than treating any error as malformed.
var ErrInvalidSchema = errors.New("domain event envelope has no usable schema")

// DecodeEnvelope reads the stable part of an event and leaves `data` raw.
//
// It enforces exactly one rule: schema numbers start at 1 and only climb, so
// `schema <= 0` is a broken envelope — an omitted field or a producer bug — and
// not a variant from the future (ADR-017 §5b). No binary, present or future,
// will ever apply it, so it is poison. Only schemas ABOVE a consumer's known
// set are skew.
//
// That distinction has teeth: a consumer that latches readiness on the future
// and mistakes a broken envelope for one hands any buggy producer a free denial
// of service. Callers must terminate an ErrInvalidSchema WITHOUT touching
// readiness.
//
// On ErrInvalidSchema the decoded envelope is returned alongside the error, so
// a caller can log the id and the offending schema instead of a zero value.
func DecodeEnvelope(data []byte) (Raw, error) {
	var env Raw
	if err := json.Unmarshal(data, &env); err != nil {
		return Raw{}, err
	}
	if env.Schema <= 0 {
		return env, ErrInvalidSchema
	}
	return env, nil
}
