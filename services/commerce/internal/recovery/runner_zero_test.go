package recovery

import (
	"errors"
	"reflect"
	"testing"

	"ticketing/services/commerce/internal/store"
)

// TKT-285 (D13, D17). A zero-total checkout skips the PSP, so payments knows no operation for
// the order and the runner's old reading of "no operation" — payment never attempted, release
// the seat — would silently cancel a comped sale that had already been finalized.
//
// The runner therefore classifies a zero-total `created` order by payments' operation LOOKUP
// (a read), not by the amount alone: found means a legacy order that bound an operation and
// keeps every existing branch; not found with the order.created fact present means the
// PSP-skipped path and is COMPLETED; not found without it means checkout never got that far and
// the order is released exactly as a paid one would be.
//
// The payments fake records every port call, so "never asked about a charge" is asserted as
// counts (status, void, refund) and as the one lookup, not as the absence of a method.

func zeroStuck(status string) store.StuckOrder {
	s := stuck(status)
	s.Amount, s.Currency = 0, "EUR"
	return s
}

// intentRecorded is the fixture tune for "checkout got as far as the order.created fact".
func intentRecorded(p *ports) { p.store.recorded = map[string]bool{"order.created": true} }

// assertNoPaymentFollowUp fails on any payments call beyond `lookups` operation lookups.
func assertNoPaymentFollowUp(t *testing.T, p *ports, lookups int) {
	t.Helper()
	if p.payments.calls != lookups {
		t.Errorf("payments lookups = %d, want %d", p.payments.calls, lookups)
	}
	if p.payments.statusCalls != 0 || p.payments.voidCalls != 0 || p.payments.refundCalls != 0 {
		t.Errorf("payments status/void/refund = %d/%d/%d, want 0/0/0: a PSP-skipped order has no charge to resolve or compensate",
			p.payments.statusCalls, p.payments.voidCalls, p.payments.refundCalls)
	}
}

// assertNothingReleasedOrFailed fails if the order was released, failed, refunded or queued.
func assertNothingReleasedOrFailed(t *testing.T, p *ports) {
	t.Helper()
	if p.inventory.releases != 0 || len(p.store.released) != 0 || len(p.journal.facts) != 0 ||
		len(p.store.outcomes) != 0 || len(p.store.refunded) != 0 || len(p.store.queued) != 0 {
		t.Errorf("order was released/failed/refunded/queued: releases=%d released=%d order.failed=%d outcomes=%v refunded=%d queued=%d",
			p.inventory.releases, len(p.store.released), len(p.journal.facts), p.store.outcomes,
			len(p.store.refunded), len(p.store.queued))
	}
}

// Row: zero + created + lookup not-found + order.created present — the PSP-skipped checkout
// that crashed after its intent fact. COMPLETED, never released: the buyer asked for a comp.
func TestZeroCreatedWithNoOperationAndAnIntentFactIsCompletedNotReleased(t *testing.T) {
	order := zeroStuck("created")
	p, resolved := run(t, []store.StuckOrder{order}, func(p *ports) {
		p.payments.found = false
		intentRecorded(p)
	})

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1; trace=%v", resolved, p.trace.steps)
	}
	assertNoPaymentFollowUp(t, p, 1)
	assertNothingReleasedOrFailed(t, p)
	if len(p.store.parked) != 0 {
		t.Errorf("parked %v, want none", p.store.parked)
	}
	if len(p.journal.created) != 1 || len(p.journal.completed) != 1 || len(p.completer.completed) != 1 {
		t.Fatalf("order.created=%d order.completed=%d completions=%d, want 1/1/1; trace=%v",
			len(p.journal.created), len(p.journal.completed), len(p.completer.completed), p.trace.steps)
	}
	if p.journal.completed[0].Amount != 0 || p.journal.completed[0].Currency != "EUR" {
		t.Errorf("order.completed carries %d %s, want 0 EUR", p.journal.completed[0].Amount, p.journal.completed[0].Currency)
	}
	// The whole chain, in order: evidence (lookup, intent read), the intent fact replayed, the
	// seat secured, the completion fact, the completion transaction, then the lease dropped.
	want := []string{
		"payments.LookupOperation",
		"store.OrderFactRecorded:order.created",
		"journal.OrderCreated",
		"inventory.Confirm",
		"journal.OrderCompleted",
		"completer.Complete",
		"store.ClearRecoveryClaim",
	}
	if !reflect.DeepEqual(p.trace.steps, want) {
		t.Fatalf("trace = %v\nwant    %v", p.trace.steps, want)
	}
	if calls := p.trace.externalCalls(); calls > MaxCallsPerOrder {
		t.Fatalf("zero completion made %d external calls, exceeding MaxCallsPerOrder=%d", calls, MaxCallsPerOrder)
	}
}

// Row: zero + confirmation_pending. Only the new skip path produces one for a zero total, so
// there is no lookup at all — the evidence is already in hand.
func TestZeroConfirmationPendingCompletesWithoutALookup(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{zeroStuck("confirmation_pending")}, nil)

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1; trace=%v", resolved, p.trace.steps)
	}
	assertNoPaymentFollowUp(t, p, 0)
	assertNothingReleasedOrFailed(t, p)
	want := []string{
		"journal.OrderCreated",
		"inventory.Confirm",
		"journal.OrderCompleted",
		"completer.Complete",
		"store.ClearRecoveryClaim",
	}
	if !reflect.DeepEqual(p.trace.steps, want) {
		t.Fatalf("trace = %v\nwant    %v", p.trace.steps, want)
	}
}

// Row: zero + created + lookup not-found + NO order.created fact. Checkout never wrote the
// intent, so it never reached the point of promising the buyer anything: released as
// `not_attempted`, byte-for-byte what a paid order with no operation gets.
func TestZeroCreatedWithNoOperationAndNoIntentFactIsNotAttemptedThenReleased(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{zeroStuck("created")}, func(p *ports) {
		p.payments.found = false
	})

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1; trace=%v", resolved, p.trace.steps)
	}
	assertNoPaymentFollowUp(t, p, 1)
	if len(p.store.outcomes) != 1 || p.store.outcomes[0] != "not_attempted" {
		t.Fatalf("outcomes = %v, want exactly [not_attempted]", p.store.outcomes)
	}
	if p.inventory.releases != 1 || len(p.journal.facts) != 1 || len(p.store.released) != 1 {
		t.Errorf("releases=%d order.failed=%d released=%d, want 1/1/1", p.inventory.releases, len(p.journal.facts), len(p.store.released))
	}
	if p.inventory.confirmed != 0 || len(p.journal.created) != 0 || len(p.journal.completed) != 0 || len(p.completer.completed) != 0 {
		t.Errorf("a pre-intent order was driven toward completion: confirm=%d created=%d completed=%d completions=%d",
			p.inventory.confirmed, len(p.journal.created), len(p.journal.completed), len(p.completer.completed))
	}
}

// A lookup that ERRORS (transport, 5xx) proves nothing. It retries; it must never be read as
// "not found" — that reading is the silent release this ticket exists to prevent.
func TestZeroLookupErrorRetriesAndIsNeverReadAsNotFound(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{zeroStuck("created")}, func(p *ports) {
		p.payments.err = errors.New("payments unavailable")
		intentRecorded(p)
	})

	if resolved != 0 {
		t.Fatalf("resolved = %d, want 0: the order must stay claimable", resolved)
	}
	if len(p.store.failed) != 1 {
		t.Fatalf("handed back %d times, want 1 (backoff)", len(p.store.failed))
	}
	assertNoPaymentFollowUp(t, p, 1)
	assertNothingReleasedOrFailed(t, p)
	if p.inventory.confirmed != 0 || len(p.completer.completed) != 0 || len(p.store.parked) != 0 {
		t.Errorf("the order was driven past an unanswered lookup: confirm=%d completions=%d parked=%v",
			p.inventory.confirmed, len(p.completer.completed), p.store.parked)
	}
}

// D17(a): a zero-total `created` order whose lookup FINDS an operation is a legacy order and
// keeps every existing branch — operation evidence decides, provider status for an unresolved
// operation, and compensation for proven captured money against a gone claim.
func TestZeroCreatedWhoseLookupFindsAnOperationKeepsTheExistingBranches(t *testing.T) {
	t.Run("captured confirms and completes with no completion fact", func(t *testing.T) {
		p, resolved := run(t, []store.StuckOrder{zeroStuck("created")}, func(p *ports) {
			p.payments.found = true
			p.payments.op = Operation{Resolved: true, Status: "captured"}
			intentRecorded(p)
		})
		if resolved != 1 || p.inventory.confirmed != 1 || len(p.completer.completed) != 1 {
			t.Fatalf("resolved=%d confirmed=%d completions=%d, want 1/1/1; trace=%v", resolved, p.inventory.confirmed, len(p.completer.completed), p.trace.steps)
		}
		// The legacy path, untouched: it never journalled order.created/order.completed.
		if len(p.journal.created) != 0 || len(p.journal.completed) != 0 {
			t.Errorf("legacy captured path journalled created=%d completed=%d, want 0/0", len(p.journal.created), len(p.journal.completed))
		}
		assertNothingReleasedOrFailed(t, p)
	})
	t.Run("unresolved operation asks the provider for status", func(t *testing.T) {
		p, _ := run(t, []store.StuckOrder{zeroStuck("created")}, func(p *ports) {
			p.payments.found = true
			p.payments.op = Operation{Resolved: false}
			p.payments.status = PSPStatus{Outcome: "declined"}
			intentRecorded(p)
		})
		if p.payments.statusCalls != 1 {
			t.Fatalf("status calls = %d, want 1: an unresolved bound operation is resolved by provider status; trace=%v", p.payments.statusCalls, p.trace.steps)
		}
		if p.inventory.confirmed != 0 || len(p.completer.completed) != 0 {
			t.Errorf("a declined legacy order was completed")
		}
	})
	t.Run("captured with a gone claim is compensated", func(t *testing.T) {
		p, resolved := run(t, []store.StuckOrder{zeroStuck("created")}, func(p *ports) {
			p.payments.found = true
			p.payments.op = Operation{Resolved: true, Status: "captured"}
			p.inventory.confirmErr = ErrClaimGone
			p.payments.status = PSPStatus{Outcome: "captured", Captured: true, Authorized: true,
				AuthorizedAmount: 1, CapturedAmount: 1, Currency: "EUR"}
			p.payments.refundResult = CompensationResult{Status: "refunded"}
			intentRecorded(p)
		})
		if resolved != 1 || len(p.store.queued) != 1 || p.payments.refundCalls != 1 || len(p.store.refunded) != 1 {
			t.Fatalf("resolved=%d queued=%d refunds=%d refunded=%d, want 1/1/1/1; trace=%v",
				resolved, len(p.store.queued), p.payments.refundCalls, len(p.store.refunded), p.trace.steps)
		}
		if len(p.store.parked) != 0 {
			t.Errorf("a legacy compensable order was parked: %v", p.store.parked)
		}
	})
}

// A claim that is gone during the PSP-skipped completion PARKS. Nothing was captured, so the
// captured-money compensation arm (provider status, refund) is the wrong place: it assumes
// money moved. Nothing is completed either — the seat can never be delivered.
func TestZeroCompletionWithAGoneClaimParksAndNeverCompensates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order store.StuckOrder
		tune  func(*ports)
	}{
		{"created", zeroStuck("created"), func(p *ports) { p.payments.found = false; intentRecorded(p) }},
		{"confirmation_pending", zeroStuck("confirmation_pending"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, resolved := run(t, []store.StuckOrder{tc.order}, func(p *ports) {
				p.inventory.confirmErr = ErrClaimGone
				if tc.tune != nil {
					tc.tune(p)
				}
			})
			if resolved != 1 || len(p.store.parked) != 1 {
				t.Fatalf("resolved=%d parked=%v, want 1 and exactly one park; trace=%v", resolved, p.store.parked, p.trace.steps)
			}
			if p.payments.statusCalls != 0 || p.payments.voidCalls != 0 || p.payments.refundCalls != 0 {
				t.Errorf("status/void/refund = %d/%d/%d, want 0/0/0", p.payments.statusCalls, p.payments.voidCalls, p.payments.refundCalls)
			}
			assertNothingReleasedOrFailed(t, p)
			if len(p.journal.completed) != 0 || len(p.completer.completed) != 0 || p.store.cleared != 0 {
				t.Errorf("a gone claim was completed: completed=%d completions=%d cleared=%d",
					len(p.journal.completed), len(p.completer.completed), p.store.cleared)
			}
		})
	}
}

// Every step of the zero completion can fail transiently. Each failure hands the order back
// for another pass and does nothing irreversible past it: no release, no park, no refund — and
// in particular the completion transaction never runs before the steps that precede it.
func TestZeroCompletionTransientFailuresRetryWithoutReleasing(t *testing.T) {
	boom := errors.New("transient")
	for _, tc := range []struct {
		name string
		tune func(*ports)
		// What had been ATTEMPTED when the failure hit: confirm calls, whether the order.completed
		// fact and the completion transaction were reached (1) or must not have been (0).
		confirmed, completedFact, completions int
	}{
		{"intent read", func(p *ports) { p.store.recordedErr = boom }, 0, 0, 0},
		{"order.created replay", func(p *ports) { p.journal.createdErr = boom }, 0, 0, 0},
		{"inventory confirm", func(p *ports) { p.inventory.confirmErr = boom }, 1, 0, 0},
		{"order.completed fact", func(p *ports) { p.journal.completedErr = boom }, 1, 1, 0},
		{"completion transaction", func(p *ports) { p.completer.err = boom }, 1, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, resolved := run(t, []store.StuckOrder{zeroStuck("created")}, func(p *ports) {
				p.payments.found = false
				intentRecorded(p)
				tc.tune(p)
			})
			if resolved != 0 || len(p.store.failed) != 1 || !errors.Is(p.store.failed[0], boom) {
				t.Fatalf("resolved=%d handed back=%v, want 0 and exactly one hand-back of the cause; trace=%v", resolved, p.store.failed, p.trace.steps)
			}
			assertNoPaymentFollowUp(t, p, 1)
			assertNothingReleasedOrFailed(t, p)
			if len(p.store.parked) != 0 || p.store.cleared != 0 {
				t.Errorf("parked=%v cleared=%d, want neither", p.store.parked, p.store.cleared)
			}
			if p.inventory.confirmed != tc.confirmed {
				t.Errorf("confirm calls = %d, want %d; trace=%v", p.inventory.confirmed, tc.confirmed, p.trace.steps)
			}
			if p.trace.indexOf("journal.OrderCompleted") >= 0 != (tc.completedFact == 1) {
				t.Errorf("order.completed attempted = %v, want %v; trace=%v", p.trace.indexOf("journal.OrderCompleted") >= 0, tc.completedFact == 1, p.trace.steps)
			}
			if p.trace.indexOf("completer.Complete") >= 0 != (tc.completions == 1) {
				t.Errorf("completion attempted = %v, want %v; trace=%v", p.trace.indexOf("completer.Complete") >= 0, tc.completions == 1, p.trace.steps)
			}
		})
	}
}

// The zero dispatch is scoped to `created` and `confirmation_pending`. The other statuses a
// zero-total order can wear keep their existing paths and their existing evidence. Only the new
// skip path produces a zero `confirmation_pending`. A zero `payment_unknown` or
// `reconciliation_required` row can come from before this change. A zero `release_pending` row can
// also come from TODAY'S recovery: the no-intent release (see the not_attempted test above)
// records the outcome and sets that status before it releases. The zero-total gone-claim park
// also produces one today: ParkForReconciliation sets `reconciliation_required` and parks the row,
// so it is never claimed again. If an operator un-parks it, resolveReconciliation asks payments
// for status, finds no operation, and parks it again.
func TestZeroTotalOrdersInOtherStatusesKeepTheirExistingPaths(t *testing.T) {
	t.Run("payment_unknown with no operation is released", func(t *testing.T) {
		p, resolved := run(t, []store.StuckOrder{zeroStuck("payment_unknown")}, func(p *ports) {
			p.payments.found = false
			intentRecorded(p)
		})
		if resolved != 1 || len(p.store.outcomes) != 1 || p.store.outcomes[0] != "not_attempted" || p.inventory.releases != 1 {
			t.Fatalf("resolved=%d outcomes=%v releases=%d, want the paid release path; trace=%v", resolved, p.store.outcomes, p.inventory.releases, p.trace.steps)
		}
		if p.inventory.confirmed != 0 || len(p.completer.completed) != 0 || len(p.journal.completed) != 0 {
			t.Error("a payment_unknown order was completed by the zero path")
		}
	})
	t.Run("reconciliation_required asks payments for status", func(t *testing.T) {
		p, resolved := run(t, []store.StuckOrder{zeroStuck("reconciliation_required")}, func(p *ports) {
			p.payments.statusErr = ErrOperationNotFound
			intentRecorded(p)
		})
		if resolved != 1 || p.payments.statusCalls != 1 || len(p.store.parked) != 1 {
			t.Fatalf("resolved=%d status=%d parked=%v, want 1/1/one park; trace=%v", resolved, p.payments.statusCalls, p.store.parked, p.trace.steps)
		}
		if p.inventory.confirmed != 0 || len(p.completer.completed) != 0 {
			t.Error("a reconciliation_required order was completed by the zero path")
		}
	})
	t.Run("release_pending finishes its release", func(t *testing.T) {
		order := zeroStuck("release_pending")
		order.TerminalOutcome = "not_attempted"
		p, resolved := run(t, []store.StuckOrder{order}, func(p *ports) { intentRecorded(p) })
		if resolved != 1 || p.inventory.releases != 1 || len(p.store.released) != 1 {
			t.Fatalf("resolved=%d releases=%d released=%d, want 1/1/1; trace=%v", resolved, p.inventory.releases, len(p.store.released), p.trace.steps)
		}
		if p.inventory.confirmed != 0 || len(p.completer.completed) != 0 {
			t.Error("a release_pending order was completed by the zero path")
		}
	})
}

// The discriminator is the persisted GROSS total, and a PAID order with the same evidence is
// still released. Without this a zero branch keyed on anything wider than `Amount == 0` — an
// intent fact alone, say — would complete paid orders whose charge was never attempted.
func TestAPaidCreatedOrderWithNoOperationIsStillReleasedEvenWithAnIntentFact(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{stuck("created")}, func(p *ports) {
		p.payments.found = false
		intentRecorded(p)
	})
	if resolved != 1 || len(p.store.outcomes) != 1 || p.store.outcomes[0] != "not_attempted" || p.inventory.releases != 1 {
		t.Fatalf("resolved=%d outcomes=%v releases=%d, want not_attempted and a release; trace=%v", resolved, p.store.outcomes, p.inventory.releases, p.trace.steps)
	}
	if p.inventory.confirmed != 0 || len(p.completer.completed) != 0 || len(p.journal.completed) != 0 || len(p.journal.created) != 0 {
		t.Error("a paid order with no operation was driven toward completion")
	}
}
