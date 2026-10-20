//go:build smoke

package testdb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DNSQuery is a question decoded from the owned TCP listener.
type DNSQuery struct {
	Name  string
	Type  dnsmessage.Type
	Class dnsmessage.Class
}

// dnsProducer answers only its absolute fault name. The Go resolver creates
// the DNS errors from these replies; the fixture never constructs them.
type dnsProducer struct {
	name         string
	listener     net.Listener
	resolver     *net.Resolver
	mu           sync.Mutex
	closed       bool
	active       map[net.Conn]struct{}
	queries      []DNSQuery
	errs         []error
	wg           sync.WaitGroup
	closeOnce    sync.Once
	frameOnce    sync.Once
	frameStarted chan struct{} // first length prefix read, before the body read
}

func newDNSProducer(name string) (*dnsProducer, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &dnsProducer{name: name, listener: ln, active: make(map[net.Conn]struct{}), frameStarted: make(chan struct{})}
	p.resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		// A real TCPConn selects stream framing even when Resolver requests UDP.
		return (&net.Dialer{}).DialContext(ctx, "tcp4", ln.Addr().String())
	}}
	p.wg.Add(1)
	go p.accept()
	return p, nil
}

func (p *dnsProducer) accept() {
	defer p.wg.Done()
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			p.recordIO("accept", err)
			return
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = conn.Close()
			return
		}
		p.active[conn] = struct{}{}
		p.wg.Add(1)
		p.mu.Unlock()
		go p.handle(conn)
	}
}

func (p *dnsProducer) recordIO(op string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed && (errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		return
	}
	p.errs = append(p.errs, fmt.Errorf("DNS %s: %w", op, err))
}

func (p *dnsProducer) recordError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs = append(p.errs, err)
}

func (p *dnsProducer) handle(conn net.Conn) {
	defer p.wg.Done()
	defer func() {
		_ = conn.Close()
		p.mu.Lock()
		delete(p.active, conn)
		p.mu.Unlock()
	}()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		p.recordIO("deadline", err)
		return
	}
	var prefix [2]byte
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		p.recordIO("length", err)
		return
	}
	p.frameOnce.Do(func() { close(p.frameStarted) })
	body := make([]byte, int(binary.BigEndian.Uint16(prefix[:])))
	if _, err := io.ReadFull(conn, body); err != nil {
		p.recordIO("body", err)
		return
	}
	var request dnsmessage.Message
	if err := request.Unpack(body); err != nil {
		p.recordError(fmt.Errorf("DNS malformed message: %w", err))
		return
	}
	// Unpack accepts EDNS0 and other additional records; they need no reply.
	if request.Response || len(request.Questions) != 1 {
		p.recordError(fmt.Errorf("DNS request: response=%v questions=%d, want one question", request.Response, len(request.Questions)))
		return
	}
	q := request.Questions[0]
	p.mu.Lock()
	p.queries = append(p.queries, DNSQuery{Name: q.Name.String(), Type: q.Type, Class: q.Class})
	p.mu.Unlock()
	if q.Name.String() != p.name || q.Class != dnsmessage.ClassINET || (q.Type != dnsmessage.TypeA && q.Type != dnsmessage.TypeAAAA) {
		p.recordError(fmt.Errorf("DNS unexpected question: %s %s %s", q.Name.String(), q.Class, q.Type))
		return
	}
	response := dnsmessage.Message{
		Header: dnsmessage.Header{ID: request.ID, Response: true, OpCode: request.OpCode,
			Authoritative: true, RecursionDesired: request.RecursionDesired, RecursionAvailable: true,
			RCode: dnsmessage.RCodeNameError},
		Questions: request.Questions,
	}
	reply, err := response.Pack()
	if err != nil {
		p.recordError(fmt.Errorf("DNS pack response: %w", err))
		return
	}
	binary.BigEndian.PutUint16(prefix[:], uint16(len(reply)))
	frame := append(prefix[:], reply...)
	if _, err := io.Copy(conn, bytes.NewReader(frame)); err != nil {
		p.recordIO("reply", err)
	}
}

func (p *dnsProducer) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		sockets := make([]net.Conn, 0, len(p.active))
		for conn := range p.active {
			sockets = append(sockets, conn)
		}
		p.mu.Unlock()
		_ = p.listener.Close()
		for _, conn := range sockets {
			_ = conn.Close()
		}
		p.wg.Wait()
	})
}

func (p *dnsProducer) snapshot() ([]DNSQuery, []error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]DNSQuery(nil), p.queries...), append([]error(nil), p.errs...)
}

// DNSName returns the exact absolute name owned by this fixture.
func (f *Fixture) DNSName() string { return f.invalid }

func (f *Fixture) dnsProducer() *dnsProducer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dns
}

// DNSQueries returns a copy of the decoded wire questions so far.
func (f *Fixture) DNSQueries() []DNSQuery {
	if p := f.dnsProducer(); p != nil {
		q, _ := p.snapshot()
		return q
	}
	return nil
}

// RequireNoDNSErrors reports producer errors on the calling test goroutine.
// Cleanup checks again after joining workers, including any late errors.
func (f *Fixture) RequireNoDNSErrors(t testing.TB) {
	t.Helper()
	if p := f.dnsProducer(); p != nil {
		_, errs := p.snapshot()
		for _, err := range errs {
			t.Errorf("DNS producer: %v", err)
		}
	}
}
