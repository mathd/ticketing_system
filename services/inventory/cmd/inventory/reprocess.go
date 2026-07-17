package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"ticketing/services/inventory/internal/consumer"
	"ticketing/services/inventory/internal/store"
)

const reprocessBatchSize = 100

// quarantineMsgID is the republished message's deterministic Nats-Msg-Id: a crashed run that
// republished but never marked can be re-run safely — JetStream dedupes inside its duplicate
// window, and inventory's consumed_events idempotency covers everything beyond it.
func quarantineMsgID(subject string, eventID uuid.UUID, schema int) string {
	return fmt.Sprintf("quarantine:%s:%s:%d", subject, eventID, schema)
}

// quarantineReprocessor is the injectable core of reprocess-quarantine; the outer command owns
// environment, Postgres, NATS and signal wiring.
type quarantineReprocessor struct {
	list     func(context.Context, *store.QuarantinedCatalogEvent, int) ([]store.QuarantinedCatalogEvent, error)
	publish  func(ctx context.Context, subject, msgID string, envelope []byte) error
	mark     func(ctx context.Context, subject string, eventID uuid.UUID) error
	supports func(subject string, schema int) bool
}

// run drains the pending quarantine oldest-first. Rows this binary supports are republished
// byte-identically to their original subject and marked only after the broker accepted the
// publish; unsupported rows stay unresolved without starving supported rows behind them (the
// keyset cursor moves past both). Any publish or mark failure aborts: the run must never claim
// success it cannot prove.
func (r quarantineReprocessor) run(ctx context.Context) (reinjected, unsupported int, err error) {
	var cursor *store.QuarantinedCatalogEvent
	for {
		batch, err := r.list(ctx, cursor, reprocessBatchSize)
		if err != nil {
			return reinjected, unsupported, fmt.Errorf("list quarantine: %w", err)
		}
		if len(batch) == 0 {
			return reinjected, unsupported, nil
		}
		for i := range batch {
			row := batch[i]
			if !r.supports(row.Subject, row.Schema) {
				unsupported++
				continue
			}
			if err := r.publish(ctx, row.Subject, quarantineMsgID(row.Subject, row.EventID, row.Schema), row.Envelope); err != nil {
				return reinjected, unsupported, fmt.Errorf("republish %s %s: %w", row.Subject, row.EventID, err)
			}
			if err := r.mark(ctx, row.Subject, row.EventID); err != nil {
				return reinjected, unsupported, fmt.Errorf("mark %s %s reinjected: %w", row.Subject, row.EventID, err)
			}
			reinjected++
		}
		cursor = &batch[len(batch)-1]
	}
}

// reprocessQuarantine is the operator subcommand (TKT-68). It exits zero when rows remain
// unsupported — that is the expected state until a binary that understands them is deployed —
// and non-zero on any store or transport failure.
func reprocessQuarantine(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: %s reprocess-quarantine (no arguments)", serviceName)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()
	st := store.New(db, time.Minute)

	nc, err := nats.Connect(os.Getenv("NATS_URL"))
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}

	r := quarantineReprocessor{
		list: st.ListCatalogQuarantine,
		publish: func(ctx context.Context, subject, msgID string, envelope []byte) error {
			_, err := js.Publish(ctx, subject, envelope, jetstream.WithMsgID(msgID))
			return err
		},
		mark:     st.MarkCatalogEventReinjected,
		supports: consumer.SupportsCatalogSchema,
	}
	reinjected, unsupported, err := r.run(ctx)
	fmt.Printf("%s reprocess-quarantine: reinjected=%d unsupported=%d\n", serviceName, reinjected, unsupported)
	if err != nil {
		return err
	}
	if unsupported > 0 {
		fmt.Printf("%s reprocess-quarantine: %d events still await a newer binary; readiness stays down until they are reprocessed\n", serviceName, unsupported)
	}
	return nil
}
