//go:build smoke

package testdb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The proxy matches its armed pattern against the client byte STREAM, not
// against one socket read: a TCP read boundary is not a statement boundary
// (ai-review R1-1). Each case forces the split. It writes one part, waits
// until the target has received it (so the proxy has read and forwarded that
// part alone), and only then writes the next part.

const splitPattern = "for update"

// splitStream is a statement whose pattern is followed by more bytes of the
// same message, as in a real query.
const splitStream = "select id from tickets where id=$1 for update of tickets"

// splitTarget is a loopback server that records every byte the proxy
// forwards to it.
type splitTarget struct {
	addr string
	mu   sync.Mutex
	conn net.Conn
	got  []byte
	eof  chan struct{}
}

func newSplitTarget(t *testing.T) *splitTarget {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &splitTarget{addr: ln.Addr().String(), eof: make(chan struct{})}
	t.Cleanup(func() {
		_ = ln.Close()
		s.mu.Lock()
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.mu.Unlock()
	})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conn = c
		s.mu.Unlock()
		buf := make([]byte, 4096)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.got = append(s.got, buf[:n]...)
				s.mu.Unlock()
			}
			if err != nil {
				close(s.eof)
				return
			}
		}
	}()
	return s
}

func (s *splitTarget) received() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.got...)
}

// waitFor waits until the target holds exactly want.
func (s *splitTarget) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if string(s.received()) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the target holds %q, want %q", s.received(), want)
}

func (s *splitTarget) reply(t *testing.T, b string) {
	t.Helper()
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		t.Fatal("the target has no connection")
	}
	if _, err := c.Write([]byte(b)); err != nil {
		t.Fatal(err)
	}
}

// splitCases cuts splitStream inside the pattern at every offset, plus one
// three-part split and a control with the whole pattern in the last part.
func splitCases() map[string][]string {
	at := strings.Index(splitStream, splitPattern)
	cases := map[string][]string{
		"whole pattern in one read": {splitStream[:at], splitStream[at:]},
		"three reads":               {splitStream[:at+3], splitStream[at+3 : at+7], splitStream[at+7:]},
	}
	for k := 1; k < len(splitPattern); k++ {
		cases[fmt.Sprintf("split after %q", splitPattern[:k])] = []string{splitStream[:at+k], splitStream[at+k:]}
	}
	return cases
}

func fired(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// writeParts writes every part but the last one, each after the target has
// received the one before, and requires that none of them fires the rule.
// held is what the target already holds.
func writeParts(t *testing.T, c net.Conn, target *splitTarget, held string, parts []string, rule <-chan struct{}) {
	t.Helper()
	sent := held
	for _, part := range parts[:len(parts)-1] {
		if _, err := c.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
		sent += part
		target.waitFor(t, sent)
		if fired(rule) {
			t.Fatalf("the rule fired before the pattern was complete, after %q", sent)
		}
	}
	if _, err := c.Write([]byte(parts[len(parts)-1])); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rule:
	case <-time.After(2 * time.Second):
		t.Fatalf("the rule did not fire when %q completed the pattern; the stream was %q", parts[len(parts)-1], sent+parts[len(parts)-1])
	}
}

// For FIN and RST the cut happens before the proxy forwards the bytes that
// complete the pattern: the target never holds the complete pattern, and the
// client meets the cut on its next read.
func TestProxyCutMatchesAcrossSocketReads(t *testing.T) {
	for _, mode := range []CutMode{CutFIN, CutRST} {
		for name, parts := range splitCases() {
			t.Run(string(mode)+"/"+name, func(t *testing.T) {
				target := newSplitTarget(t)
				p := newProxy(t, target.addr)
				rule := p.arm(splitPattern, mode)
				c, err := net.Dial("tcp4", p.addr)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				writeParts(t, c, target, "", parts, rule)
				select {
				case <-target.eof:
				case <-time.After(2 * time.Second):
					t.Fatal("the proxy did not close its server side")
				}
				got := target.received()
				if !strings.HasPrefix(splitStream, string(got)) || bytes.Contains(got, []byte(splitPattern)) {
					t.Fatalf("the target holds %q: the complete pattern was forwarded before the cut", got)
				}
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				_, err = c.Read(make([]byte, 16))
				switch mode {
				case CutFIN:
					if !errors.Is(err, io.EOF) {
						t.Fatalf("client read after a FIN cut = %v, want io.EOF", err)
					}
				case CutRST:
					if !errors.Is(err, syscall.ECONNRESET) {
						t.Fatalf("client read after an RST cut = %v, want ECONNRESET", err)
					}
				}
			})
		}
	}
}

// Withhold forwards every client byte and holds every server reply from the
// match on. A reply sent before the match still reaches the client, so the
// hold starts at the match and not before.
func TestProxyWithholdMatchesAcrossSocketReads(t *testing.T) {
	for name, parts := range splitCases() {
		t.Run(name, func(t *testing.T) {
			target := newSplitTarget(t)
			p := newProxy(t, target.addr)
			rule := p.arm(splitPattern, Withhold)
			c, err := net.Dial("tcp4", p.addr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if _, err := c.Write([]byte(parts[0])); err != nil {
				t.Fatal(err)
			}
			target.waitFor(t, parts[0])
			target.reply(t, "early")
			buf := make([]byte, 16)
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			if n, err := io.ReadFull(c, buf[:len("early")]); err != nil || string(buf[:n]) != "early" {
				t.Fatalf("a reply before the match = %q, %v; want it forwarded", buf[:n], err)
			}
			writeParts(t, c, target, parts[0], parts[1:], rule)
			target.waitFor(t, splitStream)
			target.reply(t, "late")
			_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
			if n, err := c.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("a reply after the match reached the client: %q, %v", buf[:n], err)
			}
		})
	}
}

// Bytes the proxy read before the rule was armed never complete it: neither
// bytes read under no rule, nor bytes read under an earlier rule that was
// replaced before it fired. The same connection then sends the whole pattern,
// which fires: the rule was live all along.
func TestProxyRuleIgnoresBytesReadBeforeItWasArmed(t *testing.T) {
	at := strings.Index(splitStream, splitPattern)
	for _, earlier := range []string{"no rule", "a replaced rule"} {
		t.Run(earlier, func(t *testing.T) {
			target := newSplitTarget(t)
			p := newProxy(t, target.addr)
			c, err := net.Dial("tcp4", p.addr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			var replaced <-chan struct{}
			if earlier == "a replaced rule" {
				replaced = p.arm(splitPattern, CutFIN)
			}
			before := splitStream[:at+5]
			if _, err := c.Write([]byte(before)); err != nil {
				t.Fatal(err)
			}
			target.waitFor(t, before)
			rule := p.arm(splitPattern, CutFIN)
			if _, err := c.Write([]byte(splitStream[at+5:])); err != nil {
				t.Fatal(err)
			}
			target.waitFor(t, splitStream)
			if fired(rule) || (replaced != nil && fired(replaced)) {
				t.Fatal("bytes read before arm completed the pattern")
			}
			writeParts(t, c, target, splitStream, []string{" ", splitPattern}, rule)
		})
	}
}

// The per-connection buffer is bounded: after many bytes without the
// pattern it holds at most len(pattern)-1 of them.
func TestProxyMatchBufferIsBounded(t *testing.T) {
	p := &proxy{}
	p.arm(splitPattern, CutFIN)
	s := &clientStream{}
	for range 100 {
		if p.match(s, []byte(strings.Repeat("select 1 for upd ", 4))) != nil {
			t.Fatal("the rule fired without its pattern")
		}
	}
	if len(s.tail) > len(splitPattern)-1 {
		t.Fatalf("the buffer holds %d bytes, want at most %d", len(s.tail), len(splitPattern)-1)
	}
}
