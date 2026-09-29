// Package sstransport carries tele's session transports over Shadowsocks
// 2022 (2022-blake3-aes-256-gcm) on TCP (docs/transport.md "SS2022").
//
// The PSK authenticates both ends: a listener hands out only connections
// that passed the SS2022 handshake, and treats every other connection with
// its reject policy. Every connection names the same fixed target,
// tele.internal:1; tele is not a general proxy.
package sstransport

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/database64128/shadowsocks-go/conn"
	"github.com/database64128/shadowsocks-go/netio"
	"github.com/database64128/shadowsocks-go/ss2022"
	"go.uber.org/zap"
)

// PSKLen is the key length of 2022-blake3-aes-256-gcm.
const PSKLen = 32

// Target is the address every tele connection requests. It is an agreement
// between the two ends, never resolved or dialed.
const (
	TargetHost = "tele.internal"
	TargetPort = 1
)

// rejectDrain is how long a connection that failed authentication is read,
// without any reply, before it is closed. An active prober sees the same
// silence as with a server still waiting for data. A variable for tests.
var rejectDrain = 60 * time.Second

// maxRejecting bounds the connections being drained at once; beyond it,
// rejected connections are closed at once so that probes cannot exhaust
// file descriptors. A variable for tests.
var maxRejecting = maxRejectingDefault

const (
	// HandshakeTimeout bounds the SS2022 handshake on both ends.
	HandshakeTimeout    = 15 * time.Second
	maxRejectingDefault = 64
)

// PSK is a pre-shared key. It grants shell access to the target user:
// never log it or put it in an error.
type PSK [PSKLen]byte

// NewPSK returns a random PSK.
func NewPSK() (PSK, error) {
	var k PSK
	if _, err := rand.Read(k[:]); err != nil {
		return PSK{}, err
	}
	return k, nil
}

// ParsePSK decodes a PSK in standard base64, the SS2022 key notation.
func ParsePSK(s string) (PSK, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != PSKLen {
		return PSK{}, fmt.Errorf("invalid PSK: want %d bytes in base64", PSKLen)
	}
	var k PSK
	copy(k[:], b)
	return k, nil
}

// Encode returns k in standard base64.
func (k PSK) Encode() string { return base64.StdEncoding.EncodeToString(k[:]) }

// String hides the key, so that a PSK printed by mistake leaks nothing.
func (PSK) String() string { return "PSK(redacted)" }

// GoString hides the key from %#v as well.
func (PSK) GoString() string { return "sstransport.PSK(redacted)" }

func target() conn.Addr {
	return conn.MustAddrFromDomainPort(TargetHost, TargetPort)
}

// ErrNoResponse reports that the server accepted the TCP connection but
// never answered the handshake. SS2022 gives no other signal for a wrong
// PSK, a clock skew over 30 s or a server that is not tele.
var ErrNoResponse = errors.New("no SS2022 response: wrong PSK, clock skew over 30 s between the hosts, or not a tele server")

// Dial connects to the tele server at addr (host:port) and sends the SS2022
// request header. The server answers only with its first data, so an
// authentication failure shows up on the first Read, as ErrNoResponse.
func Dial(ctx context.Context, addr string, psk PSK) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("port %q: %w", portStr, err)
	}
	server, err := conn.AddrFromHostPort(host, uint16(port))
	if err != nil {
		return nil, err
	}
	cipher, err := ss2022.NewClientCipherConfig(psk[:], nil, false)
	if err != nil {
		return nil, err
	}
	inner := (&netio.TCPClientConfig{
		Network: "tcp",
		Dialer:  conn.TCPConnectSocketOptions{}.Build(),
	}).NewTCPClient()
	client := (&ss2022.StreamClientConfig{
		InnerClient:  inner,
		Addr:         server,
		CipherConfig: cipher,
	}).NewStreamClient()

	ctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()
	c, err := client.DialStream(ctx, target(), nil)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	return &dialedConn{Conn: c, addr: addr}, nil
}

// dialedConn maps a connection that dies before any byte arrived to
// ErrNoResponse, so the user learns the likely causes.
type dialedConn struct {
	netio.Conn
	addr string
	mu   sync.Mutex
	got  bool
}

func (c *dialedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	first := !c.got
	if n > 0 {
		c.got = true
	}
	c.mu.Unlock()
	if err != nil && first && n == 0 {
		// The server stays silent towards a client it cannot
		// authenticate, so the client's own deadline usually ends the
		// wait; EOF means the server gave up first.
		return 0, fmt.Errorf("%s: %w (%w)", c.addr, ErrNoResponse, err)
	}
	return n, err
}

// Listener accepts authenticated SS2022 connections.
type Listener struct {
	tcp    *net.TCPListener
	server *ss2022.StreamServer
	log    *slog.Logger
	conns  chan net.Conn
	done   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
	reject chan struct{} // semaphore of connections being drained
}

// Listen listens on addr (host:port; an empty host means every address)
// for connections authenticated with psk. The listener stops when ctx ends
// or Close is called.
func Listen(ctx context.Context, addr string, psk PSK, log *slog.Logger) (*Listener, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ucc, err := ss2022.NewUserCipherConfig(psk[:], false)
	if err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("listen on %s: not a TCP listener (%T)", addr, ln)
	}
	l := &Listener{
		tcp:    tl,
		log:    log,
		conns:  make(chan net.Conn),
		done:   make(chan struct{}),
		reject: make(chan struct{}, maxRejecting),
	}
	l.server = (&ss2022.StreamServerConfig{
		UserCipherConfig: ucc,
		RejectPolicy:     l.rejectPolicy,
	}).NewStreamServer()
	stop := context.AfterFunc(ctx, func() { _ = l.Close() })
	l.wg.Go(func() {
		defer stop()
		l.acceptLoop()
	})
	return l, nil
}

// Addr returns the listening address.
func (l *Listener) Addr() net.Addr { return l.tcp.Addr() }

// Accept returns the next authenticated connection.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops the listener and waits for its handshakes to end.
// Connections already returned by Accept stay open.
func (l *Listener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.done)
		err = l.tcp.Close()
	})
	l.wg.Wait()
	return err
}

func (l *Listener) acceptLoop() {
	var delay time.Duration
	for {
		c, err := l.tcp.AcceptTCP()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			// Temporary errors such as EMFILE: back off and retry.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			l.log.Warn("accept", "err", err, "retry_in", delay)
			select {
			case <-time.After(delay):
			case <-l.done:
				return
			}
			continue
		}
		delay = 0
		l.wg.Go(func() { l.handshake(c) })
	}
}

func (l *Listener) handshake(c *net.TCPConn) {
	_ = c.SetDeadline(time.Now().Add(HandshakeTimeout))
	// HandleStream applies the reject policy itself when authentication
	// fails; the connection is then already dealt with.
	req, err := l.server.HandleStream(c, zap.NewNop())
	if err != nil {
		l.log.Debug("rejected connection", "remote", c.RemoteAddr().String(), "err", err)
		return
	}
	if !req.Addr.Equals(target()) {
		// Authenticated, so a peer with the PSK but not tele: refuse
		// plainly.
		l.log.Warn("authenticated connection requested an unknown target", "remote", c.RemoteAddr().String())
		_ = req.Abort(conn.DialResult{Code: conn.DialResultCodeEACCES})
		return
	}
	sc, err := req.Proceed()
	if err != nil {
		_ = c.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})
	// The client may have sent the start of the session with its request.
	var nc net.Conn = sc
	if len(req.Payload) > 0 {
		nc = &prefixConn{Conn: sc, prefix: req.Payload}
	}
	select {
	case l.conns <- nc:
	case <-l.done:
		_ = sc.Close()
	}
}

// rejectPolicy reads a connection that failed authentication until
// rejectDrain passes or the peer closes, never answering, then closes it
// (docs/transport.md "SS2022").
func (l *Listener) rejectPolicy(c *net.TCPConn, _ *zap.Logger) {
	select {
	case l.reject <- struct{}{}:
		defer func() { <-l.reject }()
	default:
		_ = c.Close()
		return
	}
	_ = c.SetDeadline(time.Now().Add(rejectDrain))
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-l.done:
			_ = c.Close()
		case <-stop:
		}
	}()
	_, _ = io.Copy(io.Discard, c)
	_ = c.Close()
}

// prefixConn returns prefix before the rest of the connection.
type prefixConn struct {
	netio.Conn
	prefix []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}
