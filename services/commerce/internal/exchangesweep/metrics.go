package exchangesweep

import (
	"context"

	"go.opentelemetry.io/otel/metric"
)

// ObserveMetrics registers the sweep's backlog gauges.
//
// These are not decoration, and they are what makes parking honest. Bounding attempts
// converts "a permanently undischargeable obligation retries forever" into "it stops" — and
// a mechanism that stops silently is worse than one that spins loudly, because the spinning
// at least shows up somewhere (ADR-062 §4).
//
// `..._awaiting_switch` is this sweep's own addition, and it earns COS 1's SECOND branch.
// The runner can drive a switched exchange's capacity to completion, but it can never
// complete one whose switch access has not confirmed — that marker is access's fact, and
// commerce inventing it is the one write that can oversell. Those rows are therefore
// "explicitly monitored and documented" rather than "driven to completion", and an operator
// looking at a stuck backlog needs to know which of the two they are looking at: one is
// inventory not recovering, the other is an access consumer that stopped delivering.
//
// Following internal/reversal's shape, including its rule: they are observability, not a
// gate. Nothing here can make commerce unready — an exchange path must not stop taking
// traffic because a reconciliation gauge cannot be read.
func (r *Runner) ObserveMetrics(meter metric.Meter) error {
	outstanding, err := meter.Int64ObservableGauge("commerce.exchange.reversal.outstanding",
		metric.WithDescription("Settled exchanges whose tickets are not switched or whose source capacity is not returned, parked included. Nonzero is normal and transient; sustained nonzero means a downstream is not recovering."))
	if err != nil {
		return err
	}
	parked, err := meter.Int64ObservableGauge("commerce.exchange.reversal.parked",
		metric.WithDescription("Outstanding exchange obligations that spent their attempt budget and are no longer retried. These need a human: the obligation is owed and nothing is driving it."))
	if err != nil {
		return err
	}
	awaitingSwitch, err := meter.Int64ObservableGauge("commerce.exchange.reversal.awaiting_switch",
		metric.WithDescription("Settled exchanges access has not confirmed switched. The sweep can NEVER complete these — only access can establish that the old tickets stopped admitting — so they are monitored rather than driven. Sustained nonzero means an access consumer is not delivering order.exchanged."))
	if err != nil {
		return err
	}
	oldestAge, err := meter.Int64ObservableGauge("commerce.exchange.reversal.oldest_age_seconds",
		metric.WithDescription("Age of the oldest outstanding exchange obligation, measured from settlement. A small but old backlog is a different problem from a large fresh one."))
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
		o.ObserveInt64(awaitingSwitch, b.AwaitingSwitch)
		o.ObserveInt64(oldestAge, b.OldestAgeSeconds)
		return nil
	}, outstanding, parked, awaitingSwitch, oldestAge)
	return err
}
