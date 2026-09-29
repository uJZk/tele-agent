package claudetest

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/ujzk/tele-agent/internal/connectproxy"
)

// Proxy is tele's CONNECT proxy with every connection sent to one
// address, whatever host the client names, recording the hosts.
type Proxy struct {
	addr string // where the proxy listens

	mu      sync.Mutex // guards targets
	targets []string
}

// NewProxy starts a proxy that sends every connection to addr. A
// non-empty token is required as the password of Proxy-Authorization.
func NewProxy(t testing.TB, addr, token string) *Proxy {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{addr: ln.Addr().String()}
	srv := &connectproxy.Server{
		Token: token,
		Dial: func(ctx context.Context, network, target string) (net.Conn, error) {
			p.mu.Lock()
			p.targets = append(p.targets, target)
			p.mu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, ln)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return p
}

// URL is the proxy URL to pass as HTTPS_PROXY, without credentials.
func (p *Proxy) URL() string { return "http://" + p.addr }

// Addr is the address the proxy listens on.
func (p *Proxy) Addr() string { return p.addr }

// Targets returns the host:port of every connection the proxy made.
func (p *Proxy) Targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}
