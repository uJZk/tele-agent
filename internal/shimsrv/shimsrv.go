// Package shimsrv is session main's side of the shim protocol
// (docs/exec.md "shim 与会话主进程", internal/proto ShimRequest): it
// accepts shims on the session's abstract unix socket, authenticates them,
// takes over their stdio fds and hands each invocation to a Handler.
//
// An abstract socket has no file permissions; any process in the network
// namespace can connect. Peers are therefore checked twice: SO_PEERCRED
// must report the session's uid, and the request must carry the session
// token (docs/security.md "本地的会话主进程").
package shimsrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/bits"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// TokenFile is the name, inside the session directory, of the file that
// holds the session token: exactly Server.Token, mode 0600.
const TokenFile = "token"

// SocketName returns the abstract socket address of a session, in Go's
// notation (a leading '@' for the abstract namespace).
func SocketName(sessionID string) string {
	return "@tele-" + sessionID
}

// Timeouts.
const (
	// exitWriteTimeout bounds sending ShimExit to a shim that stopped
	// reading.
	exitWriteTimeout = 10 * time.Second
	// maxAcceptBackoff caps the retry delay after a temporary accept error.
	maxAcceptBackoff = time.Second
)

// handshakeTimeout bounds the time from accept to a complete ShimRequest.
// A variable so that tests can shorten it.
var handshakeTimeout = 10 * time.Second

// Listen opens the abstract socket of session sessionID.
func Listen(sessionID string) (*net.UnixListener, error) {
	if err := proto.CheckSessionID(sessionID); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: SocketName(sessionID), Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("shimsrv: listen: %w", err)
	}
	return ln, nil
}

// Request is one authenticated shim invocation.
type Request struct {
	// Name is the shim that was invoked, e.g. "bash".
	Name string
	// Argv is the shim's argument vector, argv[0] included.
	Argv []string
	// Dir is the shim's working directory in Claude's (the remote) view.
	Dir string
	// Env is the shim's complete environment.
	Env []string
	// Stdin, Stdout and Stderr are the shim's fds 0, 1 and 2. They share
	// open file descriptions with Claude (stdin may even be the user's
	// terminal), so their file status flags, O_NONBLOCK included, must not
	// be changed. They were wrapped with os.NewFile, which leaves the flags
	// alone; Fd keeps them too.
	Stdin, Stdout, Stderr *os.File
	// PeerPID is the shim's pid as seen in session main's pid namespace.
	PeerPID int
}

// Handler runs shim invocations.
type Handler interface {
	// Serve runs req and returns how the shim must end. It owns
	// req.Stdin, req.Stdout and req.Stderr and must close them, possibly
	// after returning: output of background processes may continue after
	// the command's main process has exited (docs/exec.md
	// "shim 与会话主进程").
	//
	// sigs delivers the signal numbers (1-64) the shim caught. Pending
	// signals are coalesced like the kernel's standard signals, so a
	// handler that stops reading loses nothing but repeats.
	//
	// ctx is cancelled when the shim's connection ends before Serve
	// returns — the shim was killed with SIGKILL, typically by Claude's
	// timeout or interrupt, and the command must be killed too — and when
	// the Server stops. It is also done once Serve has returned, so work
	// that outlives the call needs a lifetime of its own. Serve must
	// return promptly once ctx is done.
	Serve(ctx context.Context, req *Request, sigs <-chan int) proto.ShimStatus
}

// Server accepts shims for one session.
type Server struct {
	// Token is the session token; a shim must present exactly these bytes.
	// An empty Token rejects every shim.
	Token []byte
	// UID is the only peer uid accepted, as seen in the user namespace of
	// the process calling Serve.
	UID     int
	Handler Handler
	// Logger receives diagnostics; nil discards them. Nothing is ever
	// written to a shim's stdio (docs/coding-standards.md "日志与输出").
	Logger *slog.Logger
}

// Serve accepts connections on ln until ctx ends or accepting fails, then
// waits for every connection to finish. It takes ownership of ln and closes
// it. Once ctx ends, handlers' contexts are cancelled and their statuses are
// still delivered to the shims.
func (s *Server) Serve(ctx context.Context, ln *net.UnixListener) error {
	log := s.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	defer func() { _ = ln.Close() }() // error irrelevant: stops accepting either way

	var wg sync.WaitGroup
	defer wg.Wait()
	backoff := time.Duration(0)
	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !isTemporary(err) {
				return fmt.Errorf("shimsrv: accept: %w", err)
			}
			backoff = min(max(2*backoff, 5*time.Millisecond), maxAcceptBackoff)
			log.Warn("shim accept failed; retrying", "err", err, "delay", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			continue
		}
		backoff = 0
		wg.Go(func() { s.serveConn(ctx, conn, log) })
	}
}

// isTemporary reports accept errors caused by resource limits or by a
// peer that went away, after which accepting may succeed again.
func isTemporary(err error) bool {
	for _, e := range []unix.Errno{unix.EMFILE, unix.ENFILE, unix.ENOBUFS, unix.ENOMEM, unix.ECONNABORTED, unix.EINTR} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// errTokenRejected is the handshake error the shim is told about; every
// other handshake failure just closes the connection.
var errTokenRejected = errors.New("session token rejected")

func (s *Server) serveConn(ctx context.Context, conn *net.UnixConn, log *slog.Logger) {
	defer func() { _ = conn.Close() }() // nothing left to flush
	req, err := s.handshake(ctx, conn)
	if err != nil {
		log.Warn("shim rejected", "err", err)
		if errors.Is(err, errTokenRejected) {
			sendExit(conn, proto.ShimStatus{Code: exitFailure, Msg: err.Error()}, log)
		}
		return
	}
	log.Debug("shim accepted", "name", req.Name, "argv", req.Argv, "dir", req.Dir, "pid", req.PeerPID)
	s.run(ctx, conn, req, log)
}

// exitFailure is the shim's infrastructure failure exit code
// (docs/exec.md "shim").
const exitFailure = 255

// run serves an authenticated request: it runs the handler, feeds it the
// shim's signals, and sends its status to the shim.
func (s *Server) run(ctx context.Context, conn *net.UnixConn, req *Request, log *slog.Logger) {
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()

	status := make(chan proto.ShimStatus, 1)
	sigs := make(chan int) // never closed: the handler stops reading when it returns
	go func() { status <- s.Handler.Serve(hctx, req, sigs) }()

	pending := newSignalSet()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		readSignals(conn, pending, log)
		// The shim is gone (or broke the protocol) while the handler may
		// still run: that is how a SIGKILLed shim reaches the handler.
		cancel()
	}()

	st := deliverSignals(status, sigs, pending)
	sendExit(conn, st, log)
	_ = conn.Close() // unblocks readSignals; nothing left to flush
	<-readDone
}

// deliverSignals passes pending signals to the handler until it returns
// its status. Signals are taken lowest number first, as the kernel
// delivers standard signals.
func deliverSignals(status <-chan proto.ShimStatus, sigs chan<- int, pending *signalSet) proto.ShimStatus {
	next := 0
	for {
		if next == 0 {
			next = pending.take()
		}
		var out chan<- int
		if next != 0 {
			out = sigs
		}
		select {
		case st := <-status:
			return st
		case <-pending.wake:
		case out <- next:
			next = 0
		}
	}
}

// readSignals reads ShimSignal frames into pending until the connection
// ends or the shim breaks the protocol.
func readSignals(conn *net.UnixConn, pending *signalSet, log *slog.Logger) {
	for {
		var f proto.ShimFrame
		if err := proto.ReadFrame(conn, &f, proto.MaxControlFrame); err != nil {
			log.Debug("shim connection ended", "err", err)
			return
		}
		if f.Op != proto.ShimSignal {
			log.Warn("shim sent an unexpected frame", "op", f.Op)
			return
		}
		if !pending.add(f.Signal) {
			log.Warn("shim sent an invalid signal", "signal", f.Signal)
		}
	}
}

func sendExit(conn *net.UnixConn, st proto.ShimStatus, log *slog.Logger) {
	_ = conn.SetWriteDeadline(time.Now().Add(exitWriteTimeout)) // fails only on a closed conn; the write reports that
	if err := proto.WriteFrame(conn, &proto.ShimFrame{Op: proto.ShimExit, Exit: &st}); err != nil {
		log.Debug("send shim exit status", "err", err)
	}
}

// signalSet holds pending signal numbers 1-64, coalescing repeats.
type signalSet struct {
	mu   sync.Mutex
	mask uint64 // bit n-1 set: signal n pending; guarded by mu
	// wake gets a token whenever a signal is added.
	wake chan struct{}
}

func newSignalSet() *signalSet {
	return &signalSet{wake: make(chan struct{}, 1)}
}

// add marks sig pending; it reports false for an invalid number.
func (p *signalSet) add(sig int) bool {
	if sig < 1 || sig > 64 {
		return false
	}
	p.mu.Lock()
	p.mask |= 1 << (sig - 1)
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return true
}

// take removes and returns the lowest pending signal, or 0.
func (p *signalSet) take() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mask == 0 {
		return 0
	}
	n := bits.TrailingZeros64(p.mask)
	p.mask &^= 1 << n
	return n + 1
}
