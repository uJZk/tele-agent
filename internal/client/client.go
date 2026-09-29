// Package client opens sessions with tele server: it connects over the
// resumable session layer and exchanges Hello (docs/cli.md "启动流程").
package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/ujzk/tele-agent/internal/endpoint"
	"github.com/ujzk/tele-agent/internal/mux"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/resume"
)

// handshakeTimeout bounds the Hello exchange once connected.
const handshakeTimeout = 30 * time.Second

// Session is an established session with tele server.
//
// It runs over up to three connections, each a resumable session of its
// own, so that one kind of traffic cannot hold up another behind TCP's
// head-of-line blocking (docs/transport.md "可恢复会话层"): Mux carries the
// interactive streams (exec, port forwards, change pushes), Meta the small
// file requests, Bulk file reads and writes. When a further connection
// cannot be set up, its traffic falls back to Mux.
type Session struct {
	ID string
	// Mux opens the session's exec, forward and watch streams.
	Mux *mux.Session
	// Meta and Bulk open FS streams: Bulk for FSRead and FSWrite, Meta
	// for the rest. Either may be Mux.
	Meta, Bulk *mux.Session
	// Target describes the target host and user.
	Target proto.TargetInfo
	// ScratchDir is the server-side directory of the scratch areas.
	ScratchDir string
	// Conns are the resumable sessions under the connections, the primary
	// first: Kick and Migrate react to network changes.
	Conns []*resume.Conn
	// RTT is the round-trip time of the Hello exchange.
	RTT time.Duration
	// ClockSkew is the server's clock minus the local clock, estimated
	// from the Hello exchange; zero if the server did not report its
	// time. SS2022 refuses connections beyond 30 s of skew
	// (docs/transport.md "SS2022").
	ClockSkew time.Duration

	controls []net.Conn // kept open for the session's lifetime
	muxes    []*mux.Session
}

// Close ends the session and its further connections.
func (s *Session) Close() error {
	for _, c := range s.controls {
		_ = c.Close() // closing the session closes it anyway
	}
	var errs []error
	// Further connections first: the server closes them with the session.
	for i := len(s.muxes) - 1; i >= 0; i-- {
		errs = append(errs, s.muxes[i].Close())
	}
	return errors.Join(errs...)
}

// newSessionID returns a random session ID (proto.CheckSessionID).
func newSessionID() (string, error) {
	var b [proto.SessionIDLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ErrRejected wraps the reason a server gave for refusing a session.
var ErrRejected = errors.New("server refused the session")

// Connect establishes a session with the server at ep. token is the
// session token of a unix endpoint; nil for SS2022, whose PSK is in ep.
func Connect(ctx context.Context, ep endpoint.Endpoint, token []byte, cfg resume.Config) (*Session, error) {
	return ConnectDial(ctx, Rotate([]endpoint.Endpoint{ep}), token, cfg)
}

// ConnectDial establishes a session over the transports dial opens, as
// from Rotate.
func ConnectDial(ctx context.Context, dial resume.DialFunc, token []byte, cfg resume.Config) (_ *Session, err error) {
	sid, err := newSessionID()
	if err != nil {
		return nil, fmt.Errorf("session id: %w", err)
	}
	s := &Session{ID: sid}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	sent := time.Now()
	reply, err := s.hello(ctx, dial, cfg, &proto.Hello{Version: proto.Version, Token: token, SessionID: sid})
	received := time.Now()
	if err != nil {
		return nil, err
	}
	if err := CheckTarget(reply); err != nil {
		return nil, err
	}
	s.Mux, s.Meta, s.Bulk = s.muxes[0], s.muxes[0], s.muxes[0]
	s.Target, s.ScratchDir = reply.Target, reply.ScratchDir
	s.RTT = received.Sub(sent)
	if reply.ServerTime != 0 {
		// The server read its clock about halfway through the exchange.
		mid := sent.Add(s.RTT / 2)
		s.ClockSkew = time.UnixMilli(reply.ServerTime).Sub(mid)
	}
	if len(reply.JoinKey) > 0 {
		join := &proto.Hello{Version: proto.Version, Token: token, SessionID: sid, JoinKey: reply.JoinKey}
		for _, class := range []**mux.Session{&s.Meta, &s.Bulk} {
			// A connection that cannot join leaves its traffic on Mux.
			if _, err := s.hello(ctx, dial, cfg, join); err == nil {
				*class = s.muxes[len(s.muxes)-1]
			} else if cfg.Logger != nil {
				cfg.Logger.Warn("client: further connection", "err", err)
			}
		}
	}
	return s, nil
}

// hello opens a connection over dial, sends h on its control stream and
// returns the server's accepted reply. The connection joins s only once
// the server accepted it.
func (s *Session) hello(ctx context.Context, dial resume.DialFunc, cfg resume.Config, h *proto.Hello) (_ *proto.HelloReply, err error) {
	// The connection outlives its transports: it redials whenever it
	// loses one (docs/transport.md "可恢复会话层").
	conn, err := resume.Dial(ctx, dial, cfg)
	if err != nil {
		return nil, err
	}
	m, err := mux.Client(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = m.Close()
		}
	}()
	st, err := m.Open(proto.StreamControl)
	if err != nil {
		return nil, fmt.Errorf("open control stream: %w", err)
	}
	if err := st.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return nil, err
	}
	// Ending ctx, at its deadline too, ends the exchange at once. Set
	// after the timeout, so that a ctx already done is not overridden.
	stop := context.AfterFunc(ctx, func() { _ = st.SetDeadline(time.Now()) })
	defer stop()
	c := proto.NewConn(st, proto.MaxControlFrame)
	// A server that refuses the session answers and closes it at once,
	// which can make the Hello write report the closed session although
	// the Hello went out; the refusal is read even then.
	sendErr := c.Send(h)
	var reply proto.HelloReply
	if err := c.Recv(&reply); err != nil {
		if sendErr != nil {
			return nil, fmt.Errorf("send hello: %w", sendErr)
		}
		return nil, fmt.Errorf("read hello reply: %w", err)
	}
	if reply.Err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRejected, reply.Err)
	}
	if reply.Version != proto.Version {
		return nil, fmt.Errorf("protocol version mismatch: local %d, server %d; upgrade the older side", proto.Version, reply.Version)
	}
	if err := st.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	s.muxes = append(s.muxes, m)
	s.Conns = append(s.Conns, conn)
	s.controls = append(s.controls, st)
	return &reply, nil
}

// CheckTarget validates what the server reports about itself: its paths
// end up in the view's layout and in paths Claude sees, and the remote is
// not trusted (docs/security.md "远端返回的数据").
func CheckTarget(r *proto.HelloReply) error {
	for name, p := range map[string]string{"home directory": r.Target.Home, "scratch directory": r.ScratchDir} {
		if err := proto.CheckPath(p); err != nil || p == "/" {
			return fmt.Errorf("server reported an invalid %s %q", name, p)
		}
	}
	if r.Target.User == "" {
		return errors.New("server reported no user name")
	}
	return nil
}

// Rotate returns a dial function over eps, which must not be empty. It
// sticks to the endpoint that worked last and, when that fails, tries the
// others in turn within the same call, splitting the time left among them
// so that one address that never answers cannot use it all. DNS is
// resolved again on every dial.
func Rotate(eps []endpoint.Endpoint) resume.DialFunc {
	var cur atomic.Uint64
	return func(ctx context.Context) (net.Conn, error) {
		n := uint64(len(eps))
		start := cur.Load()
		var errs []error
		for i := range n {
			actx, cancel := ctx, context.CancelFunc(func() {})
			if dl, ok := ctx.Deadline(); ok && n-i > 1 {
				actx, cancel = context.WithDeadline(ctx, time.Now().Add(time.Until(dl)/time.Duration(n-i)))
			}
			ep := eps[(start+i)%n]
			c, err := ep.Dial(actx)
			cancel()
			if err == nil {
				cur.Store((start + i) % n)
				return c, nil
			}
			errs = append(errs, err)
			if ctx.Err() != nil {
				break
			}
		}
		return nil, errors.Join(errs...)
	}
}
