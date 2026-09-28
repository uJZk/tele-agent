// Package shim is the process Claude starts in place of bash, sh, rg, git
// and the other programs in <sess>/bin (docs/exec.md "shim"). It hands its
// stdio fds and its invocation to session main, forwards the signals it
// catches, and ends exactly as the real command ended.
//
// The shim's stdin, stdout and stderr belong to the proxied command: the
// only thing the shim itself ever writes is a single "tele: <reason>" line
// on stderr, for failures and for messages from session main.
package shim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/shimsrv"
)

// ExitFailure is the exit code for infrastructure failures: session main
// unreachable, session gone, protocol errors (docs/exec.md "shim").
const ExitFailure = 255

// SessionEnv names the variable holding the session directory as Claude
// sees it; its base name is the session ID.
const SessionEnv = "TELE_SESSION"

const (
	// dialTimeout bounds connecting to session main, including the retries
	// while its listen backlog is full.
	dialTimeout = 30 * time.Second
	// The delay between those retries doubles from dialBackoffStart up to
	// dialBackoffCap.
	dialBackoffStart = time.Millisecond
	dialBackoffCap   = 100 * time.Millisecond
	// signalWriteTimeout bounds forwarding one signal to a session main
	// that stopped reading.
	signalWriteTimeout = 5 * time.Second
	// refusalReadTimeout bounds looking for session main's reason after it
	// hung up while the request was being sent.
	refusalReadTimeout = 5 * time.Second
	// maxTokenSize bounds the token file read, in case TELE_SESSION points
	// somewhere unexpected.
	maxTokenSize = 4096
)

// forwardedSignals are the catchable signals a user or Claude sends to end
// or steer a command (docs/exec.md "进程与信号").
var forwardedSignals = []os.Signal{
	unix.SIGINT, unix.SIGTERM, unix.SIGHUP, unix.SIGQUIT,
	unix.SIGUSR1, unix.SIGUSR2, unix.SIGWINCH,
}

// Main runs the shim invoked as name with the full argument vector argv
// (argv[0] included) and returns the exit code; a command that was killed
// by a signal makes Main kill the process with the same signal instead.
func Main(name string, argv []string) int {
	// The only write the shim makes is to stderr; a closed stderr must not
	// turn into death by SIGPIPE, which would misreport the exit status.
	signal.Ignore(unix.SIGPIPE)

	if err := ensureStdio(); err != nil {
		return fail(fmt.Errorf("standard fds: %w", err))
	}
	sig := make(chan os.Signal, 16)
	for _, s := range forwardedSignals {
		// A signal ignored at exec stays ignored for a local command too.
		// The runtime tracks this for SIGHUP and SIGINT, the ones nohup
		// and shells' background jobs ignore.
		if !signal.Ignored(s) {
			signal.Notify(sig, s)
		}
	}
	defer signal.Stop(sig)

	sess, err := lookupSession()
	if err != nil {
		return fail(err)
	}
	frame, err := sess.request(name, argv)
	if err != nil {
		return fail(err)
	}
	conn, err := sess.dial()
	if err != nil {
		return fail(err)
	}
	defer func() { _ = conn.Close() }() // the process ends right after
	st, err := sess.run(conn, frame, sig)
	if err != nil {
		return fail(err)
	}
	signal.Stop(sig)
	return finish(st)
}

// session is the tele session named by TELE_SESSION.
type session struct {
	dir string // the session directory as Claude sees it
	id  string
}

func lookupSession() (*session, error) {
	dir := os.Getenv(SessionEnv)
	if dir == "" {
		return nil, fmt.Errorf("%s is not set; shims only work inside a tele session", SessionEnv)
	}
	id := filepath.Base(dir)
	if err := proto.CheckSessionID(id); err != nil {
		return nil, fmt.Errorf("%s=%q is not a session directory", SessionEnv, dir)
	}
	return &session{dir: dir, id: id}, nil
}

// request returns the invocation as a ShimRequest frame. It is built before
// connecting, so that a request session main would refuse is never sent.
func (s *session) request(name string, argv []string) ([]byte, error) {
	token, err := readToken(filepath.Join(s.dir, shimsrv.TokenFile))
	if err != nil {
		return nil, err
	}
	// getcwd(2), not os.Getwd: that returns $PWD whenever it names the same
	// directory, and $PWD need not be clean, which session main requires.
	dir, err := unix.Getwd()
	if err != nil {
		return nil, fmt.Errorf("current directory: %w", err)
	}
	frame, err := proto.EncodeShimRequest(&proto.ShimRequest{Token: token, Name: name, Argv: argv, Dir: dir, Env: os.Environ()})
	if tooLarge := (*proto.FrameTooLargeError)(nil); errors.As(err, &tooLarge) {
		return nil, fmt.Errorf("argument list and environment too long to forward: %d bytes encoded, limit %d; pass large data through a file", tooLarge.Size, tooLarge.Limit)
	}
	return frame, err
}

func readToken(path string) ([]byte, error) {
	// The path comes from TELE_SESSION, set by tele for this very process;
	// its contents only ever go to the session socket that it names.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("session token: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only
	token, err := io.ReadAll(io.LimitReader(f, maxTokenSize+1))
	if err != nil {
		return nil, fmt.Errorf("session token: %w", err)
	}
	if len(token) > maxTokenSize {
		return nil, fmt.Errorf("session token %s: larger than %d bytes", path, maxTokenSize)
	}
	return token, nil
}

// dial connects to session main. An abstract socket name has no owner: once
// session main is gone, any local user can bind it and would receive the
// token and the stdio fds, and could answer with a forged exit status. So
// the listener must belong to the shim's own uid, as session main checks
// the shim's (docs/security.md "本地的会话主进程").
func (s *session) dial() (*net.UnixConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	conn, err := dialUnix(ctx, shimsrv.SocketName(s.id))
	switch {
	case errors.Is(err, unix.EAGAIN):
		return nil, fmt.Errorf("tele session %s did not accept the command within %v; it may be overloaded: %w", s.id, dialTimeout, err)
	case err != nil:
		return nil, fmt.Errorf("cannot reach tele session %s (has it ended?): %w", s.id, err)
	}
	cred, err := shimsrv.PeerCred(conn)
	if err == nil {
		err = checkListener(cred, os.Getuid())
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("tele session %s: %w", s.id, err)
	}
	return conn, nil
}

// dialUnix connects to addr, retrying while its listen backlog is full: a
// non-blocking connect(2) to a unix socket then fails with EAGAIN at once
// instead of waiting, and Go's sockets are non-blocking. The backlog fills
// when many shims start together while session main's accept loop is slow
// or backing off.
func dialUnix(ctx context.Context, addr string) (*net.UnixConn, error) {
	var (
		d    net.Dialer
		full error // the last EAGAIN
	)
	delay := dialBackoffStart
	for {
		c, err := d.DialContext(ctx, "unix", addr)
		switch {
		case err == nil:
			conn, ok := c.(*net.UnixConn)
			if !ok {
				_ = c.Close()
				return nil, fmt.Errorf("unexpected connection type %T", c)
			}
			return conn, nil
		case errors.Is(err, unix.EAGAIN):
			full = err
		case full != nil && ctx.Err() != nil:
			return nil, full // the time ran out while the backlog stayed full
		default:
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, full
		case <-time.After(delay):
		}
		delay = min(2*delay, dialBackoffCap)
	}
}

// checkListener accepts only a session socket created by uid.
func checkListener(cred *unix.Ucred, uid int) error {
	if int64(cred.Uid) != int64(uid) {
		return fmt.Errorf("its socket belongs to uid %d, not %d, so the session has ended and another user took its address; nothing was sent", cred.Uid, uid)
	}
	return nil
}

// run hands the stdio fds and the request to session main and waits for
// the exit status, forwarding caught signals meanwhile.
func (s *session) run(conn *net.UnixConn, frame []byte, sig <-chan os.Signal) (*proto.ShimStatus, error) {
	if err := send(conn, frame); err != nil {
		// Session main may have refused the request, and said why, before
		// hanging up in the middle of it.
		_ = conn.SetReadDeadline(time.Now().Add(refusalReadTimeout)) // a failure shows in the read
		if st, rerr := readExit(conn); rerr == nil {
			return st, nil
		}
		return nil, s.connError("send the command", err)
	}
	st, err := wait(conn, sig)
	if err != nil {
		return nil, s.connError("read the exit status", err)
	}
	return st, nil
}

// connError describes a failed exchange with session main. A connection
// that ends without an exit status is how session main refuses a peer it
// does not talk to, and how its own death looks; either way the session
// log has the reason.
func (s *session) connError(op string, err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, unix.ECONNRESET) || errors.Is(err, unix.EPIPE) {
		return fmt.Errorf("tele session %s closed the connection without an exit status: it ended, or refused the command; see %s",
			s.id, filepath.Join(s.dir, shimsrv.LogFile))
	}
	return fmt.Errorf("%s from tele session %s: %w", op, s.id, err)
}

// send transfers fds 0, 1 and 2 on a marker byte, then the request frame
// (internal/proto ShimRequest).
func send(conn *net.UnixConn, frame []byte) error {
	n, _, err := conn.WriteMsgUnix([]byte{0}, unix.UnixRights(0, 1, 2), nil)
	if err != nil {
		return err
	}
	if n != 1 {
		return io.ErrShortWrite
	}
	_, err = conn.Write(frame)
	return err
}

// wait forwards caught signals until session main sends the exit status.
// It waits as long as the command runs, which may be forever.
func wait(conn *net.UnixConn, sig <-chan os.Signal) (*proto.ShimStatus, error) {
	type result struct {
		st  *proto.ShimStatus
		err error
	}
	res := make(chan result, 1)
	go func() {
		st, err := readExit(conn)
		res <- result{st, err}
	}()
	for {
		select {
		case r := <-res:
			return r.st, r.err
		case s := <-sig:
			n, ok := s.(unix.Signal)
			if !ok {
				continue
			}
			_ = conn.SetWriteDeadline(time.Now().Add(signalWriteTimeout)) // a failed write surfaces in readExit
			_ = proto.WriteFrame(conn, &proto.ShimFrame{Op: proto.ShimSignal, Signal: int(n)})
		}
	}
}

func readExit(conn *net.UnixConn) (*proto.ShimStatus, error) {
	var f proto.ShimFrame
	if err := proto.ReadFrame(conn, &f, proto.MaxControlFrame); err != nil {
		return nil, err
	}
	if f.Op != proto.ShimExit || f.Exit == nil {
		return nil, fmt.Errorf("unexpected frame %d", f.Op)
	}
	return f.Exit, nil
}

// finish ends the shim the way the command ended.
func finish(st *proto.ShimStatus) int {
	if st.Msg != "" {
		report(st.Msg)
	}
	switch {
	case st.Signal != 0:
		if st.Signal < 1 || st.Signal > 64 {
			return fail(fmt.Errorf("invalid signal %d in exit status", st.Signal))
		}
		raise(unix.Signal(st.Signal))
		// Still alive: the signal's default action does not terminate.
		return 128 + st.Signal
	case st.Code < 0 || st.Code > 255:
		return fail(fmt.Errorf("invalid exit code %d in exit status", st.Code))
	default:
		return st.Code
	}
}

func fail(err error) int {
	report(err.Error())
	return ExitFailure
}

func report(msg string) {
	_, _ = fmt.Fprintf(os.Stderr, "tele: %s\n", msg) // nowhere else to report to
}
