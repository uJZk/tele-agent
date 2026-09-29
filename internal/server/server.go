// Package server is tele server: it accepts sessions from session main and
// serves exec, telefs and change-watch streams as the target user
// (docs/architecture.md "进程与角色").
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/ujzk/tele-agent/internal/execsvc"
	"github.com/ujzk/tele-agent/internal/fssvc"
	"github.com/ujzk/tele-agent/internal/hostinfo"
	"github.com/ujzk/tele-agent/internal/mux"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/resume"
)

// HandshakeTimeout bounds the time between accepting a connection and
// receiving a valid Hello.
const HandshakeTimeout = 30 * time.Second

// ExpireGrace is how long the commands of a session whose lease expired
// get between SIGTERM and SIGKILL (docs/transport.md "断线语义").
const ExpireGrace = 10 * time.Second

// Config configures a Server.
type Config struct {
	// Token authenticates sessions on a transport that does not
	// authenticate the peer (a unix socket).
	Token []byte
	// TransportAuthenticated is set when every transport of the listener
	// authenticates the peer (SS2022 with the host's PSK); the Hello token
	// is then not checked (docs/security.md "信任边界").
	TransportAuthenticated bool
	// FSRoot is the directory served as the remote "/" — always "/" in
	// production. Tests serve a temporary directory; it is not a security
	// boundary.
	FSRoot string
	// Target, if set, replaces host discovery (tests).
	Target *proto.TargetInfo
	// ScratchBase, if set, replaces ~/.cache/tele/s as the directory of
	// the sessions' scratch directories (tests).
	ScratchBase string
	// Session configures the resumable session layer; the zero value
	// takes its defaults.
	Session resume.Config
	Logger  *slog.Logger
}

// Server serves sessions.
type Server struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session // live sessions by ID, for joins
}

// New returns a Server.
func New(cfg Config) *Server {
	if cfg.FSRoot == "" {
		cfg.FSRoot = "/"
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{cfg: cfg, log: log, sessions: map[string]*session{}}
}

// Serve accepts sessions on the transports of ln until ctx ends, then
// closes every session and waits for them. A session survives the loss of
// its transport until its lease expires (internal/resume).
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cfg := s.cfg.Session
	if cfg.Logger == nil {
		cfg.Logger = s.log.With("layer", "resume")
	}
	l := resume.Listen(ln, cfg) //nolint:contextcheck // sessions outlive a transport; Close below ends the listener
	stop := context.AfterFunc(ctx, func() { _ = l.Close() })
	defer stop()

	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := l.Accept()
		if err != nil {
			_ = l.Close()
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("server: accept: %w", err)
		}
		wg.Go(func() { s.serveConn(ctx, conn) })
	}
}

// serveConn runs one session. A panic tears down the whole session, never
// just one request (docs/coding-standards.md "错误处理").
func (s *Server) serveConn(ctx context.Context, conn *resume.Conn) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("session panic", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	sess, err := mux.Server(conn)
	if err != nil {
		_ = conn.Close()
		s.log.Warn("mux", "err", err)
		return
	}
	defer sess.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()
	go func() {
		select {
		case <-sess.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	ss, joined, err := s.handshake(ctx, sess)
	if err != nil {
		s.log.Warn("handshake", "err", err)
		return
	}
	if joined {
		// A further connection of a session: it only carries streams.
		// Its session closes it when it ends.
		s.acceptStreams(ctx, ss, sess, s.log.With("sid", ss.sid, "conn", "joined"))
		return
	}
	s.register(ss)
	defer func() {
		s.unregister(ss)
		ss.close()
	}()
	// The streams stay intact until the hook returns, so commands get
	// SIGTERM and a grace period before closing the session kills them.
	conn.OnExpire(func() { ss.exec.Stop(ExpireGrace) })
	log := s.log.With("sid", ss.sid)
	log.Info("session started")
	s.acceptStreams(ctx, ss, sess, log)
	log.Info("session ended")
}

// acceptStreams serves the streams of one connection of session ss until
// the connection ends.
func (s *Server) acceptStreams(ctx context.Context, ss *session, sess *mux.Session, log *slog.Logger) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		st, err := sess.Accept()
		if err != nil {
			if !mux.IsClosed(err) {
				log.Warn("accept stream", "err", err)
			}
			return
		}
		wg.Go(func() { ss.serveStream(ctx, st, log) })
	}
}

func (s *Server) register(ss *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[ss.sid] = ss
}

func (s *Server) unregister(ss *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[ss.sid] == ss {
		delete(s.sessions, ss.sid)
	}
}

// join adds connection m to the live session sid if key is its join key.
func (s *Server) join(sid string, key []byte, m *mux.Session) (*session, bool) {
	s.mu.Lock()
	ss := s.sessions[sid]
	s.mu.Unlock()
	if ss == nil || subtle.ConstantTimeCompare(key, ss.joinKey) != 1 {
		return nil, false
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.closed {
		return nil, false
	}
	ss.joined = append(ss.joined, m)
	return ss, true
}

// session holds the services of one session.
type session struct {
	sid     string
	scratch string
	joinKey []byte
	exec    *execsvc.Service
	fs      *fssvc.Service

	mu     sync.Mutex
	joined []*mux.Session // further connections, closed with the session
	closed bool
}

// handshake reads the Hello of connection sess. It starts a session, or
// with joined set adds the connection to an existing one.
func (s *Server) handshake(ctx context.Context, sess *mux.Session) (_ *session, joined bool, _ error) {
	timer := time.AfterFunc(HandshakeTimeout, func() { _ = sess.Close() })
	defer timer.Stop()

	st, err := sess.Accept()
	if err != nil {
		return nil, false, err
	}
	kind, err := mux.ReadKind(st)
	if err != nil {
		return nil, false, err
	}
	if kind != proto.StreamControl {
		return nil, false, fmt.Errorf("first stream is %v, want control", kind)
	}
	c := proto.NewConn(st, proto.MaxControlFrame)
	var hello proto.Hello
	if err := c.Recv(&hello); err != nil {
		return nil, false, fmt.Errorf("read hello: %w", err)
	}
	reject := func(msg string) error {
		_ = c.Send(&proto.HelloReply{Version: proto.Version, Err: &proto.Error{Msg: msg}})
		return errors.New(msg)
	}
	if hello.Version != proto.Version {
		return nil, false, reject(fmt.Sprintf("protocol version mismatch: server %d, client %d; upgrade the older side", proto.Version, hello.Version))
	}
	if !s.cfg.TransportAuthenticated && (len(s.cfg.Token) == 0 || subtle.ConstantTimeCompare(hello.Token, s.cfg.Token) != 1) {
		return nil, false, reject("authentication failed")
	}
	if err := proto.CheckSessionID(hello.SessionID); err != nil {
		return nil, false, reject(err.Error())
	}
	if hello.JoinKey != nil {
		ss, ok := s.join(hello.SessionID, hello.JoinKey, sess)
		if !ok {
			return nil, false, reject("no such session to join")
		}
		if err := c.Send(&proto.HelloReply{Version: proto.Version}); err != nil {
			return nil, false, fmt.Errorf("send hello reply: %w", err)
		}
		return ss, true, nil
	}

	var target proto.TargetInfo
	if s.cfg.Target != nil {
		target = *s.cfg.Target
	} else if target, err = hostinfo.Gather(ctx); err != nil {
		return nil, false, reject(fmt.Sprintf("describe target: %v", err))
	}
	base := s.cfg.ScratchBase
	if base == "" {
		base = filepath.Join(target.Home, ".cache", "tele", "s")
	}
	joinKey := make([]byte, proto.JoinKeyLen)
	if _, err := rand.Read(joinKey); err != nil {
		return nil, false, reject(fmt.Sprintf("join key: %v", err))
	}
	ss := &session{sid: hello.SessionID, scratch: filepath.Join(base, hello.SessionID), joinKey: joinKey}
	if err := os.MkdirAll(ss.scratch, 0o700); err != nil {
		return nil, false, reject(fmt.Sprintf("create scratch dir: %v", err))
	}
	if ss.fs, err = fssvc.New(fssvc.Config{Root: s.cfg.FSRoot, Logger: s.log.With("sid", ss.sid, "svc", "fs")}); err != nil {
		ss.close()
		return nil, false, reject(fmt.Sprintf("start fs service: %v", err))
	}
	if ss.exec, err = execsvc.New(execsvc.Config{
		Target:     target,
		ScratchDir: ss.scratch,
		Syncer:     ss.fs,
		Logger:     s.log.With("sid", ss.sid, "svc", "exec"),
	}); err != nil {
		ss.close()
		return nil, false, reject(fmt.Sprintf("start exec service: %v", err))
	}
	if err := c.Send(&proto.HelloReply{Version: proto.Version, Target: target, ScratchDir: ss.scratch, ServerTime: time.Now().UnixMilli(), JoinKey: joinKey}); err != nil {
		ss.close()
		return nil, false, fmt.Errorf("send hello reply: %w", err)
	}
	return ss, false, nil
}

func (ss *session) serveStream(ctx context.Context, st net.Conn, log *slog.Logger) {
	defer st.Close()
	kind, err := mux.ReadKind(st)
	if err != nil {
		log.Warn("stream header", "err", err)
		return
	}
	switch kind {
	case proto.StreamExec:
		err = ss.exec.Serve(ctx, st)
	case proto.StreamFS:
		err = ss.fs.ServeRequest(ctx, st)
	case proto.StreamWatch:
		err = ss.fs.ServeWatch(ctx, st)
	case proto.StreamForward:
		err = serveForward(ctx, st)
	case proto.StreamControl:
		err = errors.New("unexpected second control stream")
	}
	if err != nil && !mux.IsClosed(err) {
		log.Warn("stream", "kind", kind, "err", err)
	}
}

func (ss *session) close() {
	ss.mu.Lock()
	ss.closed = true
	joined := ss.joined
	ss.joined = nil
	ss.mu.Unlock()
	for _, m := range joined {
		_ = m.Close()
	}
	if ss.exec != nil {
		_ = ss.exec.Close()
	}
	if ss.fs != nil {
		_ = ss.fs.Close()
	}
	_ = os.RemoveAll(ss.scratch)
}
