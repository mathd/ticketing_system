//go:build smoke

package smoke_test

// Abandoning a settled exchange whose source ticket was admitted, through the running stack
// (TKT-498, ADR-039 §2 as amended by TKT-498).
//
// THE RACE IS REAL. Each case holds the target pool's row lock, starts a real exchange, and
// waits until inventory's target-hold statement is blocked behind that lock. Commerce takes the
// target hold after its pre-money admission read, so at that point the read has passed. The
// test then commits a real scan of a source ticket, releases the lock, and waits for Access's
// permanent refusal of the switch. The exchange settled; the buyer used the source ticket; the
// replacement can never issue.
//
// The operator command then runs the way an operator runs it: the packaged commerce binary
// inside its own container (modelled on recordRefundOutcome). Every effect is asserted in the
// service that owns it: Commerce, Access, inventory and payments. The fake PSP sits behind the
// real payments service only.
//
// Amounts are derived from the seeded prices and fees, never read from a response. The source
// and target gross amounts differ, and the upgrade carries a passed-on fee that the exchange
// retains, so a swap of the two amounts or a use of face prices fails here.
//
// Coverage (ADR-030): the command calls Access, payments and inventory from inside the stack,
// where the chokepoints cannot see it. Every operation it uses already has direct smoke calls;
// the refund-leg and operation reads below add more.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// orderExchangedSubject mirrors events.SubjectOrderExchanged. The smoke module does not import
// the services.
const orderExchangedSubject = "platform.commerce.order.exchanged"

const (
	abandonOperator = "Ada Operator"
	abandonReason   = "source admitted before the switch"
)

// abandonSpec is one exchange: the seeded source price and passed-on fee per ticket, and the
// target price.
type abandonSpec struct {
	tag                    string
	quantity               int
	sourcePrice, fee       int64
	targetPrice            int64
	needsPaymentInstrument bool
}

func (s abandonSpec) sourceFace() int64  { return s.sourcePrice * int64(s.quantity) }
func (s abandonSpec) sourceGross() int64 { return (s.sourcePrice + s.fee) * int64(s.quantity) }
func (s abandonSpec) targetFace() int64  { return s.targetPrice * int64(s.quantity) }

// targetGross is the target face plus the fee the exchange retained from the source.
func (s abandonSpec) targetGross() int64 { return s.targetFace() + s.fee*int64(s.quantity) }
func (s abandonSpec) delta() int64       { return s.targetFace() - s.sourceFace() }

// racedExchange is one settled exchange whose switch Access refused.
type racedExchange struct {
	spec                          abandonSpec
	org, exchange                 uuid.UUID
	sourceOrder, replacementOrder uuid.UUID
	sourceCheckoutKey             string
	sourceSlot, targetSlot        string
	sourceHold, targetHold        uuid.UUID
	exchangeKey, targetType       string
	sourceTickets                 int
}

// abandonmentID and abandonedFactID repeat Commerce's derivations
// (store.ExchangeAbandonmentRefundID, store.ExchangeAbandonedFactID).
func (x racedExchange) abandonmentID() uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("exchange-abandonment:"+x.org.String()+":"+x.exchange.String()))
}

func (x racedExchange) abandonedFactID() uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("order.exchange.abandoned:"+x.org.String()+":"+x.exchange.String()))
}

// eventID repeats events.ExchangedEventID.
func (x racedExchange) eventID() uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(orderExchangedSubject+":"+x.exchange.String()))
}

// accessFailures opens a JetStream consumer of Access's failed-event records, from now on.
func accessFailures(t *testing.T, ctx context.Context) jetstream.Consumer {
	t.Helper()
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "PLATFORM")
	if err != nil {
		t.Fatal(err)
	}
	name := "tkt498-failures-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	c, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{
		Durable: name, FilterSubject: accessFailureSubject,
		DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stream.DeleteConsumer(context.Background(), name) })
	return c
}

// awaitIssuerPast waits until Access's `access-ticket-issuer` durable has an ack floor at or past
// stream sequence seq: every message up to it was acknowledged or terminated.
func awaitIssuerPast(t *testing.T, ctx context.Context, js jetstream.JetStream, seq uint64, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last uint64
	for time.Now().Before(deadline) {
		c, err := js.Consumer(ctx, "PLATFORM", "access-ticket-issuer")
		if err != nil {
			t.Fatal(err)
		}
		info, err := c.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		last = info.AckFloor.Stream
		if last >= seq {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("access-ticket-issuer ack floor = %d after %s, want >= %d (the replayed event)", last, within, seq)
}

// awaitExchangeRefusal waits for Access's `exchange_refused` record of one event. Records of
// other events are acknowledged and skipped.
func awaitExchangeRefusal(t *testing.T, c jetstream.Consumer, eventID uuid.UUID, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		msg, err := c.Next(jetstream.FetchMaxWait(time.Until(deadline)))
		if err != nil {
			break
		}
		_ = msg.Ack()
		var f accessFailureEvent
		if err := json.Unmarshal(msg.Data(), &f); err != nil {
			t.Fatalf("decode failed-event record: %v: %s", err, msg.Data())
		}
		if f.Data.SourceEventID != eventID.String() {
			continue
		}
		if f.Data.Reason != "exchange_refused" {
			t.Fatalf("access failed the exchange event %s with %+v, want exchange_refused", eventID, f.Data)
		}
		return
	}
	t.Fatalf("access never refused the exchange event %s within %s", eventID, within)
}

// raceSettledExchange buys the source, holds the target pool lock, starts the exchange, scans a
// source ticket while the exchange waits on that lock, and returns once Access has refused the
// switch.
func raceSettledExchange(t *testing.T, ctx context.Context, spec abandonSpec) racedExchange {
	t.Helper()
	suffix := admissionSuffix()
	x := racedExchange{spec: spec, org: uuid.MustParse(organizerID)}
	var sourceType string
	x.sourceSlot, sourceType = publishedSlotAt(t, "TKT-498 Source "+spec.tag+" "+suffix, 10, spec.sourcePrice)
	x.targetSlot, x.targetType = publishedSlotAt(t, "TKT-498 Target "+spec.tag+" "+suffix, 10, spec.targetPrice)

	if spec.fee > 0 {
		// A public fixed fee the buyer pays, on the source event only.
		cat, err := pgx.Connect(ctx, dsn("catalog", "catalog"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cat.Close(ctx) }()
		var eventID string
		if err := cat.QueryRow(ctx, `SELECT p.event_id FROM ticket_types t
			JOIN performances p ON p.id = t.performance_id WHERE t.id = $1`, sourceType).Scan(&eventID); err != nil {
			t.Fatal(err)
		}
		if _, err := cat.Exec(ctx, `INSERT INTO fee_rules
			(organizer_id, scope_level, scope_id, fee_code, basis, amount, currency, incidence, channel_code)
			VALUES($1,'event',$2,'service','per_ticket_fixed',$3,'EUR','passed_on',NULL)`,
			organizerID, eventID, spec.fee); err != nil {
			t.Fatal(err)
		}
	}

	// The source purchase, issued.
	code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", "tkt498-reserve-"+suffix, map[string]any{
		"organizer_id": organizerID, "ticket_type_id": sourceType, "quantity": spec.quantity,
	})
	if code != http.StatusCreated {
		t.Fatalf("source reservation %d %s", code, body)
	}
	var reservation map[string]any
	if err := json.Unmarshal(body, &reservation); err != nil {
		t.Fatal(err)
	}
	code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", "tkt498-order-"+suffix, map[string]any{
		"reservation_id": reservation["reservation_id"], "name": "TKT-498 Buyer",
		"email": "tkt498-" + suffix + "@example.test", "payment_token": "fake-ok",
	})
	if code != http.StatusOK {
		t.Fatalf("source checkout %d %s", code, body)
	}
	var source struct {
		OrderID       string `json:"order_id"`
		GuestOrderRef string `json:"guest_order_ref"`
	}
	if err := json.Unmarshal(body, &source); err != nil {
		t.Fatal(err)
	}
	x.sourceOrder = uuid.MustParse(source.OrderID)
	waitForExchangeTickets(t, source.OrderID, spec.quantity)
	code, body, _ = getWithHeaders(t, gatewayURL+"/api/access/orders/"+source.GuestOrderRef+"/tickets")
	if code != http.StatusOK {
		t.Fatalf("source tickets %d %s", code, body)
	}
	var bundle struct {
		Tickets []issuedTicket `json:"tickets"`
	}
	if err := json.Unmarshal(body, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Tickets) != spec.quantity {
		t.Fatalf("source tickets = %d, want %d", len(bundle.Tickets), spec.quantity)
	}
	x.sourceTickets = len(bundle.Tickets)

	x.exchangeKey = "tkt498-exchange-" + suffix
	x.exchange = uuid.NewSHA1(uuid.NameSpaceOID, []byte("exchange:"+organizerID+":"+x.exchangeKey))
	failures := accessFailures(t, ctx)

	// Hold the TARGET pool's row lock. The source pool is another slot and stays free.
	blocker, err := pgx.Connect(ctx, dsn("postgres", "inventory"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Close(context.Background()) }()
	monitor := inventoryAdminConn(t)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var blockerPID int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1::uuid FOR UPDATE`, x.targetSlot); err != nil {
		t.Fatal(err)
	}

	type answer struct {
		code int
		body []byte
	}
	done := make(chan answer, 1)
	// Roll back before the join, so a request queued on the lock is released on every path.
	defer func() {
		_ = tx.Rollback(context.Background())
		<-done
	}()
	request := map[string]any{"organizer_id": organizerID, "target_ticket_type_id": x.targetType,
		"actor": "tkt498@example.test", "reason": "race with a scan"}
	if spec.needsPaymentInstrument {
		request["payment_token"] = "fake-ok"
	}
	go func() {
		code, body := internalJSONAsync(t, http.MethodPost,
			fmt.Sprintf("%s/internal/orders/%s/exchanges", commerceURL, x.sourceOrder), x.exchangeKey, request)
		done <- answer{code, body}
	}()

	// The exchange's own target-hold statement, blocked by THIS transaction. Commerce takes the
	// target hold after its pre-money admission read, so that read has passed.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var n int
		if err := monitor.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE pid <> pg_backend_pid() AND $1::int = ANY(pg_blocking_pids(pid)) AND query LIKE $2`,
			blockerPID, "%closure_status FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= 1 {
			break
		}
		select {
		case a := <-done:
			done <- a
			t.Fatalf("the exchange answered %d %s before it reached the target pool lock", a.code, a.body)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no target-hold statement is blocked behind the target pool lock (%d waiters)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The scan commits while the exchange waits.
	if code, body := postWithKey(t, gatewayURL+"/api/access/scans", "tkt498-scan-"+suffix,
		scanBody(bundle.Tickets[0].QRPayload, uuid.NewString(), time.Now().UTC(), "")); code != http.StatusOK {
		t.Fatalf("source scan %d %s", code, body)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	a := <-done
	done <- a
	if a.code != http.StatusOK {
		t.Fatalf("raced exchange = %d %s, want 200: it passed admission before the scan", a.code, a.body)
	}
	var settled struct {
		ExchangeID  string `json:"exchange_id"`
		DeltaAmount int64  `json:"delta_amount"`
		Status      string `json:"status"`
	}
	if err := json.Unmarshal(a.body, &settled); err != nil {
		t.Fatal(err)
	}
	if settled.ExchangeID != x.exchange.String() || settled.DeltaAmount != spec.delta() || settled.Status != "switch_pending" {
		t.Fatalf("raced exchange = %+v, want exchange %s settled with delta %d and switch_pending", settled, x.exchange, spec.delta())
	}

	awaitExchangeRefusal(t, failures, x.eventID(), 45*time.Second)

	// The row the operator will act on, and the amounts the seeds say it must carry.
	commerce := connectOutcomeDB(t, "commerce")
	var sourceFace, sourceGross, targetFace, delta int64
	var settledAt, switchedAt *time.Time
	if err := commerce.QueryRow(ctx, `SELECT x.replacement_order_id, x.target_hold_id, r.hold_id, o.idempotency_key,
			x.source_total, x.source_gross_total, x.target_total, x.delta_amount, x.settled_at, x.tickets_exchanged_at
		FROM order_exchanges x JOIN orders o ON o.id = x.source_order_id JOIN reservations r ON r.id = o.reservation_id
		WHERE x.organizer_id = $1 AND x.id = $2`, x.org, x.exchange).
		Scan(&x.replacementOrder, &x.targetHold, &x.sourceHold, &x.sourceCheckoutKey, &sourceFace, &sourceGross, &targetFace, &delta, &settledAt, &switchedAt); err != nil {
		t.Fatalf("read the raced exchange: %v", err)
	}
	if sourceFace != spec.sourceFace() || sourceGross != spec.sourceGross() || targetFace != spec.targetFace() || delta != spec.delta() {
		t.Fatalf("exchange source=%d gross=%d target=%d delta=%d, want %d %d %d %d from the seeds",
			sourceFace, sourceGross, targetFace, delta, spec.sourceFace(), spec.sourceGross(), spec.targetFace(), spec.delta())
	}
	if settledAt == nil || switchedAt != nil {
		t.Fatalf("exchange settled_at=%v tickets_exchanged_at=%v, want settled and unswitched", settledAt, switchedAt)
	}
	return x
}

// commerceCLI runs one commerce operator command inside the commerce container.
func commerceCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", append([]string{"exec", project + "-commerce-1", "/app"}, args...)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// serviceLogCount counts request-log lines of one service that contain every fragment. Logs
// are append-only across the run, so a before/after count needs no clock agreement.
func serviceLogCount(t *testing.T, ctx context.Context, service string, fragments ...string) int {
	t.Helper()
	out, err := dockerRun(ctx, "compose", "--ansi", "never", "-p", project, "logs", "--no-color", service)
	if err != nil {
		t.Fatalf("compose logs %s: %v", service, err)
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		all := true
		for _, f := range fragments {
			if !strings.Contains(line, f) {
				all = false
				break
			}
		}
		if all {
			n++
		}
	}
	return n
}

// remoteCalls is the Access and inventory calls the abandonment can make, counted from the
// services' request logs on paths that name this exchange's orders and hold. Payments paths
// carry no exchange identity: commandPaymentsCalls counts them by the command's trace instead.
type remoteCalls struct {
	sourceAdmission, replacementAdmission, targetReturn, sourceReturn int
}

func countRemoteCalls(t *testing.T, ctx context.Context, x racedExchange) remoteCalls {
	t.Helper()
	path := func(p string) string { return `"path":"` + p + `"` }
	return remoteCalls{
		sourceAdmission:      serviceLogCount(t, ctx, "access", `"method":"GET"`, path("/internal/orders/"+x.sourceOrder.String()+"/admission")),
		replacementAdmission: serviceLogCount(t, ctx, "access", `"method":"GET"`, path("/internal/orders/"+x.replacementOrder.String()+"/admission")),
		targetReturn:         serviceLogCount(t, ctx, "inventory", `"method":"POST"`, path("/internal/holds/"+x.targetHold.String()+"/refund-capacity")),
		sourceReturn:         serviceLogCount(t, ctx, "inventory", `"method":"POST"`, path("/internal/holds/"+x.sourceHold.String()+"/refund-capacity")),
	}
}

// paymentsCalls is one command run's payments requests, by route.
type paymentsCalls struct {
	facts, refunds, operations, legs int
}

// commandTrace reads the trace id the abandon-exchange command prints on stderr (RA2-5).
func commandTrace(t *testing.T, stderr string) string {
	t.Helper()
	m := regexp.MustCompile(`abandon-exchange trace_id=([0-9a-f]{32})`).FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("abandon-exchange printed no trace id\nstderr=%s", stderr)
	}
	return m[1]
}

// commandPaymentsCalls counts the payments request-log lines that carry one command run's
// trace_id (RA2-5). The command sends its trace on every call and payments stamps it, so
// background Commerce traffic, which has other traces, cannot change these counts.
func commandPaymentsCalls(t *testing.T, ctx context.Context, traceID string) paymentsCalls {
	t.Helper()
	trace := `"trace_id":"` + traceID + `"`
	path := func(p string) string { return `"path":"` + p + `"` }
	return paymentsCalls{
		facts:      serviceLogCount(t, ctx, "payments", trace, `"method":"POST"`, path("/internal/facts")),
		refunds:    serviceLogCount(t, ctx, "payments", trace, `"method":"POST"`, path("/internal/psp/partial-refund")),
		operations: serviceLogCount(t, ctx, "payments", trace, `"method":"GET"`, path("/internal/operations")),
		legs:       serviceLogCount(t, ctx, "payments", trace, `"method":"GET"`, path("/internal/refund-legs")),
	}
}

// tracedRequests is one command run's requests to each dependency, on every route.
type tracedRequests struct {
	access, inventory, payments int
}

// commandRequests counts every request-log line that carries one command run's trace_id in
// Access, inventory and payments (R3-6). Each service's request logger stamps the trace of every
// request it serves (shared/go/obs), so a call on any route counts, listed or not.
func commandRequests(t *testing.T, ctx context.Context, traceID string) tracedRequests {
	t.Helper()
	trace, request := `"trace_id":"`+traceID+`"`, `"msg":"request"`
	return tracedRequests{
		access:    serviceLogCount(t, ctx, "access", request, trace),
		inventory: serviceLogCount(t, ctx, "inventory", request, trace),
		payments:  serviceLogCount(t, ctx, "payments", request, trace),
	}
}

// durableState is every durable effect of an abandonment, for a before/after comparison.
type durableState struct {
	abandonment                                          string
	commerceFacts, journalEntries, legs, receipts        int
	targetReturned, sourceReturned                       int32
	targetConfirmed, sourceConfirmed                     int32
	replacementTickets, replacementLifecycle, deliveries int
	sourceVoids                                          int
}

func readDurableState(t *testing.T, ctx context.Context, x racedExchange) durableState {
	t.Helper()
	var s durableState
	commerce := connectOutcomeDB(t, "commerce")
	if err := commerce.QueryRow(ctx, `SELECT concat_ws('|', abandonment_started_at, abandonment_operator_name,
			abandonment_session_user, abandonment_reason, abandonment_refund_fact_id, abandonment_refund_failed_ref,
			abandonment_target_returned_at, abandonment_written_off_amount, abandoned_at, tickets_exchanged_at, capacity_returned_at)
		FROM order_exchanges WHERE organizer_id = $1 AND id = $2`, x.org, x.exchange).Scan(&s.abandonment); err != nil {
		t.Fatal(err)
	}
	if err := commerce.QueryRow(ctx, `SELECT count(*) FROM order_facts WHERE organizer_id = $1 AND order_id = $2`,
		x.org, x.sourceOrder).Scan(&s.commerceFacts); err != nil {
		t.Fatal(err)
	}
	payments := connectOutcomeDB(t, "payments")
	if err := payments.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE organizer_id = $1 AND payload->>'order_id' = $2`,
		x.org, x.sourceOrder.String()).Scan(&s.journalEntries); err != nil {
		t.Fatal(err)
	}
	if err := payments.QueryRow(ctx, `SELECT count(*) FROM payment_refund_legs WHERE organizer_id = $1
		AND (source_idempotency_key IN ($2, $3) OR refund_idempotency_key = $4)`,
		x.org, x.sourceCheckoutKey, "exchange-charge:"+x.exchange.String(), x.abandonmentID().String()).Scan(&s.legs); err != nil {
		t.Fatal(err)
	}
	inventory := inventoryAdminConn(t)
	if err := inventory.QueryRow(ctx, `SELECT count(*) FROM claim_history WHERE organizer_id = $1 AND idempotency_key IN ($2, $3)`,
		x.org, "refund:"+x.abandonmentID().String(), "refund:"+x.exchange.String()).Scan(&s.receipts); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		hold     uuid.UUID
		returned *int32
	}{{x.targetHold, &s.targetReturned}, {x.sourceHold, &s.sourceReturned}} {
		if err := inventory.QueryRow(ctx, `SELECT returned_quantity FROM claims WHERE organizer_id = $1 AND id = $2`,
			x.org, c.hold).Scan(c.returned); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []struct {
		slot      string
		confirmed *int32
	}{{x.targetSlot, &s.targetConfirmed}, {x.sourceSlot, &s.sourceConfirmed}} {
		if err := inventory.QueryRow(ctx, `SELECT confirmed_quantity FROM inventory_pools WHERE slot_id = $1::uuid`,
			p.slot).Scan(p.confirmed); err != nil {
			t.Fatal(err)
		}
	}
	access := connectOutcomeDB(t, "access")
	if err := access.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM tickets WHERE organizer_id = $1 AND order_id = $2),
			(SELECT count(*) FROM lifecycle_events l JOIN tickets t ON t.id = l.ticket_id WHERE t.order_id = $2),
			(SELECT count(*) FROM delivery_attempts d JOIN tickets t ON t.id = d.ticket_id WHERE t.order_id = $2),
			(SELECT count(*) FROM lifecycle_events l JOIN tickets t ON t.id = l.ticket_id
			  WHERE t.order_id = $3 AND l.event_type IN ('exchanged','refunded'))`,
		x.org, x.replacementOrder, x.sourceOrder).Scan(&s.replacementTickets, &s.replacementLifecycle, &s.deliveries, &s.sourceVoids); err != nil {
		t.Fatal(err)
	}
	return s
}

// assertNoReplacementIssued is COS3: the replacement has no ticket, lifecycle event or delivery
// attempt, and the admitted source keeps its tickets, unvoided, and its capacity.
func assertNoReplacementIssued(t *testing.T, s durableState, x racedExchange, when string) {
	t.Helper()
	if s.replacementTickets != 0 || s.replacementLifecycle != 0 || s.deliveries != 0 || s.sourceVoids != 0 {
		t.Fatalf("%s: replacement tickets=%d lifecycle=%d deliveries=%d, source voids=%d; want all zero",
			when, s.replacementTickets, s.replacementLifecycle, s.deliveries, s.sourceVoids)
	}
	if s.sourceReturned != 0 {
		t.Fatalf("%s: the source claim returned %d unit(s), want 0: the admitted source keeps its capacity", when, s.sourceReturned)
	}
}

// listedCandidate reports whether the packaged listing shows the exchange.
func listedCandidate(t *testing.T, x racedExchange) bool {
	t.Helper()
	stdout, stderr, err := commerceCLI(t, "list-settled-unswitched-exchanges", organizerID)
	if err != nil {
		t.Fatalf("list-settled-unswitched-exchanges: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	return strings.Contains(stdout, "exchange="+x.exchange.String()+" ")
}

// abandonAndAssert runs the packaged command on a raced exchange and asserts the effects every
// branch shares, then the branch's own money evidence, then COS4's replay.
func abandonAndAssert(t *testing.T, ctx context.Context, x racedExchange) {
	t.Helper()
	spec := x.spec
	if !listedCandidate(t, x) {
		t.Fatalf("the listing does not show the raced exchange %s as a candidate", x.exchange)
	}
	before := readDurableState(t, ctx, x)
	assertNoReplacementIssued(t, before, x, "before the abandonment")
	if before.targetReturned != 0 || before.receipts != 0 {
		t.Fatalf("before the abandonment the target returned %d with %d receipt(s), want none", before.targetReturned, before.receipts)
	}
	callsBefore := countRemoteCalls(t, ctx, x)

	stdout, stderr, err := commerceCLI(t, "abandon-exchange", organizerID, x.exchange.String(), abandonOperator, abandonReason)
	if err != nil {
		t.Fatalf("abandon-exchange: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	for _, want := range []string{"exchange=" + x.exchange.String() + " ", fmt.Sprintf("operator=%q", abandonOperator),
		`session_user="commerce"`, fmt.Sprintf("reason=%q", abandonReason), "replay=false"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("abandon-exchange output lacks %s\nstdout=%s\nstderr=%s", want, stdout, stderr)
		}
	}
	callsAfter := countRemoteCalls(t, ctx, x)
	firstTrace := commandTrace(t, stderr)
	// Positive controls for COS4's zero-call count: the logs see this command's calls. The
	// payments counts are this run's own (its trace), so they are exact; the log lines are
	// collected asynchronously, hence the retry.
	var first paymentsCalls
	retry(t, 15*time.Second, func() error {
		first = commandPaymentsCalls(t, ctx, firstTrace)
		want := paymentsCalls{facts: 1}
		switch {
		case spec.delta() > 0:
			// The capture read, the abandonment leg read and the delta refund.
			want.operations, want.legs, want.refunds = 1, 1, 1
		case spec.delta() < 0:
			// The settlement's downgrade leg read.
			want.legs = 1
		}
		if first != want {
			return fmt.Errorf("the first run's payments calls (trace %s) = %+v, want %+v", firstTrace, first, want)
		}
		// The all-route count sees the same run: two Access reads, one capacity return, and
		// exactly the payments calls counted by route above. This is the positive control for
		// the replay's zero below.
		all := commandRequests(t, ctx, firstTrace)
		if wantAll := (tracedRequests{access: 2, inventory: 1, payments: want.facts + want.refunds + want.operations + want.legs}); all != wantAll {
			return fmt.Errorf("the first run's requests on every route (trace %s) = %+v, want %+v", firstTrace, all, wantAll)
		}
		return nil
	})
	if callsAfter.sourceAdmission-callsBefore.sourceAdmission != 1 || callsAfter.replacementAdmission-callsBefore.replacementAdmission != 1 {
		t.Fatalf("the first run's Access calls are not visible in the request logs: before %+v after %+v", callsBefore, callsAfter)
	}
	if n := callsAfter.targetReturn - callsBefore.targetReturn; n != 1 {
		t.Fatalf("inventory received %d capacity return(s) for the target hold, want 1", n)
	}
	if callsAfter.sourceReturn != callsBefore.sourceReturn {
		t.Fatalf("the abandonment returned the SOURCE claim's capacity (%d -> %d calls)", callsBefore.sourceReturn, callsAfter.sourceReturn)
	}
	after := readDurableState(t, ctx, x)

	// COS4: the identical command replays the terminal record from Commerce alone.
	again, againErr, err := commerceCLI(t, "abandon-exchange", organizerID, x.exchange.String(), abandonOperator, abandonReason)
	if err != nil {
		t.Fatalf("abandon-exchange replay: %v\nstdout=%s\nstderr=%s", err, again, againErr)
	}
	if want := strings.Replace(stdout, "replay=false", "replay=true", 1); again != want {
		t.Fatalf("the replay output differs from the original record\nfirst=%s\nreplay=%s", stdout, again)
	}
	callsReplay := countRemoteCalls(t, ctx, x)
	if callsReplay != callsAfter {
		t.Fatalf("the replay made remote calls: before %+v after %+v", callsAfter, callsReplay)
	}
	// The replay's requests, by its own trace, stay zero in every dependency and on every route
	// (R3-6). Watched for a few seconds, because a log line can arrive after the command returns.
	replayTrace := commandTrace(t, againErr)
	if replayTrace == firstTrace {
		t.Fatalf("the replay reused the first run's trace %s", firstTrace)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if replay := commandRequests(t, ctx, replayTrace); replay != (tracedRequests{}) {
			t.Fatalf("the replay (trace %s) made requests: %+v, want none", replayTrace, replay)
		}
	}
	if replayState := readDurableState(t, ctx, x); replayState != after {
		t.Fatalf("the replay changed durable state:\nbefore %+v\nafter  %+v", after, replayState)
	}

	// Commerce: the terminal record.
	commerce := connectOutcomeDB(t, "commerce")
	var operator, sessionUser, reason string
	var refundFact *uuid.UUID
	var failedRef *string
	var writtenOff *int64
	var startedAt, returnedAt, abandonedAt, switchedAt, capacityAt *time.Time
	if err := commerce.QueryRow(ctx, `SELECT abandonment_operator_name, abandonment_session_user, abandonment_reason,
			abandonment_refund_fact_id, abandonment_refund_failed_ref, abandonment_written_off_amount,
			abandonment_started_at, abandonment_target_returned_at, abandoned_at, tickets_exchanged_at, capacity_returned_at
		FROM order_exchanges WHERE organizer_id = $1 AND id = $2`, x.org, x.exchange).
		Scan(&operator, &sessionUser, &reason, &refundFact, &failedRef, &writtenOff,
			&startedAt, &returnedAt, &abandonedAt, &switchedAt, &capacityAt); err != nil {
		t.Fatalf("read the abandoned exchange: %v", err)
	}
	if operator != abandonOperator || reason != abandonReason || sessionUser != "commerce" || failedRef != nil ||
		startedAt == nil || returnedAt == nil || abandonedAt == nil || switchedAt != nil || capacityAt != nil {
		t.Fatalf("abandonment operator=%q reason=%q session_user=%q failed_ref=%v started=%v returned=%v abandoned=%v switched=%v capacity=%v",
			operator, reason, sessionUser, failedRef, startedAt, returnedAt, abandonedAt, switchedAt, capacityAt)
	}
	var factType, factCurrency string
	var factOrder uuid.UUID
	var factAmount int64
	if err := commerce.QueryRow(ctx, `SELECT fact_type, order_id, amount, currency FROM order_facts
		WHERE organizer_id = $1 AND fact_id = $2`, x.org, x.abandonedFactID()).Scan(&factType, &factOrder, &factAmount, &factCurrency); err != nil {
		t.Fatalf("read the commerce compensating fact: %v", err)
	}
	if factType != "order.exchange.abandoned" || factOrder != x.sourceOrder || factAmount != spec.targetGross() || factCurrency != "EUR" {
		t.Fatalf("commerce fact %s %s %d %s, want order.exchange.abandoned on %s for the target gross %d EUR",
			factType, factOrder, factAmount, factCurrency, x.sourceOrder, spec.targetGross())
	}

	// Payments: the same fact in the journal, with its payload.
	payments := connectOutcomeDB(t, "payments")
	var jType, jCurrency, jPayload string
	var jAmount int64
	if err := payments.QueryRow(ctx, `SELECT fact_type, amount, currency, payload::text FROM journal_entries
		WHERE organizer_id = $1 AND fact_id = $2`, x.org, x.abandonedFactID()).Scan(&jType, &jAmount, &jCurrency, &jPayload); err != nil {
		t.Fatalf("read the journal's compensating fact: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(jPayload), &payload); err != nil {
		t.Fatalf("journal payload %s: %v", jPayload, err)
	}
	wantPayload := map[string]string{
		"order_id": x.sourceOrder.String(), "exchange_id": x.exchange.String(),
		"source_gross": fmt.Sprint(spec.sourceGross()), "delta_amount": fmt.Sprint(spec.delta()),
	}
	switch {
	case spec.delta() > 0:
		if refundFact == nil {
			t.Fatal("an abandoned upgrade records no refund fact")
		}
		wantPayload["refund_fact_id"] = refundFact.String()
	case spec.delta() < 0:
		wantPayload["written_off_amount"] = fmt.Sprint(-spec.delta())
	}
	if jType != "order.exchange.abandoned" || jAmount != spec.targetGross() || jCurrency != "EUR" || !equalStringMaps(payload, wantPayload) {
		t.Fatalf("journal fact %s %d %s %v, want order.exchange.abandoned %d EUR %v", jType, jAmount, jCurrency, payload, spec.targetGross(), wantPayload)
	}
	var abandonedFacts int
	if err := payments.QueryRow(ctx, `SELECT count(*) FROM journal_entries
		WHERE organizer_id = $1 AND fact_type = 'order.exchange.abandoned' AND payload->>'exchange_id' = $2`,
		x.org, x.exchange.String()).Scan(&abandonedFacts); err != nil {
		t.Fatal(err)
	}
	if abandonedFacts != 1 {
		t.Fatalf("journal holds %d compensating facts for the exchange, want 1", abandonedFacts)
	}

	// The money, by branch.
	legsUnder := func(sourceKey string) int {
		var n int
		if err := payments.QueryRow(ctx, `SELECT count(*) FROM payment_refund_legs WHERE organizer_id = $1 AND source_idempotency_key = $2`,
			x.org, sourceKey).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	abandonmentLeg := fmt.Sprintf("%s/internal/refund-legs?%s", paymentsURL, url.Values{"organizer_id": {organizerID},
		"source_idempotency_key": {"exchange-charge:" + x.exchange.String()}, "refund_idempotency_key": {x.abandonmentID().String()}}.Encode())
	switch {
	case spec.delta() > 0:
		if writtenOff != nil {
			t.Fatalf("an upgrade wrote off %d", *writtenOff)
		}
		// One completed leg against the exchange's own charge, for the exact delta.
		var status, currency string
		var amount int64
		var legFact *uuid.UUID
		if err := payments.QueryRow(ctx, `SELECT status, amount, currency, fact_id FROM payment_refund_legs
			WHERE organizer_id = $1 AND source_idempotency_key = $2 AND refund_idempotency_key = $3`,
			x.org, "exchange-charge:"+x.exchange.String(), x.abandonmentID().String()).Scan(&status, &amount, &currency, &legFact); err != nil {
			t.Fatalf("read the abandonment's refund leg: %v", err)
		}
		if status != "refunded" || amount != spec.delta() || currency != "EUR" || legFact == nil || *legFact != *refundFact {
			t.Fatalf("abandonment leg %s %d %s fact %v, want refunded %d EUR with fact %s", status, amount, currency, legFact, spec.delta(), refundFact)
		}
		if n := legsUnder("exchange-charge:" + x.exchange.String()); n != 1 {
			t.Fatalf("legs against the exchange charge = %d, want the abandonment's only", n)
		}
		var refunded int
		var rAmount int64
		var rCurrency string
		if err := payments.QueryRow(ctx, `SELECT count(*), coalesce(max(amount),0), coalesce(max(currency),'') FROM journal_entries
			WHERE organizer_id = $1 AND fact_id = $2 AND fact_type = 'payment.refunded'`, x.org, *refundFact).Scan(&refunded, &rAmount, &rCurrency); err != nil {
			t.Fatal(err)
		}
		if refunded != 1 || rAmount != spec.delta() || rCurrency != "EUR" {
			t.Fatalf("payment.refunded facts for the abandonment = %d (%d %s), want one for %d EUR", refunded, rAmount, rCurrency, spec.delta())
		}
		if code, body := internalJSON(t, http.MethodGet, abandonmentLeg, "", nil); code != http.StatusOK {
			t.Fatalf("payments refund-leg read of the abandonment = %d %s, want 200", code, body)
		}
	case spec.delta() < 0:
		if writtenOff == nil || *writtenOff != -spec.delta() {
			t.Fatalf("a downgrade wrote off %v, want %d", writtenOff, -spec.delta())
		}
		// The settlement's downgrade leg stays the only provider refund.
		var refundKey, status string
		var amount int64
		if err := payments.QueryRow(ctx, `SELECT refund_idempotency_key, status, amount FROM payment_refund_legs
			WHERE organizer_id = $1 AND source_idempotency_key = $2`, x.org, x.sourceCheckoutKey).Scan(&refundKey, &status, &amount); err != nil {
			t.Fatalf("read the downgrade leg (exactly one expected): %v", err)
		}
		if refundKey != "exchange-refund:"+x.exchange.String() || status != "refunded" || amount != -spec.delta() {
			t.Fatalf("source leg %s %s %d, want the settlement's exchange-refund leg for %d", refundKey, status, amount, -spec.delta())
		}
		if code, body := internalJSON(t, http.MethodGet, abandonmentLeg, "", nil); code != http.StatusNotFound {
			t.Fatalf("payments refund-leg read of a downgrade abandonment = %d %s, want 404", code, body)
		}
	default:
		if writtenOff != nil || refundFact != nil {
			t.Fatalf("an even exchange recorded write-off %v and refund fact %v, want neither", writtenOff, refundFact)
		}
		if n := legsUnder(x.sourceCheckoutKey); n != 0 {
			t.Fatalf("an even exchange has %d refund leg(s) on the source charge, want 0", n)
		}
		if code, body := internalJSON(t, http.MethodGet, abandonmentLeg, "", nil); code != http.StatusNotFound {
			t.Fatalf("payments refund-leg read of an even abandonment = %d %s, want 404", code, body)
		}
	}
	if spec.delta() <= 0 {
		// Zero refund dispatches: no abandonment leg anywhere, no delta charge to refund.
		var legs int
		if err := payments.QueryRow(ctx, `SELECT count(*) FROM payment_refund_legs WHERE organizer_id = $1 AND refund_idempotency_key = $2`,
			x.org, x.abandonmentID().String()).Scan(&legs); err != nil {
			t.Fatal(err)
		}
		if legs != 0 || first.refunds != 0 {
			t.Fatalf("a non-upgrade abandonment dispatched refunds: %d leg(s), %d partial-refund call(s)", legs, first.refunds)
		}
		if code, body := internalJSON(t, http.MethodGet, fmt.Sprintf("%s/internal/operations?organizer_id=%s&idempotency_key=exchange-charge:%s",
			paymentsURL, organizerID, x.exchange), "", nil); code != http.StatusNotFound {
			t.Fatalf("exchange charge operation of a non-upgrade = %d %s, want none", code, body)
		}
	}
	// The source checkout is never refunded by the abandonment of an upgrade or an even exchange.
	if spec.delta() >= 0 {
		if n := legsUnder(x.sourceCheckoutKey); n != 0 {
			t.Fatalf("legs against the source checkout = %d, want 0", n)
		}
	}

	// Inventory: one target receipt for the whole claim, one pool decrement, no source return.
	if after.targetReturned != int32(spec.quantity) || after.targetConfirmed != before.targetConfirmed-int32(spec.quantity) ||
		after.sourceConfirmed != before.sourceConfirmed {
		t.Fatalf("target returned=%d confirmed %d->%d, source confirmed %d->%d; want the whole target (%d) back and the source unchanged",
			after.targetReturned, before.targetConfirmed, after.targetConfirmed, before.sourceConfirmed, after.sourceConfirmed, spec.quantity)
	}
	inventory := inventoryAdminConn(t)
	var targetReceipts, sourceReceipts int
	if err := inventory.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE idempotency_key = $2 AND claim_id = $3),
			count(*) FILTER (WHERE idempotency_key = $4)
		FROM claim_history WHERE organizer_id = $1`,
		x.org, "refund:"+x.abandonmentID().String(), x.targetHold, "refund:"+x.exchange.String()).Scan(&targetReceipts, &sourceReceipts); err != nil {
		t.Fatal(err)
	}
	if targetReceipts != 1 || sourceReceipts != 0 {
		t.Fatalf("target receipts=%d source receipts=%d, want 1 and 0", targetReceipts, sourceReceipts)
	}
	assertNoReplacementIssued(t, after, x, "after the abandonment")

	// The terminal row leaves the listing. The exchange replay's `abandoned` answer (D24) is
	// proven in services/commerce/internal/api/exchange_abandoned_replay_smoke_test.go.
	if listedCandidate(t, x) {
		t.Fatalf("the abandoned exchange %s is still listed", x.exchange)
	}
}

func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestAbandonARacedUpgrade is COS1, COS3 and COS4 for an upgrade with a retained fee: two
// tickets at 2500 plus a 300 fee each, exchanged to 4000 each. The delta 3000 is refunded
// against the exchange's own charge; the target gross 8600 leaves the recorded sale.
//
// COS3's replay: the frozen `order.exchanged` event is published again, and Access refuses it
// again under its locked admission check. The replacement still has no ticket.
func TestAbandonARacedUpgrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	x := raceSettledExchange(t, ctx, abandonSpec{tag: "upgrade", quantity: 2, sourcePrice: 2500, fee: 300,
		targetPrice: 4000, needsPaymentInstrument: true})
	abandonAndAssert(t, ctx, x)

	commerce := connectOutcomeDB(t, "commerce")
	var subject, envelope string
	if err := commerce.QueryRow(ctx, `SELECT subject, envelope::text FROM completion_outbox WHERE event_id = $1 AND order_id = $2`,
		x.eventID(), x.replacementOrder).Scan(&subject, &envelope); err != nil {
		t.Fatalf("read the frozen exchange event: %v", err)
	}
	if subject != orderExchangedSubject {
		t.Fatalf("frozen event subject = %q", subject)
	}
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	// A new message id, so the broker's duplicate window does not drop the redelivery.
	ack, err := js.Publish(ctx, orderExchangedSubject, []byte(envelope),
		jetstream.WithMsgID("tkt498-replay-"+uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	// Access's refusal record for this replay is NOT observable: Access publishes it under the
	// event id as message id, so the broker's duplicate window drops the second record of the
	// same event. Wait instead until Access's own durable has acknowledged (or terminated)
	// every message up to the replay, so Access has judged it.
	awaitIssuerPast(t, ctx, js, ack.Sequence, 60*time.Second)
	assertNoReplacementIssued(t, readDurableState(t, ctx, x), x, "after the frozen event was replayed")
}

// TestAbandonARacedDowngrade is COS2, COS3 and COS4 for a downgrade: one ticket at 4000 to one
// at 1500. The settlement's 2500 refund stays the only provider refund, and the abandonment
// writes it off.
func TestAbandonARacedDowngrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	x := raceSettledExchange(t, ctx, abandonSpec{tag: "downgrade", quantity: 1, sourcePrice: 4000, targetPrice: 1500})
	abandonAndAssert(t, ctx, x)
}

// TestAbandonARacedEvenExchange is the even branch (TKT-498 D11): one ticket at 3000 with a
// 150 fee to one at 3000. No refund is dispatched and nothing is written off; the target comes
// back whole and the journal still receives one compensating fact.
func TestAbandonARacedEvenExchange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	x := raceSettledExchange(t, ctx, abandonSpec{tag: "even", quantity: 1, sourcePrice: 3000, fee: 150, targetPrice: 3000})
	abandonAndAssert(t, ctx, x)
}
