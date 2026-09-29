package launcher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/ujzk/tele-agent/internal/endpoint"
	"github.com/ujzk/tele-agent/internal/mux"
	"github.com/ujzk/tele-agent/internal/proto"
)

// handshakeTimeout bounds the Hello exchange once connected.
const handshakeTimeout = 30 * time.Second

// remoteSession is an established session with tele server.
type remoteSession struct {
	ID string
	// Mux opens the session's exec, FS and watch streams.
	Mux *mux.Session
	// Target describes the target host and user.
	Target proto.TargetInfo
	// ScratchDir is the server-side directory of the scratch areas.
	ScratchDir string

	control net.Conn // kept open for the session's lifetime
}

// Close ends the session.
func (s *remoteSession) Close() error {
	_ = s.control.Close() // closing the session closes it anyway
	return s.Mux.Close()
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

// connect establishes a session with the server at ep (docs/cli.md "启动流程"
// steps 1 and 2).
func connect(ctx context.Context, ep endpoint.Endpoint, token []byte) (_ *remoteSession, err error) {
	sid, err := newSessionID()
	if err != nil {
		return nil, fmt.Errorf("session id: %w", err)
	}
	conn, err := ep.Dial(ctx)
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
	if err := c.Send(&proto.Hello{Version: proto.Version, Token: token, SessionID: sid}); err != nil {
		return nil, fmt.Errorf("send hello: %w", err)
	}
	var reply proto.HelloReply
	if err := c.Recv(&reply); err != nil {
		return nil, fmt.Errorf("read hello reply: %w", err)
	}
	if reply.Err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRejected, reply.Err)
	}
	if reply.Version != proto.Version {
		return nil, fmt.Errorf("protocol version mismatch: local %d, server %d; upgrade the older side", proto.Version, reply.Version)
	}
	if err := checkTarget(&reply); err != nil {
		return nil, err
	}
	if err := st.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return &remoteSession{ID: sid, Mux: m, Target: reply.Target, ScratchDir: reply.ScratchDir, control: st}, nil
}

// checkTarget validates what the server reports about itself: its paths
// end up in the view's layout and in paths Claude sees, and the remote is
// not trusted (docs/security.md "远端返回的数据").
func checkTarget(r *proto.HelloReply) error {
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
