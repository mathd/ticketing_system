package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/inventory/internal/store"
)

// checkClaimHistorySequence is the append_order sequence guard (TKT-295 D2). Run it after a
// value-preserving restore of claim_history, with writers stopped, before reopening writes:
// it refuses when the next append would be numbered at or below restored history, and with
// --repair advances the sequence. Runbook: docs/development.md § Restoring claim_history.
func checkClaimHistorySequence(args []string) error {
	return runCheckClaimHistorySequence(args, os.Getenv("DATABASE_URL"), os.Stdout)
}

func runCheckClaimHistorySequence(args []string, dsn string, out io.Writer) error {
	const usage = "usage: inventory check-claim-history-sequence [--repair] [--timeout=30s]"
	fs := flag.NewFlagSet("check-claim-history-sequence", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repair := fs.Bool("repair", false, "advance a sequence that is behind the stored maximum")
	timeout := fs.Duration("timeout", 30*time.Second, "bound for the whole check")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w; %s", err, usage)
	}
	if fs.NArg() != 0 || *timeout <= 0 {
		return errors.New(usage)
	}
	if dsn == "" {
		return errors.New("DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	// Parse here so a malformed DATABASE_URL is refused WITHOUT echoing it: pgx's parse error
	// quotes the whole string, query-string password included (TKT-295 review finding 1).
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return errors.New("DATABASE_URL is not a valid connection string (not echoed: it can carry a password)")
	}
	db := stdlib.OpenDB(*cfg)
	defer func() { _ = db.Close() }()

	report, err := store.CheckClaimHistoryAppendOrder(ctx, db, *repair)
	if errors.Is(err, store.ErrAppendOrderSequenceBehind) {
		_, _ = fmt.Fprintf(out, "%s check-claim-history-sequence: status=needs-repair last_value=%d is_called=%t max_append_order=%s changed=false\n",
			serviceName, report.LastValue, report.IsCalled, maxField(report.MaxAppendOrder))
		return fmt.Errorf("%w; with writers still stopped, run: %s check-claim-history-sequence --repair", err, serviceName)
	}
	if err != nil {
		return err
	}
	if report.Repaired {
		_, _ = fmt.Fprintf(out, "%s check-claim-history-sequence: status=repaired before_last_value=%d before_is_called=%t last_value=%d is_called=%t max_append_order=%s changed=true\n",
			serviceName, report.BeforeLastValue, report.BeforeIsCalled, report.LastValue, report.IsCalled, maxField(report.MaxAppendOrder))
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s check-claim-history-sequence: status=ok last_value=%d is_called=%t max_append_order=%s changed=false\n",
		serviceName, report.LastValue, report.IsCalled, maxField(report.MaxAppendOrder))
	return nil
}

func maxField(v sql.NullInt64) string {
	if !v.Valid {
		return "null"
	}
	return strconv.FormatInt(v.Int64, 10)
}
