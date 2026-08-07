// Package mail defines the provider-agnostic transactional-mail port and its offline
// fake (TKT-226, ADR-050).
//
// It lives in shared/go for the reason shared/go/fakepsp does: the port's offline
// implementation is what keeps the gate hermetic, and more than one module has the
// concept. `services/access/internal/consumer.Mailer` is the pre-existing instance —
// one interface whose only implementation logs a hash of the recipient and returns
// nil. ADR-050 records that migrating access onto this port is what a real provider
// forces, not what TKT-226 does: access's Send carries a delivery id and a link with
// no subject and no body, so adapting it means composing ticket-delivery copy, which
// is a behaviour change to a path this ticket does not touch.
//
// So: this repo has had a mail PORT since ticket issuance shipped, and has never had
// a SENDER. ADR-049's "no SMTP, no queue, no provider" is exactly true about senders
// and was read, by this ticket's own plan draft, as true about ports.
package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Message is one transactional message. Deliberately minimal: a recipient, a subject
// and a plain-text body.
//
// No HTML alternative, no attachments, no template id, no reply-to, no cc. Those are
// the fields a marketing sender needs, and TKT-226's non-goals name marketing mail,
// templates and branding. A provider that needs more gets more when something asks
// for it.
//
// The body is assembled by the caller, which is what keeps this package free of any
// knowledge about password resets, tickets, or anything else the system mails.
type Message struct {
	// To is a single recipient. Not a slice: every message this system sends is
	// addressed to one buyer, and a slice would invite a bulk path that the
	// per-message outbox row does not model.
	To string
	// Subject and Body are already-composed, already-localized text.
	Subject string
	Body    string
}

// ErrInvalidMessage reports a message no provider could accept. Returned by Validate
// and by every Sender before it attempts delivery, so a malformed message fails at the
// same point regardless of implementation.
var ErrInvalidMessage = errors.New("mail: invalid message")

// Validate refuses a message that cannot be delivered.
//
// It is called by implementations rather than by callers, deliberately: a validation
// that only the caller runs is a validation the next caller forgets. The Fake runs it
// too, so the offline gate refuses exactly what a real provider would rather than
// silently capturing garbage that would fail in production.
//
// Header injection is the reason the recipient is checked for newlines. A recipient
// carrying CR or LF can terminate the To: header and append arbitrary ones (Bcc:, a
// second To:) in any SMTP-shaped provider. This is refused at the port so no
// implementation can forget it, and it is refused rather than sanitized: an address
// containing a newline is not a typo to repair, it is an attempt.
func (m Message) Validate() error {
	switch {
	case strings.TrimSpace(m.To) == "":
		return fmt.Errorf("%w: empty recipient", ErrInvalidMessage)
	case strings.ContainsAny(m.To, "\r\n"):
		return fmt.Errorf("%w: recipient contains a line break", ErrInvalidMessage)
	case strings.TrimSpace(m.Subject) == "":
		return fmt.Errorf("%w: empty subject", ErrInvalidMessage)
	case strings.ContainsAny(m.Subject, "\r\n"):
		return fmt.Errorf("%w: subject contains a line break", ErrInvalidMessage)
	case m.Body == "":
		return fmt.Errorf("%w: empty body", ErrInvalidMessage)
	}
	return nil
}

// Sender delivers one message. The single operation is the whole port.
//
// A returned error means "delivery did not happen and may be retried". There is no
// third answer, and specifically there is no equivalent of the PSP port's `unknown`
// outcome (ADR-032): a duplicate email is an annoyance, a duplicate capture is money,
// so mail can be at-least-once without the ceremony payments needs. The caller's
// retry is safe by that argument and by nothing else — a Sender that develops a
// side effect worse than a duplicate message invalidates it.
type Sender interface {
	Send(ctx context.Context, m Message) error
}
