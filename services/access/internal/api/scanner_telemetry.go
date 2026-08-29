package api

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Abuse telemetry for the scanner polling surface (TKT-272, ADR-066).
//
// # Why this exists
//
// The voided-ticket feed is deliberately NOT rate limited. The control is
// targeted device revocation — `access revoke-scanner <device-id>` — which is
// permanent and precise where an in-process per-replica limiter is neither.
// But revocation takes a device id, so an operator who cannot SEE which device
// is polling has no input to the only control there is. This telemetry is what
// makes the chosen control usable; it refuses nothing on its own.
//
// # Vocabulary, shared on purpose
//
// TKT-195 owes the same visibility for catalog's limiter and has shipped none.
// Rather than inventing a second scheme later, the field names here are
// deliberately generic — `surface`, `subject_type`, `subject_id` — so catalog
// can emit the same record about a different subject (there, whatever safe
// non-secret identifier its credential-free surface has) without a second
// vocabulary. `surface` is the discriminator; nothing here is access-specific
// except the two constant values.
//
// # Where each field is allowed to go (ADR-012 § TKT-202)
//
// The device TOKEN never appears anywhere: not a log line, not a metric label,
// not a span attribute. Only the device's UUID identifies the subject, and even
// that is placed carefully:
//
//   - LOG: carries `subject_id`. This is the operator's path, and the one sink
//     that must not be lossy.
//   - METRIC: `surface` and `subject_type` only. A UUID label is one series per
//     enrolled device — unbounded cardinality against a value that grows with
//     the fleet. The counter answers "is this surface being hammered"; the log
//     answers "by which device".
//   - SPAN: `surface` and `subject_type` only, for trace filtering. NOT the id:
//     spans are sampled, and a sampler must not decide whether an operator can
//     see an abusive device. Spans also leave the process for a collector this
//     repo does not control (see obs/capability_span.go).
const (
	// abuseRequestMessage is the log message an operator greps for.
	abuseRequestMessage = "abuse.request"

	// abuseRequestsMetric is the aggregate counter. Deliberately not per-device.
	abuseRequestsMetric = "access.abuse.requests"

	// feedSurface names this route in both sinks. One constant, so the log and
	// the metric cannot drift apart.
	feedSurface = "scanner_voided_ticket_feed"

	// scannerDeviceSubject says what kind of thing subject_id identifies, so a
	// reader of catalog's future records can tell the two apart.
	scannerDeviceSubject = "scanner_device"
)

// scannerTelemetry emits one abuse record per authenticated feed request.
type scannerTelemetry struct {
	log      *slog.Logger
	requests metric.Int64Counter
}

func newScannerTelemetry(log *slog.Logger) *scannerTelemetry {
	return &scannerTelemetry{log: log}
}

// NewScannerTelemetry builds the emitter a service main wires in. Exported
// because main must both construct it and register its counter on the real
// meter — the same shape as the lifecycle jobs' ObserveMetrics beside it.
func NewScannerTelemetry(log *slog.Logger, meter metric.Meter) (*scannerTelemetry, error) {
	t := newScannerTelemetry(log)
	if err := t.ObserveMetrics(meter); err != nil {
		return nil, err
	}
	return t, nil
}

// ObserveMetrics registers the aggregate counter, following the shape of
// commerce/internal/reversal and access's own lifecycle jobs.
//
// Like those, this is observability and not a gate: nothing here can make
// access unready, and a feed request is never refused because a counter could
// not be registered.
func (s *scannerTelemetry) ObserveMetrics(meter metric.Meter) error {
	requests, err := meter.Int64Counter(abuseRequestsMetric,
		metric.WithDescription("Authenticated requests to a pollable, deliberately unlimited scanner surface, by surface and subject type. Aggregate only: the device identity lives in the abuse.request log record, because a per-device label is one series per enrolled device. A sustained rise means a device is polling hard - find it in the logs and revoke it with access revoke-scanner."))
	if err != nil {
		return err
	}
	s.requests = requests
	return nil
}

// observeFeedPoll records one authenticated poll of the voided-ticket feed.
//
// Called for EVERY authenticated request, including the ones this handler is
// about to refuse: a poller sending malformed cursors is still polling, and
// telemetry that counted only well-formed requests would be blind to the
// cheapest way to hammer the route.
//
// device is the AUTHENTICATED device id, resolved by the request validator from
// the token. Nothing client-submitted reaches this function — the feed has no
// organizer parameter by design, and telemetry must not reintroduce a
// client-chosen key.
func (s *scannerTelemetry) observeFeedPoll(ctx context.Context, device uuid.UUID) {
	if s == nil {
		return
	}

	fixed := []attribute.KeyValue{
		attribute.String("surface", feedSurface),
		attribute.String("subject_type", scannerDeviceSubject),
	}

	if s.requests != nil {
		s.requests.Add(ctx, 1, metric.WithAttributes(fixed...))
	}

	// Span attributes: the two fixed values only. See the note above on why the
	// id is not among them.
	trace.SpanFromContext(ctx).SetAttributes(fixed...)

	if s.log != nil {
		// Three scalars, named explicitly. Never the store.ScannerDevice struct:
		// it carries a token hash, and a struct-valued attribute would serialise
		// whatever fields exist now AND any field added later.
		s.log.InfoContext(ctx, abuseRequestMessage,
			"surface", feedSurface,
			"subject_type", scannerDeviceSubject,
			"subject_id", device.String(),
		)
	}
}
