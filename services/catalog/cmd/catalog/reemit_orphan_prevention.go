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

// orphanCorrector is the injectable core of reemit-orphan-prevention (TKT-183); the
// outer command owns environment, Postgres, NATS and signal wiring. Same shape as
// policyReemitter (TKT-96) on purpose — a second wave should not invent a second
// pattern.
type orphanCorrector struct {
	list      func(ctx context.Context, after *uuid.UUID, limit int) ([]store.Performance, error)
	publisher orphanCorrectionPublisher
}

type orphanCorrectionPublisher interface {
	PerformancePublishedOrphanCorrection(context.Context, store.Performance) error
}

// run re-emits every published slot bound to a rule-enabled seat-map version under the
// correction identity. TKT-179 shipped the setting before TKT-183 shipped the transport,
// so such slots exist: catalog emitted them at schema 4, set event_emitted_at, and will
// never emit them again — re-POSTing publish is idempotent. Inventory has an ordinary
// seated pool with no flag and no adjacency, and nothing else repairs it.
//
// Any publish failure aborts: the run must never claim a repair it cannot prove. It is
// safe to re-run — and re-running is the point, not merely a tolerance. Every run
// reconciles the full current candidate set, so a slot published at schema 4 by an
// undrained old catalog replica is corrected by the next run (ADR-041's rolling-
// deployment race). Repeats converge rather than multiply because the identity is
// deterministic per publication.
func (c orphanCorrector) run(ctx context.Context) (corrected int, err error) {
	var cursor *uuid.UUID
	for {
		if err := ctx.Err(); err != nil {
			return corrected, err
		}
		batch, err := c.list(ctx, cursor, reemitBatchSize)
		if err != nil {
			return corrected, fmt.Errorf("list orphan prevention candidates: %w", err)
		}
		if len(batch) == 0 {
			return corrected, nil
		}
		for i := range batch {
			perf := batch[i]
			if err := c.publisher.PerformancePublishedOrphanCorrection(ctx, perf); err != nil {
				return corrected, fmt.Errorf("correct %s: %w", perf.ID, err)
			}
			corrected++
		}
		last := batch[len(batch)-1].ID
		cursor = &last
	}
}

// reemitOrphanPrevention is the operator subcommand (TKT-183). One-shot, idempotent,
// out-of-band (ADR-022) — never wired into server startup, and carrying no deadline: it
// is data repair, not schema.
//
// Run it only after TKT-181's inventory and TKT-182's rule are deployed, and re-run it
// once every schema-4-producing catalog replica has drained. The second run is the
// stopping condition, not a correctness precondition — see docs/development.md.
func reemitOrphanPrevention(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: %s reemit-orphan-prevention (no arguments)", serviceName)
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
	c := orphanCorrector{
		list:      st.ListOrphanPreventionCandidates,
		publisher: pub,
	}
	corrected, err := c.run(ctx)
	fmt.Printf("%s reemit-orphan-prevention: corrected=%d\n", serviceName, corrected)
	return err
}
