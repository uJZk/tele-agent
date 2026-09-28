package connectproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// connect serves a CONNECT request: it reaches the target first, so that a
// failure can still be reported as an HTTP status, then takes over the
// client connection and splices the two.
func (h *handler) connect(w http.ResponseWriter, r *http.Request) {
	addr := r.URL.Host
	if err := checkTunnelAddr(addr); err != nil {
		http.Error(w, "tele proxy: "+err.Error(), http.StatusBadRequest)
		return
	}
	target, fromTarget, err := h.srv.dialTunnel(r.Context(), addr, r.Header.Get("User-Agent"))
	if err != nil {
		h.log.Debug("proxy tunnel failed", "target", addr, "err", err)
		http.Error(w, "tele proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	client, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		_ = target.Close()
		h.log.Warn("proxy tunnel: take over client connection", "err", err)
		http.Error(w, "tele proxy: cannot take over the connection", http.StatusInternalServerError)
		return
	}
	// http.Server may have left a read deadline from header parsing.
	_ = client.SetDeadline(time.Time{})
	// Bytes the client sent right after its request (an eager TLS
	// ClientHello) are already buffered by http.Server.
	fromClient := buffered(brw.Reader)
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		_ = client.Close()
		_ = target.Close()
		return
	}
	h.log.Debug("proxy tunnel open", "target", addr)
	splice(r.Context(), client, fromClient, target, fromTarget)
	h.log.Debug("proxy tunnel closed", "target", addr)
}

// checkTunnelAddr validates a CONNECT authority, host:port.
func checkTunnelAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return fmt.Errorf("CONNECT target %q is not host:port", addr)
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return fmt.Errorf("CONNECT target %q has an invalid port", addr)
	}
	return nil
}

// dialTunnel connects to addr directly or through the upstream proxy. The
// returned bytes arrived from the target together with the upstream
// proxy's response and must be delivered before anything read from the
// connection.
func (s *Server) dialTunnel(ctx context.Context, addr, userAgent string) (net.Conn, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	// CONNECT carries TLS in practice, so the https proxy setting applies.
	proxy, err := s.upstreamFor(&url.URL{Scheme: "https", Host: addr})
	if err != nil {
		return nil, nil, err
	}
	if proxy == nil {
		c, err := s.dial(ctx, "tcp", addr)
		return c, nil, err
	}
	c, pending, err := s.connectVia(ctx, proxy, addr, userAgent)
	if err != nil {
		return nil, nil, fmt.Errorf("upstream proxy %s: %w", describe(proxy), err)
	}
	return c, pending, nil
}

// connectVia opens a tunnel to addr through an upstream proxy.
func (s *Server) connectVia(ctx context.Context, proxy *url.URL, addr, userAgent string) (net.Conn, []byte, error) {
	conn, err := s.dialUpstream(ctx, proxy)
	if err != nil {
		return nil, nil, err
	}
	// The CONNECT exchange below has no context of its own; an expired
	// deadline interrupts it when ctx ends, and conn is then discarded, so
	// the deadline never needs clearing.
	stop := afterFuncSync(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	pending, err := upstreamConnect(conn, proxy, addr, userAgent)
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, pending, nil
}

// dialUpstream connects to an upstream proxy, with TLS for https.
func (s *Server) dialUpstream(ctx context.Context, proxy *url.URL) (net.Conn, error) {
	port := proxy.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[proxy.Scheme]
	}
	conn, err := s.dial(ctx, "tcp", net.JoinHostPort(proxy.Hostname(), port))
	if err != nil || proxy.Scheme != "https" {
		return conn, err
	}
	cfg := s.tlsConfig()
	if cfg.ServerName == "" {
		cfg.ServerName = proxy.Hostname()
	}
	tc := tls.Client(conn, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tc, nil
}

// upstreamConnect sends CONNECT addr on conn and reads the reply.
func upstreamConnect(conn net.Conn, proxy *url.URL, addr, userAgent string) ([]byte, error) {
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		// Pass the client's User-Agent on: proxy policies may match it.
		// An empty value suppresses Go's default.
		Header: http.Header{"User-Agent": {userAgent}},
	}
	if u := proxy.User; u != nil {
		pass, _ := u.Password()
		req.Header.Set("Proxy-Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+pass)))
	}
	if err := req.Write(conn); err != nil {
		return nil, err
	}
	// Bound the response header; tunnel bytes are read from conn directly
	// afterwards, so only this reader is limited.
	lr := &io.LimitedReader{R: conn, N: maxHeaderBytes}
	br := bufio.NewReader(lr)
	// Any 2xx switches to tunnel mode and its body framing headers must be
	// ignored (RFC 9110 section 9.3.6): what follows the header is tunnel
	// data, so the body is never read or closed. Otherwise conn is closed.
	resp, err := http.ReadResponse(br, req) //nolint:bodyclose // see above
	if err != nil {
		if lr.N == 0 {
			return nil, errors.New("CONNECT response header too large")
		}
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("CONNECT %s refused: %s", addr, resp.Status)
	}
	return buffered(br), nil
}

// buffered returns a copy of what br holds, without reading more.
func buffered(br *bufio.Reader) []byte {
	b, _ := br.Peek(br.Buffered()) // cannot fail within Buffered
	return bytes.Clone(b)
}

// splice relays between client and target until both directions end, then
// closes both. It first delivers fromClient to the target and fromTarget to
// the client. An EOF in one direction is passed on as a half-close so that
// the other direction can finish; an error in either direction, or the end
// of ctx, closes both at once.
func splice(ctx context.Context, client net.Conn, fromClient []byte, target net.Conn, fromTarget []byte) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = client.Close()
			_ = target.Close()
		})
	}
	stop := afterFuncSync(ctx, closeBoth)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := relay(target, client, fromClient); err != nil {
			closeBoth()
		}
	}()
	if err := relay(client, target, fromTarget); err != nil {
		closeBoth()
	}
	wg.Wait()
	stop()
	closeBoth()
}

// relay writes pending to dst, copies src to dst until EOF, then
// half-closes dst.
func relay(dst, src net.Conn, pending []byte) error {
	if len(pending) > 0 {
		if _, err := dst.Write(pending); err != nil {
			return err
		}
	}
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.New("connection cannot half-close")
}

// afterFuncSync runs f when ctx ends unless the returned stop is called
// first. stop reports whether it prevented f; if f had already started,
// stop waits for it to finish, so f never outlives stop.
func afterFuncSync(ctx context.Context, f func()) (stop func() bool) {
	done := make(chan struct{}) // closed by the AfterFunc callback
	cancel := context.AfterFunc(ctx, func() {
		defer close(done)
		f()
	})
	return func() bool {
		if cancel() {
			return true
		}
		<-done
		return false
	}
}
