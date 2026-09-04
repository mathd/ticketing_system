package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// These cases exercise the response-validation middleware with a real store.ErrNotFound.
// The declarative boundary rule in status_declaration_test.go checks every reachable handler.
func TestUnknownOrganizerIsNotFoundRatherThanContractFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		body   any
		insert func(*fakeStore) int
	}{
		{
			name: "POST /venues", path: "/venues",
			body:   VenueCreate{Name: "Halle A", GaCapacity: 500},
			insert: func(store *fakeStore) int { return len(store.venues) },
		},
		{
			name: "POST /events", path: "/events",
			body:   validEventCreate(),
			insert: func(store *fakeStore) int { return len(store.events) },
		},
		{
			name: "POST /channels", path: "/channels",
			body:   createChannelBody("pos", "Box office", "pos", nil),
			insert: func(store *fakeStore) int { return len(store.channels) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			env.store.unknownOrganizer = env.organizer
			before := tc.insert(env.store)

			recorder := env.do("POST", tc.path, tc.body)

			if recorder.Code != http.StatusNotFound {
				t.Fatalf("unknown organizer must answer 404, got %d %s", recorder.Code, recorder.Body.String())
			}
			if body := recorder.Body.String(); !strings.Contains(body, "referenced entity not found") {
				t.Fatalf("404 must carry the store's actionable message, got %s", body)
			}
			if after := tc.insert(env.store); after != before {
				t.Fatalf("refused create inserted a row: %d before, %d after", before, after)
			}
		})
	}
}

func TestUnknownOrganizerSeamIsInertWhenUnset(t *testing.T) {
	env := newEnv(t)
	if env.store.unknownOrganizer != uuid.Nil {
		t.Fatalf("seam must default to uuid.Nil, got %s", env.store.unknownOrganizer)
	}
	if recorder := env.do("POST", "/venues", VenueCreate{Name: "Halle A", GaCapacity: 500}); recorder.Code != http.StatusCreated {
		t.Fatalf("valid create with unset seam got %d %s", recorder.Code, recorder.Body.String())
	}
	env.store.unknownOrganizer = uuid.New()
	if recorder := env.do("POST", "/venues", VenueCreate{Name: "Halle B", GaCapacity: 500}); recorder.Code != http.StatusCreated {
		t.Fatalf("unrelated organizer seam got %d %s", recorder.Code, recorder.Body.String())
	}
}
