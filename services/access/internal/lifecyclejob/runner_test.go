package lifecyclejob

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/access/internal/store"
)

type fakeCheckpointStore struct {
	mu         sync.Mutex
	pending    []uuid.UUID
	observed   []store.LastRoot
	sequence   int64
	err        error
	oldest     time.Time
	hasPending bool
}

func (f *fakeCheckpointStore) PendingCheckpointOrganizers(context.Context) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending, nil
}

func (f *fakeCheckpointStore) CheckpointOrganizer(_ context.Context, id uuid.UUID, observed store.LastRoot) (store.CheckpointResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed = append(f.observed, observed)
	if f.err != nil {
		return store.CheckpointResult{}, f.err
	}
	f.sequence++
	return store.CheckpointResult{OrganizerID: id, Sequence: f.sequence, Root: []byte{byte(f.sequence)}, LeafCount: 1}, nil
}

func (f *fakeCheckpointStore) OldestPendingChange(context.Context) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.oldest, f.hasPending, nil
}

func (f *fakeCheckpointStore) calls() []store.LastRoot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.LastRoot(nil), f.observed...)
}

func TestCheckpointerRemembersEachOrganizersPosition(t *testing.T) {
	organizer := uuid.New()
	f := &fakeCheckpointStore{pending: []uuid.UUID{organizer}}
	c := NewCheckpointer(f, time.Hour, nil, nil)

	if n := c.Once(context.Background()); n != 1 {
		t.Fatalf("first pass committed %d checkpoints, want 1", n)
	}
	if n := c.Once(context.Background()); n != 1 {
		t.Fatalf("second pass committed %d checkpoints, want 1", n)
	}
	calls := f.calls()
	if len(calls) != 2 {
		t.Fatalf("%d checkpoint calls", len(calls))
	}
	// The first pass has no memory; the second carries what the first returned,
	// which is what lets the store refuse an observable regression.
	if calls[0].Sequence != 0 {
		t.Fatalf("first pass claimed to have observed sequence %d", calls[0].Sequence)
	}
	if calls[1].Sequence != 1 || len(calls[1].Root) == 0 {
		t.Fatalf("second pass observed %+v, want the first pass's position", calls[1])
	}
}

// A regression is logged and skipped, never fatal: one organizer's bad chain
// must not stop the worker checkpointing everyone else.
func TestCheckpointerSurvivesARegression(t *testing.T) {
	f := &fakeCheckpointStore{pending: []uuid.UUID{uuid.New()}, err: store.ErrCheckpointRegression}
	c := NewCheckpointer(f, time.Hour, nil, nil)
	if n := c.Once(context.Background()); n != 0 {
		t.Fatalf("a regressed chain produced %d checkpoints", n)
	}
}

// "Last success" that refreshes on a failed pass reads healthy while
// checkpointing is broken — the one thing the metric exists to rule out.
func TestCheckpointerDoesNotCallAFailedPassASuccess(t *testing.T) {
	f := &fakeCheckpointStore{pending: []uuid.UUID{uuid.New()}, err: errors.New("database on fire")}
	c := NewCheckpointer(f, time.Hour, nil, nil)
	c.Once(context.Background())
	if !c.LastSuccess().IsZero() {
		t.Fatal("a pass where every organizer failed refreshed last_success: the freshness gauge would read healthy while nothing is being checkpointed")
	}
	// Recovering restores the signal.
	f.mu.Lock()
	f.err = nil
	f.mu.Unlock()
	c.Once(context.Background())
	if c.LastSuccess().IsZero() {
		t.Fatal("a successful pass did not update last_success")
	}
}

// Freshness has to tell "idle" from "dead". Only a heartbeat on the no-change
// path does that, and it is the ONLY staleness signal there is — ADR-021 §D2 is
// explicit the checkpoint itself detects nothing.
func TestCheckpointerHeartbeatsWhenThereIsNothingToDo(t *testing.T) {
	f := &fakeCheckpointStore{}
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	c := NewCheckpointer(f, time.Hour, nil, func() time.Time { return now })
	if n := c.Once(context.Background()); n != 0 {
		t.Fatalf("an idle pass committed %d checkpoints", n)
	}
	if !c.LastSuccess().Equal(now) {
		t.Fatalf("last success = %v, want %v: an idle worker must be distinguishable from a dead one", c.LastSuccess(), now)
	}
}

func TestCheckpointerRunsImmediatelyThenOnInterval(t *testing.T) {
	f := &fakeCheckpointStore{pending: []uuid.UUID{uuid.New()}}
	c := NewCheckpointer(f, 20*time.Millisecond, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for len(f.calls()) < 2 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("the worker did not run immediately and then on its interval")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestCheckpointerDefaultsToSixtySeconds(t *testing.T) {
	if DefaultInterval != 60*time.Second {
		t.Fatalf("DefaultInterval = %v; ADR-021 §D3 sets 60s and the interval is a permanent bound on what TKT-11 can ever attest", DefaultInterval)
	}
	c := NewCheckpointer(&fakeCheckpointStore{}, 0, nil, nil)
	if c.interval != DefaultInterval {
		t.Fatalf("interval = %v with no configuration, want %v", c.interval, DefaultInterval)
	}
}

type fakeAlarmStore struct {
	mu        sync.Mutex
	queue     []store.AlarmMessage
	released  []uuid.UUID
	published []uuid.UUID
	retire    bool
}

func (f *fakeAlarmStore) ClaimAlarms(context.Context, int, time.Duration) ([]store.AlarmMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.queue
	f.queue = nil
	return out, nil
}

func (f *fakeAlarmStore) ReleaseAlarm(_ context.Context, eventID, _ uuid.UUID, _ error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, eventID)
	return nil
}

func (f *fakeAlarmStore) MarkAlarmPublished(_ context.Context, eventID, _ uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, eventID)
	return f.retire, nil
}

func (f *fakeAlarmStore) OldestUnpublishedAlarm(context.Context) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func (f *fakeAlarmStore) DeadLetteredAlarms(context.Context) (int64, error) { return 0, nil }

type fakePublisher struct {
	mu   sync.Mutex
	sent []uuid.UUID
	err  error
}

func (p *fakePublisher) PublishRaw(_ context.Context, _ string, eventID uuid.UUID, _ []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.sent = append(p.sent, eventID)
	return nil
}

func TestAlarmDrainerPublishesAndRetires(t *testing.T) {
	id := uuid.New()
	st := &fakeAlarmStore{queue: []store.AlarmMessage{{EventID: id, Subject: store.SubjectIntegrityAlarm, Envelope: []byte(`{}`), ClaimID: uuid.New()}}, retire: true}
	pub := &fakePublisher{}
	d := NewAlarmDrainer(st, pub, time.Hour, 8, nil)

	if n := d.Once(context.Background()); n != 1 {
		t.Fatalf("drained %d alarms, want 1", n)
	}
	if len(pub.sent) != 1 || pub.sent[0] != id {
		t.Fatalf("published %v", pub.sent)
	}
}

// A failed publish must return the row, not drop it: the alarm is the only
// notice that someone was admitted through a chain that did not verify.
func TestAlarmDrainerReturnsAFailedAlarmToTheQueue(t *testing.T) {
	id := uuid.New()
	st := &fakeAlarmStore{queue: []store.AlarmMessage{{EventID: id, Subject: store.SubjectIntegrityAlarm, Envelope: []byte(`{}`), ClaimID: uuid.New()}}}
	pub := &fakePublisher{err: errors.New("broker down")}
	d := NewAlarmDrainer(st, pub, time.Hour, 8, nil)

	if n := d.Once(context.Background()); n != 0 {
		t.Fatalf("counted %d publications despite a broker failure", n)
	}
	if len(st.released) != 1 || st.released[0] != id {
		t.Fatalf("released %v, want the failed alarm returned to the queue", st.released)
	}
	if len(st.published) != 0 {
		t.Fatal("a failed alarm was marked published, silently dropping a degraded admission")
	}
}

// Losing the claim mid-publish means another drainer owns the row now. The event
// did go out, so this is a possible duplicate, not a loss.
func TestAlarmDrainerDoesNotCountAPublicationItLostTheClaimFor(t *testing.T) {
	st := &fakeAlarmStore{queue: []store.AlarmMessage{{EventID: uuid.New(), Subject: store.SubjectIntegrityAlarm, Envelope: []byte(`{}`), ClaimID: uuid.New()}}, retire: false}
	d := NewAlarmDrainer(st, &fakePublisher{}, time.Hour, 8, nil)
	if n := d.Once(context.Background()); n != 0 {
		t.Fatalf("counted %d retired alarms after losing the claim", n)
	}
}
