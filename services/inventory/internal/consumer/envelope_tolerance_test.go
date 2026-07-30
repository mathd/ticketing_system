package consumer

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// Disposition-parity regression for the TKT-126 ai-review finding.
//
// Centralizing the first-pass decode is only safe if the shared view parses
// exactly what this consumer used to parse. It briefly parsed `occurred_at` too
// — and time.Time parsing is strict, so an envelope with a malformed timestamp
// failed the decode and was TERMINATED, even at a schema this binary has never
// seen. Terminating a well-formed future variant is the whole of TKT-61: the one
// binary that provably cannot read the event decided to discard it.
//
// The emitter goldens cannot express this. They prove what catalog WRITES;
// nothing in them constrains what inventory must tolerate READING. That gap is
// the reason this file exists.
// Every fixture carries the correct `type` for its subject. An earlier draft of
// this file omitted it, which quietly codified a gap rather than testing the
// thing under test (second-pass ai-review finding): inventory does not validate
// `type` against the subject at all, which is pre-existing and now tracked as
// TKT-133 — not something these fixtures should bless in passing.
const publishedType = `"type":"platform.catalog.performance.published"`

func TestFutureVariantSurvivesUnparseableEnvelopeMetadata(t *testing.T) {
	id := `"id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"`
	for _, tt := range []struct {
		name, body string
	}{
		{"malformed occurred_at", `{` + id + `,` + publishedType + `,"occurred_at":"invalid","schema":5,"data":{"performance_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"}}`},
		{"occurred_at is a number", `{` + id + `,` + publishedType + `,"occurred_at":1750000000,"schema":5,"data":{}}`},
		{"occurred_at is an object", `{` + id + `,` + publishedType + `,"occurred_at":{"secs":1},"schema":6,"data":{}}`},
		{"occurred_at null", `{` + id + `,` + publishedType + `,"occurred_at":null,"schema":7,"data":{}}`},
		{"occurred_at absent", `{` + id + `,` + publishedType + `,"schema":8,"data":{}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, st := testConsumerWithStore()
			msg := &fakeMsg{data: []byte(withSubjectType(subjectPublished, tt.body))}

			c.handle(context.Background(), msg)

			// The EXACT action sequence, not merely "not terminated". A handler
			// that quarantined and then NAK'd would leave the event outstanding
			// and stall the ack window behind it (TKT-68) while still passing a
			// weaker assertion.
			if len(msg.actions) != 1 || msg.actions[0] != "ack" {
				t.Fatalf("actions = %v, want exactly [ack] — the quarantined copy is what frees the ack window", msg.actions)
			}
			if len(st.quarantined) != 1 {
				t.Fatalf("quarantined = %d, want 1 — the variant is recoverable by a newer binary", len(st.quarantined))
			}
			if string(st.quarantined[0].envelope) != tt.body {
				t.Fatalf("quarantined %q, want the exact raw bytes", st.quarantined[0].envelope)
			}
			if c.Ready() {
				t.Fatal("version skew must still latch unready")
			}
		})
	}
}

// The same tolerance at a KNOWN schema: nothing here reads `occurred_at`, so a
// malformed one must not stop an otherwise valid publication from provisioning.
func TestKnownVariantSurvivesUnparseableOccurredAt(t *testing.T) {
	perf := uuid.MustParse("6ba7b811-9dad-11d1-80b4-00c04fd430c8")
	org := uuid.MustParse("6ba7b812-9dad-11d1-80b4-00c04fd430c8")
	eventID := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	body := `{"id":"` + eventID.String() + `",` + publishedType + `,"occurred_at":"invalid","schema":2,` +
		`"data":{"performance_id":"` + perf.String() + `","organizer_id":"` + org.String() + `","capacity":250}}`

	c, st := testConsumerWithStore()
	msg := &fakeMsg{data: []byte(withSubjectType(subjectPublished, body))}

	c.handle(context.Background(), msg)

	// Assert the work actually happened, not just that nothing bad did: a
	// handler that silently acked without provisioning would satisfy any
	// weaker check, and that is the failure this test exists to catch.
	if len(st.provisioned) != 1 || st.provisioned[0] != eventID {
		t.Fatalf("provisioned = %v, want exactly [%s] — a valid schema-2 publication must still be applied", st.provisioned, eventID)
	}
	if len(msg.actions) != 1 || msg.actions[0] != "ack" {
		t.Fatalf("actions = %v, want exactly [ack]", msg.actions)
	}
	if len(st.quarantined) != 0 {
		t.Fatal("a known schema must never be quarantined")
	}
	if !c.Ready() {
		t.Fatal("readiness must be untouched by a known, valid variant")
	}
}
