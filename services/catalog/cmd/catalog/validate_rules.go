package main

// `catalog validate-rules` — the operator sweep for rule currencies (TKT-243).
//
// Resolution filters price rules by channel BEFORE it checks their currency
// (pricing.go:264-296, TKT-237), which is the correct order — the alternative
// made one misconfigured `pos` rule return 500 for every other channel's
// requests. The cost of that order is that a wrong-currency rule on a channel
// nobody is currently buying through stays silent until a sale arrives on it,
// and then fails closed at the worst possible time. This command is where an
// operator finds it first, without needing a sale on every channel.
//
// It reports and nothing else: no writes, no gate, exit 0 whether or not it
// finds anything (COS c). Findings are information for a human, not a build
// failure — the same reason `access verify-lifecycle` has only two exit codes.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"ticketing/services/catalog/internal/store"
)

// validateRulesDeps is the injectable seam: the outer command owns Postgres and
// the process's streams, the core owns argument parsing and the report. Written
// this way so the output contract is asserted by a unit test rather than by
// reading it in a terminal (provision_staff.go:25-32 set the pattern).
type validateRulesDeps struct {
	list   func(ctx context.Context) ([]store.RuleCurrencyMismatch, error)
	stdout io.Writer
}

// validateRulesTimeout bounds the sweep. Wider than migrate's 30s (ADR-022)
// because this traverses every ticket type and both rule tables, and narrower
// than unbounded so a wedged read fails rather than hanging an operator's shell.
// `access verify-lifecycle` picked 5 minutes for the same reasons.
const validateRulesTimeout = 5 * time.Minute

func validateRules(deps validateRulesDeps, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: %s validate-rules (no arguments)", serviceName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), validateRulesTimeout)
	defer cancel()

	findings, err := deps.list(ctx)
	if err != nil {
		// Deliberately before any verdict line: a failed read must never print
		// "no currency mismatches", which would report a database that was
		// never successfully queried as a clean one.
		return fmt.Errorf("%s validate-rules: %w", serviceName, err)
	}

	// Built whole, then written once. The report is a single artefact an
	// operator reads top to bottom, so a partial write is not a partial report —
	// it is a misleading one, and one checked write is the honest unit.
	var report strings.Builder
	if len(findings) == 0 {
		fmt.Fprintf(&report, "%s validate-rules: no currency mismatches\n", serviceName)
	} else {
		fmt.Fprintf(&report, "%s validate-rules: %d currency mismatch pair(s)\n", serviceName, len(findings))
		for _, f := range findings {
			fmt.Fprintf(&report,
				"  %s rule_id=%s organizer_id=%s ticket_type_id=%s scope=%s/%s%s channel=%s rule_currency=%s ticket_currency=%s window=[%s,%s)\n",
				f.Kind, f.RuleID, f.OrganizerID, f.TicketTypeID, f.ScopeLevel, f.ScopeID,
				feeCodeField(f), channelField(f.ChannelCode),
				f.RuleCurrency, f.TicketTypeCurrency,
				orUnbounded(f.EffectiveFrom), orUnbounded(f.EffectiveUntil))
		}
	}

	// Appended on EVERY run, findings or not. ADR-021's rule is to name the
	// adversary before making a claim, and a scope statement that appears only
	// when something is wrong is missing exactly when someone is relying on a
	// clean result. The sweep reads the same rows a writer with catalog database
	// access writes, so it can only ever be an operator aid.
	report.WriteString("  scope: honest-writer operator aid over price_rules and fee_rules.\n")
	report.WriteString("  NOT covered: this is not an integrity control — a writer with catalog database access\n")
	report.WriteString("    can add or alter the rows it reads (ADR-021). Rules whose effective window has already\n")
	report.WriteString("    closed are excluded: they are inert and unrecoverable (ADR-036 §4 step 1).\n")
	report.WriteString("    Split schedules carry no currency and cannot mismatch (ADR-047).\n")

	if _, err := io.WriteString(deps.stdout, report.String()); err != nil {
		return fmt.Errorf("%s validate-rules: write report: %w", serviceName, err)
	}
	return nil
}

func feeCodeField(f store.RuleCurrencyMismatch) string {
	if f.FeeCode == "" {
		return ""
	}
	return " fee_code=" + f.FeeCode
}

// A nil channel_code is channel-agnostic — eligible in every channel including
// the public one — which is a different statement from "the empty channel".
func channelField(code *string) string {
	if code == nil {
		return "(any)"
	}
	return *code
}

func orUnbounded(s *string) string {
	if s == nil {
		return "unbounded"
	}
	return *s
}

// validateRulesCommand is the outer command: environment and Postgres. No
// signal wiring — the sweep is a single bounded read that writes nothing, so
// there is no partial state for a graceful shutdown to protect; the timeout in
// validateRules is the whole lifetime bound.
func validateRulesCommand(args []string) error {
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	st := store.NewPostgres(db)
	return validateRules(validateRulesDeps{list: st.ListRuleCurrencyMismatches, stdout: os.Stdout}, args)
}
