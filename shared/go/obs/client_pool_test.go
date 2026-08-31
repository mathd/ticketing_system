package obs

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// TKT-308: the shared cross-service client REUSES connections under concurrency.
//
// This measures the BEHAVIOUR — how many TCP connections a burst of concurrent
// requests to one host actually opens — rather than asserting that a config field
// holds a number. The distinction is the ticket's COS1 and it is not pedantry: a
// test reading MaxIdleConnsPerHost passes if someone sets the field and passes
// equally if the transport it is set on is never the one used. This counts what
// httptrace reports, which is what the kernel actually did.
//
// The defect: obs.Client() wrapped bare http.DefaultTransport, whose
// DefaultMaxIdleConnsPerHost is 2. Past two concurrent requests to one upstream,
// every additional request opens a connection and discards it on completion —
// the gateway's own transport comment makes this argument for the hop one earlier
// and it was never applied here, where commerce calls inventory and payments on
// the checkout path.
//
// WHAT THIS CANNOT SHOW, stated so the evidence is not overread: it exercises one
// process against one httptest server, so it demonstrates connection REUSE, not
// the throughput or latency effect under a real on-sale profile. The load proof is
// the harness for that, and it measures the client→gateway hop rather than this one.
func TestClientReusesConnectionsUnderConcurrency(t *testing.T) {
	const (
		concurrency = 16
		rounds      = 4
	)

	var opened atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// ConnState counts on the SERVER, which is the honest place: it observes the
	// connections that actually arrived, not what the client believes it did.
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}

	c := Client()
	// Sequential rounds of a concurrent burst. One burst alone would open
	// `concurrency` connections whatever the pool size — the pool can only help
	// on the SECOND round, when idle connections from the first are available to
	// reuse. A single-round fixture could not distinguish tuned from untuned.
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
				if err != nil {
					t.Error(err)
					return
				}
				resp, err := c.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				_ = resp.Body.Close()
			}()
		}
		wg.Wait()
	}

	got := opened.Load()
	// With DefaultMaxIdleConnsPerHost=2 only two connections survive each round,
	// so rounds 2..N re-open roughly (concurrency-2) each: ~concurrency*rounds.
	// With the pool tuned to hold the whole burst, rounds 2..N reuse and the total
	// stays near `concurrency`.
	//
	// The bound is deliberately loose. Scheduling decides how many of a burst
	// overlap, so an exact count would be flaky; what is NOT ambiguous is the
	// difference between "reuses across rounds" and "does not". Half of the
	// untuned worst case sits well clear of both.
	if limit := int64(concurrency * rounds / 2); got > limit {
		t.Errorf("opened %d connections for %d requests across %d rounds (want <= %d).\n\n"+
			"The shared client is not reusing connections between rounds, which is what "+
			"http.DefaultTransport's MaxIdleConnsPerHost of 2 does under concurrency: past "+
			"two in flight, each request opens a socket and discards it. On the checkout "+
			"path that is commerce→inventory and commerce→payments, per request, at the "+
			"moment concurrency is highest.",
			got, concurrency*rounds, rounds, limit)
	}
	t.Logf("opened %d connections for %d requests across %d rounds", got, concurrency*rounds, rounds)
}

// TKT-308: SEPARATE clients share the pool, because the transport is package-level.
//
// This is the half the single-client test above cannot see — that one calls Client()
// once, so it passes whether the transport is shared or built per call, and a mutation
// making it per-call leaves it green. Written after noticing exactly that.
//
// The case it protects is real rather than hypothetical: callers hold SEVERAL
// long-lived clients against the same upstreams — access builds one for its consumer
// and another for redelivery, commerce one for the API server and more for its runners.
// A transport each would give each its own idle pool and fragment the reuse this ticket
// exists to create, while every ceiling still read as correctly tuned.
func TestSeparateClientsShareTheConnectionPool(t *testing.T) {
	const perClient = 8

	var opened atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}

	// Warm one client's worth of connections into the pool, sequentially so exactly
	// one connection is needed, then let a DIFFERENT client do the same work. If the
	// pool is shared the second opens nothing.
	first := Client()
	for i := 0; i < perClient; i++ {
		do(t, first, srv.URL)
	}
	afterFirst := opened.Load()

	second := Client()
	for i := 0; i < perClient; i++ {
		do(t, second, srv.URL)
	}
	total := opened.Load()

	if total != afterFirst {
		t.Errorf("a second Client() opened %d further connections (%d then %d): the transport "+
			"is not shared, so each client carries its own idle pool. Callers hold several "+
			"long-lived clients against the same upstreams, and per-client pools fragment the "+
			"reuse while every ceiling still reads as correctly tuned",
			total-afterFirst, afterFirst, total)
	}
	t.Logf("first client opened %d, second opened %d more", afterFirst, total-afterFirst)
}

func do(t *testing.T, c *http.Client, url string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

