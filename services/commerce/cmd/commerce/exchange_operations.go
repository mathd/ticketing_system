package main

// Operator commands for WEDGED exchanges (TKT-255, ADR-067).
//
// A wedged exchange is one whose inventory target claim went terminal before settlement:
// `finalize` refuses forever, so the request answers 409 on every retry, and the durable
// `order_exchanges` row leaves the source order both un-exchangeable and un-refundable.
// Nothing in the service resolves it — migration 0010 records deliberately that an exchange
// has no cancelled state, which is what makes the row permanent.
//
// CLI subcommands rather than an HTTP surface, following the same reasoning as
// `list-parked`/`unpark-order` above: unwinding is a deliberate operator act taken during an
// incident, and a command that must work during an incident should not depend on the service
// being up.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/exchangeunwind"
	store "ticketing/services/commerce/internal/store"
)

// paymentsEvidenceTimeout bounds the two read-only payments calls. Shorter than
// operatorDBTimeout because it is one HTTP round trip to a sibling service, and because a
// payments that is slow to answer must surface as an indeterminate refusal rather than as a
// command that appears to hang.
const paymentsEvidenceTimeout = 10 * time.Second

// listWedgedExchanges reports every unsettled exchange.
//
// Usage: commerce list-wedged-exchanges
//
// Exit code ZERO when there is nothing unsettled, the same contract `list-parked` states:
// "nothing to do" is not a failure. Non-zero is reserved for a store failure, so a wrapper
// can tell an empty queue from a broken one.
func listWedgedExchanges(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: commerce list-wedged-exchanges (no arguments)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), operatorDBTimeout)
	defer cancel()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	wedged, err := store.ListWedgedExchanges(ctx, db)
	if err != nil {
		return err
	}
	if len(wedged) == 0 {
		fmt.Println("no unsettled exchanges")
		return nil
	}
	for _, w := range wedged {
		money := "impossible (no basis recorded)"
		if w.BasisRecorded {
			switch {
			case w.DeltaAmount > 0:
				money = fmt.Sprintf("possible: upgrade, delta +%d %s", w.DeltaAmount, w.Currency)
			case w.DeltaAmount < 0:
				money = fmt.Sprintf("possible: downgrade, delta %d %s", w.DeltaAmount, w.Currency)
			default:
				money = "impossible (even exchange, no provider call)"
			}
		}
		fmt.Printf("organizer=%s exchange=%s source_order=%s age=%s basis=%t target_hold=%s money=%q actor=%s\n",
			w.OrganizerID, w.ID, w.SourceOrderID,
			time.Since(w.CreatedAt).Truncate(time.Second), w.BasisRecorded,
			nullableUUID(w.TargetHoldID), money, w.Actor)
	}
	// The caveats belong on stderr, next to the count, because they change what the list
	// MEANS and an operator who reads only the rows will act on a misreading.
	fmt.Fprintf(os.Stderr, "\n%d unsettled exchange(s). This is a list of CANDIDATES, not of confirmed "+
		"wedges. Commerce does not hold inventory's claim state, so it cannot tell a genuinely "+
		"terminal target claim from an exchange that is simply in flight right now — read the age "+
		"column, and confirm the claim is terminal in inventory BEFORE unwinding. `money=possible` "+
		"means only that a provider call could have been made; `unwind-exchange` asks payments and "+
		"refuses if it was.\n", len(wedged))
	return nil
}

// nullableUUID renders an absent hold distinguishably from a zero one, the same distinction
// `nullableField` draws for the recovery listing.
func nullableUUID(v uuid.UUID) string {
	if v == uuid.Nil {
		return "<none>"
	}
	return v.String()
}

// unwindExchange removes a wedged exchange's binding, freeing the source order.
//
// Usage: commerce unwind-exchange <organizer-id> <exchange-id> <reason>
//
// The reason is required and recorded durably: it is the only part of the record a later
// reader cannot reconstruct, since everything else — what the exchange was, what it was
// worth, whether it had a basis — is captured automatically from the row before it is
// deleted.
//
// This does NOT compensate money. It refuses outright when payments' own records say a
// provider movement exists, and refuses equally when payments cannot be asked. A charged
// buyer with a terminal target claim is a state this command reports and declines to
// resolve, because choosing between refunding them and re-selling them a target is a
// product decision nobody has taken. See docs/development.md.
func unwindExchange(args []string) error {
	// Every argument case refused BEFORE sql.Open, which is the contract
	// recovery_operations_test.go states for an operator subcommand: the arg-validation
	// test must need no database.
	if len(args) != 3 {
		return errors.New("usage: commerce unwind-exchange <organizer-id> <exchange-id> <reason>")
	}
	org, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("organizer id: %w", err)
	}
	exchangeID, err := uuid.Parse(args[1])
	if err != nil {
		return fmt.Errorf("exchange id: %w", err)
	}
	if strings.TrimSpace(args[2]) == "" {
		return errors.New("a reason is required: it is the only part of the record a later reader cannot reconstruct")
	}
	paymentsURL := strings.TrimSuffix(os.Getenv("PAYMENTS_URL"), "/")
	if paymentsURL == "" {
		return errors.New("PAYMENTS_URL is required: the unwind refuses unless payments' own records " +
			"say no money moved, and it cannot ask without one")
	}
	token := os.Getenv("PAYMENTS_INTERNAL_TOKEN")
	if token == "" {
		token = os.Getenv("INTERNAL_SERVICE_TOKEN")
	}
	if token == "" {
		return errors.New("PAYMENTS_INTERNAL_TOKEN (or INTERNAL_SERVICE_TOKEN) is required to read payments evidence")
	}

	ctx, cancel := context.WithTimeout(context.Background(), operatorDBTimeout)
	defer cancel()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	svc := exchangeunwind.New(db, exchangeunwind.NewHTTPPayments(paymentsURL, token, paymentsEvidenceTimeout))
	w, evidence, err := svc.Unwind(ctx, org, exchangeID, args[2])
	if err != nil {
		// The refusals are reported distinguishably because they send an operator to
		// different places. Money-moved is a buyer to compensate; indeterminate is a
		// payments to fix and a command to re-run; settled is a misreading of the listing.
		switch {
		case errors.Is(err, store.ErrExchangeMoneyMoved):
			fmt.Fprintf(os.Stderr, "REFUSED: payments records a provider movement for exchange %s "+
				"(delta %d %s). The buyer's money moved and this command does not compensate it. "+
				"The binding is intact and the source order remains blocked; resolving this is a "+
				"product decision, not an unwind.\n", exchangeID, w.DeltaAmount, w.Currency)
		case errors.Is(err, exchangeunwind.ErrMoneyIndeterminate):
			fmt.Fprintf(os.Stderr, "REFUSED: payments could not give a clean answer for exchange %s, "+
				"so whether money moved is UNKNOWN. Nothing was changed. Fix the reason payments "+
				"could not answer and run this again — do not treat an unanswered question as a no.\n",
				exchangeID)
		}
		return err
	}
	fmt.Fprintf(os.Stderr, "exchange %s unwound (money evidence: %s). The binding is gone: the source "+
		"order %s can be exchanged again with a NEW idempotency key, and can be refunded. This did "+
		"not touch inventory — the old target claim stays terminal, and the source line's capacity "+
		"was never released, so nothing is oversold.\n", exchangeID, evidence, w.SourceOrderID)
	return nil
}
