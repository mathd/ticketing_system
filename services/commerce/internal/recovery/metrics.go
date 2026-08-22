package recovery

import (
	"context"

	"go.opentelemetry.io/otel/metric"
)

// ObserveMetrics registers the parked population's gauges.
//
// This closes a discovery gap, not a mechanism gap. Parking works: an order that spends
// MaxRecoveryAttempts stops being re-driven, deliberately (ADR-016 §Decision 1 — "a
// parked state with no scheduler is not recovery"). What was missing is that nothing
// told anyone it happened. The attempt-exhaustion branch logs once and says so in its
// own comment: "Parked: never claimed again, so this is the last notice anyone gets."
// A one-shot line at park time cannot answer the two questions an operator actually has
// — how many are parked right now, and how long has the oldest been waiting — which is
// why this is a gauge and not a third log line.
//
// `..._reconciliation_required` is this runner's own addition, and it is the same move
// exchangesweep made with `awaiting_switch`: a sub-population that differs in KIND gets
// its own series. These rows may hold captured money (ADR-016 §Consequences), and what a
// re-drive would do to one depends on provider evidence (ADR-032), so an operator seeing
// a backlog needs to know which of the two they are looking at. Note the split is over
// PARKED rows only — an unparked `reconciliation_required` row is queued compensation,
// not a human's inbox (migration 0005).
//
// Following internal/reversal and internal/exchangesweep, including their rule: they are
// observability, not a gate. Nothing here can make commerce unready — checkout must not
// stop taking traffic because a backlog gauge cannot be read.
func (r *Runner) ObserveMetrics(meter metric.Meter) error {
	parked, err := meter.Int64ObservableGauge("commerce.recovery.parked",
		metric.WithDescription("Parked recovery orders outside reconciliation_required: they spent their attempt budget and are no longer retried. Nothing in the service revisits them, so a nonzero value is work waiting for a human."))
	if err != nil {
		return err
	}
	reconciliation, err := meter.Int64ObservableGauge("commerce.recovery.parked.reconciliation_required",
		metric.WithDescription("Parked orders in reconciliation_required. Separate because the resolution differs in kind: these may hold captured money, and what a re-drive does to one depends on provider evidence. Unparked rows of the same status are queued compensation and are excluded."))
	if err != nil {
		return err
	}
	total, err := meter.Int64ObservableGauge("commerce.recovery.parked.total",
		metric.WithDescription("Every parked recovery order, both populations. Carried so the two splits can be reconciled against it."))
	if err != nil {
		return err
	}
	oldestAge, err := meter.Int64ObservableGauge("commerce.recovery.parked.oldest_age_seconds",
		metric.WithDescription("Age of the oldest park, measured from recovery_parked_at. A small but old backlog is a different problem from a large fresh one, and a count alone cannot tell them apart."))
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		// One callback, so all four series come from ONE database snapshot. Separate
		// callbacks would read separately and could report a total that disagrees with
		// its own splits.
		b, err := r.store.Backlog(ctx)
		if err != nil {
			return err
		}
		o.ObserveInt64(parked, b.Parked)
		o.ObserveInt64(reconciliation, b.ReconciliationRequired)
		o.ObserveInt64(total, b.Total)
		o.ObserveInt64(oldestAge, b.OldestAgeSeconds)
		return nil
	}, parked, reconciliation, total, oldestAge)
	return err
}
