package consumer

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

type terminatingJS struct {
	jetstream.JetStream
	consumer jetstream.Consumer
}

func (f terminatingJS) Stream(context.Context, string) (jetstream.Stream, error) {
	return terminatingStream{consumer: f.consumer}, nil
}

type terminatingStream struct {
	jetstream.Stream
	consumer jetstream.Consumer
}

func (f terminatingStream) CreateOrUpdateConsumer(context.Context, jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	return f.consumer, nil
}

type terminatingConsumer struct {
	jetstream.Consumer
	consumeContext *terminatingConsumeContext
}

func (f terminatingConsumer) Consume(jetstream.MessageHandler, ...jetstream.PullConsumeOpt) (jetstream.ConsumeContext, error) {
	return f.consumeContext, nil
}

func (f terminatingConsumer) Info(context.Context) (*jetstream.ConsumerInfo, error) {
	return &jetstream.ConsumerInfo{}, nil
}

type terminatingConsumeContext struct {
	closed  chan struct{}
	stopped atomic.Bool
}

func (f *terminatingConsumeContext) Stop()                   { f.stopped.Store(true) }
func (f *terminatingConsumeContext) Drain()                  {}
func (f *terminatingConsumeContext) Closed() <-chan struct{} { return f.closed }

func terminatedJetStream() (jetstream.JetStream, *terminatingConsumeContext) {
	consumeContext := &terminatingConsumeContext{closed: make(chan struct{})}
	close(consumeContext.closed)
	consumer := terminatingConsumer{consumeContext: consumeContext}
	return terminatingJS{consumer: consumer}, consumeContext
}

func TestTicketIssuerRunObservesConsumerTermination(t *testing.T) {
	js, consumeContext := terminatedJetStream()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	consumer := New(js, nil, nil, nil, "", "", "", nil, log)

	err := consumer.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "access-ticket-issuer") {
		t.Fatalf("Run error = %v, want the terminated durable name", err)
	}
	if consumer.Ready() {
		t.Fatal("terminated ticket issuer remained ready")
	}
	if !consumeContext.stopped.Load() {
		t.Fatal("Run returned without stopping the consume context")
	}
}

func TestPolicyRunObservesConsumerTermination(t *testing.T) {
	js, consumeContext := terminatedJetStream()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	consumer := NewPolicyConsumer(js, nil, log)

	err := consumer.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "access-slot-policy") {
		t.Fatalf("Run error = %v, want the terminated durable name", err)
	}
	if consumer.Ready() {
		t.Fatal("terminated policy consumer remained ready")
	}
	if !consumeContext.stopped.Load() {
		t.Fatal("Run returned without stopping the consume context")
	}
}
