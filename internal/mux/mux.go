// Package mux multiplexes typed streams over one connection between session
// main and tele server. Each stream starts with a proto.StreamHeader naming
// its kind.
package mux

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ujzk/tele-agent/internal/proto"
)

// HeaderTimeout bounds how long a peer may take to send the header of a
// stream it opened. The header is written together with the stream's
// opening frame, so it only expires for a misbehaving peer.
const HeaderTimeout = time.Minute

// Session is one multiplexed connection.
type Session struct {
	ys *yamux.Session
}

func config() *yamux.Config {
	c := yamux.DefaultConfig()
	// Liveness belongs to the session layer below (heartbeats and resumption,
	// docs/transport.md section 2). yamux must not close the connection
	// while it is paused for a reconnect, so its own keepalive and open
	// timeout are off and its write timeout matches the session lease.
	c.EnableKeepAlive = false
	c.StreamOpenTimeout = 0
	c.ConnectionWriteTimeout = 30 * time.Minute
	// Larger windows keep bulk telefs reads flowing on high-RTT links.
	c.MaxStreamWindowSize = 1 << 20
	c.LogOutput = io.Discard
	return c
}

// Client starts the opening side of a session over conn.
func Client(conn io.ReadWriteCloser) (*Session, error) {
	ys, err := yamux.Client(conn, config())
	if err != nil {
		return nil, fmt.Errorf("mux: client: %w", err)
	}
	return &Session{ys: ys}, nil
}

// Server starts the accepting side of a session over conn.
func Server(conn io.ReadWriteCloser) (*Session, error) {
	ys, err := yamux.Server(conn, config())
	if err != nil {
		return nil, fmt.Errorf("mux: server: %w", err)
	}
	return &Session{ys: ys}, nil
}

// Open opens a stream of the given kind. Closing the returned conn
// half-closes it: the peer reads EOF, and the conn can still read.
func (s *Session) Open(kind proto.StreamKind) (net.Conn, error) {
	st, err := s.ys.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("mux: open %v stream: %w", kind, err)
	}
	if err := proto.WriteFrame(st, &proto.StreamHeader{Kind: kind}); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("mux: write %v stream header: %w", kind, err)
	}
	return st, nil
}

// Accept waits for the peer to open a stream. The caller reads its kind
// with ReadKind, typically in the goroutine that serves it, so that one
// slow stream cannot block the accept loop.
func (s *Session) Accept() (net.Conn, error) {
	st, err := s.ys.AcceptStream()
	if err != nil {
		return nil, fmt.Errorf("mux: accept: %w", err)
	}
	return st, nil
}

// ReadKind reads the header of a stream returned by Accept.
func ReadKind(c net.Conn) (proto.StreamKind, error) {
	if err := c.SetReadDeadline(time.Now().Add(HeaderTimeout)); err != nil {
		return 0, fmt.Errorf("mux: set header deadline: %w", err)
	}
	var h proto.StreamHeader
	if err := proto.ReadFrame(c, &h, proto.MaxControlFrame); err != nil {
		return 0, fmt.Errorf("mux: read stream header: %w", err)
	}
	if err := c.SetReadDeadline(time.Time{}); err != nil {
		return 0, fmt.Errorf("mux: clear header deadline: %w", err)
	}
	if !h.Kind.Valid() {
		return 0, fmt.Errorf("mux: unknown stream kind %d", h.Kind)
	}
	return h.Kind, nil
}

// Close closes the session and every stream in it.
func (s *Session) Close() error {
	return s.ys.Close()
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} {
	return s.ys.CloseChan()
}

// IsClosed reports whether err means the session or stream was closed,
// as opposed to a protocol failure.
func IsClosed(err error) bool {
	return errors.Is(err, yamux.ErrSessionShutdown) ||
		errors.Is(err, yamux.ErrStreamClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed)
}
