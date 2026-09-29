// Package faultnet breaks transport connections on demand, for tests of
// the resumable session layer and what runs over it
// (docs/coding-standards.md "测试": 故障注入).
package faultnet

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// Proxy sits between a client and a server on unix sockets and breaks the
// transport on demand: it cuts connections, stops forwarding without
// closing them (a half-open connection), refuses new ones, or sends them
// to another server.
type Proxy struct {
	ln     net.Listener
	target atomic.Pointer[string] // the server's address

	mu     sync.Mutex // guards pairs, holed and refuse
	pairs  []*pair
	holed  []*pair // closed only when the test ends
	refuse bool
	wg     sync.WaitGroup
}

type pair struct {
	a, b net.Conn
	hole chan struct{} // closed to stop forwarding
}

// New starts a proxy to the unix socket target. It stops when the test
// ends.
func New(t testing.TB, target string) *Proxy {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", filepath.Join(t.TempDir(), "proxy.sock"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{ln: ln}
	p.target.Store(&target)
	p.wg.Add(1)
	go p.loop()
	t.Cleanup(func() {
		_ = ln.Close()
		p.Cut()
		p.mu.Lock()
		for _, pr := range p.holed {
			_ = pr.a.Close()
			_ = pr.b.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
	return p
}

func (p *Proxy) loop() {
	defer p.wg.Done()
	for {
		a, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		refuse := p.refuse
		p.mu.Unlock()
		if refuse {
			_ = a.Close()
			continue
		}
		var d net.Dialer
		b, err := d.DialContext(context.Background(), "unix", *p.target.Load())
		if err != nil {
			_ = a.Close()
			continue
		}
		pr := &pair{a: a, b: b, hole: make(chan struct{})}
		p.mu.Lock()
		p.pairs = append(p.pairs, pr)
		p.mu.Unlock()
		p.wg.Add(2)
		go p.copy(pr, a, b)
		go p.copy(pr, b, a)
	}
}

// copy forwards src to dst until either fails or the pair is holed, when
// it keeps reading but drops everything, like a network that lost the
// route without either end noticing.
func (p *Proxy) copy(pr *pair, dst, src net.Conn) {
	defer p.wg.Done()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			select {
			case <-pr.hole:
			default:
				if _, werr := dst.Write(buf[:n]); werr != nil {
					_ = src.Close()
					return
				}
			}
		}
		if err != nil {
			_ = dst.Close()
			return
		}
	}
}

// Addr is the address clients dial.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Cut closes every connection.
func (p *Proxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pr := range p.pairs {
		_ = pr.a.Close()
		_ = pr.b.Close()
	}
	p.pairs = nil
}

// Hole stops forwarding on the existing connections without closing them.
func (p *Proxy) Hole() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pr := range p.pairs {
		close(pr.hole)
	}
	p.holed = append(p.holed, p.pairs...)
	p.pairs = nil
}

// SetRefuse makes the proxy close new connections at once, or stop doing
// so.
func (p *Proxy) SetRefuse(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refuse = on
}

// SetTarget sends new connections to another server.
func (p *Proxy) SetTarget(addr string) { p.target.Store(&addr) }
