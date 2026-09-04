package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"ticketing/services/catalog/internal/events"
	"ticketing/services/catalog/internal/store"
)

const reemitBatchSize = 100

// policyReemitter is the injectable core of reemit-policies (TKT-96); the outer
// command owns environment, Postgres, NATS and signal wiring. It re-emits the
// publication of every already-published ungrouped slot under a DISTINCT
// deterministic id (events.PerformancePublishedBackfill), so access re-projects
// the current re_entry policy for slots published before the field existed.
type policyReemitter struct {
	list      func(ctx context.Context, after *uuid.UUID, limit int) ([]store.Performance, error)
	publisher policyBackfillPublisher
}

type policyBackfillPublisher interface {
	PerformancePublishedBackfill(context.Context, store.Performance) error
}

// run drains the published ungrouped slots oldest-id first and re-emits each. Any
// publish failure aborts: the run must never claim success it cannot prove.
// Idempotency is downstream, not here — a re-run publishes the same deterministic
// ids, which access's consumed_events and JetStream's dedup window absorb as
// no-ops (COS-2). It is safe to re-run after a crash.
func (r policyReemitter) run(ctx context.Context) (reemitted int, err error) {
	var cursor *uuid.UUID
	for {
		if err := ctx.Err(); err != nil {
			return reemitted, err
		}
		batch, err := r.list(ctx, cursor, reemitBatchSize)
		if err != nil {
			return reemitted, fmt.Errorf("list published performances: %w", err)
		}
		if len(batch) == 0 {
			return reemitted, nil
		}
		for i := range batch {
			perf := batch[i]
			if err := r.publisher.PerformancePublishedBackfill(ctx, perf); err != nil {
				return reemitted, fmt.Errorf("re-emit %s: %w", perf.ID, err)
			}
			reemitted++
		}
		last := batch[len(batch)-1].ID
		cursor = &last
	}
}

// reemitPolicies is the operator subcommand (TKT-96). It is a one-shot,
// idempotent re-emission run out-of-band (ADR-022) — never wired into the server
// startup path and, unlike migrate, carrying no 30s deadline: it is data repair,
// not schema, and may traverse the whole published set. Safe to re-run.
func reemitPolicies(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: %s reemit-policies (no arguments)", serviceName)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	nc, err := nats.Connect(os.Getenv("NATS_URL"))
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()
	pub, err := events.NewJetStream(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}

	st := store.NewPostgres(db)
	r := policyReemitter{
		list:      st.ListPublishedUngroupedPerformances,
		publisher: pub,
	}
	reemitted, err := r.run(ctx)
	fmt.Printf("%s reemit-policies: reemitted=%d\n", serviceName, reemitted)
	return err
}
