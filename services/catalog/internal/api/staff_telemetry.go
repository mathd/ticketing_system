package api

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Abuse telemetry for the anonymous staff-login limiter (TKT-195, ADR-042).
//
// # Why this exists
//
// The limiter shipped operationally silent: it refused requests and said so to
// nobody. An operator could not tell a quiet morning from a limiter holding back
// a credential-stuffing run, because both look identical from outside — no
// metric, no event, just 429s the attacker sees and the operator does not.
//
// This refuses nothing on its own. It makes the control that already exists
// legible, which is the same job `access`'s scanner telemetry does for a surface
// that is deliberately unlimited.
//
// # Vocabulary, borrowed on purpose
//
// `services/access/internal/api/scanner_telemetry.go` (TKT-272) chose generic
// field names — `surface`, `subject_type` — precisely so this ticket would not
// invent a second scheme. Same names, same meanings, different `surface` value.
//
// # The field that is deliberately ABSENT
//
// The scanner emits `subject_id`: an AUTHENTICATED device UUID, which is a safe
// non-secret identifier and the exact input `access revoke-scanner` takes. This
// surface has no such value. It is anonymous by construction, and its only
// candidate identifier is the login identifier — attacker-chosen, and forbidden
// in every sink by this ticket's COS.
//
// A hash or a truncation of it is not a way round that. It would still be one
// series per attacker-chosen input, so an attacker would control metric
// cardinality directly, and it identifies nothing an operator can act on: you
// cannot revoke a hash. So there is no `subject_id` here, and the honest
// consequence is written into ADR-042 rather than hidden: this telemetry answers
// "is this surface under pressure, and which budget is firing", never "who".
//
// # Where each field is allowed to go (ADR-012 § TKT-202)
//
// Every value emitted here is one of a fixed set of constants. Nothing derived
// from the request — not the identifier, not the normalized identifier, not the
// password, not the client address — reaches any sink. That is why all three
// sinks can carry the same attributes: there is no value among them that is safe
// in one and unsafe in another.
const (
	// staffLoginAbuseMessage is the log message an operator greps for.
	staffLoginAbuseMessage = "abuse.request"

	// staffLoginAttemptsMetric counts limiter decisions by outcome.
	staffLoginAttemptsMetric = "catalog.staff_login.attempts"

	// staffLoginSurface names this route in every sink. One constant, so the log
	// and the metric cannot drift apart.
	staffLoginSurface = "staff_login"

	// staffLoginSubject says what kind of thing this surface is about, so a reader
	// comparing these records with access's can tell the two apart. There is no
	// subject_id to go with it, and that is the point: see the note above.
	staffLoginSubject = "anonymous_login_identifier"
)

// The outcome of one limiter decision. ONE attribute rather than an `outcome`
// plus a separate `limiter`, because two fields encoding one fact can disagree:
// `outcome=allowed` with `limiter=subject` is meaningless, and a dashboard built
// on either field would silently diverge from one built on the other. Three
// values, three series, no combination that contradicts itself.
const (
	staffLoginAllowed          = "allowed"
	staffLoginThrottledSource  = "throttled_source"
	staffLoginThrottledSubject = "throttled_subject"
)

// staffLoginTelemetry emits one record per limiter decision.
type staffLoginTelemetry struct {
	log      *slog.Logger
	attempts metric.Int64Counter
}

// NewStaffLoginTelemetry builds the emitter a service main wires in. Exported
// because main must both construct it and register its counter on the real
// meter — the same shape as access's NewScannerTelemetry.
func NewStaffLoginTelemetry(log *slog.Logger, meter metric.Meter) (*staffLoginTelemetry, error) {
	t := &staffLoginTelemetry{log: log}
	if err := t.ObserveMetrics(meter); err != nil {
		return nil, err
	}
	return t, nil
}

// ObserveMetrics registers the counter.
//
// Observability, not a gate: nothing here can make catalog unready, and a login
// attempt is never refused — nor allowed — because a counter could not be
// registered.
func (t *staffLoginTelemetry) ObserveMetrics(meter metric.Meter) error {
	attempts, err := meter.Int64Counter(staffLoginAttemptsMetric,
		metric.WithDescription("Staff-login limiter decisions, by outcome: allowed, throttled_source or throttled_subject. Aggregate only - this surface is anonymous, so there is deliberately no per-subject label and no way to attribute a rise to one caller. A sustained rise in throttled_subject means one account is being ground; in throttled_source, one client is walking a list. Neither identifies who: the control is the limiter itself, and this counter is how you see it working."))
	if err != nil {
		return err
	}
	t.attempts = attempts
	return nil
}

// observeDecision records one limiter decision.
//
// Called for EVERY decision including the allowed ones, deliberately. Counting
// only refusals would make "no telemetry" and "everything allowed" indis-
// tinguishable, which is precisely the question an operator is asking.
//
// outcome is one of the three constants above. Nothing else reaches this
// function: it takes no identifier, no address and no request, so there is no
// path by which a request-controlled value could be emitted, by accident or by
// a later edit. That is a stronger guarantee than remembering not to log one.
func (t *staffLoginTelemetry) observeDecision(ctx context.Context, outcome string) {
	if t == nil {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("surface", staffLoginSurface),
		attribute.String("subject_type", staffLoginSubject),
		attribute.String("outcome", outcome),
	}

	if t.attempts != nil {
		t.attempts.Add(ctx, 1, metric.WithAttributes(attrs...))
	}

	trace.SpanFromContext(ctx).SetAttributes(attrs...)

	if t.log != nil {
		t.log.InfoContext(ctx, staffLoginAbuseMessage,
			"surface", staffLoginSurface,
			"subject_type", staffLoginSubject,
			"outcome", outcome,
		)
	}
}
