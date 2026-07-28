package consumer

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// Wire-compatibility golden for the ticket-issuance failure record (TKT-126).
//
// Captured from the PRE-refactor emitter — this package's own `FailureEvent`
// struct — and committed before the shared envelope package existed (ADR-017 §5b′: a
// fixture built from the type under test cannot fail). Reviewer's check: at the
// commit introducing this file, the shared package does not exist yet —
// `git show <that-commit>:shared/go/domainevent/envelope.go` must fail.
//
// The event is built through the real derivation (failureRecord: deterministic
// id over subject+identity+reason, SHA-256 message fingerprint, the
// PII-free/body-free payload TKT-74 pinned) with only `occurred_at` overridden,
// since that is the single non-deterministic field on the path.
func TestWireGoldenFailureEvent(t *testing.T) {
	sourceID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	event := failureRecord([]byte(`{"poison":true}`), sourceID, StageContract, ReasonInvalidContract, 3)
	event.OccurredAt = time.Date(2026, 7, 20, 12, 34, 56, 123456789, time.UTC)

	body, err := failureEnvelope(event)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"dbdc749b-ff62-5f34-9e72-90155503fa45","type":"platform.access.ticket-issuance.failed","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":1,"data":{"source_event_id":"11111111-1111-4111-8111-111111111111","message_fingerprint":"fd1434c9ef6557e7dd12a047f0955336283f6f84a6361019da3eff4dea7797e6","reason":"invalid_contract","stage":"contract","attempts":3}}`
	if string(body) != want {
		t.Fatalf("wire bytes changed (TKT-126 forbids it)\n got: %s\nwant: %s", body, want)
	}
}
