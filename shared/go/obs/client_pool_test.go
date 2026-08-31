package obs

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

	// ConnState counts on the SERVER, which is the honest place: it observes the
	// connections that actually arrived, not what the client believes it did.
	//
	// UNSTARTED, then Start(), because assigning Config.ConnState on a server that is
	// already serving races net/http's accept loop reading it — confirmed with -race
	// (ai-review [medium]). A test whose own measurement apparatus is racy is not
	// measuring anything reliably.
	opened, srv := countingServer()
	defer srv.Close()

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

	opened, srv := countingServer()
	defer srv.Close()

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


// countingServer is an httptest server that counts the connections it ACCEPTS.
//
// Built unstarted so ConnState is installed before the accept loop can read it; doing
// it the other way round is a data race that -race catches (ai-review [medium]).
func countingServer() (*atomic.Int64, *httptest.Server) {
	opened := new(atomic.Int64)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	return opened, srv
}

// TKT-308: a caller with its OWN deadline still shares the pool.
//
// This is the boundary the fix nearly broke, and the one the two tests above cannot
// see — both use Client(), so both passed on origin/main, where every cross-service
// client resolved to http.DefaultTransport and shared a pool BY ACCIDENT.
//
// Cloning the transport for Client() alone would have split that accident apart:
// commerce's recovery runner and inventory's catalog resolver call the same upstreams
// as their Client() siblings and were built as `&http.Client{Timeout: …}` with a nil
// Transport. They would have kept untuned, unshared connections while the tuned pool
// sat beside them — the measured improvement real, the fragmentation invisible
// (ai-review [medium]).
//
// So the assertion is that a differently-bounded client opens NOTHING new, and the
// timeout is asserted too: sharing the pool must not silently hand a caller the
// default deadline, which is the obvious way to "fix" this wrongly.
func TestAClientWithItsOwnTimeoutSharesThePool(t *testing.T) {
	const perClient = 8

	g := newGate()
	opened, srv := gatedServer(g)
	defer srv.Close()

	// BURST ONE, held open until all of it has arrived, so exactly perClient
	// connections exist and the pool afterwards holds all of them.
	//
	// Two earlier versions of this got it wrong. Sequential warming opens one
	// connection, which DefaultTransport can hold too, so the second client reused it
	// and the test passed whether or not the pool was shared. Merely concurrent is
	// better and still not deterministic — the server answered immediately, so nothing
	// forced overlap (ai-review pass 2 [medium]).
	warm := Client()
	burst(t, warm, srv.URL, perClient, g)
	afterWarm := opened.Load()
	if afterWarm != int64(perClient) {
		t.Fatalf("warm burst opened %d connections, want %d — the barrier did not hold them "+
			"all in flight, so the idle pool below is not the size this test assumes",
			afterWarm, perClient)
	}

	tighter := ClientWithTimeout(3 * time.Second)
	if tighter.Timeout != 3*time.Second {
		t.Fatalf("timeout = %s, want 3s — sharing the pool must not override the caller's "+
			"deadline; commerce's recovery runner is deliberately tighter than ClientTimeout "+
			"and its lease is sized against that number", tighter.Timeout)
	}

	// BURST TWO, barriered for a reason the sequential version could not give:
	// DefaultTransport still holds 2 idle connections per host, so a sequential loop
	// reuses one and opens nothing — the mutation putting this client back on
	// DefaultTransport PASSED that version. Only a burst wider than 2 separates them.
	//
	// On the shared transport the warmed pool holds perClient, so this opens none. On
	// DefaultTransport it holds at most 2, so this opens at least perClient-2.
	burst(t, tighter, srv.URL, perClient, g)

	if total := opened.Load(); total != afterWarm {
		t.Errorf("a client with its own timeout opened %d further connections (%d then %d): it "+
			"is not on the shared transport. Before TKT-308 these callers shared a pool with "+
			"obs.Client() because both resolved to http.DefaultTransport; tuning only one of "+
			"them fragments that", total-afterWarm, afterWarm, total)
	}
}

// gate holds every in-flight request until a burst of known width has arrived, then
// releases them together. Re-armable, so one server can host several bursts — which
// matters because the point of the second burst is that it meets the pool the FIRST
// one left behind.
type gate struct {
	mu      sync.Mutex
	arrived chan struct{}
	release chan struct{}
}

func newGate() *gate {
	return &gate{arrived: make(chan struct{}, 1024), release: make(chan struct{})}
}

// wait is called from the handler: announce arrival, then block until released.
func (g *gate) wait() {
	g.mu.Lock()
	arrived, release := g.arrived, g.release
	g.mu.Unlock()
	arrived <- struct{}{}
	<-release
}

// openFor blocks until n requests are in flight, then releases them and re-arms.
func (g *gate) openFor(n int) {
	for i := 0; i < n; i++ {
		<-g.arrived
	}
	g.mu.Lock()
	close(g.release)
	g.release = make(chan struct{})
	g.mu.Unlock()
}

// burst fires n concurrent requests and holds them all in flight before releasing.
func burst(t *testing.T, c *http.Client, url string, n int, g *gate) {
	t.Helper()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); do(t, c, url) }()
	}
	g.openFor(n)
	wg.Wait()
}

// gatedServer counts accepted connections and routes every request through the gate.
func gatedServer(g *gate) (*atomic.Int64, *httptest.Server) {
	opened := new(atomic.Int64)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		g.wait()
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	return opened, srv
}
