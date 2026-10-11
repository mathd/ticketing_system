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

// TKT-327: access-ticket-issuer is pre-provisioned by nats-init under the admin credential, and
// access binds to it and verifies its config before it consumes. These tests cover the
// comparison (one case per compared field), the production Run path (drift and bind error), and
// the committed default JSON that nats-init applies.

const ticketIssuerDurable = "access-ticket-issuer"

// validTicketIssuerInfo is written by hand, NOT derived from consumerConfig(): a fixture built
// from the function under test cannot catch a mistake in it. It also carries the values the
// server fills in for fields the service leaves unset (AckWait from BackOff[0], MaxAckPending
// 1000, MaxWaiting 512, one replica), because that is what a real INFO returns and the
// comparison must accept it.
func validTicketIssuerInfo() *jetstream.ConsumerInfo {
	return &jetstream.ConsumerInfo{
		Stream: "PLATFORM",
		Name:   ticketIssuerDurable,
		Config: jetstream.ConsumerConfig{
			Name:           ticketIssuerDurable,
			Durable:        ticketIssuerDurable,
			FilterSubjects: []string{"platform.commerce.order.completed", "platform.commerce.order.exchanged"},
			DeliverPolicy:  jetstream.DeliverAllPolicy,
			AckPolicy:      jetstream.AckExplicitPolicy,
			AckWait:        time.Second,
			MaxDeliver:     -1,
			BackOff:        []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute},
			ReplayPolicy:   jetstream.ReplayInstantPolicy,
			MaxWaiting:     512,
			MaxAckPending:  1000,
			Replicas:       1,
		},
	}
}

func defaultTicketIssuerConfig() jetstream.ConsumerConfig {
	return (&Consumer{backoff: DefaultOptions().BackOff}).consumerConfig(ticketIssuerDurable)
}

func TestVerifyTicketIssuerConfig(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*jetstream.ConsumerConfig)
		field string // empty: the config must be accepted
	}{
		{name: "matching config with server defaults", edit: func(*jetstream.ConsumerConfig) {}},
		{name: "filters in another order", edit: func(c *jetstream.ConsumerConfig) {
			c.FilterSubjects = []string{"platform.commerce.order.exchanged", "platform.commerce.order.completed"}
		}},
		{name: "filter missing", field: "filter_subjects", edit: func(c *jetstream.ConsumerConfig) {
			c.FilterSubjects = []string{"platform.commerce.order.completed"}
		}},
		// The attack TKT-327 closes: a widened filter reads another service's subject.
		{name: "filter added", field: "filter_subjects", edit: func(c *jetstream.ConsumerConfig) {
			c.FilterSubjects = append(c.FilterSubjects, "platform.catalog.performance.archived")
		}},
		{name: "filter added through the singular field", field: "filter_subjects", edit: func(c *jetstream.ConsumerConfig) {
			c.FilterSubject = "platform.catalog.performance.archived"
		}},
		{name: "filter duplicated in place of one", field: "filter_subjects", edit: func(c *jetstream.ConsumerConfig) {
			c.FilterSubjects = []string{"platform.commerce.order.completed", "platform.commerce.order.completed"}
		}},
		{name: "ack policy", field: "ack_policy", edit: func(c *jetstream.ConsumerConfig) {
			c.AckPolicy = jetstream.AckNonePolicy
		}},
		{name: "deliver policy", field: "deliver_policy", edit: func(c *jetstream.ConsumerConfig) {
			c.DeliverPolicy = jetstream.DeliverNewPolicy
		}},
		// Options.MaxDeliver is 6, but the durable's MaxDeliver is unlimited: publication of the
		// terminal failure record must stay retryable. A durable capped at 6 drops it silently.
		{name: "max deliver", field: "max_deliver", edit: func(c *jetstream.ConsumerConfig) {
			c.MaxDeliver = 6
		}},
		{name: "backoff interval changed", field: "backoff", edit: func(c *jetstream.ConsumerConfig) {
			c.BackOff[1] = 6 * time.Second
		}},
		{name: "backoff reordered", field: "backoff", edit: func(c *jetstream.ConsumerConfig) {
			c.BackOff[0], c.BackOff[1] = c.BackOff[1], c.BackOff[0]
		}},
		{name: "backoff shorter", field: "backoff", edit: func(c *jetstream.ConsumerConfig) {
			c.BackOff = c.BackOff[:5]
		}},
		{name: "backoff absent", field: "backoff", edit: func(c *jetstream.ConsumerConfig) {
			c.BackOff = nil
		}},
		{name: "headers only", field: "headers_only", edit: func(c *jetstream.ConsumerConfig) {
			c.HeadersOnly = true
		}},
		// TKT-327 review: settings that let the service bind and report ready while it consumes
		// nothing, or lose the durable. One case per check in consumptionDrift.
		{name: "pause_until zero, as a nats-init durable reports it", edit: func(c *jetstream.ConsumerConfig) {
			c.PauseUntil = &time.Time{}
		}},
		{name: "pause_until in the past", edit: func(c *jetstream.ConsumerConfig) {
			past := time.Now().Add(-time.Hour)
			c.PauseUntil = &past
		}},
		{name: "ephemeral: no durable name", field: "durable_name", edit: func(c *jetstream.ConsumerConfig) {
			c.Durable = ""
		}},
		{name: "inactive threshold", field: "inactive_threshold", edit: func(c *jetstream.ConsumerConfig) {
			c.InactiveThreshold = time.Hour
		}},
		// Consume asks for 500 messages; the broker refuses each pull with 409 Exceeded
		// MaxRequestBatch, and nats.go reports that only to the consume error callback.
		{name: "max batch below the client's", field: "max_batch", edit: func(c *jetstream.ConsumerConfig) {
			c.MaxRequestBatch = 10
		}},
		{name: "max expires below the client's", field: "max_expires", edit: func(c *jetstream.ConsumerConfig) {
			c.MaxRequestExpires = time.Second
		}},
		{name: "max bytes", field: "max_bytes", edit: func(c *jetstream.ConsumerConfig) {
			c.MaxRequestMaxBytes = 1024
		}},
		{name: "paused into the future", field: "pause_until", edit: func(c *jetstream.ConsumerConfig) {
			future := time.Now().Add(time.Hour)
			c.PauseUntil = &future
		}},
		{name: "replay at original pacing", field: "replay_policy", edit: func(c *jetstream.ConsumerConfig) {
			c.ReplayPolicy = jetstream.ReplayOriginalPolicy
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := validTicketIssuerInfo()
			tc.edit(&info.Config)
			err := verifyTicketIssuerConfig(defaultTicketIssuerConfig(), info)
			if tc.field == "" {
				if err != nil {
					t.Fatalf("verify refused a valid durable: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("verify accepted a durable whose %s drifted", tc.field)
			}
			if !strings.Contains(err.Error(), ticketIssuerDurable) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error = %q, want it to name the durable %q and the field %q", err, ticketIssuerDurable, tc.field)
			}
		})
	}
}

func TestVerifyTicketIssuerConfigRefusesMissingInfo(t *testing.T) {
	err := verifyTicketIssuerConfig(defaultTicketIssuerConfig(), nil)
	if err == nil || !strings.Contains(err.Error(), ticketIssuerDurable) {
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

func TestTicketIssuerRunRefusesADriftedDurable(t *testing.T) {
	info := validTicketIssuerInfo()
	info.Config.FilterSubjects = append(info.Config.FilterSubjects, "platform.catalog.performance.archived")
	var consumes atomic.Int32
	var bound []string
	js := bindJS{stream: bindStream{consumer: bindConsumer{info: info, consumes: &consumes}, bound: &bound}}
	c := New(js, nil, nil, nil, "", "", "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := c.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), ticketIssuerDurable) || !strings.Contains(err.Error(), "filter_subjects") {
		t.Fatalf("Run error = %v, want a refusal naming %s and filter_subjects", err, ticketIssuerDurable)
	}
	if !slices.Equal(bound, []string{ticketIssuerDurable}) {
		t.Fatalf("Run bound %v, want exactly [%s]", bound, ticketIssuerDurable)
	}
	if n := consumes.Load(); n != 0 {
		t.Fatalf("Run called Consume %d times on a drifted durable, want 0", n)
	}
	if c.Ready() {
		t.Fatal("ticket issuer is ready on a drifted durable")
	}
}

func TestTicketIssuerRunRefusesAMissingDurable(t *testing.T) {
	var consumes atomic.Int32
	var bound []string
	js := bindJS{stream: bindStream{err: jetstream.ErrConsumerNotFound, bound: &bound}}
	c := New(js, nil, nil, nil, "", "", "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := c.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), ticketIssuerDurable) || !errors.Is(err, jetstream.ErrConsumerNotFound) {
		t.Fatalf("Run error = %v, want a %s-named error wrapping ErrConsumerNotFound", err, ticketIssuerDurable)
	}
	if !slices.Equal(bound, []string{ticketIssuerDurable}) {
		t.Fatalf("Run bound %v, want exactly [%s]", bound, ticketIssuerDurable)
	}
	if n := consumes.Load(); n != 0 {
		t.Fatalf("Run called Consume %d times without a durable, want 0", n)
	}
	if c.Ready() {
		t.Fatal("ticket issuer is ready without its durable")
	}
}

// committedConsumerFile reads a file nats-init applies. The path is relative to this package
// directory, which is where `go test` runs.
func committedConsumerFile(t *testing.T, name string) (jetstream.ConsumerConfig, []string) {
	t.Helper()
	raw, err := os.ReadFile("../../../../deploy/nats/consumers/" + name)
	if err != nil {
		t.Fatalf("read committed consumer config: %v", err)
	}
	var cfg jetstream.ConsumerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("decode %s keys: %v", name, err)
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	slices.Sort(names)
	return cfg, names
}

// The `make up` path applies this file and no gate runs `make up`; the smoke stack applies the
// .smoke.json variant. This is the guard that the default file and the code still agree.
func TestCommittedDefaultTicketIssuerConfigMatchesTheCode(t *testing.T) {
	cfg, keys := committedConsumerFile(t, "access-ticket-issuer.json")

	// Exactly the fields consumerConfig() sets. A field the code leaves to the server default
	// must stay absent, or init-created and service-created durables stop being equal.
	wantKeys := []string{"ack_policy", "backoff", "deliver_policy", "durable_name", "filter_subjects", "max_deliver"}
	if !slices.Equal(keys, wantKeys) {
		t.Fatalf("committed file sets %v, want exactly %v", keys, wantKeys)
	}
	if cfg.Durable != ticketIssuerDurable {
		t.Fatalf("durable_name = %q, want %q", cfg.Durable, ticketIssuerDurable)
	}
	if !slices.Equal(cfg.BackOff, DefaultOptions().BackOff) {
		t.Fatalf("backoff = %v, want DefaultOptions().BackOff %v", cfg.BackOff, DefaultOptions().BackOff)
	}
	if err := verifyTicketIssuerConfig(defaultTicketIssuerConfig(), &jetstream.ConsumerInfo{Config: cfg}); err != nil {
		t.Fatalf("committed default file does not match consumerConfig(): %v", err)
	}
}
