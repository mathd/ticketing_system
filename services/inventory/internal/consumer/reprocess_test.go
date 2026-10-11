package consumer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
)

// In-process reprocess (TKT-326 D5). Every fixture is a handwritten JSON literal, never built
// from the Go struct under test (ADR-017). Each case runs the live handler through the
// recording delivery and asserts the outcome AND what the store was asked to do, because an
// outcome alone cannot tell "applied" from "acked without a projection".

const (
	rpEvent = "8d3e1a52-4b1f-4c11-9a51-2f8f0a9b7c01"
	rpSlot  = "8d3e1a52-4b1f-4c11-9a51-2f8f0a9b7c02"
	rpOrg   = "8d3e1a52-4b1f-4c11-9a51-2f8f0a9b7c03"
)

func rpPublication(schema, data string) []byte {
	return []byte(`{"id":"` + rpEvent + `","type":"platform.catalog.performance.published","schema":` + schema + `,"data":` + data + `}`)
}

func rpArchive() []byte {
	return []byte(`{"id":"` + rpEvent + `","type":"platform.catalog.performance.archived","schema":2,"data":{"performance_id":"` + rpSlot + `","organizer_id":"` + rpOrg + `"}}`)
}

var (
	rpSchema2 = rpPublication("2", `{"performance_id":"`+rpSlot+`","organizer_id":"`+rpOrg+`","capacity":120}`)
	rpSchema1 = rpPublication("1", `{"performance_id":"`+rpSlot+`","organizer_id":"`+rpOrg+`"}`)
)

func TestReprocessorOutcomes(t *testing.T) {
	dbDown := errors.New("db down")
	event := uuid.MustParse(rpEvent)
	for _, tc := range []struct {
		name     string
		subject  string
		envelope []byte
		store    fakeCatalogStore
		resolver PerformanceResolver
		want     Outcome
		wantErr  bool  // the call must fail
		errIs    error // and, when set, the failure must wrap this error
		check    func(t *testing.T, st *fakeCatalogStore)
	}{
		{
			name: "a supported publication is applied", subject: subjectPublished, envelope: rpSchema2,
			want: OutcomeApplied,
			check: func(t *testing.T, st *fakeCatalogStore) {
				if len(st.provisioned) != 1 || st.provisioned[0] != event {
					t.Fatalf("provisioned %v, want exactly event %s", st.provisioned, event)
				}
			},
		},
		{
			name: "a moot publication is applied once its record is written", subject: subjectPublished, envelope: rpSchema1,
			resolver: fakeResolver{err: ErrPerformanceNotFound}, want: OutcomeApplied,
			check: func(t *testing.T, st *fakeCatalogStore) {
				if len(st.moot) != 1 || st.moot[0].source != store.MootSourcePublication {
					t.Fatalf("moot records %v, want one publication record", st.moot)
				}
			},
		},
		{
			name: "a payload invalid at its known schema is terminal", subject: subjectPublished,
			envelope: rpPublication("2", `{"performance_id":"`+rpSlot+`","organizer_id":"`+rpOrg+`","capacity":0}`),
			want:     OutcomeTerminal,
			check: func(t *testing.T, st *fakeCatalogStore) {
				if len(st.provisioned) != 0 {
					t.Fatalf("a terminal event provisioned %v", st.provisioned)
				}
			},
		},
		{
			name: "an offering event with no pool and no moot record is deferred", subject: subjectArchived, envelope: rpArchive(),
			store: fakeCatalogStore{err: store.ErrNotFound}, want: OutcomeDeferred,
			check: func(t *testing.T, st *fakeCatalogStore) {
				if len(st.fallbacks) != 1 || len(st.archived) != 0 {
					t.Fatalf("fallbacks %v archived %v, want one fallback question and no archive", st.fallbacks, st.archived)
				}
			},
		},
		{
			name: "a catalog outage is deferred", subject: subjectPublished, envelope: rpSchema1,
			resolver: fakeResolver{err: errResolveUnavailable}, want: OutcomeDeferred,
			check: func(t *testing.T, st *fakeCatalogStore) {
				if len(st.provisioned) != 0 || len(st.moot) != 0 {
					t.Fatalf("an outage wrote provisioned=%v moot=%v", st.provisioned, st.moot)
				}
			},
		},
		{
			// No delivery count exists in reprocess, so the bound cannot be judged: the event is
			// deferred and never parked (D3). A park would succeed in this fake and be recorded.
			name: "an unusable answer is deferred and never parked", subject: subjectPublished, envelope: rpSchema1,
			resolver: fakeResolver{err: errCatalogUnusable}, want: OutcomeDeferred,
			check: func(t *testing.T, st *fakeCatalogStore) {
				if len(st.parked) != 0 {
					t.Fatalf("reprocess parked %v; it has no delivery count to bound", st.parked)
				}
			},
		},
		{
			name: "a projection store failure (an immediate NAK) is an error", subject: subjectPublished, envelope: rpSchema2,
			store: fakeCatalogStore{err: dbDown}, wantErr: true,
		},
		{
			// The moot write fails and the handler answers with a DELAYED NAK, the same call a
			// deferral makes. Only the observed store error tells them apart.
			name: "a moot write failure followed by a delayed NAK is an error", subject: subjectPublished, envelope: rpSchema1,
			resolver: fakeResolver{err: ErrPerformanceNotFound}, store: fakeCatalogStore{mootErr: dbDown}, wantErr: true, errIs: dbDown,
		},
		{
			name: "a moot fallback failure followed by a delayed NAK is an error", subject: subjectArchived, envelope: rpArchive(),
			store: fakeCatalogStore{err: store.ErrNotFound, fallbackErr: dbDown}, wantErr: true, errIs: dbDown,
		},
		{
			// The command gates on SupportsCatalogSchema first. If a future row ever got here, it
			// must not be written back into the quarantine it came from.
			name: "a future schema is refused, never re-quarantined", subject: subjectPublished,
			envelope: rpPublication("9", `{"slot_ref":"a"}`), wantErr: true,
			check: func(t *testing.T, st *fakeCatalogStore) {
				if len(st.quarantined) != 0 {
					t.Fatalf("reprocess re-quarantined %v", st.quarantined)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.store
			process := Reprocessor(&st, tc.resolver, slog.New(slog.NewTextHandler(io.Discard, nil)))

			got, err := process(context.Background(), tc.subject, tc.envelope)

			switch {
			case !tc.wantErr && err != nil:
				t.Fatalf("err = %v, want outcome %d", err, tc.want)
			case tc.wantErr && (err == nil || tc.errIs != nil && !errors.Is(err, tc.errIs)):
				t.Fatalf("outcome = %d, err = %v; want an error wrapping %v — a store failure must fail the run, not read as an outcome", got, err, tc.errIs)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("outcome = %d, want %d", got, tc.want)
			}
			if tc.check != nil {
				tc.check(t, &st)
			}
		})
	}
}

// The classification table of D5, including the calls no handler branch makes today: zero
// calls and several calls. Neither is reachable through handle, so they are asserted here.
func TestReprocessClassification(t *testing.T) {
	storeErr := errors.New("store down")
	for _, tc := range []struct {
		name     string
		calls    []string
		storeErr error
		want     Outcome
		wantErr  bool
	}{
		{"ack is applied", []string{"ack"}, nil, OutcomeApplied, false},
		{"term is terminal", []string{"term"}, nil, OutcomeTerminal, false},
		{"term after a store error is still terminal", []string{"term"}, storeErr, OutcomeTerminal, false},
		{"an immediate nak is a store failure", []string{"nak"}, nil, 0, true},
		{"a delayed nak is a deferral", []string{"nak-delay"}, nil, OutcomeDeferred, false},
		{"a delayed nak after a store error is a store failure", []string{"nak-delay"}, storeErr, 0, true},
		{"no call is an error", nil, nil, 0, true},
		{"two calls are an error", []string{"ack", "term"}, nil, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyReprocess(tc.calls, tc.storeErr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("outcome = %d, want %d", got, tc.want)
			}
		})
	}
}

// A reprocess run has no delivery count, so an unusable catalog answer takes the handler's
// unreadable-metadata branch and defers. The operator reads only the deferred count and the log,
// so that log line must carry the catalog answer's cause, not only "no delivery count" (TKT-326
// review finding 1).
func TestReprocessorLogsTheCauseOfAnUnusableAnswer(t *testing.T) {
	cause := fmt.Errorf("%w: capacity_group_id is all zeros (cause-marker-5e1c)", errCatalogUnusable)
	var logs bytes.Buffer
	st := &fakeCatalogStore{}
	process := Reprocessor(st, fakeResolver{err: cause}, slog.New(slog.NewTextHandler(&logs, nil)))

	got, err := process(context.Background(), subjectPublished, rpSchema1)
	if err != nil || got != OutcomeDeferred {
		t.Fatalf("outcome = %d, err = %v; want a deferral", got, err)
	}
	if !strings.Contains(logs.String(), "cause-marker-5e1c") {
		t.Fatalf("log = %q; want it to carry the unusable answer's cause, or the operator cannot see why the row deferred", logs.String())
	}
}
