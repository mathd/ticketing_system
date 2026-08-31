package consumer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// TKT-307: the resolver CLASSIFIES its failures, and the handler keys on that
// classification rather than on the schema.
//
// This test exists because of what the fix changed and what a fake cannot prove. The
// publication handler used to retry on `e.Schema == 1`, which swept deterministic
// failures into an endless NAK loop. It now retries on errResolveUnavailable — and that
// is only correct if the resolver actually wraps every reach-failure in it. The consumer
// tests assert against a fake, so on their own they would prove the fake and the handler
// agree and nothing else (AGENTS.md's tier rule). The classification lives here, so the
// assertion does too.
//
// Two properties, and both matter:
//   - a 404 is ErrPerformanceNotFound and NOT errResolveUnavailable — it is catalog's one
//     definitive answer, and conflating it would make an archived slot retry for ever;
//   - everything else IS errResolveUnavailable — miss one and that failure mode
//     terminates a publication permanently, losing a slot's inventory silently.
func TestTheCatalogResolverClassifiesEveryFailure(t *testing.T) {
	ok := `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":10}`

	for name, tc := range map[string]struct {
		status      int
		body        string
		unavailable bool // want errResolveUnavailable
		notFound    bool // want ErrPerformanceNotFound
	}{
		"a 404 is catalog's definitive answer": {status: 404, body: `{}`, notFound: true},
		"a 500 could not be answered":          {status: 500, body: `{}`, unavailable: true},
		"a 503 could not be answered":          {status: 503, body: `{}`, unavailable: true},
		"a 401 could not be answered":          {status: 401, body: `{}`, unavailable: true},
		// A 200 whose body is unreadable: catalog is broken, and whether that is
		// transient is not knowable from here. Retrying costs an ack-pending slot;
		// terminating loses the publication. The asymmetry decides it.
		"an undecodable body":       {status: 200, body: `<html>`, unavailable: true},
		"a body missing organizer":  {status: 200, body: `{"capacity":10}`, unavailable: true},
		"a body with zero capacity": {status: 200, body: `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":0}`, unavailable: true},
		"a half-specified festival": {status: 200, body: `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":10,"capacity_group_id":"7c9e6679-7425-40de-944b-e07fc1f90ae7"}`, unavailable: true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := NewCatalogResolver(srv.URL, "token", srv.Client()).
				PublishedPerformance(context.Background(), uuid.New())
			if err == nil {
				t.Fatal("no error")
			}
			if got := errors.Is(err, errResolveUnavailable); got != tc.unavailable {
				t.Errorf("errResolveUnavailable = %v, want %v (err %v). The publication handler "+
					"retries on exactly this and terminates otherwise, so a misclassification "+
					"here either parks poison for ever or permanently loses a publication",
					got, tc.unavailable, err)
			}
			if got := errors.Is(err, ErrPerformanceNotFound); got != tc.notFound {
				t.Errorf("ErrPerformanceNotFound = %v, want %v (err %v)", got, tc.notFound, err)
			}
		})
	}

	// Catalog unreachable at the transport layer — the case with no HTTP response at all.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now
	if _, err := NewCatalogResolver(url, "token", http.DefaultClient).
		PublishedPerformance(context.Background(), uuid.New()); !errors.Is(err, errResolveUnavailable) {
		t.Errorf("a dead catalog gave %v, want errResolveUnavailable — this is the case the "+
			"retry branch exists for, and terminating it drops the publication for ever", err)
	}

	// And the happy path still parses, or every assertion above is about a resolver that
	// never works.
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(ok))
	}))
	defer good.Close()
	got, err := NewCatalogResolver(good.URL, "token", good.Client()).
		PublishedPerformance(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("a well-formed answer failed: %v", err)
	}
	if got.Capacity != 10 {
		t.Fatalf("capacity = %d, want 10", got.Capacity)
	}
}
