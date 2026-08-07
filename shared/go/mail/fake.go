package mail

import (
	"context"
	"errors"
	"sync"
)

// Fake is the offline sender. It performs no I/O, keeps every message it accepted, and
// is what `make up`, the smoke stack and the gate all run against (ADR-032's rule that
// the fake is the default and a configured provider is what selects the real one).
//
// It is in shared/go rather than a _test.go file because it is PRODUCTION wiring for the
// local stack: commerce's main.go selects it when no provider is configured, which is
// every environment this repo currently has. A fake that only tests can construct would
// leave `make up` with no sender at all.
type Fake struct {
	mu   sync.Mutex
	sent []Message
	// failWith, when non-nil, is returned instead of accepting. It exists so the
	// drainer's retry, backoff and dead-letter paths can be driven without a broken
	// provider — those paths are the reason the outbox exists, and an implementation
	// that cannot fail cannot exercise them.
	failWith error
}

// NewFake returns an empty capturing sender.
func NewFake() *Fake { return &Fake{} }

// Send validates and captures. It returns the configured failure, if any, WITHOUT
// capturing the message — a sender that both fails and records having sent would make
// `Sent()` mean two different things and would let a test assert delivery of a message
// that was refused.
func (f *Fake) Send(_ context.Context, m Message) error {
	if err := m.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return f.failWith
	}
	f.sent = append(f.sent, m)
	return nil
}

// Sent returns a COPY of everything accepted, oldest first. A copy because the caller
// is usually a test asserting on it while the drainer's goroutine is still running;
// handing out the backing slice makes that a data race.
func (f *Fake) Sent() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.sent...)
}

// FailWith makes every subsequent Send return err. Passing nil restores acceptance.
func (f *Fake) FailWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = err
}

// ErrFakeRefused is a stand-in provider failure for tests that only need "the send
// failed" and do not care why.
var ErrFakeRefused = errors.New("mail: fake sender refused")
