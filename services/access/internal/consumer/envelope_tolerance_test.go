package consumer

import (
	"context"
	"testing"
)

// Disposition-parity regressions for the TKT-126 ai-review finding, for both of
// access's consumers.
//
// Centralizing the first-pass decode is only safe if the shared view parses
// exactly what these consumers used to parse. It briefly parsed `occurred_at`
// too — and time.Time parsing is strict, so a malformed timestamp failed the
// decode and turned into a disposition change:
//
//   - order.completed at a FUTURE schema went from park + latch unready to an
//     invalid_json failure record and termination. That drops tickets for an
//     order that was already paid for, which is the exact harm ADR-017 §5b′'s
//     parking rule exists to prevent.
//   - performance.published at a future schema went from park + unready to
//     termination, silently disenforcing pass policy on every slot it described.
//   - at KNOWN schemas both simply terminated an otherwise valid event over a
//     field neither consumer reads.
//
// The emitter goldens cannot express any of this: they prove what this repo
// WRITES. These prove what it must tolerate READING.

func TestFutureOrderVariantSurvivesUnparseableOccurredAt(t *testing.T) {
	const id = `"id":"10000000-0000-0000-0000-000000000001"`
	const typ = `"type":"platform.commerce.order.completed"`
	for name, body := range map[string]string{
		"malformed occurred_at":   `{` + id + `,` + typ + `,"occurred_at":"invalid","schema":2,"data":{"order_ref":"r"}}`,
		"occurred_at is a number": `{` + id + `,` + typ + `,"occurred_at":1750000000,"schema":2,"data":{}}`,
		"occurred_at is an array": `{` + id + `,` + typ + `,"occurred_at":[1],"schema":9,"data":{}}`,
		"occurred_at null":        `{` + id + `,` + typ + `,"occurred_at":null,"schema":5,"data":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			published := false
			c := testConsumer(func(context.Context, FailureEvent) error { published = true; return nil })
			msg := &fakeMsg{data: []byte(body), delivery: 1}

			c.handle(context.Background(), msg)

			if len(msg.actions) != 1 || msg.actions[0] != "nak-delay" {
				t.Fatalf("actions = %v, want parked — an unreadable timestamp must not make a paid order poison", msg.actions)
			}
			if published {
				t.Fatal("published a failure record for a future variant — it is not a failure")
			}
			if c.Ready() {
				t.Fatal("readiness must still latch false on version skew")
			}
		})
	}
}

// A known, otherwise valid order must still issue: nothing on this path reads
// `occurred_at`, so a malformed one must not reach a disposition at all.
func TestKnownOrderVariantSurvivesUnparseableOccurredAt(t *testing.T) {
	body := `{"id":"10000000-0000-0000-0000-000000000001","type":"platform.commerce.order.completed","occurred_at":"invalid","schema":1,` +
		`"data":{"order_id":"10000000-0000-0000-0000-000000000002","guest_order_ref":"10000000-0000-0000-0000-000000000003",` +
		`"organizer_id":"10000000-0000-0000-0000-000000000004","buyer_id":"10000000-0000-0000-0000-000000000005",` +
		`"slot_id":"10000000-0000-0000-0000-000000000006","ticket_type_id":"10000000-0000-0000-0000-000000000007","quantity":1}}`

	processed := false
	c := testConsumer(func(context.Context, FailureEvent) error { return nil })
	c.process = func(context.Context, completed) (FailureStage, error) { processed = true; return "", nil }
	msg := &fakeMsg{data: []byte(body), delivery: 1}

	c.handle(context.Background(), msg)

	if !processed {
		t.Fatalf("a valid schema-1 order was not processed; actions = %v", msg.actions)
	}
	if !c.Ready() {
		t.Fatal("readiness must be untouched by a valid known variant")
	}
}

func TestFuturePublicationSurvivesUnparseableOccurredAtInPolicyProjector(t *testing.T) {
	const head = `"id":"20000000-0000-0000-0000-000000000001","type":"platform.catalog.performance.published"`
	for name, body := range map[string]string{
		"malformed occurred_at":   `{` + head + `,"occurred_at":"invalid","schema":9,"data":{"re_entry_v2":{}}}`,
		"occurred_at is a number": `{` + head + `,"occurred_at":1750000000,"schema":9,"data":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			st := &fakePolicyStore{}
			c := newPolicyConsumerForTest(st)
			msg := policyMsg(body)

			c.handle(context.Background(), msg)

			if len(msg.actions) != 1 || msg.actions[0] != "nak-delay" {
				t.Fatalf("actions = %v, want parked — terminating disenforces pass policy on every slot it described", msg.actions)
			}
			if c.Ready() {
				t.Fatal("readiness must still latch false on version skew")
			}
		})
	}
}

func TestKnownPublicationSurvivesUnparseableOccurredAtInPolicyProjector(t *testing.T) {
	body := `{"id":"20000000-0000-0000-0000-000000000001","type":"platform.catalog.performance.published","occurred_at":"invalid","schema":2,` +
		`"data":{"performance_id":"20000000-0000-0000-0000-000000000002","organizer_id":"20000000-0000-0000-0000-000000000003",` +
		`"re_entry":{"mode":"multi","requires_exit":false}}}`

	st := &fakePolicyStore{}
	c := newPolicyConsumerForTest(st)
	msg := policyMsg(body)

	c.handle(context.Background(), msg)

	if len(st.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1 — a valid publication was dropped over a field nothing reads; actions = %v", len(st.upserts), msg.actions)
	}
	if st.upserts[0].Policy.Mode != "multi" {
		t.Fatalf("mode = %q, want multi", st.upserts[0].Policy.Mode)
	}
	if !c.Ready() {
		t.Fatal("readiness must be untouched by a valid known variant")
	}
}
