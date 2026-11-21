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

const recordRefundOutcomeUsage = "usage: commerce record-refund-outcome <organizer-id> <refund-id> refunded_outside <operator> <external-ref> <reason> | " +
	"commerce record-refund-outcome <organizer-id> <refund-id> abandoned <operator> <reason>"

// recordRefundOutcome records one operator outcome.
//
// Usage:
//
//	commerce record-refund-outcome <organizer-id> <refund-id> refunded_outside <operator> <external-ref> <reason>
//	commerce record-refund-outcome <organizer-id> <refund-id> abandoned <operator> <reason>
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
// abandoned means: the operator has confirmed that no outside transfer occurred and none is in
// flight, and that nobody will refund this attempt (TKT-525 S2). Payments proves the leg's
// provider refund terminally failed, commits its marker and returns the provider's reference,
// which is recorded as the outcome's external reference; the operator never types it. The
// refund stays pending. Each service releases ONLY this refund's allowance, so staff can then
// refund the buyer again under a NEW key through the Back Office. No command re-submits.
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
		if len(args) != 5 {
			return errors.New(recordRefundOutcomeUsage)
		}
		in = store.RefundOutcomeRequest{
			OrganizerID: org, RefundID: refundID, Kind: store.OutcomeAbandoned,
			OperatorName: args[3], Reason: args[4],
		}
	default:
		return fmt.Errorf("outcome kind must be %s or %s (%s)", store.OutcomeRefundedOutside, store.OutcomeAbandoned, recordRefundOutcomeUsage)
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
		return errors.New("PAYMENTS_INTERNAL_TOKEN (or INTERNAL_SERVICE_TOKEN) is required to reach payments")
	}

	ctx, cancel := context.WithTimeout(context.Background(), operatorDBTimeout)
	defer cancel()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	if in.Kind == store.OutcomeAbandoned {
		return recordAbandoned(ctx, db, in, paymentsURL, token)
	}
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

// recordAbandoned is the `abandoned` form. Payments first, then Commerce (ADR-087).
func recordAbandoned(ctx context.Context, db *sql.DB, in store.RefundOutcomeRequest, paymentsURL, token string) error {
	svc := refundoutcome.NewAbandon(db, refundoutcome.NewHTTPAbandoner(paymentsURL, token, paymentsEvidenceTimeout))
	res, err := svc.RecordAbandoned(ctx, in)
	if err != nil {
		reportAbandonedRefusal(in.RefundID, err)
		return err
	}
	o := res.Outcome
	fmt.Printf("organizer=%s refund=%s outcome=%s amount=%d currency=%s external_ref=%q operator=%q session_user=%q recorded_at=%s replay=%t\n",
		in.OrganizerID, in.RefundID, o.Kind, o.Amount, o.Currency, o.ExternalRef, o.OperatorName, o.SessionUser,
		o.RecordedAt.UTC().Format(time.RFC3339Nano), res.Replay)
	if res.Replay {
		fmt.Fprintf(os.Stderr, "refund %s already carried this exact outcome; nothing changed. The record above "+
			"is the original, with its original time and attribution.\n", in.RefundID)
		return nil
	}
	fmt.Fprintf(os.Stderr, "refund %s is recorded as abandoned (provider refund %s failed). It stays pending and is "+
		"never re-submitted. Its %d %s allowance is released in both services: staff can refund the buyer again "+
		"under a NEW key through the Back Office.\n", in.RefundID, o.ExternalRef, o.Amount, o.Currency)
	return nil
}

// reportAbandonedRefusal tells the operator where each refusal sends them.
func reportAbandonedRefusal(refundID uuid.UUID, err error) {
	switch {
	case errors.Is(err, store.ErrRefundOutcomeNotFound):
		fmt.Fprintf(os.Stderr, "REFUSED: no refund %s for this organizer. Nothing was recorded. Check both ids.\n", refundID)
	case errors.Is(err, store.ErrRefundOutcomeConflict):
		fmt.Fprintf(os.Stderr, "REFUSED: refund %s already carries a different outcome. Nothing was recorded. "+
			"An outcome is append-only; investigate before any further money action.\n", refundID)
	case errors.Is(err, store.ErrRefundOutcomeIneligible):
		fmt.Fprintf(os.Stderr, "REFUSED: refund %s is completed. There is nothing to abandon.\n", refundID)
	case errors.Is(err, refundoutcome.ErrAbandonRefused):
		fmt.Fprintf(os.Stderr, "REFUSED: payments refused to abandon refund %s (%v). Nothing was recorded in either "+
			"service. A missing leg means the target is wrong; a completed leg or a provider success goes through "+
			"ordinary refund reconciliation.\n", refundID, err)
	case errors.Is(err, refundoutcome.ErrAbandonUncertain):
		fmt.Fprintf(os.Stderr, "UNCERTAIN: payments' answer for refund %s is unknown (%v). Payments may have released "+
			"its allowance; Commerce recorded nothing and keeps the quantity reserved. Run the SAME command again. "+
			"A refund with no provider stamp can never be proven; its allowances stay reserved.\n", refundID, err)
	case errors.Is(err, store.ErrRefundOutcomeStale):
		fmt.Fprintf(os.Stderr, "REFUSED: refund %s changed while the command ran. Commerce recorded nothing. Run the "+
			"SAME command again.\n", refundID)
	default:
		fmt.Fprintf(os.Stderr, "FAILED: refund %s was not recorded in Commerce (%v). If payments already accepted, its "+
			"allowance is released and Commerce keeps the quantity reserved. Run the SAME command again.\n", refundID, err)
	}
}
