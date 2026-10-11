package consumer

import (
	"fmt"
	"slices"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
)

// verifyTicketIssuerConfig compares the durable that nats-init pre-provisioned with the config
// this binary expects, before Run consumes from it (TKT-327, ADR-034 amendment).
//
// Why the service no longer creates the durable: its filters are PLURAL, so a create request
// carries them in the BODY and no subject permission can constrain them. A holder of the access
// credential could re-create the durable with a widened filter and read another service's
// events (ADR-072 §5). The CREATE grant is removed, so the durable now has two sources of truth:
// consumerConfig() here and deploy/nats/consumers/access-ticket-issuer*.json. This check makes a
// forgotten half fail at startup instead of silently.
//
// It compares ONLY the fields consumerConfig() sets. Fields the code leaves to the server default
// (AckWait, MaxAckPending, MaxWaiting, replicas) are not compared: a durable the old service
// created and one nats-init creates from the same fields get the same defaults, and a NATS
// upgrade that changes a default cannot cause a false refusal.
//
// The filter set is order-independent and exact, and it includes the singular FilterSubject, so
// a filter added through either field is refused.
func verifyTicketIssuerConfig(want jetstream.ConsumerConfig, info *jetstream.ConsumerInfo) error {
	if info == nil {
		return fmt.Errorf("durable %s on stream PLATFORM: the bind returned no consumer info to verify", want.Durable)
	}
	got := info.Config
	var drift []string
	if gotFilters, wantFilters := filterSet(got), filterSet(want); !slices.Equal(gotFilters, wantFilters) {
		drift = append(drift, fmt.Sprintf("filter_subjects is %v, want %v", gotFilters, wantFilters))
	}
	if got.AckPolicy != want.AckPolicy {
		drift = append(drift, fmt.Sprintf("ack_policy is %v, want %v", got.AckPolicy, want.AckPolicy))
	}
	if got.DeliverPolicy != want.DeliverPolicy {
		drift = append(drift, fmt.Sprintf("deliver_policy is %v, want %v", got.DeliverPolicy, want.DeliverPolicy))
	}
	if got.MaxDeliver != want.MaxDeliver {
		drift = append(drift, fmt.Sprintf("max_deliver is %d, want %d", got.MaxDeliver, want.MaxDeliver))
	}
	// Exact and ordered: the schedule is the retry contract for issuance and for the terminal
	// failure record (ACCESS_EVENT_RETRY_BACKOFF must agree with the provisioned file).
	if !slices.Equal(got.BackOff, want.BackOff) {
		drift = append(drift, fmt.Sprintf("backoff is %v, want %v", got.BackOff, want.BackOff))
	}
	if got.HeadersOnly != want.HeadersOnly {
		drift = append(drift, fmt.Sprintf("headers_only is %t, want %t", got.HeadersOnly, want.HeadersOnly))
	}
	if len(drift) > 0 {
		return fmt.Errorf("durable %s on stream PLATFORM does not match this binary's consumer config: %s "+
			"(nats-init provisions it from deploy/nats/consumers; see docs/development.md to reconcile it)",
			want.Durable, strings.Join(drift, "; "))
	}
	return nil
}

// filterSet returns every filter a config declares, plural and singular, sorted.
func filterSet(cfg jetstream.ConsumerConfig) []string {
	set := slices.Clone(cfg.FilterSubjects)
	if cfg.FilterSubject != "" {
		set = append(set, cfg.FilterSubject)
	}
	slices.Sort(set)
	return set
}
