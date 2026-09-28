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
)

// closeFDsEnv makes the shim helper close the listed fds before Main runs,
// as if Claude had started it with them closed.
const closeFDsEnv = "SHIM_TEST_CLOSE_FDS"

func TestMain(m *testing.M) {
	helperproc.Register("shim", shimHelper)
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

// session is a session directory with a shimsrv.Server behind it.
type session struct {
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

func newSession(t *testing.T) *session {
	t.Helper()
	dir, sid := newSessionDir(t)
	s := &session{dir: dir, token: []byte("secret-token\n"), h: newFakeHandler()}
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
	if o.stdout != "" || o.stderr != "tele: session token rejected\n" {
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
	if o.stderr != "tele: tele session closed the connection before the command finished\n" {
		t.Errorf("stderr %q", o.stderr)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func acceptAndHangUp(ln *net.UnixListener) error {
	conn, err := ln.AcceptUnix()
	if err != nil {
		return err
	}
	defer conn.Close()
	oob := make([]byte, unix.CmsgSpace(3*4))
	_, oobn, _, _, err := conn.ReadMsgUnix(make([]byte, 1), oob)
	if err != nil {
		return err
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
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
	var req proto.ShimRequest
	if err := proto.ReadFrame(conn, &req, proto.MaxControlFrame); err != nil {
		return err
	}
	if len(fds) != 3 || req.Name != "uname" || string(req.Token) != "t" {
		return fmt.Errorf("got %d fds and request %+v", len(fds), req)
	}
	return nil
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
