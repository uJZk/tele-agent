package shim

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/shimsrv"
	"github.com/ujzk/tele-agent/internal/testutil/helperproc"
	"github.com/ujzk/tele-agent/internal/testutil/privtest"
)

// closeFDsEnv makes the shim helper close the listed fds before Main runs,
// as if Claude had started it with them closed.
const closeFDsEnv = "SHIM_TEST_CLOSE_FDS"

func TestMain(m *testing.M) {
	helperproc.Register("shim", shimHelper)
	helperproc.Register("listen-as-nobody", listenAsNobody)
	helperproc.Dispatch()
	goleak.VerifyTestMain(m)
}

// shimHelper is the shim process: the test binary started through a
// symlink named like the shim, as in <sess>/bin.
func shimHelper([]string) int {
	for _, s := range strings.Split(os.Getenv(closeFDsEnv), ",") {
		if fd, err := strconv.Atoi(s); err == nil {
			_ = unix.Close(fd)
		}
	}
	return Main(filepath.Base(os.Args[0]), os.Args)
}

// echoed is what the fake handler's "echo" command reports.
type echoed struct {
	Name    string
	Argv    []string
	Dir     string
	Var     string
	PeerPID int
}

// fakeHandler implements commands named by argv[1].
type fakeHandler struct {
	calls     atomic.Int32
	cancelled chan string   // argv[1] of a command whose ctx ended early
	late      chan struct{} // closed to release "late" output
	lateOnce  sync.Once
	wg        sync.WaitGroup // output written after Serve returned
}

func newFakeHandler() *fakeHandler {
	return &fakeHandler{cancelled: make(chan string, 8), late: make(chan struct{})}
}

func (h *fakeHandler) releaseLate() { h.lateOnce.Do(func() { close(h.late) }) }

func (h *fakeHandler) Serve(ctx context.Context, req *shimsrv.Request, sigs <-chan int) proto.ShimStatus {
	h.calls.Add(1)
	closeAll := func() {
		_ = req.Stdin.Close()
		_ = req.Stdout.Close()
		_ = req.Stderr.Close()
	}
	args := slices.Clone(req.Argv[1:])
	if len(args) == 0 {
		args = []string{""}
	}
	num := func(i int) int {
		n, _ := strconv.Atoi(args[i])
		return n
	}
	var st proto.ShimStatus
	switch args[0] {
	case "echo":
		var v string
		for _, kv := range req.Env {
			if s, ok := strings.CutPrefix(kv, "SHIM_TEST_VAR="); ok {
				v = s
			}
		}
		_ = json.NewEncoder(req.Stdout).Encode(echoed{req.Name, req.Argv, req.Dir, v, req.PeerPID})
		_, _ = io.WriteString(req.Stderr, "to stderr")
	case "exit":
		st.Code = num(1)
		if len(args) > 2 {
			st.Msg = args[2]
		}
	case "signal":
		st.Signal = num(1)
		if len(args) > 2 {
			st.Msg = args[2]
		}
	case "count":
		_, _ = fmt.Fprintf(req.Stdout, "%d", len(req.Argv))
	case "cat":
		n, _ := io.Copy(req.Stdout, req.Stdin)
		_, _ = fmt.Fprintf(req.Stderr, "%d bytes", n)
	case "wait-signal":
		_, _ = io.WriteString(req.Stdout, "ready\n")
		select {
		case s := <-sigs:
			_, _ = fmt.Fprintf(req.Stdout, "got %d\n", s)
		case <-ctx.Done():
			st.Code = 1
		}
	case "block":
		_, _ = io.WriteString(req.Stdout, "ready\n")
		<-ctx.Done()
		h.cancelled <- args[0]
		st.Code = 1
	case "late":
		_, _ = io.WriteString(req.Stdout, "early\n")
		h.wg.Go(func() {
			<-h.late
			_, _ = io.WriteString(req.Stdout, "late\n")
			closeAll()
		})
		return st
	}
	closeAll()
	return st
}

// fakeSession is a session directory with a shimsrv.Server behind it.
type fakeSession struct {
	dir   string
	token []byte
	h     *fakeHandler
}

func newSessionDir(t *testing.T) (dir, sid string) {
	t.Helper()
	var b [8]byte
	_, _ = rand.Read(b[:])
	sid = hex.EncodeToString(b[:])
	dir = filepath.Join(t.TempDir(), sid)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, sid
}

func newSession(t *testing.T) *fakeSession {
	t.Helper()
	dir, sid := newSessionDir(t)
	s := &fakeSession{dir: dir, token: []byte("secret-token\n"), h: newFakeHandler()}
	if err := os.WriteFile(filepath.Join(dir, shimsrv.TokenFile), s.token, 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := shimsrv.Listen(sid)
	if err != nil {
		t.Fatal(err)
	}
	srv := &shimsrv.Server{Token: s.token, UID: os.Getuid(), Handler: s.h}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		s.h.releaseLate()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
		s.h.wg.Wait()
	})
	return s
}

// shimCmd returns a command running the shim name with args, through a
// symlink in dir/bin, with TELE_SESSION=dir.
func shimCmd(t *testing.T, dir, name string, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bin", name)
	if err := os.Symlink(exe, link); err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	cmd := helperproc.Command(t, "shim")
	cmd.Path = link
	cmd.Args = append([]string{link}, args...)
	cmd.Env = append(cmd.Env, SessionEnv+"="+dir,
		// Under -race every exit would otherwise wait a second for late
		// race reports.
		"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	cmd.Stderr = nil
	return cmd
}

type outcome struct {
	stdout, stderr string
	status         syscall.WaitStatus
}

func run(t *testing.T, cmd *exec.Cmd) outcome {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("%q: %v", cmd.Args, err)
	}
	ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	return outcome{stdout.String(), stderr.String(), ws}
}

func wantExit(t *testing.T, o outcome, code int) {
	t.Helper()
	if !o.status.Exited() || o.status.ExitStatus() != code {
		t.Errorf("status %v, want exit %d (stderr %q)", o.status, code, o.stderr)
	}
}

func TestInvocationReachesHandler(t *testing.T) {
	s := newSession(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := shimCmd(t, s.dir, "rg", "echo", "a b", "--x", "")
	cmd.Dir = dir
	cmd.Env = append(cmd.Env, "SHIM_TEST_VAR=v=1")
	o := run(t, cmd)
	wantExit(t, o, 0)
	var got echoed
	if err := json.Unmarshal([]byte(o.stdout), &got); err != nil {
		t.Fatalf("stdout %q: %v", o.stdout, err)
	}
	want := echoed{"rg", []string{filepath.Join(s.dir, "bin", "rg"), "echo", "a b", "--x", ""}, dir, "v=1", cmd.Process.Pid}
	if !slices.Equal(got.Argv, want.Argv) || got.Name != want.Name || got.Dir != want.Dir || got.Var != want.Var || got.PeerPID != want.PeerPID {
		t.Errorf("handler saw %+v, want %+v", got, want)
	}
	if o.stderr != "to stderr" {
		t.Errorf("stderr %q: only the handler may write to it", o.stderr)
	}
}

func TestExitCode(t *testing.T) {
	s := newSession(t)
	tests := []struct {
		args   []string
		code   int
		stderr string
	}{
		{[]string{"exit", "0"}, 0, ""},
		{[]string{"exit", "42"}, 42, ""},
		{[]string{"exit", "255"}, 255, ""},
		{[]string{"exit", "3", "remote: boom"}, 3, "tele: remote: boom\n"},
		{[]string{"exit", "300"}, ExitFailure, "tele: invalid exit code 300 in exit status\n"},
		{[]string{"exit", "-1"}, ExitFailure, "tele: invalid exit code -1 in exit status\n"},
		{[]string{"signal", "65"}, ExitFailure, "tele: invalid signal 65 in exit status\n"},
	}
	for _, tt := range tests {
		o := run(t, shimCmd(t, s.dir, "bash", tt.args...))
		wantExit(t, o, tt.code)
		if o.stderr != tt.stderr || o.stdout != "" {
			t.Errorf("%q: stdout %q, stderr %q; want stderr %q", tt.args, o.stdout, o.stderr, tt.stderr)
		}
	}
}

func TestSignalExit(t *testing.T) {
	s := newSession(t)
	killedBy := []unix.Signal{
		unix.SIGHUP, unix.SIGINT, unix.SIGQUIT, unix.SIGABRT, unix.SIGKILL, unix.SIGUSR1,
		unix.SIGSEGV, unix.SIGUSR2, unix.SIGPIPE, unix.SIGALRM, unix.SIGTERM, 34, 64,
	}
	for _, sig := range killedBy {
		o := run(t, shimCmd(t, s.dir, "tele-exec", "signal", strconv.Itoa(int(sig)), "why"))
		if !o.status.Signaled() || o.status.Signal() != sig || o.status.CoreDump() {
			t.Errorf("signal %d: shim status %v, want killed by it without core", sig, o.status)
		}
		if o.stderr != "tele: why\n" {
			t.Errorf("signal %d: stderr %q", sig, o.stderr)
		}
	}
	// Signals whose default action does not terminate cannot be
	// reproduced; the shim exits like a shell reports them.
	for _, sig := range []unix.Signal{unix.SIGCHLD, unix.SIGCONT, unix.SIGSTOP, unix.SIGTSTP, unix.SIGURG, unix.SIGWINCH} {
		o := run(t, shimCmd(t, s.dir, "tele-exec", "signal", strconv.Itoa(int(sig))))
		wantExit(t, o, 128+int(sig))
	}
}

func TestSignalsForwarded(t *testing.T) {
	s := newSession(t)
	for _, sig := range []unix.Signal{unix.SIGINT, unix.SIGTERM, unix.SIGHUP, unix.SIGQUIT, unix.SIGUSR1, unix.SIGUSR2, unix.SIGWINCH} {
		cmd := shimCmd(t, s.dir, "sh", "wait-signal")
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		rd := bufio.NewReader(out)
		if line, err := rd.ReadString('\n'); line != "ready\n" {
			t.Fatalf("read %q, %v", line, err)
		}
		if err := cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
		rest, _ := io.ReadAll(rd)
		if err := cmd.Wait(); err != nil {
			t.Errorf("signal %d: %v", sig, err)
		}
		if want := fmt.Sprintf("got %d\n", sig); string(rest) != want {
			t.Errorf("signal %d: handler wrote %q, want %q", sig, rest, want)
		}
	}
}

func TestKilledShimCancelsHandler(t *testing.T) {
	s := newSession(t)
	cmd := shimCmd(t, s.dir, "git", "block")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(out).ReadString('\n'); line != "ready\n" {
		t.Fatalf("read %q, %v", line, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.h.cancelled:
	case <-time.After(30 * time.Second):
		t.Fatal("handler context not cancelled after the shim was killed")
	}
	_ = cmd.Wait() // killed, as intended
}

func TestStdin(t *testing.T) {
	s := newSession(t)
	cmd := shimCmd(t, s.dir, "bash", "cat")
	cmd.Stdin = strings.NewReader("hello\n")
	o := run(t, cmd)
	wantExit(t, o, 0)
	if o.stdout != "hello\n" || o.stderr != "6 bytes" {
		t.Errorf("stdout %q, stderr %q", o.stdout, o.stderr)
	}
}

func TestClosedStdio(t *testing.T) {
	s := newSession(t)

	// A closed stdin reaches the command as /dev/null, not as whatever
	// the next open() would have produced.
	cmd := shimCmd(t, s.dir, "bash", "cat")
	cmd.Stdin = strings.NewReader("must not be read\n")
	cmd.Env = append(cmd.Env, closeFDsEnv+"=0")
	o := run(t, cmd)
	wantExit(t, o, 0)
	if o.stdout != "" || o.stderr != "0 bytes" {
		t.Errorf("closed stdin: stdout %q, stderr %q", o.stdout, o.stderr)
	}

	cmd = shimCmd(t, s.dir, "bash", "exit", "7")
	cmd.Env = append(cmd.Env, closeFDsEnv+"=0,1,2")
	o = run(t, cmd)
	wantExit(t, o, 7)
	if o.stdout != "" || o.stderr != "" {
		t.Errorf("closed stdio: stdout %q, stderr %q", o.stdout, o.stderr)
	}
}

// TestOutputAfterExit: the shim exits when the command's main process
// does, while output (of background children) may still follow.
func TestOutputAfterExit(t *testing.T) {
	s := newSession(t)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	cmd := shimCmd(t, s.dir, "bash", "late")
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	s.h.releaseLate()
	out, _ := io.ReadAll(pr)
	if string(out) != "early\nlate\n" {
		t.Errorf("output %q", out)
	}
}

func TestBadToken(t *testing.T) {
	s := newSession(t)
	if err := os.WriteFile(filepath.Join(s.dir, shimsrv.TokenFile), []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := run(t, shimCmd(t, s.dir, "bash", "echo"))
	wantExit(t, o, ExitFailure)
	if o.stdout != "" || !strings.HasPrefix(o.stderr, "tele: session token rejected: ") || strings.Count(o.stderr, "\n") != 1 {
		t.Errorf("stdout %q, stderr %q", o.stdout, o.stderr)
	}
	if n := s.h.calls.Load(); n != 0 {
		t.Errorf("handler called %d times", n)
	}
}

func TestSessionUnavailable(t *testing.T) {
	noServer, _ := newSessionDir(t)
	if err := os.WriteFile(filepath.Join(noServer, shimsrv.TokenFile), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	noToken, _ := newSessionDir(t)
	tests := []struct {
		env     string // TELE_SESSION; "-" leaves it unset
		wantErr string
	}{
		{"-", "tele: TELE_SESSION is not set"},
		{"/tmp", "is not a session directory"},
		{noToken, "tele: session token: open"},
		{noServer, "tele: cannot reach tele session"},
	}
	for _, tt := range tests {
		cmd := shimCmd(t, noServer, "bash", "echo")
		cmd.Env = slices.DeleteFunc(cmd.Env, func(kv string) bool { return strings.HasPrefix(kv, SessionEnv+"=") })
		if tt.env != "-" {
			cmd.Env = append(cmd.Env, SessionEnv+"="+tt.env)
		}
		o := run(t, cmd)
		wantExit(t, o, ExitFailure)
		if o.stdout != "" || !strings.Contains(o.stderr, tt.wantErr) || strings.Count(o.stderr, "\n") != 1 {
			t.Errorf("TELE_SESSION=%s: stdout %q, stderr %q, want one line with %q", tt.env, o.stdout, o.stderr, tt.wantErr)
		}
	}
}

// TestConnectionLostBeforeExit uses a bare listener that accepts the
// request and hangs up without an exit status.
func TestConnectionLostBeforeExit(t *testing.T) {
	dir, sid := newSessionDir(t)
	if err := os.WriteFile(filepath.Join(dir, shimsrv.TokenFile), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := shimsrv.Listen(sid)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	served := make(chan error, 1)
	go func() { served <- acceptAndHangUp(ln) }()

	o := run(t, shimCmd(t, dir, "uname", "-a"))
	wantExit(t, o, ExitFailure)
	want := fmt.Sprintf("tele: tele session %s closed the connection without an exit status: it ended, or refused the command; see %s\n",
		sid, filepath.Join(dir, shimsrv.LogFile))
	if o.stderr != want {
		t.Errorf("stderr %q, want %q", o.stderr, want)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func acceptAndHangUp(ln *net.UnixListener) error {
	conn, err := acceptStdio(ln)
	if err != nil {
		return err
	}
	defer conn.Close()
	var req proto.ShimRequest
	if err := proto.ReadFrame(conn, &req, proto.MaxShimRequest); err != nil {
		return err
	}
	if req.Name != "uname" || string(req.Token) != "t" {
		return fmt.Errorf("got request %+v", req)
	}
	return nil
}

// acceptStdio accepts a shim and takes the three fds it sends.
func acceptStdio(ln *net.UnixListener) (*net.UnixConn, error) {
	conn, err := ln.AcceptUnix()
	if err != nil {
		return nil, err
	}
	oob := make([]byte, unix.CmsgSpace(3*4))
	_, oobn, _, _, err := conn.ReadMsgUnix(make([]byte, 1), oob)
	if err == nil {
		err = closeRights(oob[:oobn])
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func closeRights(oob []byte) error {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil || len(msgs) != 1 {
		return fmt.Errorf("control messages %v: %w", msgs, err)
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil {
		return err
	}
	for _, fd := range fds {
		_ = unix.Close(fd)
	}
	if len(fds) != 3 {
		return fmt.Errorf("got %d fds", len(fds))
	}
	return nil
}

// TestRefusalReason: a session main that refuses the request in the middle
// of it, and hangs up, still gets its reason to the user. The request is
// larger than the socket buffer, so the shim is still writing it.
func TestRefusalReason(t *testing.T) {
	dir, sid := newSessionDir(t)
	if err := os.WriteFile(filepath.Join(dir, shimsrv.TokenFile), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := shimsrv.Listen(sid)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	served := make(chan error, 1)
	go func() {
		conn, err := acceptStdio(ln)
		if err != nil {
			served <- err
			return
		}
		defer conn.Close() // with most of the request unread
		var hdr [4]byte
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			served <- err
			return
		}
		served <- proto.WriteFrame(conn, &proto.ShimFrame{Op: proto.ShimExit, Exit: &proto.ShimStatus{Code: 255, Msg: "refused: too large"}})
	}()

	o := run(t, shimCmd(t, dir, "git", fillerArgs(1<<20)...))
	wantExit(t, o, ExitFailure)
	if o.stderr != "tele: refused: too large\n" {
		t.Errorf("stderr %q", o.stderr)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

// fillerArgs returns arguments of about n bytes in total.
func fillerArgs(n int) []string {
	args := make([]string, n/100)
	for i := range args {
		args[i] = strings.Repeat("a", 99)
	}
	return args
}

// TestLargeInvocation: an argv larger than other control messages may be
// reaches the handler.
func TestLargeInvocation(t *testing.T) {
	const size = proto.MaxControlFrame + 256<<10
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_STACK, &lim); err != nil {
		t.Fatal(err)
	}
	// execve(2) takes at most a quarter of the stack limit for argv and env.
	if lim.Cur != unix.RLIM_INFINITY && lim.Cur/4 < size+size/10+64<<10 {
		t.Skipf("RLIMIT_STACK %d too small to exec %d bytes of arguments", lim.Cur, size)
	}
	s := newSession(t)
	args := append([]string{"count"}, fillerArgs(size)...)
	o := run(t, shimCmd(t, s.dir, "rg", args...))
	wantExit(t, o, 0)
	if want := strconv.Itoa(len(args) + 1); o.stdout != want {
		t.Errorf("handler saw %s arguments, want %s", o.stdout, want)
	}
}

// TestRequestTooLarge: a request session main would refuse is not sent.
func TestRequestTooLarge(t *testing.T) {
	dir, sid := newSessionDir(t)
	if err := os.WriteFile(filepath.Join(dir, shimsrv.TokenFile), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHIM_TEST_HUGE", strings.Repeat("x", proto.MaxShimRequest))
	_, err := (&session{dir: dir, id: sid}).request("bash", []string{"bash"})
	if err == nil || !strings.Contains(err.Error(), "too long to forward") {
		t.Errorf("request = %v, want too long", err)
	}
}

// TestDirIsClean: the working directory sent is getcwd(2)'s, even when
// $PWD names the same directory in an unclean form.
func TestDirIsClean(t *testing.T) {
	s := newSession(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := shimCmd(t, s.dir, "bash", "echo")
	cmd.Dir = dir
	cmd.Env = append(cmd.Env, "PWD="+dir+"/./")
	o := run(t, cmd)
	wantExit(t, o, 0)
	var got echoed
	if err := json.Unmarshal([]byte(o.stdout), &got); err != nil || got.Dir != dir {
		t.Errorf("handler saw dir %q (%v), want %q", got.Dir, err, dir)
	}
}

// TestDialRetriesFullBacklog: a full listen backlog makes connect(2) fail
// with EAGAIN at once; dialUnix keeps trying until its context ends.
func TestDialRetriesFullBacklog(t *testing.T) {
	_, sid := newSessionDir(t)
	addr := shimsrv.SocketName(sid)
	ln := listenBacklog0(t, addr)
	var queued []*net.UnixConn
	defer func() {
		for _, c := range queued {
			_ = c.Close()
		}
	}()
	for {
		c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: addr, Net: "unix"})
		if errors.Is(err, unix.EAGAIN) {
			break
		}
		if err != nil || len(queued) > 64 {
			t.Fatalf("filling the backlog: %v after %d connections", err, len(queued))
		}
		queued = append(queued, c)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err := dialUnix(ctx, addr)
	ended := ctx.Err()
	cancel()
	if !errors.Is(err, unix.EAGAIN) || ended == nil {
		t.Fatalf("dialUnix = %v with the context %v; want EAGAIN after the context ended", err, ended)
	}

	// Once session main accepts again, a waiting dial gets through.
	dialed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := dialUnix(ctx, addr)
		if err == nil {
			_ = c.Close()
		}
		dialed <- err
	}()
	for range queued {
		c, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
	}
	if err := <-dialed; err != nil {
		t.Errorf("dialUnix after the backlog drained: %v", err)
	}
}

// listenBacklog0 listens on the abstract address addr with the smallest
// backlog, which net.Listen does not offer.
func listenBacklog0(t *testing.T, addr string) *net.UnixListener {
	t.Helper()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), "listener")
	defer f.Close() // net.FileListener dups it
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: addr}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	fl, err := net.FileListener(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fl.Close() })
	ln, ok := fl.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener type %T", fl)
	}
	return ln
}

func TestCheckListener(t *testing.T) {
	if err := checkListener(&unix.Ucred{Pid: 1, Uid: 1000}, 1000); err != nil {
		t.Errorf("same uid rejected: %v", err)
	}
	for _, uid := range []uint32{0, 999, 1001, 65534} {
		if err := checkListener(&unix.Ucred{Pid: 1, Uid: uid}, 1000); err == nil {
			t.Errorf("listener of uid %d accepted for 1000", uid)
		}
	}
}

// nobody is the uid the foreign listener runs as.
const nobody = 65534

// listenAsNobody listens on the abstract address args[0] as uid nobody,
// reports "ready", then accepts one connection and reports how many bytes
// arrived on it.
func listenAsNobody(args []string) int {
	if err := unix.Setresgid(nobody, nobody, nobody); err != nil {
		return fail(err)
	}
	if err := unix.Setresuid(nobody, nobody, nobody); err != nil {
		return fail(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: args[0], Net: "unix"})
	if err != nil {
		return fail(err)
	}
	defer ln.Close()
	fmt.Println("ready")
	conn, err := ln.AcceptUnix()
	if err != nil {
		return fail(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	n, _ := io.Copy(io.Discard, conn)
	fmt.Println(n)
	return 0
}

// TestForeignListener: a socket another user bound at the session's
// address gets neither the stdio fds nor the request.
func TestForeignListener(t *testing.T) {
	privtest.RequireRoot(t)
	dir, sid := newSessionDir(t)
	if err := os.WriteFile(filepath.Join(dir, shimsrv.TokenFile), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener := helperproc.Command(t, "listen-as-nobody", shimsrv.SocketName(sid))
	listener.Env = append(listener.Env, "GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	out, err := listener.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Start(); err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(out)
	if line, err := rd.ReadString('\n'); line != "ready\n" {
		t.Fatalf("listener: %q, %v", line, err)
	}

	o := run(t, shimCmd(t, dir, "bash", "echo"))
	wantExit(t, o, ExitFailure)
	if want := fmt.Sprintf("tele: tele session %s: its socket belongs to uid %d, not %d", sid, nobody, os.Getuid()); !strings.HasPrefix(o.stderr, want) {
		t.Errorf("stderr %q, want it to start with %q", o.stderr, want)
	}
	received, _ := rd.ReadString('\n')
	if err := listener.Wait(); err != nil {
		t.Fatal(err)
	}
	if received != "0\n" {
		t.Errorf("the foreign listener received %q bytes", strings.TrimSpace(received))
	}
}

func TestConcurrentShims(t *testing.T) {
	s := newSession(t)
	var wg sync.WaitGroup
	for i := range 16 {
		cmd := shimCmd(t, s.dir, "rg", "exit", strconv.Itoa(i))
		wg.Go(func() {
			err := cmd.Run()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != i {
				t.Errorf("shim %d: %v", i, err)
			}
		})
	}
	wg.Wait()
}
