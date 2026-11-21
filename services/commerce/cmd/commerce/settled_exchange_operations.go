package main

// Operator commands for SETTLED exchanges whose ticket switch never happened (TKT-498).
//
// Access refuses the switch when a source ticket was already admitted, so the exchange stays
// settled and unswitched for good: the buyer used the source tickets, the replacement order
// exists, and the target capacity stays claimed. The owner's resolution is to abandon the
// exchange (ADR-039 §2). The listing below is the read half; it reads Commerce only.
// abandon-exchange is the write half.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/exchangeabandon"
	store "ticketing/services/commerce/internal/store"
	"ticketing/shared/runtimecfg"
)

// listSettledUnswitchedExchanges reports one organizer's settled, unswitched exchanges.
//
// Usage: commerce list-settled-unswitched-exchanges <organizer-id>
//
// Exit code ZERO when there is nothing to list, as for list-parked and list-wedged-exchanges.
func listSettledUnswitchedExchanges(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: commerce list-settled-unswitched-exchanges <organizer-id>")
	}
	org, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("organizer id: %w", err)
	}

	dsn, err := requiredDatabaseURL()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), operatorDBTimeout)
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	rows, err := store.ListSettledUnswitchedExchanges(ctx, db, org)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("no settled unswitched exchanges")
		return nil
	}
	printSettledUnswitched(os.Stdout, rows)
	// The caveat goes to stderr, next to the count, because it changes what the rows mean.
	fmt.Fprintf(os.Stderr, "\n%d settled exchange(s) without a ticket switch. These are CANDIDATES. "+
		"Commerce cannot see Access's tickets: a row can be a switch whose callback is late, or an "+
		"exchange Access refused because a source ticket was already admitted. This listing reads "+
		"Commerce only and cannot prove either. Do not act on a row from this listing alone. "+
		"`abandonment=refund-failed` means the provider refused the upgrade's delta refund: the "+
		"buyer is still charged and the target is still held, and running the abandonment again "+
		"reports the same failure without a new refund. See docs/runbooks/order-recovery.md.\n", len(rows))
	return nil
}

// printSettledUnswitched writes one line per exchange, with the abandonment's progress when
// one has started: `started`, `refund-failed` with the provider reference, or
// `target-returned` when only the terminal step is left.
func printSettledUnswitched(w io.Writer, rows []store.SettledExchange) {
	for _, r := range rows {
		state := "none"
		switch {
		case r.AbandonmentRefundFailedRef != "":
			state = "refund-failed"
		case r.AbandonmentTargetReturned:
			state = "target-returned"
		case r.AbandonmentStarted:
			state = "started"
		}
		_, _ = fmt.Fprintf(w, "organizer=%s exchange=%s source_order=%s replacement_order=%s target_hold=%s "+
			"settled_at=%s quantity=%d delta=%s %s abandonment=%s",
			r.OrganizerID, r.ID, r.SourceOrderID, r.ReplacementOrderID, r.TargetHoldID,
			r.SettledAt.UTC().Format(time.RFC3339), r.Quantity, signedAmount(r.DeltaAmount), r.Currency, state)
		if r.AbandonmentStarted {
			_, _ = fmt.Fprintf(w, " abandonment_started_at=%s operator=%q",
				r.AbandonmentStartedAt.UTC().Format(time.RFC3339), r.AbandonmentOperator)
		}
		if r.AbandonmentRefundFailedRef != "" {
			_, _ = fmt.Fprintf(w, " refund_failed_ref=%s", r.AbandonmentRefundFailedRef)
		}
		_, _ = fmt.Fprintln(w)
	}
}

// signedAmount renders a delta with its sign, so an upgrade reads `+1000` and not `1000`.
func signedAmount(v int64) string {
	if v > 0 {
		return fmt.Sprintf("+%d", v)
	}
	return fmt.Sprintf("%d", v)
}

const abandonExchangeUsage = `usage: commerce abandon-exchange <organizer-id> <exchange-id> "<operator-name>" "<reason>"`

// partialRefundTimeout bounds the delta refund POST (R3-3). Payments answers it only after its
// provider work: the Stripe adapter lists the payment's refunds, submits and reads the refund
// back, each call with its own 15 s budget. Payments' response-write deadline
// (HTTP_WRITE_TIMEOUT, 30 s by default) is the latest moment it can still answer, so a longer
// client wait gains nothing. 35 s is that deadline plus 5 s for the connection and the transfer.
// A Payments deployment with a longer write deadline is not waited for: the command stops as
// unresolved, and the same refund key resolves it on the next run.
const partialRefundTimeout = 35 * time.Second

// abandonDeadline bounds the whole command (R2-5, R3-3). It is the service's longest command,
// exchangeabandon.MaxRemoteCalls, with the partial refund at partialRefundTimeout and every other
// call at paymentsEvidenceTimeout, plus exchangeabandon.DatabaseAllowance: 29 min 5 s. A run
// that stops at the deadline keeps its recorded progress, and the same command resumes it.
func abandonDeadline() time.Duration {
	return exchangeabandon.CommandDeadline(paymentsEvidenceTimeout, partialRefundTimeout)
}

// abandonExchange abandons one settled exchange whose source Access admitted (TKT-498).
//
// Usage: commerce abandon-exchange <organizer-id> <exchange-id> "<operator-name>" "<reason>"
//
// The operator name is the operator's personal attestation; the database login is recorded
// beside it. Only the first trimmed name and reason pair owns the abandonment: the same pair
// resumes it from its recorded progress, and any other pair is refused, before and after
// completion. A completed abandonment answers from Commerce alone, with replay=true.
//
// Every argument and every required setting is checked BEFORE the database is opened or any
// service is called.
func abandonExchange(args []string) error {
	if len(args) != 4 {
		return errors.New(abandonExchangeUsage)
	}
	org, err := uuid.Parse(args[0])
	if err != nil || org == uuid.Nil {
		return fmt.Errorf("organizer id: %v (%s)", errOrNil(err), abandonExchangeUsage)
	}
	exchangeID, err := uuid.Parse(args[1])
	if err != nil || exchangeID == uuid.Nil {
		return fmt.Errorf("exchange id: %v (%s)", errOrNil(err), abandonExchangeUsage)
	}
	attribution, err := store.NewAbandonmentAttribution(args[2], args[3])
	if err != nil {
		return err
	}
	// The abandonment proves admission in Access, moves money and journals through payments,
	// and returns capacity to inventory. A setting it cannot use would let it reserve, and
	// perhaps refund, before it finds that out (RA2-3).
	accessURL, err := serviceBaseURL("ACCESS_URL")
	if err != nil {
		return err
	}
	paymentsURL, err := serviceBaseURL("PAYMENTS_URL")
	if err != nil {
		return err
	}
	inventoryURL, err := serviceBaseURL("INVENTORY_URL")
	if err != nil {
		return err
	}
	internalToken, err := runtimecfg.InternalTokenFromEnv()
	if err != nil {
		return err
	}
	// The dedicated credential, never the shared one (R2-4, supersedes D21's fallback):
	// payments refuses the shared token, and an even exchange with no pending refund would
	// return the target before the journal append found that out.
	paymentsToken, err := runtimecfg.PaymentsTokenFromEnv()
	if err != nil {
		return err
	}
	dsn, err := requiredDatabaseURL()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), abandonDeadline())
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	// One trace per run (RA2-5): every service's request log stamps it, so this run's calls
	// can be found and counted apart from every other caller's.
	traceID, spanID, err := newCommandTrace()
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stderr, "abandon-exchange trace_id=%s\n", traceID)
	client := newAbandonClient(accessURL, paymentsURL, inventoryURL, internalToken, paymentsToken).WithTrace(traceID, spanID)
	res, err := exchangeabandon.New(db, client, client, client).Abandon(ctx, org, exchangeID, attribution)
	if err != nil {
		reportAbandonRefusal(os.Stderr, exchangeID, res.Exchange, err)
		return err
	}
	printAbandoned(os.Stdout, res)
	if res.Replay {
		fmt.Fprintf(os.Stderr, "exchange %s was already abandoned with this operator name and reason; nothing changed. "+
			"The record above is the original, with its original times.\n", exchangeID)
	}
	return nil
}

// newAbandonClient builds the command's client to Access, payments and inventory.
func newAbandonClient(accessURL, paymentsURL, inventoryURL, internalToken, paymentsToken string) exchangeabandon.HTTP {
	return exchangeabandon.NewHTTP(accessURL, paymentsURL, inventoryURL, internalToken, paymentsToken,
		paymentsEvidenceTimeout, partialRefundTimeout)
}

// newCommandTrace returns a random W3C trace id and parent span id, as lowercase hex.
func newCommandTrace() (traceID, spanID string, err error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", fmt.Errorf("command trace id: %w", err)
	}
	return hex.EncodeToString(raw[:16]), hex.EncodeToString(raw[16:]), nil
}

// requiredDatabaseURL reads DATABASE_URL for both settled-exchange commands. pgx reads an empty
// connection string as "use the PG* environment defaults", which can select another database
// (RA2-3, R3-4).
func requiredDatabaseURL() (string, error) {
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		return "", errors.New("DATABASE_URL is required: an empty value would select the shell's PostgreSQL defaults")
	}
	return dsn, nil
}

// serviceBaseURL reads one dependency's base URL. Each call appends its path to the value, so it
// accepts only an absolute http or https URL with a non-empty host name, an explicit port (if
// any) in 1-65535, no query, and no fragment delimiter anywhere: after a `#`, even a bare one,
// the appended path is part of the fragment and is never sent (R3-4).
func serviceBaseURL(name string) (string, error) {
	raw := strings.TrimSuffix(os.Getenv(name), "/")
	if raw == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || !validPort(u) {
		return "", fmt.Errorf("%s must be an absolute http or https base URL with a host name, a port in 1-65535 if any, "+
			"and no query or fragment", name)
	}
	return raw, nil
}

// validPort accepts a URL with no explicit port, or with a decimal port in 1-65535. A host that
// ends in a colon names an empty port.
func validPort(u *url.URL) bool {
	if !strings.HasSuffix(u.Host, ":") && u.Port() == "" {
		return true
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port >= 1 && port <= 65535
}

func errOrNil(err error) error {
	if err == nil {
		return errors.New("the nil UUID is not an id")
	}
	return err
}

// printAbandoned writes the terminal record: the attribution, the progress instants, and the
// branch's money evidence.
func printAbandoned(w io.Writer, res exchangeabandon.Result) {
	e := res.Exchange
	_, _ = fmt.Fprintf(w, "organizer=%s exchange=%s source_order=%s replacement_order=%s target_hold=%s quantity=%d "+
		"delta=%s %s operator=%q session_user=%q reason=%q started_at=%s target_returned_at=%s abandoned_at=%s",
		e.OrganizerID, e.ID, e.SourceOrderID, e.ReplacementOrderID, e.TargetHoldID, e.Quantity,
		signedAmount(e.DeltaAmount), e.Currency, e.AbandonmentOperator, e.AbandonmentSessionUser, e.AbandonmentReason,
		e.AbandonmentStartedAt.UTC().Format(time.RFC3339Nano), e.AbandonmentTargetReturnedAt.UTC().Format(time.RFC3339Nano),
		e.AbandonedAt.UTC().Format(time.RFC3339Nano))
	switch {
	case e.DeltaAmount > 0:
		_, _ = fmt.Fprintf(w, " refund_fact=%s", e.AbandonmentRefundFactID)
	case e.DeltaAmount < 0:
		_, _ = fmt.Fprintf(w, " written_off=%d", e.AbandonmentWrittenOff)
	}
	_, _ = fmt.Fprintf(w, " replay=%t\n", res.Replay)
}

// reportAbandonRefusal tells the operator what each refusal means and what to do next. The
// refusals are distinguishable because the next action differs.
func reportAbandonRefusal(w io.Writer, exchangeID uuid.UUID, e store.SettledExchange, err error) {
	started := e.AbandonmentStarted
	switch {
	case errors.Is(err, store.ErrExchangeNotFound):
		_, _ = fmt.Fprintf(w, "REFUSED: no exchange %s for this organizer. Nothing changed. Check both ids.\n", exchangeID)
	case errors.Is(err, store.ErrAbandonmentAttributionConflict):
		_, _ = fmt.Fprintf(w, "REFUSED: the abandonment of exchange %s belongs to another operator name and reason. Nothing "+
			"changed. Only the first pair can resume or replay it; repeat that pair exactly.\n", exchangeID)
	case errors.Is(err, store.ErrAbandonmentRefundFailed):
		_, _ = fmt.Fprintf(w, "STOPPED: the provider refused the upgrade's delta refund for exchange %s (%s). The buyer is "+
			"still charged and the target is still held. Nothing will re-submit the refund. TKT-588 owns the next step; "+
			"see docs/runbooks/order-recovery.md.\n", exchangeID, e.AbandonmentRefundFailedRef)
	case errors.Is(err, store.ErrExchangeNotSettled), errors.Is(err, store.ErrExchangeAlreadySwitched),
		errors.Is(err, store.ErrExchangeAbandonmentInconsistent), errors.Is(err, store.ErrExchangeGrossFactsInconsistent):
		_, _ = fmt.Fprintf(w, "REFUSED: exchange %s is not a candidate (%v). Nothing changed.\n", exchangeID, err)
	case errors.Is(err, exchangeabandon.ErrSourceNotAdmitted), errors.Is(err, exchangeabandon.ErrReplacementIssued):
		_, _ = fmt.Fprintf(w, "REFUSED: Access does not prove an admitted source with an unissued replacement for exchange %s "+
			"(%v). Nothing changed. A replacement with tickets is a switch whose callback is late: the exchange "+
			"sweep owns it.\n", exchangeID, err)
	case errors.Is(err, exchangeabandon.ErrDeltaEvidence), errors.Is(err, exchangeabandon.ErrReplacementRefundLeg),
		errors.Is(err, store.ErrReplacementRefundCompleted):
		_, _ = fmt.Fprintf(w, "REFUSED: payments evidence for exchange %s needs reconciliation first (%v). started=%t.\n",
			exchangeID, err, started)
	default:
		_, _ = fmt.Fprintf(w, "INCOMPLETE: exchange %s was not abandoned (%v). started=%t. Every recorded step stays; run "+
			"the SAME command again (same operator name and reason) once the cause is cleared.\n", exchangeID, err, started)
	}
}
