// Package connectproxy is the local HTTP proxy that carries Claude's
// outbound traffic: API requests, WebFetch, OAuth refresh.
//
// Claude runs in the remote view, where /etc/resolv.conf and /etc/hosts are
// the remote host's, so it must not resolve names itself. Session main sets
// HTTPS_PROXY and HTTP_PROXY to this proxy, which runs in session main and
// therefore resolves and connects in the local view; the user's own proxy,
// if any, is chained behind it (docs/filesystem.md "本地集合",
// docs/claude-code.md "注入的环境").
//
// The proxy serves CONNECT tunnels and absolute-form http:// requests.
// Targets on the loopback interface are always reached directly: through an
// upstream proxy they would land on the proxy's host (docs/exec.md
// "端口转发").
//
// Every forwarded request carries a Via entry whose pseudonym is unique to
// one Serve call. A request that arrives with it has looped back, typically
// because the upstream proxy setting names this proxy; it is refused with
// 508, since forwarding it again would recurse until the process runs out
// of file descriptors.
package connectproxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// maxHeaderBytes bounds the header block of client requests and of
	// upstream CONNECT responses.
	maxHeaderBytes = 64 << 10
	// defaultHeaderTimeout bounds reading a request line and its headers.
	// Clients are local, so a slow header is a stuck or hostile client.
	defaultHeaderTimeout = 30 * time.Second
	// idleTimeout closes keep-alive connections without a request.
	idleTimeout = 2 * time.Minute
	// dialTimeout bounds reaching a target: the TCP connect, and with an
	// upstream proxy also its TLS handshake and CONNECT exchange.
	dialTimeout = 30 * time.Second
)

// Server is an HTTP proxy. The zero value proxies without authentication,
// directly, with a default dialer.
type Server struct {
	// Token, if set, is required as the password of Basic
	// Proxy-Authorization; the user name is ignored. Clients get it from
	// the userinfo of their proxy URL.
	Token string
	// Upstream chooses the proxy for a target URL (scheme "https" for
	// CONNECT, "http" for plain requests); nil or a nil result means
	// direct. Supported upstream schemes are http and https, with
	// credentials taken from the URL's userinfo. See UpstreamFromEnv.
	Upstream func(*url.URL) (*url.URL, error)
	// Dial connects to targets and upstream proxies; nil means a
	// net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Logger receives diagnostics; nil discards them.
	Logger *slog.Logger

	// headerTimeout overrides defaultHeaderTimeout in tests.
	headerTimeout time.Duration
	// upstreamTLS is the TLS configuration for https upstream proxies;
	// nil means system roots. Set by tests.
	upstreamTLS *tls.Config
}

// Serve accepts proxy connections on ln until ctx is done or ln fails, and
// closes ln.
//
// When ctx is done, Serve stops at once rather than draining: listeners and
// connections are closed, tunnels torn down and upstream requests
// canceled. The proxy only outlives its clients at the end of a session,
// when nothing is left to drain. Serve returns after every handler has
// finished; it returns nil after ctx is done and the listener error
// otherwise.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	log := s.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	tr := s.transport()
	defer tr.CloseIdleConnections()

	h := &handler{srv: s, log: log, pseudonym: "tele-" + rand.Text()}
	h.rp = &httputil.ReverseProxy{
		// The absolute-form URL and the Host header are forwarded as
		// received; ReverseProxy strips hop-by-hop headers, including
		// Proxy-Authorization, and never adds X-Forwarded-* with Rewrite.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.Header.Add("Via", h.viaEntry(pr.In))
		},
		Transport: tr,
		// Forward bytes as they arrive: streamed responses must not stall.
		FlushInterval:  -1,
		ModifyResponse: rejectProxyAuth,
		ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelDebug),
		ErrorHandler:   h.proxyError,
	}
	hs := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: s.headerTimeoutOrDefault(),
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		// Request contexts end when Serve stops; tunnels and upstream
		// requests watch them.
		BaseContext: func(net.Listener) context.Context { return ctx },
		ErrorLog:    slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}

	errc := make(chan error, 1) // written once by the serving goroutine
	go func() { errc <- hs.Serve(ln) }()
	var serveErr error // nil: stopped by ctx; hs.Serve never returns nil
	select {
	case serveErr = <-errc:
	case <-ctx.Done():
	}
	cancel()
	// Close does not wait for handlers and does not know hijacked
	// connections; canceling ctx ends those, and h.active waits for them.
	_ = hs.Close()
	if serveErr == nil {
		<-errc
	}
	h.active.closeAndWait()
	if serveErr == nil {
		return nil
	}
	return fmt.Errorf("connectproxy: serve: %w", serveErr)
}

func (s *Server) headerTimeoutOrDefault() time.Duration {
	if s.headerTimeout > 0 {
		return s.headerTimeout
	}
	return defaultHeaderTimeout
}

func (s *Server) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if s.Dial != nil {
		return s.Dial(ctx, network, addr)
	}
	d := net.Dialer{Timeout: dialTimeout}
	return d.DialContext(ctx, network, addr)
}

// transport forwards plain HTTP requests. It is owned by one Serve call.
func (s *Server) transport() *http.Transport {
	return &http.Transport{
		Proxy:       func(r *http.Request) (*url.URL, error) { return s.upstreamFor(r.URL) },
		DialContext: s.dial,
		// Only used for https upstream proxies: targets are http://.
		TLSClientConfig:        s.tlsConfig(),
		TLSHandshakeTimeout:    dialTimeout,
		MaxResponseHeaderBytes: maxHeaderBytes,
		IdleConnTimeout:        idleTimeout,
		// A proxy must pass Content-Encoding through, not negotiate its
		// own and decode it.
		DisableCompression: true,
	}
}

func (s *Server) tlsConfig() *tls.Config {
	if s.upstreamTLS != nil {
		return s.upstreamTLS.Clone()
	}
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

// authorized checks Proxy-Authorization against Token in constant time.
func (s *Server) authorized(r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	pass, ok := basicPassword(r.Header.Get("Proxy-Authorization"))
	return ok && subtle.ConstantTimeCompare([]byte(pass), []byte(s.Token)) == 1
}

// basicPassword extracts the password from a Basic credentials header.
func basicPassword(h string) (string, bool) {
	scheme, cred, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cred))
	if err != nil {
		return "", false
	}
	_, pass, ok := strings.Cut(string(raw), ":")
	return pass, ok
}

type handler struct {
	srv *Server
	log *slog.Logger
	rp  *httputil.ReverseProxy
	// pseudonym identifies this proxy in Via; random, so that chained tele
	// proxies do not mistake each other for a loop.
	pseudonym string
	active    handlerGroup
}

// loopAdvice explains a request loop to the user.
const loopAdvice = "the upstream proxy setting (HTTPS_PROXY, HTTP_PROXY) leads back to this proxy; " +
	"point it at the real upstream proxy or unset it"

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.active.enter() {
		http.Error(w, "tele proxy: shutting down", http.StatusServiceUnavailable)
		return
	}
	defer h.active.leave()

	// Before authentication: a looped request carries the credentials of
	// the upstream setting, which need not match Token.
	if h.looped(r) {
		h.log.Warn("proxy request loop refused")
		http.Error(w, "tele proxy: request loop: "+loopAdvice, http.StatusLoopDetected)
		return
	}
	if !h.srv.authorized(r) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="tele"`)
		http.Error(w, "tele proxy: proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	switch {
	case r.Method == http.MethodConnect:
		h.connect(w, r)
	case r.URL.Scheme == "http" && r.URL.Host != "":
		h.log.Debug("proxy http request", "host", r.URL.Host)
		h.rp.ServeHTTP(w, r)
	default:
		http.Error(w, "tele proxy: only CONNECT and absolute-form http:// requests are proxied",
			http.StatusBadRequest)
	}
}

// viaEntry is the Via entry this proxy adds when forwarding r (RFC 9110
// section 7.6.3).
func (h *handler) viaEntry(r *http.Request) string {
	return fmt.Sprintf("%d.%d %s", r.ProtoMajor, r.ProtoMinor, h.pseudonym)
}

// looped reports whether r has already passed through this proxy. The
// pseudonym is matched anywhere in Via because intermediaries may rewrite
// the protocol part or merge entries.
func (h *handler) looped(r *http.Request) bool {
	for _, v := range r.Header.Values("Via") {
		if strings.Contains(v, h.pseudonym) {
			return true
		}
	}
	return false
}

// rejectProxyAuth turns a 407 into an error. The client has passed this
// proxy's own check, so the 407 comes from the upstream proxy; relayed as
// is, it would read as a rejection of the client's credentials.
func rejectProxyAuth(resp *http.Response) error {
	if resp.StatusCode == http.StatusProxyAuthRequired {
		return fmt.Errorf("upstream proxy refused the request: %s", resp.Status)
	}
	return nil
}

func (h *handler) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	h.log.Debug("proxy http request failed", "host", r.URL.Host, "err", err)
	http.Error(w, "tele proxy: "+err.Error(), http.StatusBadGateway)
}

// handlerGroup tracks running handlers so that Serve can wait for them.
type handlerGroup struct {
	mu     sync.Mutex
	closed bool // guarded by mu; once set, no handler starts
	wg     sync.WaitGroup
}

// enter registers a handler; false means the server is stopping.
func (g *handlerGroup) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.wg.Add(1)
	return true
}

func (g *handlerGroup) leave() { g.wg.Done() }

// closeAndWait refuses new handlers and waits for running ones.
func (g *handlerGroup) closeAndWait() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.wg.Wait()
}
