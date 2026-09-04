package api

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/access/internal/store"
)

// What the database cannot see (ai-review F3).
//
// The smoke and browser tests assert on `redelivery_attempts` and `lifecycle_events`,
// and those counts CANNOT observe a duplicate send: the message ids are derived, so a
// second Send under the same id followed by MarkRedelivered leaves both counts
// unchanged — the UPDATE is idempotent by construction. An implementation that re-sent
// on every replay would pass every row-counting assertion in this ticket while the
// transport delivered the capability again and again.
//
// So these tests assert what the code OBSERVED, at the boundary the value crosses on
// its way out: the transport itself. That is the rule the redelivery smoke test's own
// header quotes and did not follow.

// countingMailer records every Send. The recipient and link are captured too, because
// "how many" is only half the question — "carrying what" is the other half.
type countingMailer struct {
	mu    sync.Mutex
	sends []sentMessage
	fail  func(messageID uuid.UUID) error
}

type sentMessage struct {
	messageID uuid.UUID
	email     string
	link      string
}

func (m *countingMailer) Send(_ context.Context, messageID uuid.UUID, email, link string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		if err := m.fail(messageID); err != nil {
			return err
		}
	}
	m.sends = append(m.sends, sentMessage{messageID, email, link})
	return nil
}

func (m *countingMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sends)
}

func (m *countingMailer) countOf(id uuid.UUID) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sends {
		if s.messageID == id {
			n++
		}
	}
	return n
}

type staticAddressBook struct {
	email string
	err   error
}

func (a staticAddressBook) DeliveryEmail(context.Context, uuid.UUID) (string, error) {
	return a.email, a.err
}

// fakeRedeliveryStore stands in for the postgres store at this tier.
//
// It models the ONE property the transport assertions depend on and nothing else: a
// claim commits before any send, so a replay must hand back which tickets are still
// outstanding. The row-level guarantees (uniqueness, the bound, the chain) belong to
// the store tier and are asserted there against real PostgreSQL — a fake enforcing them
// in Go would prove the fake and the handler agree, and nothing else.
type fakeRedeliveryStore struct {
	tickets  []store.RedeliveryTicket
	claimed  bool
	accepted map[uuid.UUID]bool
	claimErr error
	markErr  func(ticketID uuid.UUID) error
	marks    int
	order    uuid.UUID
}

func (f *fakeRedeliveryStore) ClaimRedelivery(_ context.Context, _, order uuid.UUID, _ string) (store.RedeliveryClaim, error) {
	if f.claimErr != nil {
		return store.RedeliveryClaim{}, f.claimErr
	}
	f.order = order
	replay := f.claimed
	f.claimed = true
	out := make([]store.RedeliveryTicket, len(f.tickets))
	copy(out, f.tickets)
	for i := range out {
		out[i].Accepted = f.accepted[out[i].TicketID]
	}
	return store.RedeliveryClaim{OrderID: order, Tickets: out, Replay: replay}, nil
}

func (f *fakeRedeliveryStore) MarkRedelivered(_ context.Context, _ uuid.UUID, _ string, ticketID, _ uuid.UUID) error {
	if f.markErr != nil {
		if err := f.markErr(ticketID); err != nil {
			return err
		}
	}
	f.marks++
	f.accepted[ticketID] = true
	return nil
}

// redeliverWith drives the REAL handler — Server.redeliver — against the fake store and
// a counting mailer.
//
// The first version of this helper reproduced the handler's claim/send/mark sequence
// instead of calling it, and ai-review pass 2 was right that this made every assertion
// below a fact about the TEST: reverting the shipped handler to return early on a replay,
// or to iterate claim.Tickets instead of Outstanding(), left them all green. The
// assertions are worth something only if the code under test is the code that ships.
func redeliverWith(f *fakeRedeliveryStore, m *countingMailer, a staticAddressBook, publicURL string) (int, bool, error) {
	s := newTestServer(nil, nil).WithRedeliveryStore(f).WithRedelivery(a, m, publicURL)
	out, err := s.redeliver(context.Background(), uuid.New(), uuid.New(), "the-key")
	return out.TicketCount, out.Replay, err
}

func twoTickets() *fakeRedeliveryStore {
	a, b := uuid.New(), uuid.New()
	return &fakeRedeliveryStore{
		accepted: map[uuid.UUID]bool{},
		tickets: []store.RedeliveryTicket{
			{TicketID: a, BuyerID: uuid.New(), GuestOrderRef: uuid.New(), MessageID: uuid.New()},
			{TicketID: b, BuyerID: uuid.New(), GuestOrderRef: uuid.New(), MessageID: uuid.New()},
		},
	}
}

// The invariant, stated without naming the implementation: one redelivery request hands
// each ticket to the transport exactly once, however many times it is submitted.
func TestOneKeySendsEachTicketExactlyOnceAcrossReplays(t *testing.T) {
	f, m := twoTickets(), &countingMailer{}
	addr := staticAddressBook{email: "buyer@example.test"}

	if _, replay, err := redeliverWith(f, m, addr, "https://tickets.test"); err != nil || replay {
		t.Fatalf("first request: replay=%v err=%v", replay, err)
	}
	if m.count() != 2 {
		t.Fatalf("first request sent %d messages, want one per ticket (2)", m.count())
	}

	for i := 0; i < 3; i++ {
		if _, replay, err := redeliverWith(f, m, addr, "https://tickets.test"); err != nil || !replay {
			t.Fatalf("resubmit %d: replay=%v err=%v", i, replay, err)
		}
	}
	// THE assertion the row counts cannot make. A re-send under the same derived
	// message id leaves redelivery_attempts and lifecycle_events untouched, so every
	// database-level check in this ticket stays green while the customer receives the
	// mail four times.
	if m.count() != 2 {
		t.Fatalf("the transport received %d messages after one request and three resubmits, "+
			"want 2: replays must not re-send", m.count())
	}
	for _, tk := range f.tickets {
		if n := m.countOf(tk.MessageID); n != 1 {
			t.Fatalf("message %s was handed to the transport %d times, want 1", tk.MessageID, n)
		}
	}
}

// F2, directly. A request that dies mid-send leaves a COMMITTED claim with outstanding
// tickets; the retry must finish the job rather than report it done.
func TestAPartialSendIsResumedRatherThanReportedComplete(t *testing.T) {
	f, m := twoTickets(), &countingMailer{}
	addr := staticAddressBook{email: "buyer@example.test"}
	second := f.tickets[1].MessageID

	// The transport takes the first ticket and refuses the second.
	m.fail = func(id uuid.UUID) error {
		if id == second {
			return errors.New("transport refused")
		}
		return nil
	}
	if _, _, err := redeliverWith(f, m, addr, "https://tickets.test"); err == nil {
		t.Fatal("a refused send reported success")
	}
	if m.count() != 1 {
		t.Fatalf("%d messages reached the transport before the failure, want 1", m.count())
	}

	// The retry: the transport is healthy again.
	m.fail = nil
	count, replay, err := redeliverWith(f, m, addr, "https://tickets.test")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !replay {
		t.Fatal("the retry was treated as a new request rather than a resume")
	}
	if count != 2 {
		t.Fatalf("the retry reported %d tickets, want 2", count)
	}
	// The heart of F2: the second ticket must now have been sent, and the first must
	// NOT have been sent again. A replay that returned success without resuming would
	// leave the customer half their tickets while the API said the order was done.
	if m.countOf(second) != 1 {
		t.Fatalf("the outstanding ticket was handed to the transport %d times, want 1: "+
			"a replay that reports success without resuming leaves it permanently unsent",
			m.countOf(second))
	}
	if m.countOf(f.tickets[0].MessageID) != 1 {
		t.Fatalf("the already-accepted ticket was re-sent %d times, want 1",
			m.countOf(f.tickets[0].MessageID))
	}
}

// The other half of the crash window: the transport accepted, and the process died
// before the trail recorded it. The retry must re-send under the SAME message id — that
// is what makes transport-level deduplication able to save it — and must not skip the
// ticket as though it were done.
func TestASendAcceptedButNotRecordedIsRetriedUnderTheSameMessageID(t *testing.T) {
	f, m := twoTickets(), &countingMailer{}
	addr := staticAddressBook{email: "buyer@example.test"}
	first := f.tickets[0].TicketID

	f.markErr = func(id uuid.UUID) error {
		if id == first {
			return errors.New("crashed before the trail was written")
		}
		return nil
	}
	if _, _, err := redeliverWith(f, m, addr, "https://tickets.test"); err == nil {
		t.Fatal("a failed trail write reported success")
	}

	f.markErr = nil
	if _, _, err := redeliverWith(f, m, addr, "https://tickets.test"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	// Sent twice — unavoidable, and the honest outcome: the platform cannot know the
	// transport took the first one. What it CAN guarantee is that both carry the same
	// message id, so a transport that deduplicates delivers once. ADR-068 records this
	// as a requirement on the transport rather than a closed hole.
	if n := m.countOf(f.tickets[0].MessageID); n != 2 {
		t.Fatalf("the un-recorded send was retried %d times, want 2 (one original, one retry)", n)
	}
	if len(m.sends) != 3 {
		t.Fatalf("%d total sends, want 3: two tickets plus the one retry", len(m.sends))
	}
	// Both attempts under ONE id. If the id were re-derived per attempt, a deduplicating
	// transport could not tell them apart from two genuine sends.
	ids := map[uuid.UUID]bool{}
	for _, s := range m.sends {
		ids[s.messageID] = true
	}
	if len(ids) != 2 {
		t.Fatalf("%d distinct message ids across 3 sends, want 2: a retry must reuse the "+
			"original id or transport deduplication cannot work", len(ids))
	}
}

// The address reaches the transport and nothing else does. A failure to resolve it must
// stop the send rather than deliver to an empty or partial recipient.
func TestTheTransportReceivesTheResolvedAddressAndTheCapabilityLink(t *testing.T) {
	f, m := twoTickets(), &countingMailer{}
	if _, _, err := redeliverWith(f, m, staticAddressBook{email: "on-file@example.test"}, "https://tickets.test"); err != nil {
		t.Fatal(err)
	}
	for _, s := range m.sends {
		if s.email != "on-file@example.test" {
			t.Fatalf("the transport received %q, want the address resolved from commerce", s.email)
		}
		if s.link == "https://tickets.test/en/tickets/" {
			t.Fatal("the capability link carries no reference")
		}
	}

	failing, m2 := twoTickets(), &countingMailer{}
	if _, _, err := redeliverWith(failing, m2, staticAddressBook{err: errors.New("commerce down")}, "https://tickets.test"); err == nil {
		t.Fatal("an unresolvable address reported success")
	}
	if m2.count() != 0 {
		t.Fatalf("%d messages were sent despite the address lookup failing", m2.count())
	}
}
