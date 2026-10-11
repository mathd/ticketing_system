package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// TKT-327: inventory-catalog-offering is pre-provisioned by nats-init under the admin credential,
// and inventory binds to it and verifies its config before it consumes. These tests cover the
// comparison (one case per compared field), the production Run path (drift and bind error), and
// the committed JSON that nats-init applies.

const offeringDurable = "inventory-catalog-offering"

// validOfferingInfo is written by hand, NOT derived from consumerConfig(): a fixture built from
// the function under test cannot catch a mistake in it. It also carries the values the server
// fills in for fields the service leaves unset (AckWait 30s, MaxWaiting 512, one replica),
// because that is what a real INFO returns and the comparison must accept it.
func validOfferingInfo() *jetstream.ConsumerInfo {
	return &jetstream.ConsumerInfo{
		Stream: "PLATFORM",
		Name:   offeringDurable,
		Config: jetstream.ConsumerConfig{
			Name:    offeringDurable,
			Durable: offeringDurable,
			FilterSubjects: []string{
				"platform.catalog.performance.published",
				"platform.catalog.performance.archived",
				"platform.catalog.performance.closed",
				"platform.catalog.performance.reopened",
			},
			DeliverPolicy: jetstream.DeliverAllPolicy,
			AckPolicy:     jetstream.AckExplicitPolicy,
			AckWait:       30 * time.Second,
			MaxDeliver:    -1,
			ReplayPolicy:  jetstream.ReplayInstantPolicy,
			MaxWaiting:    512,
			MaxAckPending: 64,
			Replicas:      1,
		},
	}
}

func TestVerifyOfferingConfig(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*jetstream.ConsumerConfig)
		field string // empty: the config must be accepted
	}{
		{name: "matching config with server defaults", edit: func(*jetstream.ConsumerConfig) {}},
		{name: "filters in another order", edit: func(c *jetstream.ConsumerConfig) {
			slices.Reverse(c.FilterSubjects)
		}},
		// The code leaves BackOff nil; JSON decoding or a server can hand back an empty slice.
		{name: "empty rather than nil backoff", edit: func(c *jetstream.ConsumerConfig) {
			c.BackOff = []time.Duration{}
		}},
		{name: "filter missing", field: "filter_subjects", edit: func(c *jetstream.ConsumerConfig) {
			c.FilterSubjects = c.FilterSubjects[:3]
		}},
		// The executed attack TKT-327 closes: a widened filter that reads commerce's
		// order.completed, which carries guest_order_ref.
		{name: "filter added", field: "filter_subjects", edit: func(c *jetstream.ConsumerConfig) {
			c.FilterSubjects = append(c.FilterSubjects, "platform.commerce.order.completed")
		}},
		{name: "filter added through the singular field", field: "filter_subjects", edit: func(c *jetstream.ConsumerConfig) {
			c.FilterSubject = "platform.commerce.order.completed"
		}},
		{name: "ack policy", field: "ack_policy", edit: func(c *jetstream.ConsumerConfig) {
			c.AckPolicy = jetstream.AckAllPolicy
		}},
		{name: "deliver policy", field: "deliver_policy", edit: func(c *jetstream.ConsumerConfig) {
			c.DeliverPolicy = jetstream.DeliverNewPolicy
		}},
		{name: "max deliver", field: "max_deliver", edit: func(c *jetstream.ConsumerConfig) {
			c.MaxDeliver = 5
		}},
		{name: "backoff present", field: "backoff", edit: func(c *jetstream.ConsumerConfig) {
			c.BackOff = []time.Duration{time.Second}
		}},
		// TKT-68: the backpressure bound is explicit, not the server default of 1000.
		{name: "max ack pending", field: "max_ack_pending", edit: func(c *jetstream.ConsumerConfig) {
			c.MaxAckPending = 1000
		}},
		{name: "headers only", field: "headers_only", edit: func(c *jetstream.ConsumerConfig) {
			c.HeadersOnly = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := validOfferingInfo()
			tc.edit(&info.Config)
			err := verifyOfferingConfig((&Consumer{}).consumerConfig(), info)
			if tc.field == "" {
				if err != nil {
					t.Fatalf("verify refused a valid durable: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("verify accepted a durable whose %s drifted", tc.field)
			}
			if !strings.Contains(err.Error(), offeringDurable) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error = %q, want it to name the durable %q and the field %q", err, offeringDurable, tc.field)
			}
		})
	}
}

func TestVerifyOfferingConfigRefusesMissingInfo(t *testing.T) {
	err := verifyOfferingConfig((&Consumer{}).consumerConfig(), nil)
	if err == nil || !strings.Contains(err.Error(), offeringDurable) {
		t.Fatalf("error = %v, want a durable-named refusal of a bind with no INFO", err)
	}
}

// Fakes for Run's bind path. bindStream has NO CreateOrUpdateConsumer: the embedded nil
// interface panics if Run ever falls back to creating the durable.
type bindJS struct {
	jetstream.JetStream
	stream jetstream.Stream
}

func (f bindJS) Stream(context.Context, string) (jetstream.Stream, error) { return f.stream, nil }

type bindStream struct {
	jetstream.Stream
	consumer jetstream.Consumer
	err      error
	bound    *[]string
}

// DeleteConsumer is the legacy-durable removal Run still performs.
func (f bindStream) DeleteConsumer(context.Context, string) error { return nil }

func (f bindStream) Consumer(_ context.Context, name string) (jetstream.Consumer, error) {
	*f.bound = append(*f.bound, name)
	if f.err != nil {
		return nil, f.err
	}
	return f.consumer, nil
}

type bindConsumer struct {
	jetstream.Consumer
	info     *jetstream.ConsumerInfo
	consumes *atomic.Int32
}

func (f bindConsumer) CachedInfo() *jetstream.ConsumerInfo { return f.info }

func (f bindConsumer) Consume(jetstream.MessageHandler, ...jetstream.PullConsumeOpt) (jetstream.ConsumeContext, error) {
	f.consumes.Add(1)
	return nil, errors.New("test double: Consume reached")
}

func TestOfferingRunRefusesADriftedDurable(t *testing.T) {
	info := validOfferingInfo()
	info.Config.FilterSubjects = append(info.Config.FilterSubjects, "platform.commerce.order.completed")
	var consumes atomic.Int32
	var bound []string
	c := &Consumer{
		js:  bindJS{stream: bindStream{consumer: bindConsumer{info: info, consumes: &consumes}, bound: &bound}},
		st:  &fakeCatalogStore{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	err := c.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), offeringDurable) || !strings.Contains(err.Error(), "filter_subjects") {
		t.Fatalf("Run error = %v, want a refusal naming %s and filter_subjects", err, offeringDurable)
	}
	if !slices.Equal(bound, []string{offeringDurable}) {
		t.Fatalf("Run bound %v, want exactly [%s]", bound, offeringDurable)
	}
	if n := consumes.Load(); n != 0 {
		t.Fatalf("Run called Consume %d times on a drifted durable, want 0", n)
	}
	if c.Ready() {
		t.Fatal("inventory consumer is ready on a drifted durable")
	}
}

func TestOfferingRunRefusesAMissingDurable(t *testing.T) {
	var consumes atomic.Int32
	var bound []string
	c := &Consumer{
		js:  bindJS{stream: bindStream{err: jetstream.ErrConsumerNotFound, bound: &bound}},
		st:  &fakeCatalogStore{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	err := c.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), offeringDurable) || !errors.Is(err, jetstream.ErrConsumerNotFound) {
		t.Fatalf("Run error = %v, want a %s-named error wrapping ErrConsumerNotFound", err, offeringDurable)
	}
	if !slices.Equal(bound, []string{offeringDurable}) {
		t.Fatalf("Run bound %v, want exactly [%s]", bound, offeringDurable)
	}
	if n := consumes.Load(); n != 0 {
		t.Fatalf("Run called Consume %d times without a durable, want 0", n)
	}
	if c.Ready() {
		t.Fatal("inventory consumer is ready without its durable")
	}
}

// The `make up` path and the smoke stack both apply this file; no gate runs `make up`. This is
// the guard that the file and the code still agree.
func TestCommittedOfferingConfigMatchesTheCode(t *testing.T) {
	raw, err := os.ReadFile("../../../../deploy/nats/consumers/inventory-catalog-offering.json")
	if err != nil {
		t.Fatalf("read committed consumer config: %v", err)
	}
	var cfg jetstream.ConsumerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode committed consumer config: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("decode committed consumer config keys: %v", err)
	}
	got := make([]string, 0, len(keys))
	for k := range keys {
		got = append(got, k)
	}
	slices.Sort(got)

	// Exactly the fields consumerConfig() sets. A field the code leaves to the server default
	// must stay absent, or init-created and service-created durables stop being equal.
	wantKeys := []string{"ack_policy", "deliver_policy", "durable_name", "filter_subjects", "max_ack_pending", "max_deliver"}
	if !slices.Equal(got, wantKeys) {
		t.Fatalf("committed file sets %v, want exactly %v", got, wantKeys)
	}
	if cfg.Durable != offeringDurable {
		t.Fatalf("durable_name = %q, want %q", cfg.Durable, offeringDurable)
	}
	if cfg.MaxAckPending != 64 {
		t.Fatalf("max_ack_pending = %d, want 64", cfg.MaxAckPending)
	}
	if err := verifyOfferingConfig((&Consumer{}).consumerConfig(), &jetstream.ConsumerInfo{Config: cfg}); err != nil {
		t.Fatalf("committed file does not match consumerConfig(): %v", err)
	}
}
