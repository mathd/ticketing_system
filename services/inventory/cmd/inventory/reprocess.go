package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/consumer"
	"ticketing/services/inventory/internal/store"
	"ticketing/shared/obs"
	"ticketing/shared/runtimecfg"
)

const reprocessBatchSize = 100

// quarantineReprocessor is the injectable core of reprocess-quarantine; the outer command owns
// environment, Postgres, the catalog resolver and signal wiring.
type quarantineReprocessor struct {
	list func(context.Context, *store.QuarantinedCatalogEvent, int) ([]store.QuarantinedCatalogEvent, error)
	// process runs the live consumer's handler on one stored envelope, in this process
	// (consumer.Reprocessor). Nothing is published to NATS (TKT-326).
	process  func(ctx context.Context, subject string, envelope []byte) (consumer.Outcome, error)
	mark     func(ctx context.Context, subject string, eventID uuid.UUID) error
	supports func(subject string, schema int) bool
}

// reprocessCounts is what one run did with the pending rows it examined.
type reprocessCounts struct{ applied, terminal, unsupported, deferred int }

// run drains the pending quarantine oldest-first, one pass. A row this binary supports is
// processed in-process with its original subject and exact bytes. An applied or terminal row is
// marked resolved after processing returns. A deferred row and an unsupported row stay pending
// without starving rows behind them (the keyset cursor moves past both). A processing, list or
// mark failure aborts, and so does cancellation: the run must never claim success it cannot prove.
func (r quarantineReprocessor) run(ctx context.Context) (reprocessCounts, error) {
	var n reprocessCounts
	var cursor *store.QuarantinedCatalogEvent
	for {
		// A cancelled run must fail, not report the rows it could not judge as deferred (D6/A4).
		if err := ctx.Err(); err != nil {
			return n, fmt.Errorf("reprocess interrupted: %w", err)
		}
		batch, err := r.list(ctx, cursor, reprocessBatchSize)
		if err != nil {
			return n, fmt.Errorf("list quarantine: %w", err)
		}
		if len(batch) == 0 {
			return n, nil
		}
		for i := range batch {
			if err := ctx.Err(); err != nil {
				return n, fmt.Errorf("reprocess interrupted: %w", err)
			}
			row := batch[i]
			if !r.supports(row.Subject, row.Schema) {
				n.unsupported++
				continue
			}
			outcome, err := r.process(ctx, row.Subject, row.Envelope)
			if err != nil {
				return n, fmt.Errorf("reprocess %s %s: %w", row.Subject, row.EventID, err)
			}
			switch outcome {
			case consumer.OutcomeApplied, consumer.OutcomeTerminal:
				if err := r.mark(ctx, row.Subject, row.EventID); err != nil {
					return n, fmt.Errorf("mark %s %s resolved: %w", row.Subject, row.EventID, err)
				}
				if outcome == consumer.OutcomeApplied {
					n.applied++
				} else {
					n.terminal++
				}
			case consumer.OutcomeDeferred:
				n.deferred++
			default:
				return n, fmt.Errorf("reprocess %s %s: unknown outcome %d", row.Subject, row.EventID, outcome)
			}
		}
		cursor = &batch[len(batch)-1]
	}
}

// reprocessQuarantine is the operator subcommand (TKT-68, reworked by TKT-326). It runs the live
// consumer's handler on each supported quarantined event in this process, so it needs Postgres
// and catalog, and no NATS connection at all. It exits zero when rows remain unsupported or
// deferred (they stay pending, and readiness stays down after a restart), and non-zero on a
// store failure, a missing catalog configuration, or cancellation.
func reprocessQuarantine(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: %s reprocess-quarantine (no arguments)", serviceName)
	}
	// Refuse before any row is listed (D6/A3). Without catalog access, the handler cannot judge
	// schema-1, schema-4 or schema-5 publications or any closure.
	catalogURL := strings.TrimRight(os.Getenv("CATALOG_URL"), "/")
	if catalogURL == "" {
		return errors.New("CATALOG_URL required: reprocess resolves publications and closures against catalog")
	}
	credential, err := runtimecfg.InternalTokenFromEnv()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()
	st := store.New(db, time.Minute)
	resolver := consumer.NewCatalogResolver(catalogURL, credential, obs.ClientWithTimeout(5*time.Second))
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	r := quarantineReprocessor{
		list:     st.ListCatalogQuarantine,
		process:  consumer.Reprocessor(st, resolver, log),
		mark:     st.MarkCatalogEventReinjected,
		supports: consumer.SupportsCatalogSchema,
	}
	n, err := r.run(ctx)
	fmt.Printf("%s reprocess-quarantine: applied=%d terminal=%d unsupported=%d deferred=%d\n",
		serviceName, n.applied, n.terminal, n.unsupported, n.deferred)
	if err != nil {
		return err
	}
	if n.unsupported > 0 {
		fmt.Printf("%s reprocess-quarantine: %d events still await a newer binary; readiness stays down until they are reprocessed\n", serviceName, n.unsupported)
	}
	if n.deferred > 0 {
		fmt.Printf("%s reprocess-quarantine: %d events were deferred (an offering event before its pool, a catalog outage, or an unusable catalog answer; the log has each cause); they stay pending, and readiness stays down after a restart until a later run applies them\n", serviceName, n.deferred)
	}
	return nil
}
