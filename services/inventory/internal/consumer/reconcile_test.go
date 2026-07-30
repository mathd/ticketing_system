package consumer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
)

// Reconciliation tests (TKT-90): the startup pass acts only on positive catalog
// assertions and reuses the event path's apply functions — never a second write path.

type fakeOfferStateResolver struct {
	fakeResolver
	states map[uuid.UUID]PoolOfferState
	errs   map[uuid.UUID]error
}

func (r fakeOfferStateResolver) PoolOfferState(_ context.Context, id uuid.UUID) (PoolOfferState, error) {
	if err, ok := r.errs[id]; ok {
		return PoolOfferState{}, err
	}
	if s, ok := r.states[id]; ok {
		return s, nil
	}
	return PoolOfferState{}, ErrPoolStateNotFound
}

func reconcileConsumer(st catalogStore, r PerformanceResolver) *Consumer {
	return &Consumer{st: st, resolver: r, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestReconcileArchivesPoolWhoseSlotIsArchived(t *testing.T) {
	pool := uuid.New()
	st := &fakeCatalogStore{pools: []store.PoolOffering{{SlotID: pool, ClosureStatus: "open"}}}
	c := reconcileConsumer(st, fakeOfferStateResolver{states: map[uuid.UUID]PoolOfferState{
		pool: {Kind: "performance", Lifecycle: "archived"},
	}})
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.archived) != 1 || st.archived[0] != pool {
		t.Fatalf("archived = %v, want exactly [%s]", st.archived, pool)
	}
	if len(st.closures) != 0 {
		t.Fatalf("closures = %v, want none", st.closures)
	}
}

func TestReconcileAppliesNewerClosureBothDirections(t *testing.T) {
	stale, reopened := uuid.New(), uuid.New()
	st := &fakeCatalogStore{pools: []store.PoolOffering{
		{SlotID: stale, ClosureStatus: "open"},      // catalog says closed@2 → close
		{SlotID: reopened, ClosureStatus: "closed"}, // catalog says open@3 → reopen
	}}
	c := reconcileConsumer(st, fakeOfferStateResolver{states: map[uuid.UUID]PoolOfferState{
		stale:    {Kind: "performance", Lifecycle: "published", ClosureStatus: "closed", ClosureVersion: 2},
		reopened: {Kind: "performance", Lifecycle: "published", ClosureStatus: "open", ClosureVersion: 3},
	}})
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []closureCall{{stale, stale, true, 2}, {reopened, reopened, false, 3}}
	if len(st.closures) != 2 || st.closures[0] != want[0] || st.closures[1] != want[1] {
		t.Fatalf("closures = %v, want %v", st.closures, want)
	}
	if len(st.archived) != 0 {
		t.Fatalf("archived = %v, want none", st.archived)
	}
}

func TestReconcileWritesNothingOnNonPositiveAnswers(t *testing.T) {
	festival, unknown, draft, converged := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	st := &fakeCatalogStore{pools: []store.PoolOffering{
		{SlotID: festival, ClosureStatus: "open"},
		{SlotID: unknown, ClosureStatus: "open"},
		{SlotID: draft, ClosureStatus: "open"},
		{SlotID: converged, ClosureStatus: "closed"},
	}}
	c := reconcileConsumer(st, fakeOfferStateResolver{states: map[uuid.UUID]PoolOfferState{
		festival: {Kind: "festival"},
		draft:    {Kind: "performance", Lifecycle: "draft"},
		// same state as the pool: no drift, no write
		converged: {Kind: "performance", Lifecycle: "published", ClosureStatus: "closed", ClosureVersion: 1},
		// `unknown` falls through to ErrPoolStateNotFound
	}})
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.archived) != 0 || len(st.closures) != 0 {
		t.Fatalf("writes = %v/%v — reconciliation must act only on positive drift", st.archived, st.closures)
	}
}

func TestReconcileUsesFreshEventIDs(t *testing.T) {
	p1, p2 := uuid.New(), uuid.New()
	st := &fakeCatalogStore{pools: []store.PoolOffering{
		{SlotID: p1, ClosureStatus: "open"},
		{SlotID: p2, ClosureStatus: "open"},
	}}
	c := reconcileConsumer(st, fakeOfferStateResolver{states: map[uuid.UUID]PoolOfferState{
		p1: {Kind: "performance", Lifecycle: "archived"},
		p2: {Kind: "performance", Lifecycle: "published", ClosureStatus: "closed", ClosureVersion: 1},
	}})
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ids := append(append([]uuid.UUID(nil), st.archiveEventIDs...), st.closureEventIDs...)
	if len(ids) != 2 || ids[0] == uuid.Nil || ids[1] == uuid.Nil || ids[0] == ids[1] {
		t.Fatalf("event ids = %v, want two distinct non-nil ids — a deterministic id would dedupe-block later reconciliations", ids)
	}
}

func TestReconcileReturnsErrorOnResolverFailure(t *testing.T) {
	ok, bad := uuid.New(), uuid.New()
	st := &fakeCatalogStore{pools: []store.PoolOffering{
		{SlotID: bad, ClosureStatus: "open"},
		{SlotID: ok, ClosureStatus: "open"},
	}}
	c := reconcileConsumer(st, fakeOfferStateResolver{
		states: map[uuid.UUID]PoolOfferState{ok: {Kind: "performance", Lifecycle: "archived"}},
		errs:   map[uuid.UUID]error{bad: errors.New("catalog 500")},
	})
	if err := c.reconcile(context.Background()); err == nil {
		t.Fatal("reconcile() = nil, want error so the pass is retried")
	}
	// The failure must not stop the rest of the pass: the healthy pool still converges.
	if len(st.archived) != 1 || st.archived[0] != ok {
		t.Fatalf("archived = %v, want [%s]", st.archived, ok)
	}
}

func TestStartupConvergeIsFailOpenAfterRetries(t *testing.T) {
	pool := uuid.New()
	st := &fakeCatalogStore{pools: []store.PoolOffering{{SlotID: pool, ClosureStatus: "open"}}}
	c := reconcileConsumer(st, fakeOfferStateResolver{errs: map[uuid.UUID]error{pool: errors.New("catalog down")}})
	if err := c.startupConverge(context.Background()); err != nil {
		t.Fatalf("startupConverge() = %v — a catalog outage must not take inventory down", err)
	}
	if !c.Ready() {
		t.Fatal("Ready() = false — exhausted retries must latch ready (fail-open) and log, not block readiness forever")
	}
}

func TestStartupConvergeStaysUnreadyOnPendingQuarantine(t *testing.T) {
	c := reconcileConsumer(&fakeCatalogStore{pending: true}, fakeOfferStateResolver{})
	if err := c.startupConverge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.Ready() {
		t.Fatal("Ready() = true — a clean reconciliation must not override the quarantine latch")
	}
}

func TestStartupConvergeLatchesReadyAfterCleanPass(t *testing.T) {
	c := reconcileConsumer(&fakeCatalogStore{}, fakeOfferStateResolver{})
	if c.Ready() {
		t.Fatal("Ready() = true before the pass")
	}
	if err := c.startupConverge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.Ready() {
		t.Fatal("Ready() = false after a clean pass")
	}
}

// CatalogResolver.PoolOfferState is a trust boundary: the decode path validates
// shape before reconcile acts on it.
func TestCatalogResolverPoolOfferState(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		want       PoolOfferState
		wantErr    error
		wantAnyErr bool
	}{
		{name: "performance", status: 200,
			body: `{"kind":"performance","lifecycle":"published","closure_status":"closed","closure_version":2}`,
			want: PoolOfferState{Kind: "performance", Lifecycle: "published", ClosureStatus: "closed", ClosureVersion: 2}},
		{name: "archived performance", status: 200,
			body: `{"kind":"performance","lifecycle":"archived","closure_status":"open","closure_version":0}`,
			want: PoolOfferState{Kind: "performance", Lifecycle: "archived", ClosureStatus: "open"}},
		{name: "festival", status: 200, body: `{"kind":"festival"}`, want: PoolOfferState{Kind: "festival"}},
		{name: "not found", status: 404, body: `{"error":"referenced entity not found"}`, wantErr: ErrPoolStateNotFound},
		{name: "unknown kind", status: 200, body: `{"kind":"season"}`, wantAnyErr: true},
		{name: "bad lifecycle", status: 200, body: `{"kind":"performance","lifecycle":"gone","closure_status":"open","closure_version":0}`, wantAnyErr: true},
		{name: "bad closure status", status: 200, body: `{"kind":"performance","lifecycle":"published","closure_status":"ajar","closure_version":1}`, wantAnyErr: true},
		{name: "negative version", status: 200, body: `{"kind":"performance","lifecycle":"published","closure_status":"open","closure_version":-1}`, wantAnyErr: true},
		{name: "server error", status: 500, body: `{}`, wantAnyErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			r := NewCatalogResolver(srv.URL, "tok", srv.Client())
			got, err := r.PoolOfferState(context.Background(), uuid.New())
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.wantAnyErr:
				if err == nil {
					t.Fatalf("err = nil, want validation error (got state %+v)", got)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if got != tt.want {
					t.Fatalf("state = %+v, want %+v", got, tt.want)
				}
			}
		})
	}
}
