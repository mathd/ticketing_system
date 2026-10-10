package consumer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
//   - a 404 is ErrPerformanceNotFound, and it is neither of the other two — catalog's one
//     definitive answer;
//   - a failure to reach catalog, or a non-404 status, is errResolveUnavailable — a dependency
//     outage, retried without a limit;
//   - a 200 whose body cannot be used is errCatalogUnusable (TKT-317 D2). Catalog was reached,
//     so it is neither of the others. The consumer bounds it by delivery count and parks it.
//
// Miss one and that failure mode is retried for ever, or terminated, or parked on a transport
// blip. Only the separation is right.
func TestTheCatalogResolverClassifiesEveryFailure(t *testing.T) {
	ok := `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":10}`

	for name, tc := range map[string]struct {
		status      int
		body        string
		unavailable bool // want errResolveUnavailable
		unusable    bool // want errCatalogUnusable
		notFound    bool // want ErrPerformanceNotFound
	}{
		"a 404 is catalog's definitive answer": {status: 404, body: `{}`, notFound: true},
		"a 500 could not be answered":          {status: 500, body: `{}`, unavailable: true},
		"a 503 could not be answered":          {status: 503, body: `{}`, unavailable: true},
		"a 401 could not be answered":          {status: 401, body: `{}`, unavailable: true},
		// A 200 is catalog's answer. An answer that cannot be used is a class of its own, so the
		// consumer can bound it. Each row fails one check: syntax, type, a missing field, a
		// range, or one direction of a partial festival pair.
		"a body that is not JSON":       {status: 200, body: `<html>`, unusable: true},
		"a truncated body":              {status: 200, body: `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8",`, unusable: true},
		"an empty body":                 {status: 200, body: ``, unusable: true},
		"a capacity of the wrong type":  {status: 200, body: `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":"ten"}`, unusable: true},
		"a body missing organizer":      {status: 200, body: `{"capacity":10}`, unusable: true},
		"a nil organizer":               {status: 200, body: `{"organizer_id":"00000000-0000-0000-0000-000000000000","capacity":10}`, unusable: true},
		"a body with zero capacity":     {status: 200, body: `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":0}`, unusable: true},
		"a body with negative capacity": {status: 200, body: `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":-3}`, unusable: true},
		"a festival group without its shared capacity": {status: 200,
			body:     `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":10,"capacity_group_id":"7c9e6679-7425-40de-944b-e07fc1f90ae7"}`,
			unusable: true},
		"a festival shared capacity without its group": {status: 200,
			body:     `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":10,"shared_capacity":100}`,
			unusable: true},
		"a festival with zero shared capacity": {status: 200,
			body:     `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":10,"capacity_group_id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","shared_capacity":0}`,
			unusable: true},
		// R4 (TKT-317): an all-zero group is a non-nil pointer to uuid.Nil. It names no group, so the
		// answer cannot be used. Each row pairs the zero group with one shared-capacity shape.
		"a festival with an all-zero group and positive shared capacity": {status: 200,
			body:     `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":10,"capacity_group_id":"00000000-0000-0000-0000-000000000000","shared_capacity":100}`,
			unusable: true},
		"a festival with an all-zero group and zero shared capacity": {status: 200,
			body:     `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":10,"capacity_group_id":"00000000-0000-0000-0000-000000000000","shared_capacity":0}`,
			unusable: true},
		"a festival with an all-zero group and no shared capacity": {status: 200,
			body:     `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":10,"capacity_group_id":"00000000-0000-0000-0000-000000000000"}`,
			unusable: true},
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
			if got := errors.Is(err, errCatalogUnusable); got != tc.unusable {
				t.Errorf("errCatalogUnusable = %v, want %v (err %v). The consumer parks this class "+
					"after bounded deliveries and retries the transport class without a bound, so "+
					"a misclassification either parks a transient failure or retries an answer for ever",
					got, tc.unusable, err)
			}
		})
	}

	// A connection that drops mid-body is a transport failure, not an answer that cannot be used.
	// The server declares more bytes than it sends, then closes the connection.
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "200")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"organizer_id":"6ba7b810-`))
	}))
	defer cut.Close()
	if _, err := NewCatalogResolver(cut.URL, "token", cut.Client()).
		PublishedPerformance(context.Background(), uuid.New()); !errors.Is(err, errResolveUnavailable) || errors.Is(err, errCatalogUnusable) {
		t.Errorf("a body cut short gave %v, want errResolveUnavailable and not errCatalogUnusable: "+
			"a dropped connection is transport, and transport is never parked", err)
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

	// A festival with a real group and a positive shared capacity is still accepted. The table above
	// refuses the all-zero group, and this row shows that the refusal does not refuse every festival.
	festival := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":10,"capacity_group_id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","shared_capacity":100}`))
	}))
	defer festival.Close()
	pair, err := NewCatalogResolver(festival.URL, "token", festival.Client()).
		PublishedPerformance(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("a well-formed festival answer failed: %v", err)
	}
	if pair.CapacityGroupID == nil || pair.SharedCapacity == nil || *pair.SharedCapacity != 100 {
		t.Fatalf("festival fields = group %v shared %v, want a group and shared capacity 100", pair.CapacityGroupID, pair.SharedCapacity)
	}
}

// TKT-317 R3: the performance body is read through a cap before it is decoded. A body holds one
// valid value and then more bytes than the cap. The tail is whitespace, which is valid JSON, so only
// the cap can refuse it. A body of exactly the cap still decodes. The cap is written as a literal
// here (64 KiB), so changing the constant is a visible change to this test.
func TestTKT317PerformanceLookupBodyIsCapped(t *testing.T) {
	const capBytes = 64 << 10
	valid := `{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":10}`
	for name, tc := range map[string]struct {
		size     int
		unusable bool
	}{
		"a body one byte under the cap decodes":   {size: capBytes - 1},
		"a body of exactly the cap decodes":       {size: capBytes},
		"a body one byte over the cap is refused": {size: capBytes + 1, unusable: true},
	} {
		t.Run(name, func(t *testing.T) {
			body := valid + strings.Repeat(" ", tc.size-len(valid))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			_, err := NewCatalogResolver(srv.URL, "token", srv.Client()).
				PublishedPerformance(context.Background(), uuid.New())
			if tc.unusable {
				if !errors.Is(err, errCatalogUnusable) {
					t.Fatalf("err = %v, want errCatalogUnusable: a body past the cap is an answer that cannot be used", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a body of exactly the cap failed: %v", err)
			}
		})
	}
}

// endlessBody serves prefix, then spaces, with no end. read counts the bytes the caller takes. It
// fails at ceiling, and when ctx ends, so a read that is not capped fails the test at once rather
// than running for ever.
type endlessBody struct {
	ctx     context.Context
	prefix  []byte
	ceiling int64
	read    int64
}

var errEndlessBodyCeiling = errors.New("test body ceiling reached: the read was not capped")

func (b *endlessBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if b.read >= b.ceiling {
		return 0, errEndlessBodyCeiling
	}
	for i := range p {
		if b.read < int64(len(b.prefix)) {
			p[i] = b.prefix[b.read]
		} else {
			p[i] = ' '
		}
		b.read++
	}
	return len(p), nil
}

func (b *endlessBody) Close() error { return nil }

// roundTripFunc is a test transport. It answers each request itself, so no network is involved.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TKT-317 R3 (second review): the table above cannot tell a capped read from an uncapped one. Its
// bodies are finite, so removing io.LimitReader still passes: ReadAll reads the whole body, and the
// length check then refuses it. This test serves a body that never ends, and it counts the bytes the
// lookup takes. A capped read stops at maxPerformanceBytes+1 bytes, and the answer is unusable. An
// uncapped read reaches the body's ceiling, which is 64 times the cap, and fails there. The context
// bounds the whole call as well.
func TestTKT317PerformanceLookupStopsReadingAtTheCap(t *testing.T) {
	const capBytes = 64 << 10
	const ceiling = 4 << 20
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body := &endlessBody{
		ctx:     ctx,
		prefix:  []byte(`{"organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":10}`),
		ceiling: ceiling,
	}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}, nil
	})}
	_, err := NewCatalogResolver("http://catalog.invalid", "token", client).
		PublishedPerformance(ctx, uuid.New())
	if !errors.Is(err, errCatalogUnusable) {
		t.Fatalf("err = %v after reading %d bytes, want errCatalogUnusable: a body past the cap is an answer that cannot be used",
			err, body.read)
	}
	if body.read != capBytes+1 {
		t.Fatalf("the lookup read %d bytes, want exactly %d: a capped read stops one byte past the cap", body.read, capBytes+1)
	}
}
