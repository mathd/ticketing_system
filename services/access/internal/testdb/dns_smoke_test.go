//go:build smoke

package testdb

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func testDNSProducer(t *testing.T) *dnsProducer {
	t.Helper()
	p, err := newDNSProducer("tkt557-" + randomHex(t, 8) + ".invalid.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func requireDNSProperties(t *testing.T, err error, name string) {
	t.Helper()
	var dns *net.DNSError
	if !errors.As(err, &dns) {
		t.Fatalf("want real DNS error, got %T: %v", err, err)
	}
	if dns.Name != name || !dns.IsNotFound || dns.IsTemporary || dns.IsTimeout {
		t.Fatalf("DNS properties: name=%q notfound=%v temporary=%v timeout=%v; want %q NXDOMAIN", dns.Name, dns.IsNotFound, dns.IsTemporary, dns.IsTimeout, name)
	}
}

func requireProducerQuestions(t *testing.T, p *dnsProducer, exactCalls int) {
	t.Helper()
	queries, errs := p.snapshot()
	if len(errs) != 0 {
		t.Fatalf("producer errors: %v", errs)
	}
	counts := map[dnsmessage.Type]int{}
	for _, q := range queries {
		if q.Name != p.name || q.Class != dnsmessage.ClassINET || (q.Type != dnsmessage.TypeA && q.Type != dnsmessage.TypeAAAA) {
			t.Fatalf("unexpected wire question: %+v", q)
		}
		counts[q.Type]++
	}
	if counts[dnsmessage.TypeA] == 0 || counts[dnsmessage.TypeAAAA] == 0 {
		t.Fatalf("missing A/AAAA wire questions: %+v", queries)
	}
	if exactCalls > 0 && (len(queries) != 2*exactCalls || counts[dnsmessage.TypeA] != exactCalls || counts[dnsmessage.TypeAAAA] != exactCalls) {
		t.Fatalf("LookupHost question counts = %v, want %d of each", counts, exactCalls)
	}
	// Mutating an observation copy must not change the producer's records.
	queries[0].Name = "changed.invalid."
	fresh, _ := p.snapshot()
	if fresh[0].Name != p.name {
		t.Fatal("query snapshot aliases producer storage")
	}
}

func TestDNSProducerRealResolver(t *testing.T) {
	for _, method := range []string{"LookupHost", "DialContext"} {
		t.Run(method, func(t *testing.T) {
			p := testDNSProducer(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var err error
			if method == "LookupHost" {
				_, err = p.resolver.LookupHost(ctx, p.name)
			} else {
				var conn net.Conn
				conn, err = (&net.Dialer{Resolver: p.resolver}).DialContext(ctx, "tcp", net.JoinHostPort(p.name, "5432"))
				if conn != nil {
					_ = conn.Close()
				}
				var op *net.OpError
				if !errors.As(err, &op) || op.Op != "dial" {
					t.Fatalf("no real dial OpError: %v", err)
				}
			}
			requireDNSProperties(t, err, p.name)
			p.Close()
			requireProducerQuestions(t, p, 1)
		})
	}
}

func TestDNSProducerConcurrentResolver(t *testing.T) {
	const n = 12
	for _, method := range []string{"LookupHost", "DialContext"} {
		t.Run(method, func(t *testing.T) {
			p := testDNSProducer(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := make(chan struct{})
			results := make(chan error, n)
			var ready sync.WaitGroup
			ready.Add(n)
			for range n {
				go func() {
					ready.Done()
					<-start
					if method == "LookupHost" {
						_, err := p.resolver.LookupHost(ctx, p.name)
						results <- err
					} else {
						conn, err := (&net.Dialer{Resolver: p.resolver}).DialContext(ctx, "tcp", net.JoinHostPort(p.name, "5432"))
						if conn != nil {
							_ = conn.Close()
						}
						results <- err
					}
				}()
			}
			ready.Wait()
			close(start)
			for range n {
				requireDNSProperties(t, <-results, p.name)
			}
			p.Close()
			exactCalls := n
			if method == "DialContext" {
				exactCalls = 0
			} // DialContext can share a lookup singleflight.
			requireProducerQuestions(t, p, exactCalls)
		})
	}
}

func dnsClient(t *testing.T, p *dnsProducer) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", p.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

func writeDNSFrame(t *testing.T, conn net.Conn, body []byte) {
	t.Helper()
	prefix := make([]byte, 2)
	binary.BigEndian.PutUint16(prefix, uint16(len(body)))
	if _, err := conn.Write(append(prefix, body...)); err != nil {
		t.Fatal(err)
	}
}

func dnsQuestion(name string) dnsmessage.Question {
	return dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
}

func TestDNSProducerProtocol(t *testing.T) {
	for _, typ := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		for _, rd := range []bool{false, true} {
			t.Run(typ.String()+"/RD="+map[bool]string{false: "false", true: "true"}[rd], func(t *testing.T) {
				p := testDNSProducer(t)
				conn := dnsClient(t, p)
				q := dnsQuestion(p.name)
				q.Type = typ
				request := dnsmessage.Message{Header: dnsmessage.Header{ID: 0x5a7b, OpCode: 2, RecursionDesired: rd}, Questions: []dnsmessage.Question{q},
					Additionals: []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("."), Type: dnsmessage.TypeOPT, Class: 1232}, Body: &dnsmessage.OPTResource{}}}}
				body, err := request.Pack()
				if err != nil {
					t.Fatal(err)
				}
				writeDNSFrame(t, conn, body)
				var prefix [2]byte
				if _, err := io.ReadFull(conn, prefix[:]); err != nil {
					t.Fatal(err)
				}
				reply := make([]byte, binary.BigEndian.Uint16(prefix[:]))
				if _, err := io.ReadFull(conn, reply); err != nil {
					t.Fatal(err)
				}
				var got dnsmessage.Message
				if err := got.Unpack(reply); err != nil {
					t.Fatal(err)
				}
				want := dnsmessage.Header{ID: request.ID, OpCode: request.OpCode, Response: true, Authoritative: true, RecursionDesired: rd, RecursionAvailable: true, RCode: dnsmessage.RCodeNameError}
				if got.Header != want || !reflect.DeepEqual(got.Questions, request.Questions) || len(got.Answers)+len(got.Authorities)+len(got.Additionals) != 0 {
					t.Fatalf("NXDOMAIN response = %+v, want header %+v and original question", got, want)
				}
				var extra [1]byte
				if n, err := conn.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
					t.Fatalf("one-question connection not closed: n=%d err=%v", n, err)
				}
				p.Close()
				queries, errs := p.snapshot()
				if len(errs) != 0 || len(queries) != 1 || queries[0] != (DNSQuery{Name: p.name, Type: typ, Class: dnsmessage.ClassINET}) {
					t.Fatalf("observations: %v errors=%v", queries, errs)
				}
			})
		}
	}
}

func TestDNSProducerRejectsInvalidRequests(t *testing.T) {
	for _, kind := range []string{"name", "class", "type", "two_questions", "response", "malformed", "empty", "truncated_frame"} {
		t.Run(kind, func(t *testing.T) {
			p := testDNSProducer(t)
			conn := dnsClient(t, p)
			q := dnsQuestion(p.name)
			request := dnsmessage.Message{Questions: []dnsmessage.Question{q}}
			switch kind {
			case "name":
				request.Questions[0].Name = dnsmessage.MustNewName("other.invalid.")
			case "class":
				request.Questions[0].Class = dnsmessage.ClassCHAOS
			case "type":
				request.Questions[0].Type = dnsmessage.TypeTXT
			case "two_questions":
				request.Questions = append(request.Questions, q)
			case "response":
				request.Response = true
			}
			body, err := request.Pack()
			if err != nil {
				t.Fatal(err)
			}
			if kind == "malformed" {
				body = []byte{1, 2, 3}
			}
			if kind == "empty" {
				body = nil
			}
			if kind == "truncated_frame" {
				if _, err := conn.Write([]byte{0, 20, 1}); err != nil {
					t.Fatal(err)
				}
				if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
			} else {
				writeDNSFrame(t, conn, body)
			}
			var b [1]byte
			if n, err := conn.Read(b[:]); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("invalid request not closed immediately: n=%d err=%v", n, err)
			}
			p.Close()
			queries, errs := p.snapshot()
			if len(errs) != 1 {
				t.Fatalf("want one recorded protocol/I/O error, got %v", errs)
			}
			if kind == "name" || kind == "class" || kind == "type" {
				if len(queries) != 1 || queries[0].Name != request.Questions[0].Name.String() || !strings.Contains(errs[0].Error(), "unexpected question") {
					t.Fatalf("missing decoded invalid question: %v %v", queries, errs)
				}
			}
			// Close must retain a protocol error that was observed before shutdown.
			p.Close()
			_, after := p.snapshot()
			if len(after) != len(errs) {
				t.Fatal("Close discarded errors")
			}
		})
	}
}

func TestDNSProducerClosePartialFrame(t *testing.T) {
	p := testDNSProducer(t)
	conn := dnsClient(t, p)
	if _, err := conn.Write([]byte{0, 20, 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.frameStarted:
	case <-time.After(time.Second):
		t.Fatal("handler never read the frame prefix")
	}
	done := make(chan struct{})
	go func() { p.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not terminate the partial-frame handler within 1s")
	}
	var b [1]byte
	if n, err := conn.Read(b[:]); n != 0 || (!errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, syscall.ECONNRESET)) {
		t.Fatalf("client not closed: n=%d err=%v", n, err)
	}
	p.mu.Lock()
	active := len(p.active)
	p.mu.Unlock()
	if active != 0 {
		t.Fatalf("%d connections remain registered", active)
	}
	_, errs := p.snapshot()
	if len(errs) != 0 {
		t.Fatalf("shutdown produced unexpected errors: %v", errs)
	}
	p.Close()
	if conn, err := net.DialTimeout("tcp4", p.listener.Addr().String(), time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("listener still accepts after Close")
	}
}
