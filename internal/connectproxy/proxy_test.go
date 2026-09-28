package connectproxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// ioTimeout bounds every blocking step of a test.
const ioTimeout = 10 * time.Second

// dialer maps made-up host:port names to real listeners, so that targets
// are not loopback addresses (which are never sent upstream), and records
// what was dialed.
type dialer struct {
	hosts  map[string]string // read-only after construction
	mu     sync.Mutex
	dialed []string // guarded by mu
}

func newDialer(hosts map[string]string) *dialer { return &dialer{hosts: hosts} }

func (d *dialer) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	d.mu.Unlock()
	if to, ok := d.hosts[addr]; ok {
		addr = to
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, addr)
}

func (d *dialer) Dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.dialed)
}

// serveOn runs s on ln until the test ends.
func serveOn(t *testing.T, s *Server, ln net.Listener) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
}

// serve runs s on a loopback listener until the test ends.
func serve(t *testing.T, s *Server) string {
	t.Helper()
	ln := listen(t)
	serveOn(t, s, ln)
	return ln.Addr().String()
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// startTCP runs handle on every connection to a loopback listener until
// the test ends; handle must return once its peer closes.
func startTCP(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln := listen(t)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(ioTimeout))
				handle(c)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return ln.Addr().String()
}

func echo(c net.Conn) { _, _ = io.Copy(c, c) }

// replyAfterEOF answers only once the client half-closed.
func replyAfterEOF(c net.Conn) {
	b, err := io.ReadAll(c)
	if err != nil {
		return
	}
	_, _ = c.Write(append([]byte("got:"), b...))
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// rawConn is a client connection to the proxy.
type rawConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *rawConn) Read(p []byte) (int, error) { return c.br.Read(p) }

func dialRaw(t *testing.T, addr string) *rawConn {
	t.Helper()
	d := net.Dialer{Timeout: ioTimeout}
	c, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(ioTimeout))
	return &rawConn{Conn: c, br: bufio.NewReader(c)}
}

// send writes raw and reads the response header, leaving the body in
// resp.Body.
func (c *rawConn) send(t *testing.T, raw string) *http.Response {
	t.Helper()
	if _, err := io.WriteString(c.Conn, raw); err != nil {
		t.Fatal(err)
	}
	method, _, _ := strings.Cut(raw, " ")
	resp, err := http.ReadResponse(c.br, &http.Request{Method: method})
	if err != nil {
		t.Fatalf("read response to %q: %v", raw, err)
	}
	return resp
}

// reply is a response read in full.
type reply struct {
	Status     string
	StatusCode int
	Header     http.Header
	Body       string
}

// roundTrip writes raw and reads the whole response. After a successful
// CONNECT the connection carries the tunnel.
func (c *rawConn) roundTrip(t *testing.T, raw string) reply {
	t.Helper()
	resp := c.send(t, raw)
	// Closing does not drain: without Content-Length the body is unbounded
	// and marked closing.
	defer resp.Body.Close()
	r := reply{Status: resp.Status, StatusCode: resp.StatusCode, Header: resp.Header}
	if resp.Request.Method == http.MethodConnect && resp.StatusCode/100 == 2 {
		return r // net/http would present the tunnel as the body
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	r.Body = string(b)
	return r
}

func connectReq(target, auth string) string {
	s := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if auth != "" {
		s += "Proxy-Authorization: " + auth + "\r\n"
	}
	return s + "\r\n"
}

// tunnel opens a CONNECT tunnel through the proxy at addr.
func tunnel(t *testing.T, addr, target, auth string) *rawConn {
	t.Helper()
	c := dialRaw(t, addr)
	resp := c.roundTrip(t, connectReq(target, auth))
	if msg := resp.Body; resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s: %s: %s", target, resp.Status, msg)
	}
	return c
}

// get fetches u with c.
func get(t *testing.T, c *http.Client, u string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Do(req)
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// proxyClient is an HTTP client using the proxy at proxyURL and trusting
// roots.
func proxyClient(t *testing.T, proxyURL string, roots *x509.CertPool) *http.Client {
	t.Helper()
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{
		Proxy:           http.ProxyURL(u),
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: ioTimeout}
}

// selfSigned returns a certificate for 127.0.0.1 and a pool trusting it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "upstream proxy"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// origins starts a TLS and a plain HTTP server. The TLS certificate is
// valid for example.com, so clients verify the origin end to end.
type origins struct {
	tls, plain *httptest.Server
	roots      *x509.CertPool
	hosts      map[string]string
	mu         sync.Mutex
	seen       []http.Header // guarded by mu
}

func startOrigins(t *testing.T) *origins {
	t.Helper()
	o := &origins{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.seen = append(o.seen, r.Header.Clone())
		o.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Connection", "X-Resp-Hop")
		w.Header().Set("X-Resp-Hop", "1")
		w.Header().Set("X-Resp-Keep", "1")
		fmt.Fprintf(w, "%s %s host=%s body=%s", r.Method, r.URL.RequestURI(), r.Host, b)
	})
	o.tls = httptest.NewTLSServer(h)
	t.Cleanup(o.tls.Close)
	o.plain = httptest.NewServer(h)
	t.Cleanup(o.plain.Close)
	o.roots = x509.NewCertPool()
	o.roots.AddCert(o.tls.Certificate())
	o.hosts = map[string]string{
		"example.com:443": o.tls.Listener.Addr().String(),
		"plain.test:80":   o.plain.Listener.Addr().String(),
	}
	return o
}

// last returns the header of the last request an origin received.
func (o *origins) last(t *testing.T) http.Header {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.seen) == 0 {
		t.Fatal("origin saw no request")
	}
	return o.seen[len(o.seen)-1]
}

func TestConnectTLS(t *testing.T) {
	o := startOrigins(t)
	addr := serve(t, &Server{Token: "tok", Dial: newDialer(o.hosts).Dial})
	c := proxyClient(t, "http://claude:tok@"+addr, o.roots)

	resp, err := get(t, c, "https://example.com/path?q=1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := body(t, resp), "GET /path?q=1 host=example.com body="; got != want {
		t.Fatalf("body %q, want %q", got, want)
	}
}

func TestPlainHTTP(t *testing.T) {
	o := startOrigins(t)
	addr := serve(t, &Server{Token: "tok", Dial: newDialer(o.hosts).Dial})

	c := dialRaw(t, addr)
	resp := c.roundTrip(t, "POST http://plain.test/p?q=1 HTTP/1.1\r\n"+
		"Host: plain.test\r\n"+
		"Proxy-Authorization: "+basic("u", "tok")+"\r\n"+
		"Proxy-Connection: keep-alive\r\n"+
		"Connection: X-Hop\r\n"+
		"X-Hop: 1\r\n"+
		"X-Keep: 1\r\n"+
		"Content-Length: 4\r\n\r\nping")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %s", resp.Status)
	}
	if got, want := resp.Body, "POST /p?q=1 host=plain.test body=ping"; got != want {
		t.Fatalf("body %q, want %q", got, want)
	}
	if resp.Header.Get("X-Resp-Hop") != "" || resp.Header.Get("X-Resp-Keep") != "1" {
		t.Errorf("response headers not filtered: %v", resp.Header)
	}
	hdr := o.last(t)
	for _, h := range []string{"Proxy-Authorization", "Proxy-Connection", "X-Hop", "X-Forwarded-For"} {
		if v := hdr.Get(h); v != "" {
			t.Errorf("origin saw %s: %q", h, v)
		}
	}
	if hdr.Get("X-Keep") != "1" {
		t.Errorf("origin lost X-Keep: %v", hdr)
	}

	// The connection stays usable for another request.
	again := c.roundTrip(t, "GET http://plain.test/again HTTP/1.1\r\nHost: plain.test\r\n"+
		"Proxy-Authorization: "+basic("u", "tok")+"\r\n\r\n")
	if got := again.Body; !strings.HasPrefix(got, "GET /again") {
		t.Fatalf("second request: %q", got)
	}
}

func TestPlainHTTPStreams(t *testing.T) {
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	t.Cleanup(origin.Close)
	addr := serve(t, &Server{Dial: newDialer(map[string]string{"sse.test:80": origin.Listener.Addr().String()}).Dial})

	c := dialRaw(t, addr)
	resp := c.send(t, "GET http://sse.test/ HTTP/1.1\r\nHost: sse.test\r\n\r\n")
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first event %q, %v; the proxy held back a streamed response", line, err)
	}
	close(release)
	rest, err := io.ReadAll(br)
	if err != nil || string(rest) != "\ndata: second\n\n" {
		t.Fatalf("rest %q, %v", rest, err)
	}
}

func TestAuth(t *testing.T) {
	target := startTCP(t, echo)
	addr := serve(t, &Server{Token: "s3cret"})
	tests := []struct {
		name string
		auth string
		ok   bool
	}{
		{name: "missing"},
		{name: "wrong password", auth: basic("u", "nope")},
		{name: "token as user name", auth: basic("s3cret", "")},
		{name: "prefix of token", auth: basic("u", "s3cre")},
		{name: "bearer", auth: "Bearer s3cret"},
		{name: "bad base64", auth: "Basic !!!"},
		{name: "no colon", auth: "Basic " + base64.StdEncoding.EncodeToString([]byte("s3cret"))},
		{name: "any user", auth: basic("claude", "s3cret"), ok: true},
		{name: "empty user", auth: basic("", "s3cret"), ok: true},
		{name: "scheme case", auth: "bAsIc " + base64.StdEncoding.EncodeToString([]byte("u:s3cret")), ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, raw := range []string{
				connectReq(target, tt.auth),
				"GET http://" + target + "/ HTTP/1.1\r\nHost: " + target + "\r\nProxy-Authorization: " + tt.auth + "\r\n\r\n",
			} {
				c := dialRaw(t, addr)
				resp := c.roundTrip(t, raw)
				if tt.ok {
					if resp.StatusCode == http.StatusProxyAuthRequired {
						t.Fatalf("%q rejected", raw)
					}
					continue
				}
				if resp.StatusCode != http.StatusProxyAuthRequired {
					t.Fatalf("%q: status %s, want 407", raw, resp.Status)
				}
				if got := resp.Header.Get("Proxy-Authenticate"); got != `Basic realm="tele"` {
					t.Fatalf("Proxy-Authenticate %q", got)
				}
			}
		})
	}
}

func TestHalfClose(t *testing.T) {
	target := startTCP(t, replyAfterEOF)
	hosts := map[string]string{"half.test:7": target}
	upAddr := serve(t, &Server{Dial: newDialer(hosts).Dial})
	for name, s := range map[string]*Server{
		"direct": {Dial: newDialer(hosts).Dial},
		"chained": {Upstream: func(*url.URL) (*url.URL, error) {
			return &url.URL{Scheme: "http", Host: upAddr}, nil
		}},
	} {
		t.Run(name, func(t *testing.T) {
			c := tunnel(t, serve(t, s), "half.test:7", "")
			if _, err := io.WriteString(c, "ping"); err != nil {
				t.Fatal(err)
			}
			tc, ok := c.Conn.(*net.TCPConn)
			if !ok {
				t.Fatalf("client connection is %T", c.Conn)
			}
			if err := tc.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(c)
			if err != nil || string(b) != "got:ping" {
				t.Fatalf("read %q, %v; want got:ping then EOF", b, err)
			}
		})
	}
}

func TestPendingBytes(t *testing.T) {
	t.Run("client sends before the reply", func(t *testing.T) {
		target := startTCP(t, echo)
		addr := serve(t, &Server{})
		c := dialRaw(t, addr)
		if _, err := io.WriteString(c.Conn, connectReq(target, "")+"early"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(c.br, &http.Request{Method: http.MethodConnect})
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT: %v, %v", resp, err)
		}
		_ = resp.Body.Close()
		b := make([]byte, 5)
		if _, err := io.ReadFull(c, b); err != nil || string(b) != "early" {
			t.Fatalf("echo %q, %v", b, err)
		}
	})

	for _, reply := range []string{
		"HTTP/1.1 200 OK\r\n\r\nBANNER",
		// Framing headers of a 2xx CONNECT reply must be ignored.
		"HTTP/1.0 200 OK\r\nContent-Length: 2\r\n\r\nBANNER",
		"HTTP/1.1 204 No Content\r\nTransfer-Encoding: chunked\r\n\r\nBANNER",
	} {
		t.Run(strings.SplitN(reply, "\r\n", 2)[0], func(t *testing.T) {
			reqs := make(chan *http.Request, 1)
			up := startTCP(t, func(c net.Conn) {
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				reqs <- req
				if _, err := io.WriteString(c, reply); err != nil {
					return
				}
				_, _ = io.Copy(c, br)
			})
			addr := serve(t, &Server{Upstream: func(*url.URL) (*url.URL, error) {
				return url.Parse("http://us%65r:p%40ss@" + up)
			}})
			c := dialRaw(t, addr)
			resp := c.roundTrip(t, "CONNECT far.test:22 HTTP/1.1\r\nHost: far.test:22\r\nUser-Agent: claude-cli/1\r\n\r\n")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %s", resp.Status)
			}
			b := make([]byte, 6)
			if _, err := io.ReadFull(c, b); err != nil || string(b) != "BANNER" {
				t.Fatalf("banner %q, %v", b, err)
			}
			req := <-reqs
			if req.Method != http.MethodConnect || req.RequestURI != "far.test:22" || req.Host != "far.test:22" {
				t.Errorf("upstream got %s %s Host %s", req.Method, req.RequestURI, req.Host)
			}
			if got := req.Header.Get("Proxy-Authorization"); got != basic("user", "p@ss") {
				t.Errorf("upstream Proxy-Authorization %q", got)
			}
			if got := req.Header.Get("User-Agent"); got != "claude-cli/1" {
				t.Errorf("upstream User-Agent %q", got)
			}
		})
	}
}

func TestUpstreamChain(t *testing.T) {
	o := startOrigins(t)
	upDialer := newDialer(o.hosts)
	upAddr := serve(t, &Server{Token: "up-secret", Dial: upDialer.Dial})
	echoAddr := startTCP(t, echo)

	front := func(t *testing.T, env map[string]string) (string, *dialer) {
		d := newDialer(o.hosts)
		getenv := func(k string) string { return strings.ReplaceAll(env[k], "UP", upAddr) }
		return serve(t, &Server{Token: "tok", Upstream: UpstreamFromEnv(getenv), Dial: d.Dial}), d
	}

	t.Run("tunnel and plain through upstream", func(t *testing.T) {
		addr, d := front(t, map[string]string{
			"HTTPS_PROXY": "http://u:up-secret@UP",
			"http_proxy":  "UP", // scheme-less and lower-case, as users write it
		})
		c := proxyClient(t, "http://x:tok@"+addr, o.roots)
		resp, err := get(t, c, "https://example.com/tls")
		if err != nil {
			t.Fatal(err)
		}
		if got := body(t, resp); !strings.HasPrefix(got, "GET /tls host=example.com") {
			t.Fatalf("tls body %q", got)
		}
		if got := d.Dialed(); !slices.Equal(got, []string{upAddr}) {
			t.Fatalf("front dialed %v, want only the upstream", got)
		}
		if got := upDialer.Dialed(); !slices.Contains(got, "example.com:443") {
			t.Fatalf("upstream dialed %v", got)
		}
	})

	t.Run("plain through authenticated upstream", func(t *testing.T) {
		addr, _ := front(t, map[string]string{"HTTP_PROXY": "http://u:up-secret@UP"})
		c := proxyClient(t, "http://x:tok@"+addr, nil)
		resp, err := get(t, c, "http://plain.test/p")
		if err != nil {
			t.Fatal(err)
		}
		if got := body(t, resp); got != "GET /p host=plain.test body=" {
			t.Fatalf("body %q", got)
		}
		if v := o.last(t).Get("Proxy-Authorization"); v != "" {
			t.Fatalf("origin saw Proxy-Authorization %q", v)
		}
	})

	t.Run("upstream rejects credentials", func(t *testing.T) {
		addr, _ := front(t, map[string]string{
			"HTTPS_PROXY": "http://u:wrong-pw@UP",
			"HTTP_PROXY":  "http://u:wrong-pw@UP",
		})
		c := dialRaw(t, addr)
		resp := c.roundTrip(t, connectReq("example.com:443", basic("", "tok")))
		msg := resp.Body
		if resp.StatusCode != http.StatusBadGateway || !strings.Contains(msg, "407") {
			t.Fatalf("%s: %q, want 502 naming the upstream 407", resp.Status, msg)
		}
		if strings.Contains(msg, "wrong-pw") {
			t.Fatalf("reply leaks the upstream password: %q", msg)
		}
		resp = c.roundTrip(t, "GET http://plain.test/ HTTP/1.1\r\nHost: plain.test\r\nProxy-Authorization: "+basic("", "tok")+"\r\n\r\n")
		if msg := resp.Body; resp.StatusCode != http.StatusBadGateway || !strings.Contains(msg, "407") || strings.Contains(msg, "wrong-pw") {
			t.Fatalf("plain: %s: %q, want 502 naming the upstream 407", resp.Status, msg)
		}
	})

	t.Run("NO_PROXY and loopback go direct", func(t *testing.T) {
		addr, d := front(t, map[string]string{
			"HTTPS_PROXY": "http://u:up-secret@UP",
			"NO_PROXY":    "example.com",
		})
		c := proxyClient(t, "http://x:tok@"+addr, o.roots)
		resp, err := get(t, c, "https://example.com/direct")
		if err != nil {
			t.Fatal(err)
		}
		_ = body(t, resp)
		_, port, _ := net.SplitHostPort(echoAddr)
		for _, target := range []string{echoAddr, "localhost:" + port} {
			_ = tunnel(t, addr, target, basic("", "tok"))
		}
		want := []string{"example.com:443", echoAddr, "localhost:" + port}
		if got := d.Dialed(); !slices.Equal(got, want) {
			t.Fatalf("front dialed %v, want %v", got, want)
		}
	})

	t.Run("loopback never upstream even if Upstream says so", func(t *testing.T) {
		addr := serve(t, &Server{Upstream: func(*url.URL) (*url.URL, error) {
			return url.Parse("http://unreachable.invalid:1")
		}})
		c := tunnel(t, addr, echoAddr, "")
		if _, err := io.WriteString(c, "hi"); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 2)
		if _, err := io.ReadFull(c, b); err != nil || string(b) != "hi" {
			t.Fatalf("echo %q, %v", b, err)
		}
	})
}

func TestUpstreamHTTPS(t *testing.T) {
	o := startOrigins(t)
	cert, pool := selfSigned(t)
	ln := tls.NewListener(listen(t), &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	serveOn(t, &Server{Token: "up", Dial: newDialer(o.hosts).Dial}, ln)
	upURL := &url.URL{Scheme: "https", User: url.UserPassword("u", "up"), Host: ln.Addr().String()}

	addr := serve(t, &Server{
		Upstream:    func(*url.URL) (*url.URL, error) { return upURL, nil },
		upstreamTLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	})
	c := proxyClient(t, "http://"+addr, o.roots)
	for _, u := range []string{"https://example.com/a", "http://plain.test/b"} {
		resp, err := get(t, c, u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		if got := body(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %s %q", u, resp.Status, got)
		}
	}

	// Without the pool, the upstream's certificate is not trusted.
	addr = serve(t, &Server{Upstream: func(*url.URL) (*url.URL, error) { return upURL, nil }})
	resp := dialRaw(t, addr).roundTrip(t, connectReq("example.com:443", ""))
	if msg := resp.Body; resp.StatusCode != http.StatusBadGateway || !strings.Contains(msg, "certificate") {
		t.Fatalf("%s %q, want 502 for an untrusted upstream", resp.Status, msg)
	}
}

func TestUpstreamErrors(t *testing.T) {
	tests := []struct {
		name     string
		upstream func(*url.URL) (*url.URL, error)
		want     string
	}{
		{
			name:     "socks",
			upstream: func(*url.URL) (*url.URL, error) { return url.Parse("socks5://u:hidden@127.0.0.1:1080") },
			want:     `scheme "socks5" is not supported`,
		},
		{
			name: "unparseable setting",
			upstream: UpstreamFromEnv(func(k string) string {
				return map[string]string{"HTTPS_PROXY": "http://u:hidden@[bad", "HTTP_PROXY": "http://u:hidden@[bad"}[k]
			}),
			want: "upstream proxy setting is invalid",
		},
		{
			name:     "lookup error",
			upstream: func(*url.URL) (*url.URL, error) { return nil, errors.New("hidden") },
			want:     "upstream proxy setting is invalid",
		},
		{
			name:     "path",
			upstream: func(*url.URL) (*url.URL, error) { return url.Parse("http://proxy.test/u:hidden") },
			want:     "upstream proxy setting is invalid",
		},
		{
			name:     "no host",
			upstream: func(*url.URL) (*url.URL, error) { return &url.URL{Scheme: "http"}, nil },
			want:     "missing host",
		},
		{
			name:     "unreachable",
			upstream: func(*url.URL) (*url.URL, error) { return url.Parse("http://127.0.0.1:1") },
			want:     "connection refused",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := serve(t, &Server{Upstream: tt.upstream})
			c := dialRaw(t, addr)
			for _, raw := range []string{
				connectReq("far.test:443", ""),
				"GET http://far.test/ HTTP/1.1\r\nHost: far.test\r\n\r\n",
			} {
				resp := c.roundTrip(t, raw)
				msg := resp.Body
				if resp.StatusCode != http.StatusBadGateway || !strings.Contains(msg, tt.want) {
					t.Errorf("%q: %s %q, want 502 containing %q", raw, resp.Status, msg, tt.want)
				}
				if strings.Contains(msg, "hidden") {
					t.Errorf("reply leaks a secret: %q", msg)
				}
			}
		})
	}
}

func TestBadRequests(t *testing.T) {
	closed := listen(t)
	refused := closed.Addr().String()
	_ = closed.Close()
	addr := serve(t, &Server{Dial: newDialer(map[string]string{"refused.test:1": refused}).Dial})
	tests := []struct {
		raw  string
		want int
		msg  string
	}{
		{raw: "GET / HTTP/1.1\r\nHost: x\r\n\r\n", want: 400, msg: "only CONNECT"},
		{raw: "GET https://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n", want: 400, msg: "only CONNECT"},
		{raw: "CONNECT example.com HTTP/1.1\r\n\r\n", want: 400, msg: "not host:port"},
		{raw: "CONNECT :443 HTTP/1.1\r\n\r\n", want: 400, msg: "not host:port"},
		{raw: "CONNECT example.com:0 HTTP/1.1\r\n\r\n", want: 400, msg: "invalid port"},
		{raw: "CONNECT example.com:65536 HTTP/1.1\r\n\r\n", want: 400, msg: "invalid port"},
		{raw: "CONNECT example.com:https HTTP/1.1\r\n\r\n", want: 400}, // rejected by net/http
		{raw: "CONNECT refused.test:1 HTTP/1.1\r\n\r\n", want: 502, msg: "connection refused"},
		{raw: "NONSENSE\r\n\r\n", want: 400},
		{raw: "GET http://x/ HTTP/1.1\r\nHost: x\r\nX-Big: " + strings.Repeat("a", maxHeaderBytes+8192) + "\r\n\r\n", want: 431},
	}
	for _, tt := range tests {
		resp := dialRaw(t, addr).roundTrip(t, tt.raw)
		msg := resp.Body
		if resp.StatusCode != tt.want || !strings.Contains(msg, tt.msg) {
			t.Errorf("%.60q: %s %q, want %d containing %q", tt.raw, resp.Status, msg, tt.want, tt.msg)
		}
	}
}

func TestHeaderTimeout(t *testing.T) {
	target := startTCP(t, echo)
	s := &Server{headerTimeout: 50 * time.Millisecond}
	addr := serve(t, s)

	stalled := dialRaw(t, addr)
	if _, err := io.WriteString(stalled.Conn, "GET http://x/ HTTP/1.1\r\n"); err != nil {
		t.Fatal(err)
	}
	if n, err := stalled.Read(make([]byte, 1)); n != 0 || err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled request: read %d, %v; want the proxy to hang up", n, err)
	}

	// An established tunnel does not inherit the header deadline.
	c := tunnel(t, addr, target, "")
	<-time.After(4 * s.headerTimeout) // let the deadline pass; the property under test
	if _, err := io.WriteString(c, "late"); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "late" {
		t.Fatalf("tunnel after header timeout: %q, %v", b, err)
	}
}

func TestShutdown(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Run("scenario", func(t *testing.T) {
		target := startTCP(t, echo)
		started := make(chan struct{}, 1)
		origin := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			started <- struct{}{}
			<-r.Context().Done() // until the proxy abandons the request
		}))
		t.Cleanup(origin.Close)

		ln := listen(t)
		addr := ln.Addr().String()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- (&Server{}).Serve(ctx, ln) }()

		idle := dialRaw(t, addr)
		tun := tunnel(t, addr, target, "")
		if _, err := io.WriteString(tun, "x"); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(tun, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		inflight := dialRaw(t, addr)
		if _, err := io.WriteString(inflight.Conn, "GET http://"+origin.Listener.Addr().String()+"/ HTTP/1.1\r\nHost: o\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		<-started

		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Serve after cancel: %v", err)
			}
		case <-time.After(ioTimeout):
			t.Fatal("Serve did not return after cancel")
		}
		for name, c := range map[string]*rawConn{"idle": idle, "tunnel": tun, "in-flight": inflight} {
			if _, err := io.ReadAll(c); err != nil && !errors.Is(err, net.ErrClosed) && !isReset(err) {
				t.Errorf("%s connection: %v, want it closed", name, err)
			}
		}
		d := net.Dialer{Timeout: ioTimeout}
		if c, err := d.DialContext(t.Context(), "tcp", addr); err == nil {
			_ = c.Close()
			t.Error("listener still accepts after Serve returned")
		}
	})
	goleak.VerifyNone(t, ignore)
}

func isReset(err error) bool { return errors.Is(err, unix.ECONNRESET) }

func TestServeListenerError(t *testing.T) {
	ln := listen(t)
	_ = ln.Close()
	if err := (&Server{}).Serve(context.Background(), ln); err == nil {
		t.Fatal("Serve on a closed listener returned nil")
	}
}

func TestUpstreamMisbehaves(t *testing.T) {
	t.Run("oversized reply header", func(t *testing.T) {
		up := startTCP(t, func(c net.Conn) {
			if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
				return
			}
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nX-Big: "+strings.Repeat("a", 2*maxHeaderBytes)+"\r\n\r\n")
		})
		addr := serve(t, &Server{Upstream: func(*url.URL) (*url.URL, error) { return url.Parse("http://" + up) }})
		resp := dialRaw(t, addr).roundTrip(t, connectReq("far.test:443", ""))
		if resp.StatusCode != http.StatusBadGateway || !strings.Contains(resp.Body, "too large") {
			t.Fatalf("%s %q, want 502 for an oversized upstream header", resp.Status, resp.Body)
		}
	})

	t.Run("silent upstream during shutdown", func(t *testing.T) {
		got := make(chan struct{})
		up := startTCP(t, func(c net.Conn) {
			if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
				return
			}
			close(got)
			_, _ = io.Copy(io.Discard, c) // never answer; wait for the proxy to hang up
		})
		ln := listen(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		s := &Server{Upstream: func(*url.URL) (*url.URL, error) { return url.Parse("http://" + up) }}
		go func() { done <- s.Serve(ctx, ln) }()
		c := dialRaw(t, ln.Addr().String())
		if _, err := io.WriteString(c.Conn, connectReq("far.test:443", "")); err != nil {
			t.Fatal(err)
		}
		<-got
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(ioTimeout):
			t.Fatal("Serve blocked on an upstream that does not answer")
		}
	})
}
