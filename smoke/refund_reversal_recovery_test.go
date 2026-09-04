//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Outstanding refund reversals are driven to completion with no human replaying the
// request (TKT-163, ADR-062).
//
// This test lives at the composed-stack seam because the claim it makes is not about
// commerce: it is that a refund taken while ACCESS IS DOWN is revisited and completed after
// access comes back. Nothing below this tier can express "access is down" — the runner's
// unit tests drive a fake that returns whatever they say, and the store tests prove which
// rows are claimable, not that anything claims them. Stopping a real container is the only
// way the outage is real.
//
// It is also the only tier that can observe the WIRING: that run() actually starts the
// reconciler and mounts it on a live process. A runner that is perfect and never started
// looks exactly like a runner that works, to every test above this one.

// refundState is what commerce reports about a refund's two obligations. The booleans are
// required in the response, never omitted — a caller who cannot tell "voided" from "we did
// not get to it" assumes the first (ADR-038 §6).
type refundState struct {
	RefundID         string `json:"refund_id"`
	TicketsVoided    bool   `json:"tickets_voided"`
	CapacityReturned bool   `json:"capacity_returned"`
}

// TestARefundTakenWhileAccessIsDownCompletesItselfAfterwards is COS 1 and COS 3.
//
// The sequence is the ticket's own demo: stop access, refund, observe the obligation
// outstanding, restart access, watch it complete with NO further request. The middle
// assertion is not decoration — without it the test would pass against a build where the
// refund never needed reconciling at all, which is the ordinary case and would make the
// whole test vacuous.
func TestARefundTakenWhileAccessIsDownCompletesItselfAfterwards(t *testing.T) {
	// A generous backstop, not the bound: the phase deadlines below are what the
	// assertions are written against. A context that expired mid-recovery would leave
	// access stopped for every later test in this package, which is the one failure mode
	// worth over-budgeting for.
	//
	// 10 minutes rather than 5 because the outer context must OUTLAST the poll it wraps:
	// verified by running this against a build with the reconciler unwired, where a 5m
	// context expired first and the failure read "context deadline exceeded" from a psql
	// call — true, but it names the harness rather than the defect. The poll's own
	// deadline has to be the thing that fires, so the message says which obligation was
	// never discharged.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	container := project + "-access-1"
	reversalInterval, err := commerceReversalInterval(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slot, tt := publishedSlot(t, "Reversal Hall", 10)
	// buyOne waits for issuance before returning, which matters here: access answers 503
	// ("not yet") while a refund outruns issuance, and a test that refunded earlier would
	// be observing that race rather than the outage it means to create.
	order := buyOne(t, slot, tt, "reversal-"+slot, 2)

	// Access goes away. Restarting it is registered before the stop, so a failure between
	// here and the restart below cannot strand the stack for the rest of the suite.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer ccancel()
		if out, err := dockerRun(cctx, "start", container); err != nil {
			t.Errorf("restart access in cleanup: %v: %s", err, out)
		}
	})
	if out, err := dockerRun(ctx, "stop", container); err != nil {
		t.Fatalf("stop access: %v: %s", err, out)
	}

	// The refund still succeeds: the money has moved and the refund row is durable, so the
	// honest answer is a 200 reporting the reversal outstanding rather than a failure that
	// invites a retry of money already returned (ADR-038 §6).
	code, body := internalJSON(t, http.MethodPost,
		fmt.Sprintf("%s/internal/orders/%s/refunds", commerceURL, order), "reversal-refund-"+slot,
		map[string]any{"organizer_id": organizerID, "quantity": 1,
			"actor": "ops@example.test", "reason": "access outage reversal test"})
	if code != http.StatusOK {
		t.Fatalf("refund during access outage: %d %s (the money path must not fail because a "+
			"downstream obligation cannot be discharged)", code, body)
	}
	var refund refundState
	if err := json.Unmarshal(body, &refund); err != nil {
		t.Fatal(err)
	}

	// THE PRECONDITION. Without this the test proves nothing: a refund that was never
	// outstanding is completed by the original request, and the reconciler could be deleted
	// entirely with this test still green.
	if refund.TicketsVoided {
		t.Fatalf("the refund reported tickets_voided=true while access was stopped — this test "+
			"cannot observe reconciliation because there was nothing to reconcile; refund=%+v", refund)
	}

	// Still outstanding while access stays down. This separates "the reconciler completed
	// it" from "it was never outstanding for long", and it is what would catch a capacity
	// return that ran WITHOUT the voiding — the one ordering that can oversell (ADR-038 §1).
	stillDown := readRefundState(t, ctx, refund.RefundID)
	if stillDown.TicketsVoided || stillDown.CapacityReturned {
		t.Fatalf("an obligation was discharged while access was stopped: %+v — a capacity "+
			"return without a voiding is the sequence that oversells", stillDown)
	}

	// Access comes back. Nothing else is sent: no replayed refund, no operator action.
	if out, err := dockerRun(ctx, "start", container); err != nil {
		t.Fatalf("start access: %v: %s", err, out)
	}
	retry(t, 90*time.Second, func() error {
		if out, err := dockerRun(ctx, "exec", container, "/app", "healthcheck"); err != nil {
			return fmt.Errorf("access not healthy yet: %v: %s", err, out)
		}
		return nil
	})

	// The deadline is derived from the runner's configured interval rather than guessed, so
	// this does not become a flake the day the default changes. Several intervals plus room
	// for the pass itself and for access's issuance path to settle.
	deadline := 8 * reversalInterval
	if deadline < 20*time.Second {
		deadline = 20 * time.Second
	}
	// A read error is retried, not fatal: psql can lose a race with a container restart,
	// and killing the poll on it would report a transport hiccup as "the reconciler never
	// ran". The distinction matters because this poll IS the assertion — when it fails, the
	// message is the whole diagnosis.
	retry(t, deadline, func() error {
		s, err := refundStateOf(ctx, refund.RefundID)
		if err != nil {
			return fmt.Errorf("read refund row: %w", err)
		}
		if !s.TicketsVoided {
			return fmt.Errorf("tickets STILL not voided %s after access came back: nothing is "+
				"driving the outstanding reversal — is the runner wired into run()? state=%+v",
				deadline, s)
		}
		if !s.CapacityReturned {
			return fmt.Errorf("tickets voided but capacity STILL not returned %s after access "+
				"came back: the second obligation is not being driven; state=%+v", deadline, s)
		}
		return nil
	})
}

// readRefundState reads the refund's two obligations straight from commerce's database.
//
// THE READ MUST NOT DRIVE. The obvious alternative — replaying the refund under the same
// idempotency key — returns both booleans and is one line, but a replay calls DriveReversal
// itself: it is precisely the manual retry this ticket exists to remove. A test polling that
// endpoint would discharge the obligation with its own poll and stay green with the
// reconciler deleted, which is the exact shape of a test that cannot reach the state it
// claims to detect. There is also no GET for a single refund, and adding one would make a
// new documented 2xx operation this ticket deliberately avoids (ADR-030's coverage gate).
//
// So it reads the row. `psql` in the postgres container rather than a Go driver, because the
// smoke package talks to the stack over docker and has no database dependency — adding one
// for two booleans would be the larger change.
func refundStateOf(ctx context.Context, refundID string) (refundState, error) {
	out, err := dockerRun(ctx, "exec", project+"-postgres-1", "psql", "-U", "commerce",
		"-d", "commerce", "-tA", "-c",
		fmt.Sprintf(`SELECT tickets_voided_at IS NOT NULL, capacity_returned_at IS NOT NULL
		             FROM order_refunds WHERE id='%s'`, refundID))
	if err != nil {
		return refundState{}, fmt.Errorf("%v: %s", err, out)
	}
	fields := strings.Split(strings.TrimSpace(out), "|")
	if len(fields) != 2 {
		return refundState{}, fmt.Errorf("refund %s not found in commerce (psql said %q)",
			refundID, strings.TrimSpace(out))
	}
	return refundState{
		RefundID:         refundID,
		TicketsVoided:    fields[0] == "t",
		CapacityReturned: fields[1] == "t",
	}, nil
}

// readRefundState is the fatal form, for the single observation that must succeed on the
// first try — the one taken while access is stopped, where a retry would only give the
// reconciler time to change the answer being asserted.
func readRefundState(t *testing.T, ctx context.Context, refundID string) refundState {
	t.Helper()
	s, err := refundStateOf(ctx, refundID)
	if err != nil {
		t.Fatalf("read refund row: %v", err)
	}
	return s
}

func commerceReversalInterval(ctx context.Context) (time.Duration, error) {
	container := project + "-commerce-1"
	out, err := dockerRun(ctx, "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", container)
	if err != nil {
		return 0, fmt.Errorf("inspect %s environment: %w: %s", container, err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		value, found := strings.CutPrefix(line, "REFUND_REVERSAL_INTERVAL=")
		if !found {
			continue
		}
		interval, parseErr := time.ParseDuration(value)
		if parseErr != nil || interval <= 0 {
			return 0, fmt.Errorf("%s has invalid REFUND_REVERSAL_INTERVAL %q", container, value)
		}
		if interval > 5*time.Second {
			return 0, fmt.Errorf("%s REFUND_REVERSAL_INTERVAL=%s is not a short smoke cadence", container, interval)
		}
		return interval, nil
	}
	return 0, fmt.Errorf("%s has no REFUND_REVERSAL_INTERVAL", container)
}
