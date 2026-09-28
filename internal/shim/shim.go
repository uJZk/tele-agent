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
	"syscall"
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
	// dialTimeout bounds connecting to session main, whose listen backlog
	// may be full when many shims start at once.
	dialTimeout = 30 * time.Second
	// signalWriteTimeout bounds forwarding one signal to a session main
	// that stopped reading.
	signalWriteTimeout = 5 * time.Second
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

	conn, err := connect(name, argv)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = conn.Close() }() // the process ends right after
	st, err := wait(conn, sig)
	if err != nil {
		return fail(err)
	}
	signal.Stop(sig)
	return finish(st)
}

// connect sends the stdio fds and the request to session main.
func connect(name string, argv []string) (*net.UnixConn, error) {
	sessDir := os.Getenv(SessionEnv)
	if sessDir == "" {
		return nil, fmt.Errorf("%s is not set; shims only work inside a tele session", SessionEnv)
	}
	sid := filepath.Base(sessDir)
	if err := proto.CheckSessionID(sid); err != nil {
		return nil, fmt.Errorf("%s=%q is not a session directory", SessionEnv, sessDir)
	}
	token, err := readToken(filepath.Join(sessDir, shimsrv.TokenFile))
	if err != nil {
		return nil, err
	}
	dir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("current directory: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", shimsrv.SocketName(sid))
	if err != nil {
		return nil, fmt.Errorf("cannot reach tele session %s (has it ended?): %w", sid, err)
	}
	conn, ok := c.(*net.UnixConn)
	if !ok {
		_ = c.Close()
		return nil, fmt.Errorf("unexpected connection type %T", c)
	}
	if err := send(conn, &proto.ShimRequest{Token: token, Name: name, Argv: argv, Dir: dir, Env: os.Environ()}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send request to tele session %s: %w", sid, err)
	}
	return conn, nil
}

// send transfers fds 0, 1 and 2 on a marker byte, then the request
// (internal/proto ShimRequest).
func send(conn *net.UnixConn, req *proto.ShimRequest) error {
	n, _, err := conn.WriteMsgUnix([]byte{0}, unix.UnixRights(0, 1, 2), nil)
	if err != nil {
		return err
	}
	if n != 1 {
		return io.ErrShortWrite
	}
	return proto.WriteFrame(conn, req)
}

func readToken(path string) ([]byte, error) {
	// The path comes from TELE_SESSION, set by tele for this very process;
	// its contents only ever go to the session socket that it names.
	f, err := os.Open(path) //nolint:gosec // G703: see above

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
			n, ok := s.(syscall.Signal)
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
		if errors.Is(err, io.EOF) {
			return nil, errors.New("tele session closed the connection before the command finished")
		}
		return nil, fmt.Errorf("read exit status from tele session: %w", err)
	}
	if f.Op != proto.ShimExit || f.Exit == nil {
		return nil, fmt.Errorf("tele session sent unexpected frame %d", f.Op)
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
