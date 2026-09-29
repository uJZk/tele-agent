package claudetest

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Proxy is an HTTP CONNECT proxy that tunnels every request to one
// address, whatever host the client names, and records the hosts.
type Proxy struct {
	srv *httptest.Server
	to  string

	mu      sync.Mutex // guards targets
	targets []string

	wg sync.WaitGroup // tunnels
}

// NewProxy starts a proxy that tunnels to addr.
func NewProxy(t testing.TB, addr string) *Proxy {
	t.Helper()
	p := &Proxy{to: addr}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(func() {
		p.srv.Close()
		p.wg.Wait()
	})
	return p
}

// URL is the proxy URL to pass as HTTPS_PROXY.
func (p *Proxy) URL() string { return p.srv.URL }

// Targets returns the host:port of every CONNECT request so far.
func (p *Proxy) Targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.targets = append(p.targets, r.Host)
	p.mu.Unlock()
	if r.Method != http.MethodConnect {
		http.Error(w, "only CONNECT", http.StatusMethodNotAllowed)
		return
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	up, err := d.DialContext(r.Context(), "tcp", p.to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = up.Close()
		http.Error(w, "no hijacking", http.StatusInternalServerError)
		return
	}
	down, rw, err := hj.Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	if _, err := io.WriteString(down, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		_ = up.Close()
		_ = down.Close()
		return
	}
	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		_, _ = io.Copy(up, rw) // ends when either side closes
		_ = up.Close()
	}()
	go func() {
		defer p.wg.Done()
		_, _ = io.Copy(down, up)
		_ = down.Close()
	}()
}
