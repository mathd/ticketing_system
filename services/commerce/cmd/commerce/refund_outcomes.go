package main

// Operator outcomes for failed post-purchase refunds (TKT-525, ADR-087).
//
// A staff or bulk-cancellation refund whose provider refund failed stays pending: payments
// never re-submits a failed refund, and both allowances stay reserved. This command records
// what an operator established outside the platform. It is a CLI subcommand, as unpark-order
// and unwind-exchange are, because it is a deliberate act during an incident.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/refundoutcome"
	store "ticketing/services/commerce/internal/store"
)

const recordRefundOutcomeUsage = "usage: commerce record-refund-outcome <organizer-id> <refund-id> refunded_outside <operator> <external-ref> <reason>"

// recordRefundOutcome records one operator outcome.
//
// Usage:
//
//	commerce record-refund-outcome <organizer-id> <refund-id> refunded_outside <operator> <external-ref> <reason>
//
// refunded_outside means: the operator has confirmed that the buyer received the FULL stored
// amount of this refund, in its stored currency, by a settled transfer outside the platform.
// Running the command is that confirmation; the amount and currency are not typed, they are
// copied from the refund and printed in the result.
//
// The command first asks payments, read-only, to prove that the refund's leg is still bound
// and unmarked and that the provider's own refund for it terminally failed. Without that proof
// nothing is recorded. It makes no writing HTTP call.
//
// The `abandoned` form is TKT-525 S2 and is refused in this release.
func recordRefundOutcome(args []string) error {
	// Every argument is checked BEFORE the database is opened or payments is called, the
	// contract recovery_operations_test.go states for an operator subcommand.
	if len(args) < 3 {
		return errors.New(recordRefundOutcomeUsage)
	}
	// A nil UUID parses; RefundOutcomeRequest.Validate refuses it, below, before any connection.
	org, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("organizer id: %w (%s)", err, recordRefundOutcomeUsage)
	}
	refundID, err := uuid.Parse(args[1])
	if err != nil {
		return fmt.Errorf("refund id: %w (%s)", err, recordRefundOutcomeUsage)
	}
	var in store.RefundOutcomeRequest
	switch args[2] {
	case store.OutcomeRefundedOutside:
		if len(args) != 6 {
			return errors.New(recordRefundOutcomeUsage)
		}
		in = store.RefundOutcomeRequest{
			OrganizerID: org, RefundID: refundID, Kind: store.OutcomeRefundedOutside,
			OperatorName: args[3], ExternalRef: args[4], Reason: args[5],
		}
	case store.OutcomeAbandoned:
		return errors.New("the abandoned outcome is not available in this release (TKT-525 S2); nothing was recorded")
	default:
		return fmt.Errorf("outcome kind must be %s (%s)", store.OutcomeRefundedOutside, recordRefundOutcomeUsage)
	}
	if err := in.Validate(); err != nil {
		return err
	}
	paymentsURL := strings.TrimSuffix(os.Getenv("PAYMENTS_URL"), "/")
	if paymentsURL == "" {
		return errors.New("PAYMENTS_URL is required: nothing is recorded unless payments proves the refund failed")
	}
	token := os.Getenv("PAYMENTS_INTERNAL_TOKEN")
	if token == "" {
		token = os.Getenv("INTERNAL_SERVICE_TOKEN")
	}
	if token == "" {
		return errors.New("PAYMENTS_INTERNAL_TOKEN (or INTERNAL_SERVICE_TOKEN) is required to read the payments proof")
	}

	ctx, cancel := context.WithTimeout(context.Background(), operatorDBTimeout)
	defer cancel()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	svc := refundoutcome.New(db, refundoutcome.NewHTTPPayments(paymentsURL, token, paymentsEvidenceTimeout))
	res, err := svc.RecordRefundedOutside(ctx, in)
	if err != nil {
		reportRefundOutcomeRefusal(refundID, err)
		return err
	}
	o := res.Outcome
	fmt.Printf("organizer=%s refund=%s outcome=%s amount=%d currency=%s external_ref=%q operator=%q session_user=%q recorded_at=%s replay=%t\n",
		org, refundID, o.Kind, o.Amount, o.Currency, o.ExternalRef, o.OperatorName, o.SessionUser,
		o.RecordedAt.UTC().Format(time.RFC3339Nano), res.Replay)
	if res.Replay {
		fmt.Fprintf(os.Stderr, "refund %s already carried this exact outcome; nothing changed. The record above "+
			"is the original, with its original time and attribution.\n", refundID)
		return nil
	}
	fmt.Fprintf(os.Stderr, "refund %s is completed as refunded outside the platform for %d %s (provider refund %s "+
		"failed). Both refund allowances stay consumed. The reversal reconciler will void the tickets and then "+
		"return the capacity; watch the reversal backlog. No payments fact was written.\n",
		refundID, o.Amount, o.Currency, res.Proof.ProviderRef)
	return nil
}

// reportRefundOutcomeRefusal tells the operator where each refusal sends them. The refusals
// are distinguishable because the next action differs.
func reportRefundOutcomeRefusal(refundID uuid.UUID, err error) {
	switch {
	case errors.Is(err, store.ErrRefundOutcomeNotFound):
		fmt.Fprintf(os.Stderr, "REFUSED: no refund %s for this organizer. Nothing was recorded. Check both ids.\n", refundID)
	case errors.Is(err, store.ErrRefundOutcomeConflict):
		fmt.Fprintf(os.Stderr, "REFUSED: refund %s already carries a different outcome. Nothing was recorded. "+
			"An outcome is append-only; investigate before any further money action.\n", refundID)
	case errors.Is(err, store.ErrRefundOutcomeIneligible):
		fmt.Fprintf(os.Stderr, "REFUSED: refund %s was completed through payments. Its money moved through the "+
			"platform; there is no outcome to record.\n", refundID)
	case errors.Is(err, refundoutcome.ErrProofRefused):
		fmt.Fprintf(os.Stderr, "REFUSED: payments refused the failure proof for refund %s (%v). Nothing was "+
			"recorded. The leg may have completed, been abandoned, or the provider may say the refund did not "+
			"fail: reconcile the original refund first.\n", refundID, err)
	case errors.Is(err, refundoutcome.ErrProofUnavailable), errors.Is(err, refundoutcome.ErrProofMismatch):
		fmt.Fprintf(os.Stderr, "REFUSED: payments could not prove that refund %s failed (%v). Nothing was "+
			"recorded and both allowances stay reserved. Fix the cause and run the same command again; a "+
			"refund with no provider stamp can never be proven.\n", refundID, err)
	case errors.Is(err, store.ErrRefundOutcomeStale):
		fmt.Fprintf(os.Stderr, "REFUSED: refund %s changed while the command ran. Nothing was recorded. Run it again.\n", refundID)
	}
}
