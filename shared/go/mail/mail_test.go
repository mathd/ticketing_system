package mail

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidateRefusesUndeliverableMessages(t *testing.T) {
	ok := Message{To: "buyer@example.test", Subject: "Reset your password", Body: "link"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a complete message must validate, got %v", err)
	}

	// Each case names the property, so a failure says which rule stopped holding.
	for _, tc := range []struct {
		name string
		m    Message
	}{
		{"empty recipient", Message{To: "", Subject: "s", Body: "b"}},
		{"blank recipient", Message{To: "   ", Subject: "s", Body: "b"}},
		{"empty subject", Message{To: "a@b.test", Subject: "", Body: "b"}},
		{"blank subject", Message{To: "a@b.test", Subject: " \t", Body: "b"}},
		{"empty body", Message{To: "a@b.test", Subject: "s", Body: ""}},
		// Header injection. A recipient or subject carrying CR/LF can terminate its
		// header and append others (a second To:, a Bcc:) in any SMTP-shaped provider.
		{"recipient with LF", Message{To: "a@b.test\nBcc: attacker@evil.test", Subject: "s", Body: "b"}},
		{"recipient with CR", Message{To: "a@b.test\rBcc: attacker@evil.test", Subject: "s", Body: "b"}},
		{"subject with LF", Message{To: "a@b.test", Subject: "s\nBcc: attacker@evil.test", Body: "b"}},
		{"subject with CR", Message{To: "a@b.test", Subject: "s\rBcc: attacker@evil.test", Body: "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.m.Validate()
			if !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("%s must be ErrInvalidMessage, got %v", tc.name, err)
			}
		})
	}
}

// The body is deliberately NOT newline-checked: a body is where line breaks belong.
// Pinned as a test so a later "consistency" pass does not add the check and break
// every multi-line message.
func TestValidateAllowsLineBreaksInTheBody(t *testing.T) {
	m := Message{To: "a@b.test", Subject: "s", Body: "line one\nline two\r\nline three"}
	if err := m.Validate(); err != nil {
		t.Fatalf("a multi-line body is legitimate, got %v", err)
	}
}

func TestFakeCapturesAcceptedMessagesInOrder(t *testing.T) {
	f := NewFake()
	if got := f.Sent(); len(got) != 0 {
		t.Fatalf("a new fake has sent nothing, got %d", len(got))
	}
	first := Message{To: "one@example.test", Subject: "first", Body: "b1"}
	second := Message{To: "two@example.test", Subject: "second", Body: "b2"}
	for _, m := range []Message{first, second} {
		if err := f.Send(context.Background(), m); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	got := f.Sent()
	if len(got) != 2 {
		t.Fatalf("want 2 captured, got %d", len(got))
	}
	if got[0] != first || got[1] != second {
		t.Fatalf("captured messages must be intact and oldest-first, got %+v", got)
	}
}

// The whole point of the fake: a test can assert what WOULD have been sent. If Send
// mutated or dropped the recipient or body, this is what notices.
func TestFakeDoesNotMutateWhatItCaptured(t *testing.T) {
	f := NewFake()
	m := Message{To: "buyer@example.test", Subject: "Reset your password", Body: "https://x.test/r?token=abc"}
	if err := f.Send(context.Background(), m); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := f.Sent()[0]
	if got.To != m.To || got.Subject != m.Subject || got.Body != m.Body {
		t.Fatalf("captured message differs from the one sent:\n sent %+v\n got  %+v", m, got)
	}
	if !strings.Contains(got.Body, "token=abc") {
		t.Fatalf("the body a test needs to read must survive capture, got %q", got.Body)
	}
}

// Sent() must hand out a copy. Without it, a caller appending to the returned slice
// (or reading it while the drainer sends) races the fake's own state.
func TestSentReturnsACopy(t *testing.T) {
	f := NewFake()
	if err := f.Send(context.Background(), Message{To: "a@b.test", Subject: "s", Body: "b"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := f.Sent()
	got[0].To = "mutated@example.test"
	if f.Sent()[0].To != "a@b.test" {
		t.Fatal("mutating the slice Sent() returned changed the fake's own record")
	}
}

func TestFakeReturnsTheConfiguredFailureAndCapturesNothing(t *testing.T) {
	f := NewFake()
	f.FailWith(ErrFakeRefused)
	err := f.Send(context.Background(), Message{To: "a@b.test", Subject: "s", Body: "b"})
	if !errors.Is(err, ErrFakeRefused) {
		t.Fatalf("want ErrFakeRefused, got %v", err)
	}
	// A refused send must not be recorded as sent, or `Sent()` would mean both
	// "accepted" and "attempted" and a test could assert delivery of a refusal.
	if got := f.Sent(); len(got) != 0 {
		t.Fatalf("a refused send must not be captured, got %d", len(got))
	}
	f.FailWith(nil)
	if err := f.Send(context.Background(), Message{To: "a@b.test", Subject: "s", Body: "b"}); err != nil {
		t.Fatalf("clearing the failure must restore acceptance, got %v", err)
	}
	if got := f.Sent(); len(got) != 1 {
		t.Fatalf("want 1 captured after recovery, got %d", len(got))
	}
}

// The fake refuses exactly what a provider would. Without this the offline gate would
// happily capture a message that fails on contact with a real sender.
func TestFakeValidatesBeforeCapturing(t *testing.T) {
	f := NewFake()
	err := f.Send(context.Background(), Message{To: "a@b.test\nBcc: attacker@evil.test", Subject: "s", Body: "b"})
	if !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("want ErrInvalidMessage, got %v", err)
	}
	if got := f.Sent(); len(got) != 0 {
		t.Fatalf("an invalid message must not be captured, got %d", len(got))
	}
}
