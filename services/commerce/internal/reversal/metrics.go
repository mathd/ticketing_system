package reversal

import (
	"context"

	"go.opentelemetry.io/otel/metric"
)

// ObserveMetrics registers the reconciler's backlog gauges.
//
// These are not decoration, and they are the reason parking is honest. Bounding attempts
// converts "a permanently undischargeable obligation retries forever" into "it stops" — and
// a mechanism that stops silently is worse than one that spins loudly, because the spinning
// at least shows up somewhere. `..._parked` is what makes stopping visible, and it is what
// earns the ticket's COS 1 second branch: an explicitly monitored reconciler that is part of
// the deployment contract.
//
// The oldest-age gauge exists because a count cannot distinguish a small, old backlog from a
// large, fresh one, and those are different incidents: the first is something stuck, the
// second is something down.
//
// Following access/internal/lifecyclejob's shape, including its rule: they are
// observability, not a gate. Nothing here can make commerce unready — a refund path must not
// stop taking traffic because a reconciliation gauge cannot be read.
func (r *Runner) ObserveMetrics(meter metric.Meter) error {
	outstanding, err := meter.Int64ObservableGauge("commerce.refund.reversal.outstanding",
		metric.WithDescription("Completed refunds whose tickets are not voided or whose capacity is not returned, parked included. Nonzero is normal and transient; sustained nonzero means a downstream is not recovering."))
	if err != nil {
		return err
	}
	parked, err := meter.Int64ObservableGauge("commerce.refund.reversal.parked",
		metric.WithDescription("Outstanding reversals that spent their attempt budget and are no longer retried. These need a human: the obligation is owed and nothing is driving it."))
	if err != nil {
		return err
	}
	oldestAge, err := meter.Int64ObservableGauge("commerce.refund.reversal.oldest_age_seconds",
		metric.WithDescription("Age of the oldest outstanding reversal obligation. A small but old backlog is a different problem from a large fresh one."))
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		b, err := r.store.Backlog(ctx)
		if err != nil {
			return err
		}
		o.ObserveInt64(outstanding, b.Outstanding)
		o.ObserveInt64(parked, b.Parked)
		o.ObserveInt64(oldestAge, b.OldestAgeSeconds)
		return nil
	}, outstanding, parked, oldestAge)
	return err
}
