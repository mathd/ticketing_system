package consumer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/access/internal/store"
)

// The order.exchanged arm (TKT-166, ADR-039 §5).
//
// EVERY envelope in this file is hand-written JSON. Not one is marshaled from
// exchangedData, and that is the whole point: a fixture built from the type under test
// encodes the compatibility it claims to prove and CANNOT FAIL (ADR-017 §5b′). TKT-61
// shipped this exact bug twice — past a mutation-checked suite and a full review pass —
// which is why the rule is written down rather than assumed.

// A well-formed schema-1 exchange reaches processing. The baseline the skew cases are
// measured against: if this did not pass, "parked" below would prove nothing.
// The id is the DERIVED one for exchange 3000…0001, written out as a literal. Calling
// exchangedEventID here would make the fixture agree with the code by construction —
// the same trap as building a payload from the type under test.
const validExchangedJSON = `{"id":"edb420ae-aec5-5ed1-8a88-c229e70f6e55",` +
	`"type":"platform.commerce.order.exchanged","occurred_at":"2026-07-31T10:00:00Z","schema":1,` +
	`"data":{"exchange_id":"30000000-0000-0000-0000-000000000001",` +
	`"source_order_id":"30000000-0000-0000-0000-000000000002",` +
	`"replacement_order_id":"30000000-0000-0000-0000-000000000003",` +
	`"guest_order_ref":"30000000-0000-0000-0000-000000000004",` +
	`"organizer_id":"30000000-0000-0000-0000-000000000005",` +
	`"buyer_id":"30000000-0000-0000-0000-000000000006",` +
	`"slot_id":"30000000-0000-0000-0000-000000000007",` +
	`"ticket_type_id":"30000000-0000-0000-0000-000000000008","quantity":2}}`

func TestExchangedEventIsProcessedAndAcked(t *testing.T) {
	var seen exchanged
	c := testConsumer(func(context.Context, FailureEvent) error {
		t.Fatal("a valid event must not publish a failure record")
		return nil
	})
	c.processExchange = func(_ context.Context, e exchanged) (FailureStage, error) { seen = e; return "", nil }
	msg := &fakeMsg{data: []byte(validExchangedJSON), delivery: 1}
	c.handle(context.Background(), msg)

	if len(msg.actions) != 1 || msg.actions[0] != "ack" {
		t.Fatalf("actions = %v, want ack", msg.actions)
	}
	if seen.Data.Quantity != 2 || seen.Data.ExchangeID == uuid.Nil || seen.Data.ReplacementOrderID == uuid.Nil {
		t.Fatalf("decoded payload is wrong: %+v", seen.Data)
	}
	// The SOURCE order's guest reference travels with the event, so the buyer's existing
	// link shows old and new tickets together.
	if seen.Data.GuestOrderRef == uuid.Nil {
		t.Fatal("guest_order_ref did not survive decoding")
	}
}

// AC5. A future variant of THIS subject is parked and latches readiness — it is not a
// failure, and judging its `data` by today's struct would terminate a settled exchange
// whose buyer has already paid the difference.
//
// The renamed-keys case is the one that matters: it decodes cleanly into exchangedData as
// a zero value, so an implementation that decoded before checking the schema would call it
// an invalid contract and terminate it. Parse compatibility is not semantic compatibility
// (ADR-017 §3).
func TestFutureExchangedSchemaIsParkedNotJudged(t *testing.T) {
	cases := map[string]string{
		"renamed keys, changed types": `{"id":"20000000-0000-0000-0000-000000000001","type":"platform.commerce.order.exchanged","schema":2,"data":{"exchange":"e-1","qty":"2"}}`,
		"empty data":                  `{"id":"20000000-0000-0000-0000-000000000001","type":"platform.commerce.order.exchanged","schema":2,"data":{}}`,
		"data not an object":          `{"id":"20000000-0000-0000-0000-000000000001","type":"platform.commerce.order.exchanged","schema":7,"data":[1,2,3]}`,
		"otherwise valid schema 2":    `{"id":"20000000-0000-0000-0000-000000000001","type":"platform.commerce.order.exchanged","schema":2,"data":{"exchange_id":"30000000-0000-0000-0000-000000000001","source_order_id":"30000000-0000-0000-0000-000000000002","replacement_order_id":"30000000-0000-0000-0000-000000000003","guest_order_ref":"30000000-0000-0000-0000-000000000004","organizer_id":"30000000-0000-0000-0000-000000000005","buyer_id":"30000000-0000-0000-0000-000000000006","slot_id":"30000000-0000-0000-0000-000000000007","ticket_type_id":"30000000-0000-0000-0000-000000000008","quantity":2}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			published := false
			c := testConsumer(func(context.Context, FailureEvent) error { published = true; return nil })
			c.processExchange = func(context.Context, exchanged) (FailureStage, error) {
				t.Fatal("a future variant reached processing — it was judged, not parked")
				return "", nil
			}
			msg := &fakeMsg{data: []byte(body), delivery: 1}
			c.handle(context.Background(), msg)
			if len(msg.actions) != 1 || msg.actions[0] != "nak-delay" {
				t.Fatalf("actions = %v, want parked (nak-delay)", msg.actions)
			}
			if published {
				t.Fatal("published a failure record for a future variant — it is not a failure")
			}
			if c.Ready() {
				t.Fatal("readiness not latched false on version skew")
			}
		})
	}
}

// The BOTTOM end of the poison/skew line, which is the half that gets forgotten
// (ADR-017 §5b). `schema <= 0` is a broken envelope, not the future: parking it would NAK
// forever and latch readiness for an event no binary will ever apply.
func TestBrokenExchangedEnvelopeTerminatesAndStaysReady(t *testing.T) {
	cases := map[string]string{
		"schema zero":     `{"id":"20000000-0000-0000-0000-000000000001","type":"platform.commerce.order.exchanged","schema":0,"data":{}}`,
		"schema negative": `{"id":"20000000-0000-0000-0000-000000000001","type":"platform.commerce.order.exchanged","schema":-3,"data":{}}`,
		"no id":           `{"id":"00000000-0000-0000-0000-000000000000","type":"platform.commerce.order.exchanged","schema":1,"data":{}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			published := false
			c := testConsumer(func(context.Context, FailureEvent) error { published = true; return nil })
			msg := &fakeMsg{data: []byte(body), delivery: 1}
			c.handle(context.Background(), msg)
			if len(msg.actions) != 1 || msg.actions[0] != "term" {
				t.Fatalf("actions = %v, want term", msg.actions)
			}
			if !published {
				t.Fatal("a broken envelope must publish its failure record before terminating")
			}
			if !c.Ready() {
				t.Fatal("readiness must NOT be latched by a broken producer")
			}
		})
	}
}

// The two subjects version INDEPENDENTLY. A schema-2 `order.completed` is still parked
// after this ticket, and a schema-1 `order.exchanged` is still processed — one shared
// ceiling would couple them and park a readable event because the other subject moved on.
func TestSubjectCeilingsAreIndependent(t *testing.T) {
	c := testConsumer(func(context.Context, FailureEvent) error { return nil })
	reached := false
	c.processExchange = func(context.Context, exchanged) (FailureStage, error) { reached = true; return "", nil }
	c.process = func(context.Context, completed) (FailureStage, error) {
		t.Fatal("a schema-2 order.completed reached processing")
		return "", nil
	}

	skewed := &fakeMsg{data: bumpSchema(t, 2), delivery: 1}
	c.handle(context.Background(), skewed)
	if len(skewed.actions) != 1 || skewed.actions[0] != "nak-delay" {
		t.Fatalf("completed skew actions = %v, want parked", skewed.actions)
	}

	c.ready.Store(true)
	fine := &fakeMsg{data: []byte(validExchangedJSON), delivery: 1}
	c.handle(context.Background(), fine)
	if !reached {
		t.Fatal("a schema-1 exchange was not processed")
	}
	if len(fine.actions) != 1 || fine.actions[0] != "ack" {
		t.Fatalf("exchange actions = %v, want ack", fine.actions)
	}
}

// An unknown subject is still an invalid contract, terminated without touching readiness.
// Adding a second arm must not turn the dispatch into "anything goes".
func TestUnknownSubjectIsStillRefused(t *testing.T) {
	published := false
	c := testConsumer(func(context.Context, FailureEvent) error { published = true; return nil })
	msg := &fakeMsg{data: []byte(`{"id":"20000000-0000-0000-0000-000000000001","type":"platform.commerce.order.cancelled","schema":1,"data":{}}`), delivery: 1}
	c.handle(context.Background(), msg)
	if len(msg.actions) != 1 || msg.actions[0] != "term" {
		t.Fatalf("actions = %v, want term", msg.actions)
	}
	if !published || !c.Ready() {
		t.Fatalf("published=%t ready=%t — an unknown subject is a contract failure, not skew", published, c.Ready())
	}
}

// A payload missing its required fields is a contract failure of a KNOWN variant, and is
// terminated after its retries — not parked.
func TestInvalidExchangedPayloadIsRefused(t *testing.T) {
	published := false
	c := testConsumer(func(context.Context, FailureEvent) error { published = true; return nil })
	msg := &fakeMsg{data: []byte(`{"id":"20000000-0000-0000-0000-000000000001","type":"platform.commerce.order.exchanged","schema":1,"data":{"quantity":0}}`), delivery: 1}
	c.handle(context.Background(), msg)
	if len(msg.actions) != 1 || msg.actions[0] != "term" {
		t.Fatalf("actions = %v, want term", msg.actions)
	}
	if !published {
		t.Fatal("an invalid known-variant payload must publish its failure record")
	}
}

// ai-review F2, and the half of it that is false.
//
// The finding says a callback outage past `MaxProcessAttempts` strands capacity
// "indefinitely" because nothing scans for the unfinished obligation. The scan does not
// exist — that part is true — but the recovery does: the terminal failure record and its
// documented republish converge, and this test is why.
//
// A republished event finds its `consumed_events` receipt, so SwitchExchange is a no-op —
// and the callback runs ANYWAY, because processExchanged does not branch on whether the
// switch was fresh. That is what makes the manual procedure sufficient. If someone ever
// "optimises" the replay path by returning early on a receipt hit, capacity really would
// be stranded forever, and this test is what fails.
func TestReplayedExchangeStillDrivesTheCallback(t *testing.T) {
	var switches, callbacks int
	c := testConsumer(func(context.Context, FailureEvent) error { return nil })
	c.processExchange = nil
	c.switchExchange = func(context.Context, exchanged) error { switches++; return nil }
	c.notifyExchangeSwitched = func(context.Context, exchanged) error { callbacks++; return nil }
	c.deliverOrder = func(context.Context, uuid.UUID) error { return nil }

	for i := 0; i < 2; i++ {
		msg := &fakeMsg{data: []byte(validExchangedJSON), delivery: uint64(i + 1)}
		c.handle(context.Background(), msg)
		if len(msg.actions) != 1 || msg.actions[0] != "ack" {
			t.Fatalf("delivery %d actions = %v, want ack", i+1, msg.actions)
		}
	}
	if switches != 2 || callbacks != 2 {
		t.Fatalf("switches=%d callbacks=%d — a redelivery must re-drive the callback, which is what makes the documented republish converge", switches, callbacks)
	}
}

// The callback failing is a RETRY, not a switch failure. The switch has committed by then,
// so the message must stay unacknowledged rather than be acked as done.
func TestCallbackFailureLeavesTheMessageUnacknowledged(t *testing.T) {
	c := testConsumer(func(context.Context, FailureEvent) error { return nil })
	c.processExchange = nil
	c.switchExchange = func(context.Context, exchanged) error { return nil }
	c.notifyExchangeSwitched = func(context.Context, exchanged) error { return errors.New("commerce down") }
	c.deliverOrder = func(context.Context, uuid.UUID) error {
		t.Fatal("delivery ran despite an unfinished capacity return")
		return nil
	}
	msg := &fakeMsg{data: []byte(validExchangedJSON), delivery: 1}
	c.handle(context.Background(), msg)
	if len(msg.actions) != 0 {
		t.Fatalf("actions = %v, want the message left unacknowledged for the backoff schedule", msg.actions)
	}
}

// A permanent refusal must not be reported as exhaustion. The two say different things to
// whoever reads the failure record: exhaustion means "a dependency was broken, retry it",
// while this means "the exchange is settled, it will never switch, decide what the buyer
// gets". Terminating on the first delivery is the point — nothing about an admission that
// already happened will be different in 36 seconds.
func TestPermanentExchangeRefusalTerminatesImmediately(t *testing.T) {
	for name, cause := range map[string]error{
		"already admitted": store.ErrSourceTicketsAlreadyAdmitted,
		"already voided":   store.ErrSourceTicketsAlreadyVoided,
	} {
		t.Run(name, func(t *testing.T) {
			var record FailureEvent
			c := testConsumer(func(_ context.Context, e FailureEvent) error { record = e; return nil })
			c.processExchange = nil
			c.switchExchange = func(context.Context, exchanged) error { return fmt.Errorf("switch: %w", cause) }
			c.notifyExchangeSwitched = func(context.Context, exchanged) error {
				t.Fatal("the callback ran after a refused switch — capacity must not be returned")
				return nil
			}
			// delivery 1: the FIRST attempt, far below maxProcessAttempts.
			msg := &fakeMsg{data: []byte(validExchangedJSON), delivery: 1}
			c.handle(context.Background(), msg)
			if len(msg.actions) != 1 || msg.actions[0] != "term" {
				t.Fatalf("actions = %v, want term on the first delivery", msg.actions)
			}
			if record.Data.Reason != ReasonExchangeRefused {
				t.Fatalf("reason = %q, want %q — exhaustion would misdirect the operator", record.Data.Reason, ReasonExchangeRefused)
			}
			if !c.Ready() {
				t.Fatal("a refused exchange must not latch readiness")
			}
		})
	}
}

// ai-review pass 3 F1, the half that is closeable without a round trip.
//
// The envelope id is derived from the exchange id, so a payload naming exchange A while
// carrying another exchange's orders is self-inconsistent and detectable locally. Without
// this check `exchange_id` is a field the message merely asserts about itself: access
// would void the named source order and then use its internal credential to report a
// DIFFERENT exchange switched, releasing that exchange's capacity.
// Same payload, an id that belongs to no exchange in it.
const mismatchedExchangedJSON = `{"id":"20000000-0000-0000-0000-000000000001",` +
	`"type":"platform.commerce.order.exchanged","occurred_at":"2026-07-31T10:00:00Z","schema":1,` +
	`"data":{"exchange_id":"30000000-0000-0000-0000-000000000001",` +
	`"source_order_id":"30000000-0000-0000-0000-000000000002",` +
	`"replacement_order_id":"30000000-0000-0000-0000-000000000003",` +
	`"guest_order_ref":"30000000-0000-0000-0000-000000000004",` +
	`"organizer_id":"30000000-0000-0000-0000-000000000005",` +
	`"buyer_id":"30000000-0000-0000-0000-000000000006",` +
	`"slot_id":"30000000-0000-0000-0000-000000000007",` +
	`"ticket_type_id":"30000000-0000-0000-0000-000000000008","quantity":2}}`

func TestExchangedEventMustDeriveFromItsExchange(t *testing.T) {
	published := false
	c := testConsumer(func(context.Context, FailureEvent) error { published = true; return nil })
	c.processExchange = func(context.Context, exchanged) (FailureStage, error) {
		t.Fatal("a mismatched event id reached processing")
		return "", nil
	}
	// Every field is well-formed; only the pairing is wrong — which is exactly the shape a
	// confused-deputy message has, and exactly what a non-nil check cannot see.
	msg := &fakeMsg{data: []byte(mismatchedExchangedJSON), delivery: 1}
	c.handle(context.Background(), msg)
	if len(msg.actions) != 1 || msg.actions[0] != "term" {
		t.Fatalf("actions = %v, want term", msg.actions)
	}
	if !published {
		t.Fatal("a mismatched event must publish its failure record")
	}
}

// And the honest producer's event passes, which is what stops the check above from being
// satisfiable by rejecting everything. The id here is derived, not hand-picked.
func TestDerivedExchangedEventIsAccepted(t *testing.T) {
	exchangeID := uuid.MustParse("30000000-0000-0000-0000-000000000001")
	body := `{"id":"` + exchangedEventID(exchangeID).String() + `",` +
		`"type":"platform.commerce.order.exchanged","occurred_at":"2026-07-31T10:00:00Z","schema":1,` +
		`"data":{"exchange_id":"` + exchangeID.String() + `",` +
		`"source_order_id":"30000000-0000-0000-0000-000000000002",` +
		`"replacement_order_id":"30000000-0000-0000-0000-000000000003",` +
		`"guest_order_ref":"30000000-0000-0000-0000-000000000004",` +
		`"organizer_id":"30000000-0000-0000-0000-000000000005",` +
		`"buyer_id":"30000000-0000-0000-0000-000000000006",` +
		`"slot_id":"30000000-0000-0000-0000-000000000007",` +
		`"ticket_type_id":"30000000-0000-0000-0000-000000000008","quantity":2}}`
	reached := false
	c := testConsumer(func(context.Context, FailureEvent) error { return nil })
	c.processExchange = func(context.Context, exchanged) (FailureStage, error) { reached = true; return "", nil }
	msg := &fakeMsg{data: []byte(body), delivery: 1}
	c.handle(context.Background(), msg)
	if !reached || len(msg.actions) != 1 || msg.actions[0] != "ack" {
		t.Fatalf("a correctly derived event was refused: reached=%t actions=%v", reached, msg.actions)
	}
}
