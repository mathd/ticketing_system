package lifecyclejob

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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

// RequireAlarmRoute checks at startup that a durable consumer exists and filters
// the alarm subject, and refuses to start if it does not.
//
// # What this proves, and what it does not
//
// It proves this class's alarm has somewhere durable to land: the subject is
// bound to a consumer, so an alarm is RETAINED for an operator to collect rather
// than published into a void and dropped.
//
// It does NOT prove anyone is reading it, and this repository ships no consumer
// for it — `nats-init` creates the durable and nothing drains it. So a default
// deployment passes this check and is still unmonitored. An earlier version of
// this comment said it made an unmonitored deployment unable to start; that was
// false, and it was the same overclaim ADR-021 §The trust boundary exists to
// stop — a control described by its intent rather than its reach. ADR-021 §D6's
// "an unmonitored deployment must not run this scheme in fail-open" is a
// DEPLOYMENT obligation. This code cannot discharge it: no boot-time check can
// prove a human will act on a page.
//
// The closest runtime signal is ObserveAlarmRoute's pending depth — a durable
// nobody drains accumulates, which is observable. Alert on it.
//
// Boot-time rather than readiness is still deliberate: Access's container
// healthcheck probes /readyz, so a continuous check here would let a broker
// hiccup take every turnstile offline — denying real customers, which is the
// exact failure §D6 chose fail-open to avoid.
// AlarmRoute names one alarm class's boot-checked route.
//
// A struct rather than more positional strings: this helper serves THREE classes, each
// with its own durable, subject and environment variable, and the signature already
// carried three adjacent strings. A fourth would be a swap the compiler cannot catch, and
// the whole point of TKT-119 is that these values were being reported wrongly. Named
// fields make the class/env/subject association reviewable at each call site.
//
// DurableEnv is the variable an operator must set — it belongs in the "is required"
// sentence, which is the only message that asks for an action. Class names the alarm class
// for the broker-lookup failures, which need to say WHICH route is broken.
type AlarmRoute struct {
	Stream     string
	Durable    string
	Subject    string
	DurableEnv string
	Class      string
}

func RequireAlarmRoute(ctx context.Context, js jetstream.JetStream, route AlarmRoute) error {
	stream, durable, subject := route.Stream, route.Durable, route.Subject
	if durable == "" {
		return fmt.Errorf("%s is required: fail-open (ADR-021 §D6) needs somewhere durable for %s alarms to land", route.DurableEnv, route.Class)
	}
	s, err := js.Stream(ctx, stream)
	if err != nil {
		return fmt.Errorf("%s alarm stream %q: %w", route.Class, stream, err)
	}
	c, err := s.Consumer(ctx, durable)
	if err != nil {
		return fmt.Errorf("%s alarm durable %q on stream %q: %w", route.Class, durable, stream, err)
	}
	info, err := c.Info(ctx)
	if err != nil {
		return fmt.Errorf("%s alarm durable %q: %w", route.Class, durable, err)
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
	return fmt.Errorf("%s alarm durable %q does not filter %q (filters: %v)", route.Class, durable, subject, filters)
}

// ObserveAlarmRoute registers the operator durable's pending depth.
//
// This is the only evidence this repository can offer that alarms are actually
// being COLLECTED rather than merely stored. RequireAlarmRoute proves an inbox
// exists; nothing proves a reader exists — but a durable nobody drains
// accumulates, and that is observable. Sustained non-zero pending means the
// alarms are piling up unread, which is ADR-021 §D6's forbidden unmonitored
// deployment, visible at last.
//
// It is a metric and not a gate, like everything else here: a broker that has
// stopped being drained is an operations failure, not a reason to close a
// turnstile on paying customers.
func ObserveAlarmRoute(meter metric.Meter, js jetstream.JetStream, stream, durable string) error {
	pending, err := meter.Int64ObservableGauge("access.lifecycle.alarm.durable_pending",
		metric.WithDescription("Alarms sitting unread in an operator durable, per the durable attribute (integrity, admission-conflict and policy-conflict classes). Sustained non-zero means nobody is collecting them: alarms are retained but unmonitored, which ADR-021 §D6 forbids for a fail-open deployment."))
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		s, err := js.Stream(ctx, stream)
		if err != nil {
			return nil // The route is validated at boot; a transient broker error is not a metric.
		}
		c, err := s.Consumer(ctx, durable)
		if err != nil {
			return nil
		}
		info, err := c.Info(ctx)
		if err != nil {
			return nil
		}
		// The durable is a series attribute: three alarm classes (integrity,
		// admission-conflict, policy-conflict) each register this gauge, and
		// without it their observations would collide.
		o.ObserveInt64(pending, int64(info.NumPending), metric.WithAttributes(attribute.String("durable", durable)))
		return nil
	}, pending)
	return err
}
