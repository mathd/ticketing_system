//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	orderCompletedSubject = "platform.commerce.order.completed"
	accessFailureSubject  = "platform.access.ticket-issuance.failed"

	// The durable this test deletes, and the exact line access writes when it
	// notices. The literal is assembled by three pieces of production code and
	// asserted here verbatim on purpose — it is the whole discriminator (see
	// TestAccessDurableDeletionTerminatesAndRecovers):
	//   consumer/run.go       "%s: consume context closed (durable deleted or subscription terminated)"
	//   consumer/policy.go    passes "access-slot-policy" as %s, returns the error unwrapped
	//   cmd/access/main.go    fmt.Fprintf(os.Stderr, "%s: %v\n", serviceName, err) then os.Exit(1)
	// Changing any of the three should break this test loudly rather than
	// silently degrade it to "access restarted for some reason".
	policyDurable     = "access-slot-policy"
	policyTermination = "access: access-slot-policy: consume context closed " +
		"(durable deleted or subscription terminated)"
	// consumer.SubjectPerformancePublished — the smoke module is its own Go
	// module and does not import the services, so the literal is repeated here
	// as every other subject in this package is.
	performancePublishedSubject = "platform.catalog.performance.published"
)

type accessFailureEvent struct {
	Data struct {
		SourceEventID      string `json:"source_event_id"`
		MessageFingerprint string `json:"message_fingerprint"`
		Reason             string `json:"reason"`
		Stage              string `json:"stage"`
		Attempts           uint64 `json:"attempts"`
	} `json:"data"`
}

// TestAccessDurableDeletionTerminatesAndRecovers is the broker-level half of
// TKT-97 (TKT-99). TKT-97 unit-tested the *reaction* to ConsumeContext.Closed()
// firing (consumer.waitConsume) against a hand-closed channel; nothing proved
// that nats.go actually fires Closed() when the durable is deleted underneath a
// live consumer. That needs a real JetStream, so it lives here.
//
// What is asserted, and why not /readyz. waitConsume latches ready false AND
// returns an error that tears the process down. ADR-017 §236-241 is explicit
// that Compose does not act on an unhealthy container, so the 503 window lasts
// only from ready.Store(false) to os.Exit(1) — a few scheduling turns. Polling
// for it from the host would be a race dressed up as a test. The durable
// observables are the process exit (a container restart, since every Go service
// runs restart: unless-stopped) and the exact diagnostic on stderr. Both are
// required: a restart alone is satisfied by any crash, and only the
// durable-named message ties the restart to waitConsume.
//
// The half this does NOT cover: removing only ready.Store(false) is invisible
// from here, because the process exits before any host-side probe could see the
// 503. TestWaitConsumeAsyncTerminationLatchesUnreadyAndErrors
// (services/access/internal/consumer/run_test.go) pins that half. Two tests,
// two halves — neither is sufficient alone.
//
// access-slot-policy rather than access-ticket-issuer, deliberately: identical
// CreateOrUpdateConsumer → Consume → cc.Closed() → waitConsume path, so nothing
// about the library mechanism is lost, but the DeliverAll replay on recreation
// is an idempotent projection upsert (store.UpsertSlotPolicy, keyed by envelope
// id) instead of re-running signed lifecycle issuance.
//
// Source position is LOAD-BEARING, not a cost optimisation — an earlier draft
// of this comment claimed the latter and was wrong (ai-review R3). This file
// sorts first in the package, so the stream holds no performance.published
// history and the replay on recreation is empty. Two things depend on that:
//
// The benign one is speed. The one that matters is that access's policy
// consumer only latches ready TRUE once its replay reaches zero pending
// (consumer/policy.go). A single parked message in that history — an unknown or
// future schema, which ADR-017 §5b′ says to park rather than drop — is never
// acked, so pending never reaches zero, so access never becomes ready again
// after the restart this test forces. Recovery and its cleanup fallback would
// both fail and the rest of the package would run against a dead service.
//
// That is latent today (nothing sorts before this file, and nothing publishes a
// parked policy event), which is exactly why it is written down: the failure
// would appear as sixty unrelated tests breaking. If you add a test file
// alphabetically before this one, or teach an earlier test to publish a
// performance.published event, re-read this. The "replay still draining" error
// below is the symptom to grep for.
func TestAccessDurableDeletionTerminatesAndRecovers(t *testing.T) {
	// 90s is a backstop, not the bound: the 15s and 20s ceilings below are what
	// the assertions are written against and what must fire first. A context
	// that expires mid-recovery would leave the stack poisoned for every later
	// test in this package, which is the one failure mode worth over-budgeting.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	container := project + "-access-1"

	connection, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	// t.Cleanup runs LAST-registered-FIRST. Registering the close here — before
	// the recovery cleanup below — is what keeps the connection alive while that
	// cleanup is still using it. Reverse the two and recovery meets a closed
	// connection at the exact moment it matters most.
	t.Cleanup(connection.Close)

	js, err := jetstream.New(connection)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "PLATFORM")
	if err != nil {
		t.Fatal(err)
	}

	// healthy reports whether access is serving with its policy durable intact.
	// Shared by the recovery assertion and the cleanup fallback so the two can
	// never disagree about what "recovered" means.
	healthy := func(ctx context.Context) (*jetstream.ConsumerInfo, error) {
		consumer, err := stream.Consumer(ctx, policyDurable)
		if err != nil {
			return nil, fmt.Errorf("durable %s absent: %w", policyDurable, err)
		}
		info, err := consumer.Info(ctx)
		if err != nil {
			return nil, err
		}
		// Recreated faithfully, not merely present: CreateOrUpdateConsumer is
		// what rebuilds it, and a drifted config would leave the stack subtly
		// wrong for every later test rather than loudly broken.
		if info.Config.FilterSubject != performancePublishedSubject ||
			info.Config.DeliverPolicy != jetstream.DeliverAllPolicy ||
			info.Config.AckPolicy != jetstream.AckExplicitPolicy ||
			info.Config.MaxDeliver != -1 {
			return nil, fmt.Errorf("recreated durable drifted: %+v", info.Config)
		}
		if info.NumPending != 0 || info.NumAckPending != 0 {
			// If this never converges, suspect a parked message in the replayed
			// history rather than slowness — see the file-ordering note on the
			// test above.
			return nil, fmt.Errorf("replay still draining: pending=%d ack_pending=%d "+
				"(if this never converges, a parked performance.published event is blocking readiness)",
				info.NumPending, info.NumAckPending)
		}
		if out, err := dockerRun(ctx, "exec", container, "/app", "healthcheck"); err != nil {
			return nil, fmt.Errorf("access healthcheck: %v: %s", err, out)
		}
		return info, nil
	}

	// settled requires TWO consecutive healthy samples with no restart between
	// them. One sample is not enough: a crash-looping container is healthy at
	// intervals, and "healthy twice with a restart in between" is a crash loop
	// wearing a green badge. Shared by the recovery assertion and the cleanup
	// fallback deliberately — a safety net held to a weaker standard than the
	// assertion it backs up is not a safety net (ai-review R1).
	//
	// Each sample is BRACKETED by restart counts rather than followed by one.
	// Reading health first and the count second is not enough (ai-review R5):
	// if access dies after the healthcheck and before the count, the sample
	// pairs the old process's health with the new process's count, and the next
	// sample then matches it — two "consecutive" samples spanning the restart
	// they exist to detect. Bracketing makes the sample atomic with respect to
	// restarts: an unequal pair means one happened mid-sample, and the streak
	// resets. The bracket compares the start TIME as well as the count
	// (ai-review R7) — RestartCount alone is not a process identity, since it
	// resets when the container is recreated, and two samples either side of
	// such a reset would compare equal. StartedAt is already in hand, so this
	// costs one comparison.
	//
	// The phase deadline is a real context, not just poll's bookkeeping
	// (ai-review R6): poll only checks `within` between attempts, so with the
	// parent context passed through, a single attempt could sit in a 15s
	// healthcheck plus a 15s inspect and blow a 10s phase by 3x. Scoping the
	// context to `within` bounds the subprocesses too, which is what keeps the
	// cleanup phases below adding up to less than the cleanup budget.
	settled := func(parent context.Context, within time.Duration) (*jetstream.ConsumerInfo, error) {
		ctx, cancel := context.WithTimeout(parent, within)
		defer cancel()
		var latest *jetstream.ConsumerInfo
		var atStart time.Time
		seen, at := 0, -1
		err := poll(within, 500*time.Millisecond, func() error {
			seen0 := seen
			seen = 0
			opened, openedAt, err := restartState(ctx, container)
			if err != nil {
				return err
			}
			info, err := healthy(ctx)
			if err != nil {
				return err
			}
			closed, closedAt, err := restartState(ctx, container)
			if err != nil {
				return err
			}
			if opened != closed || !openedAt.Equal(closedAt) {
				return fmt.Errorf("access restarted during the health sample (%d@%s → %d@%s)",
					opened, openedAt, closed, closedAt)
			}
			if seen0 > 0 && (closed != at || !closedAt.Equal(atStart)) {
				return fmt.Errorf("access restarted between samples (%d@%s → %d@%s)",
					at, atStart, closed, closedAt)
			}
			latest, seen, at, atStart = info, seen0+1, closed, closedAt
			if seen < 2 {
				return fmt.Errorf("healthy once; want two consecutive samples")
			}
			return nil
		})
		return latest, err
	}

	if _, err := healthy(ctx); err != nil {
		t.Fatalf("precondition: access is not healthy before the deletion: %v", err)
	}
	before, err := stream.Consumer(ctx, policyDurable)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := before.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	baseRestarts, baseStart, err := restartState(ctx, container)
	if err != nil {
		t.Fatal(err)
	}
	// Count occurrences rather than test for presence: a later run of this test,
	// or any earlier termination, would leave the message in the log and make a
	// presence check pass without access having done anything.
	baseDiagnostics, err := accessLogCount(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Registered BEFORE the deletion, so an assertion failure — or an
	// intentional RED mutation that stops access self-healing — still leaves a
	// working stack for the rest of the package.
	t.Cleanup(func() {
		// Its own context, not the test's: the test context may already be
		// expired by the time cleanup runs, and cleanup is the last thing
		// standing between a failure here and 60-odd failing tests after it.
		//
		// The budget is spent as 10s (is it already fine?) + 15s (restart) +
		// 45s (recover) + 15s (logs) = 85s worst case, and every one of those is a
		// real deadline now that settled scopes its own context. The 35s of
		// headroom is what stops the diagnostic logs — the only thing that
		// explains a failed cleanup — from being the work that gets cancelled.
		cctx, ccancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer ccancel()
		if _, err := settled(cctx, 10*time.Second); err == nil {
			return
		}
		if out, err := dockerRun(cctx, "restart", container); err != nil {
			t.Errorf("forced restart of %s failed: %v: %s", container, err, out)
		}
		if _, err := settled(cctx, 45*time.Second); err != nil {
			logs, _ := dockerRun(cctx, "compose", "-p", project, "logs", "--no-color", "--tail", "50", "access")
			t.Errorf("access did not recover after a forced restart: %v\n%s", err, logs)
		}
	})

	if err := stream.DeleteConsumer(ctx, policyDurable); err != nil {
		t.Fatalf("delete durable %s: %v", policyDurable, err)
	}

	// 15s is generous, not tight: the server answers the consumer's outstanding
	// pull request with 409 Consumer Deleted the moment the durable goes, and
	// nats.go treats ErrConsumerDeleted as terminal (jetstream/pull.go:722),
	// closing Closed() immediately. It is NOT bounded by the idle-heartbeat
	// timeout. Anyone tempted to raise this because it flaked should find out
	// why that path stopped being prompt instead.
	if err := poll(15*time.Second, 250*time.Millisecond, func() error {
		diagnostics, err := accessLogCount(ctx)
		if err != nil {
			return err
		}
		if diagnostics <= baseDiagnostics {
			return fmt.Errorf("termination diagnostic count still %d (want > %d)", diagnostics, baseDiagnostics)
		}
		restarts, start, err := restartState(ctx, container)
		if err != nil {
			return err
		}
		if restarts <= baseRestarts {
			return fmt.Errorf("restart count still %d (want > %d)", restarts, baseRestarts)
		}
		if !start.After(baseStart) {
			return fmt.Errorf("container start time still %s (want after)", start)
		}
		return nil
	}); err != nil {
		logs, _ := dockerRun(ctx, "compose", "-p", project, "logs", "--no-color", "--tail", "50", "access")
		t.Fatalf("access did not terminate on durable deletion: %v\n%s", err, logs)
	}

	// Recovery is a hard postcondition, not politeness: every later test in this
	// package needs access issuing tickets, and a half-recovered stack would
	// fail them with the blame pointing anywhere but here.
	info, err := settled(ctx, 20*time.Second)
	if err != nil {
		logs, _ := dockerRun(ctx, "compose", "-p", project, "logs", "--no-color", "--tail", "50", "access")
		t.Fatalf("access did not recover after the durable was recreated: %v\n%s", err, logs)
	}
	if !info.Created.After(beforeInfo.Created) {
		t.Fatalf("durable created at %s is not newer than %s — deletion never took",
			info.Created, beforeInfo.Created)
	}
}

// dockerRun shells out to docker and returns combined output. Two departures
// from the package's inspect helper, both deliberate:
//
// It returns an error instead of calling Fatal, because every caller here is
// either polling or running inside t.Cleanup, where Fatal is not allowed.
//
// It takes a context AND imposes its own per-command ceiling. Without one, every
// deadline in this test is fiction: a wedged Docker daemon blocks inside
// CombinedOutput, the enclosing poll only checks its deadline after the
// subprocess returns, and a single hung call can eat the whole budget — or hang
// the cleanup that is supposed to hand a working stack to the next 60 tests
// (ai-review R2). 15s is well above any healthy docker inspect/exec/logs call
// and well below the phase budgets, so it only ever fires on a sick daemon.
func dockerRun(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// accessLogCount counts occurrences of the termination diagnostic in the access
// container's log. Container restarts do not truncate it, which is what makes a
// before/after count meaningful across the restart this test causes.
func accessLogCount(ctx context.Context) (int, error) {
	out, err := dockerRun(ctx, "compose", "-p", project, "logs", "--no-color", "access")
	if err != nil {
		return 0, fmt.Errorf("compose logs access: %v: %s", err, out)
	}
	return strings.Count(out, policyTermination), nil
}

func restartState(ctx context.Context, container string) (int, time.Time, error) {
	out, err := dockerRun(ctx, "inspect", "-f", "{{.RestartCount}} {{.State.StartedAt}}", container)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("docker inspect %s: %v: %s", container, err, out)
	}
	count, at, found := strings.Cut(out, " ")
	if !found {
		return 0, time.Time{}, fmt.Errorf("docker inspect %s: unparseable %q", container, out)
	}
	restarts, err := strconv.Atoi(count)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("parse restart count %q: %w", count, err)
	}
	started, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("parse container start time %q: %w", at, err)
	}
	return restarts, started, nil
}

// poll retries fn until it succeeds or the deadline passes, returning fn's last
// error. The package's retry helper fixes a 2s interval and calls Fatal; both
// are wrong here — the reaction window is short enough that 2s of granularity
// would eat most of the budget, and the recovery caller needs the error back.
func poll(within, every time.Duration, fn func() error) error {
	deadline := time.Now().Add(within)
	for {
		err := fn()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(every)
	}
}

func TestAccessPoisonEventPolicy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connection, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	js, err := jetstream.New(connection)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "PLATFORM")
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := stream.Consumer(ctx, "access-ticket-issuer")
	if err != nil {
		t.Fatal(err)
	}
	info, err := issuer.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.MaxDeliver != -1 || len(info.Config.BackOff) != 6 || info.Config.BackOff[0] != 100*time.Millisecond {
		t.Fatalf("access consumer does not preserve terminal-record publication retries: %+v", info.Config)
	}

	consumerName := "access-failures-smoke-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	failures, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{
		Durable: consumerName, FilterSubject: accessFailureSubject, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stream.DeleteConsumer(context.Background(), consumerName) })

	// Genuine poison at a schema access knows: quantity 0 fails the schema-1
	// contract. An unknown schema is not poison — since TKT-74 it parks and
	// publishes no failure record (ADR-017 §5b′), so it cannot be the vehicle
	// to the reject path here.
	invalidID := uuid.New()
	invalidBody, _ := json.Marshal(map[string]any{
		"id": invalidID, "type": orderCompletedSubject, "schema": 1, "attacker_marker": "must-not-be-copied",
		"data": map[string]any{"quantity": 0},
	})
	if _, err := js.Publish(ctx, orderCompletedSubject, invalidBody, jetstream.WithMsgID(invalidID.String())); err != nil {
		t.Fatal(err)
	}
	invalidFailure := nextAccessFailure(t, failures, 5*time.Second)
	if invalidFailure.Data.SourceEventID != invalidID.String() || invalidFailure.Data.Reason != "invalid_contract" || invalidFailure.Data.Stage != "contract" || invalidFailure.Data.Attempts != 1 {
		t.Fatalf("permanent failure = %+v", invalidFailure)
	}
	encoded, _ := json.Marshal(invalidFailure)
	if strings.Contains(string(encoded), "must-not-be-copied") || invalidFailure.Data.MessageFingerprint == "" {
		t.Fatalf("failed-event record is not sanitized: %s", encoded)
	}

	transientID := uuid.New()
	transientBody, _ := json.Marshal(map[string]any{
		"id": transientID, "type": orderCompletedSubject, "schema": 1,
		"data": map[string]any{
			"order_id": uuid.New(), "guest_order_ref": uuid.New(), "organizer_id": uuid.New(), "buyer_id": uuid.New(),
			"slot_id": uuid.New(), "ticket_type_id": uuid.New(), "quantity": 1,
		},
	})
	if _, err := js.Publish(ctx, orderCompletedSubject, transientBody, jetstream.WithMsgID(transientID.String())); err != nil {
		t.Fatal(err)
	}
	exhausted := nextAccessFailure(t, failures, 10*time.Second)
	if exhausted.Data.SourceEventID != transientID.String() || exhausted.Data.Reason != "delivery_retries_exhausted" || exhausted.Data.Stage != "delivery" || exhausted.Data.Attempts != 4 {
		t.Fatalf("exhausted failure = %+v", exhausted)
	}

	retry(t, 15*time.Second, func() error {
		query := url.QueryEscape(`access_event_failures_total{service_name="access"}`)
		code, body := get(t, promURL+"/api/v1/query?query="+query, nil)
		if code != http.StatusOK || !strings.Contains(string(body), `"result":[{`) {
			return fmt.Errorf("access failure metric not exported yet: %d %s", code, body)
		}
		return nil
	})
}

func nextAccessFailure(t *testing.T, consumer jetstream.Consumer, wait time.Duration) accessFailureEvent {
	t.Helper()
	message, err := consumer.Next(jetstream.FetchMaxWait(wait))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = message.Ack() }()
	var failure accessFailureEvent
	if err := json.Unmarshal(message.Data(), &failure); err != nil {
		t.Fatalf("decode failed-event record: %v: %s", err, message.Data())
	}
	return failure
}
