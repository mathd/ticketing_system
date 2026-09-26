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

type orderingCorrector struct {
	list      func(ctx context.Context, after *uuid.UUID, limit int) ([]store.Performance, error)
	publisher orderingCorrectionPublisher
}

type orderingCorrectionPublisher interface {
	PerformancePublishedBestAvailableOrderingCorrection(context.Context, store.Performance) error
}

func (c orderingCorrector) run(ctx context.Context) (corrected int, err error) {
	var cursor *uuid.UUID
	for {
		if err := ctx.Err(); err != nil {
			return corrected, err
		}
		batch, err := c.list(ctx, cursor, reemitBatchSize)
		if err != nil {
			return corrected, fmt.Errorf("list best-available ordering candidates: %w", err)
		}
		if len(batch) == 0 {
			return corrected, nil
		}
		for i := range batch {
			perf := batch[i]
			if err := c.publisher.PerformancePublishedBestAvailableOrderingCorrection(ctx, perf); err != nil {
				return corrected, fmt.Errorf("correct %s: %w", perf.ID, err)
			}
			corrected++
		}
		last := batch[len(batch)-1].ID
		cursor = &last
	}
}

func reemitBestAvailableOrdering(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: %s reemit-best-available-ordering (no arguments)", serviceName)
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
	c := orderingCorrector{
		list:      st.ListBestAvailableOrderingCandidates,
		publisher: pub,
	}
	corrected, err := c.run(ctx)
	fmt.Printf("%s reemit-best-available-ordering: corrected=%d\n", serviceName, corrected)
	return err
}
