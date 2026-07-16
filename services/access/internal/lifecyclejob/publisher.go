package lifecyclejob

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// JetStreamPublisher publishes owed alarms. Access has only ever consumed
// events, so this is its first producer — deliberately the same shape as
// commerce's (services/commerce/internal/events), including transmitting the
// frozen envelope rather than rebuilding it: a rebuilt payload would vary with
// retry timing while the message id stayed fixed.
type JetStreamPublisher struct{ js jetstream.JetStream }

func NewJetStreamPublisher(js jetstream.JetStream) *JetStreamPublisher {
	return &JetStreamPublisher{js: js}
}

func (p *JetStreamPublisher) PublishRaw(ctx context.Context, subject string, eventID uuid.UUID, envelope []byte) error {
	msg := &nats.Msg{Subject: subject, Data: envelope, Header: nats.Header{"Nats-Msg-Id": []string{eventID.String()}}}
	if _, err := p.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

// RequireAlarmRoute checks at startup that the operator's durable consumer
// exists and filters the alarm subject, and refuses to start if it does not.
//
// This is ADR-021 §D6's "an unmonitored deployment must not run this scheme in
// fail-open", enforced where it belongs. It is a boot-time check, not a
// readiness probe, and the difference matters: Access's container healthcheck
// probes /readyz, so a continuous check here would let a broker hiccup take
// every turnstile offline — denying real customers, which is the exact failure
// §D6 chose fail-open to avoid. A misconfigured deployment never starts; a
// running one is not hostage to NATS.
func RequireAlarmRoute(ctx context.Context, js jetstream.JetStream, stream, durable, subject string) error {
	if durable == "" {
		return fmt.Errorf("ACCESS_LIFECYCLE_ALARM_DURABLE is required: fail-open (ADR-021 §D6) is only safe while integrity alarms reach an operator")
	}
	s, err := js.Stream(ctx, stream)
	if err != nil {
		return fmt.Errorf("integrity alarm stream %q: %w", stream, err)
	}
	c, err := s.Consumer(ctx, durable)
	if err != nil {
		return fmt.Errorf("integrity alarm durable %q on stream %q: %w", durable, stream, err)
	}
	info, err := c.Info(ctx)
	if err != nil {
		return fmt.Errorf("integrity alarm durable %q: %w", durable, err)
	}
	filters := info.Config.FilterSubjects
	if info.Config.FilterSubject != "" {
		filters = append(filters, info.Config.FilterSubject)
	}
	for _, f := range filters {
		if f == subject {
			return nil
		}
	}
	return fmt.Errorf("integrity alarm durable %q does not filter %q (filters: %v)", durable, subject, filters)
}
