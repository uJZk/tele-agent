package rexec

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/execsvc"
	"github.com/ujzk/tele-agent/internal/mux"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/scratch"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

const testTimeout = 30 * time.Second

type fakeSyncer struct{ seq atomic.Uint64 }

func (f *fakeSyncer) Sync() uint64 { return f.seq.Add(1) }

type env struct {
	client  *Client
	scratch string // the server's scratch directory
	syncer  *fakeSyncer
}

// muxPair returns both ends of a mux session over net.Pipe; serve handles
// every stream the client opens.
func muxPair(t *testing.T, serve func(net.Conn)) *mux.Session {
	t.Helper()
	a, b := net.Pipe()
	cs, err := mux.Client(a)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := mux.Server(b)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			st, err := ss.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				if _, err := mux.ReadKind(st); err != nil {
					_ = st.Close()
					return
				}
				serve(st)
			})
		}
	})
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
		wg.Wait()
	})
	return cs
}

// newEnv serves a real execsvc.Service to a Client.
func newEnv(t *testing.T, barrier Barrier) *env {
	t.Helper()
	home := t.TempDir()
	e := &env{scratch: filepath.Join(home, "scratch"), syncer: &fakeSyncer{}}
	svc, err := execsvc.New(execsvc.Config{
		Target:     proto.TargetInfo{User: "u", Home: home, Shell: "/bin/sh", LoginPath: "/usr/local/bin:/usr/bin:/bin"},
		ScratchDir: e.scratch,
		Syncer:     e.syncer,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cs := muxPair(t, func(c net.Conn) { _ = svc.Serve(ctx, c) })
	// Registered after muxPair, so it runs first.
	t.Cleanup(func() {
		cancel()
		_ = svc.Close()
	})
	e.client = &Client{Opener: cs, Barrier: barrier}
	return e
}

// capture collects everything written to a pipe until its write end is
// closed everywhere.
type capture struct {
	w    *os.File
	done chan struct{}
	buf  bytes.Buffer
}

func newCapture(t *testing.T) *capture {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c := &capture{w: w, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		_, _ = io.Copy(&c.buf, r)
		_ = r.Close()
	}()
	t.Cleanup(func() {
		_ = c.w.Close()
		<-c.done
	})
	return c
}

// finish closes the test's write end and returns everything captured.
func (c *capture) finish() string {
	_ = c.w.Close()
	<-c.done
	return c.buf.String()
}

func waitDone(t *testing.T, p *Process) {
	t.Helper()
	select {
	case <-p.Done():
	case <-time.After(testTimeout):
		t.Fatal("process not done")
	}
}

type ran struct {
	res            Result
	stdout, stderr string
}

func runCmd(t *testing.T, e *env, cmd Command) ran {
	t.Helper()
	out, errOut := newCapture(t), newCapture(t)
	cmd.Stdout, cmd.Stderr = out.w, errOut.w
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	p, err := e.client.Start(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, p)
	return ran{res: res, stdout: out.finish(), stderr: errOut.finish()}
}

func sh(script string, args ...string) []string {
	return append([]string{"sh", "-c", script, "sh"}, args...)
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return
		}
		if i := bytes.LastIndexByte(data, ')'); i >= 0 && i+2 < len(data) && data[i+2] == 'Z' {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d still running", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExitStatus(t *testing.T) {
	e := newEnv(t, nil)
	tests := []struct {
		script       string
		code, signal int
	}{
		{"exit 0", 0, 0},
		{"exit 42", 42, 0},
		{"kill -9 $$", 0, 9},
	}
	for _, tt := range tests {
		r := runCmd(t, e, Command{Argv: sh(tt.script)})
		if r.res.StartErr != nil || r.res.Code != tt.code || r.res.Signal != tt.signal {
			t.Errorf("%q: %+v, want code %d signal %d", tt.script, r.res, tt.code, tt.signal)
		}
	}
}

func TestOutputSeparate(t *testing.T) {
	e := newEnv(t, nil)
	r := runCmd(t, e, Command{Argv: sh("echo out; echo err >&2")})
	if r.stdout != "out\n" || r.stderr != "err\n" {
		t.Fatalf("stdout %q stderr %q", r.stdout, r.stderr)
	}
	// Output to nil files is dropped.
	p, err := e.client.Start(t.Context(), Command{Argv: sh("echo out; echo err >&2"), Dir: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := p.Wait(t.Context()); err != nil || res.Code != 0 {
		t.Fatalf("Wait = %+v, %v", res, err)
	}
	waitDone(t, p)
}

func TestStartErr(t *testing.T) {
	e := newEnv(t, nil)
	r := runCmd(t, e, Command{Argv: []string{"tele-no-such-command"}})
	if r.res.StartErr == nil || !errors.Is(r.res.StartErr, unix.ENOENT) {
		t.Fatalf("StartErr = %v, want ENOENT", r.res.StartErr)
	}
	if !r.res.ScratchWritten {
		t.Error("ScratchWritten unset for a command that failed to start")
	}
	if _, err := e.client.Start(t.Context(), Command{}); err == nil {
		t.Fatal("Start accepted an empty command")
	}
}

func TestSignal(t *testing.T) {
	e := newEnv(t, nil)
	p, err := e.client.Start(t.Context(), Command{Argv: []string{"sleep", "1000"}, Dir: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Signal(t.Context(), 0); err == nil {
		t.Fatal("Signal(0) accepted")
	}
	if err := p.Signal(t.Context(), int(unix.SIGTERM)); err != nil {
		t.Fatal(err)
	}
	res, err := p.Wait(t.Context())
	if err != nil || res.Signal != int(unix.SIGTERM) {
		t.Fatalf("Wait = %+v, %v; want signal 15", res, err)
	}
	if err := p.Signal(t.Context(), int(unix.SIGTERM)); !errors.Is(err, ErrFinished) {
		t.Fatalf("Signal after exit = %v, want ErrFinished", err)
	}
	waitDone(t, p)
}

func TestStdin(t *testing.T) {
	e := newEnv(t, nil)
	want := make([]byte, 1<<20+5)
	_, _ = rand.Read(want)

	nonblocking := func(t *testing.T) (*os.File, *os.File) {
		r, w, err := os.Pipe() // Go makes both ends non-blocking
		if err != nil {
			t.Fatal(err)
		}
		return r, w
	}
	blocking := func(t *testing.T) (*os.File, *os.File) {
		var fds [2]int
		if err := unix.Pipe2(fds[:], unix.O_CLOEXEC); err != nil {
			t.Fatal(err)
		}
		return os.NewFile(uintptr(fds[0]), "r"), os.NewFile(uintptr(fds[1]), "w")
	}
	socket := func(t *testing.T) (*os.File, *os.File) {
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		return os.NewFile(uintptr(fds[0]), "r"), os.NewFile(uintptr(fds[1]), "w")
	}
	for name, mk := range map[string]func(*testing.T) (*os.File, *os.File){
		"nonblocking pipe": nonblocking, "blocking pipe": blocking, "socketpair": socket,
	} {
		t.Run(name, func(t *testing.T) {
			in, feed := mk(t)
			defer func() { _ = in.Close() }()
			out, stdoutFeed := mk(t)
			defer func() { _ = out.Close() }()
			flags := func() [2]int {
				return [2]int{fcntlFlags(t, in), fcntlFlags(t, stdoutFeed)}
			}
			before := flags()

			var wg sync.WaitGroup
			defer wg.Wait()
			wg.Go(func() {
				_, _ = feed.Write(want)
				_ = feed.Close()
			})
			var got bytes.Buffer
			wg.Go(func() { _, _ = io.Copy(&got, out) })

			p, err := e.client.Start(t.Context(), Command{Argv: []string{"cat"}, Dir: "/", Stdin: in, Stdout: stdoutFeed})
			if err != nil {
				t.Fatal(err)
			}
			res, err := p.Wait(t.Context())
			if err != nil || res.Code != 0 {
				t.Fatalf("Wait = %+v, %v", res, err)
			}
			waitDone(t, p)
			if after := flags(); after != before {
				t.Fatalf("fd flags changed from %#x to %#x", before, after)
			}
			_ = stdoutFeed.Close()
			wg.Wait()
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("cat returned %d bytes, want %d intact", got.Len(), len(want))
			}
		})
	}

	// A nil Stdin is empty input.
	r := runCmd(t, e, Command{Argv: []string{"cat"}})
	if r.res.Code != 0 || r.stdout != "" {
		t.Fatalf("cat with nil stdin: %+v %q", r.res, r.stdout)
	}
}

func fcntlFlags(t *testing.T, f *os.File) int {
	t.Helper()
	rc, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var ferr error
	if err := rc.Control(func(fd uintptr) { flags, ferr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
		t.Fatal(err)
	}
	if ferr != nil {
		t.Fatal(ferr)
	}
	return flags
}

func TestStdinNotReadAfterExit(t *testing.T) {
	e := newEnv(t, nil)
	in, feed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close(); _ = feed.Close() }()
	p, err := e.client.Start(t.Context(), Command{Argv: []string{"true"}, Dir: "/", Stdin: in})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitDone(t, p)
	// Input written after the command exited belongs to the next reader.
	if _, err := feed.WriteString("later"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := in.Read(buf)
	if err != nil || string(buf[:n]) != "later" {
		t.Fatalf("next reader got %q, %v", buf[:n], err)
	}
}

func TestLargeOutput(t *testing.T) {
	e := newEnv(t, nil)
	data := make([]byte, 3<<20+11)
	_, _ = rand.Read(data)
	name := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r := runCmd(t, e, Command{Argv: sh(`cat "$1"; cat "$1" >&2`, name)})
	if r.res.Code != 0 || r.stdout != string(data) || r.stderr != string(data) {
		t.Fatalf("exit %+v, stdout %d stderr %d bytes, want %d intact", r.res, len(r.stdout), len(r.stderr), len(data))
	}
}

func TestOutputToRegularFile(t *testing.T) {
	// Claude hands background tasks a regular file as stdout.
	e := newEnv(t, nil)
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	p, err := e.client.Start(t.Context(), Command{Argv: sh("head -c 100000 /dev/zero; echo end"), Dir: "/", Stdout: f})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Everything the command wrote is in the file once Wait returns.
	data, err := os.ReadFile(f.Name())
	if err != nil || len(data) != 100004 || !strings.HasSuffix(string(data), "end\n") {
		t.Fatalf("file has %d bytes, %v; want 100004", len(data), err)
	}
	waitDone(t, p)
}

func TestLocalReaderGone(t *testing.T) {
	e := newEnv(t, nil)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	defer func() { _ = w.Close() }()
	errOut := newCapture(t)
	p, err := e.client.Start(t.Context(), Command{Argv: sh("head -c 1000000 /dev/zero; echo done >&2"), Dir: "/", Stdout: w, Stderr: errOut.w})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Wait(t.Context())
	if err != nil || res.Code != 0 {
		t.Fatalf("Wait = %+v, %v", res, err)
	}
	waitDone(t, p)
	if got := errOut.finish(); got != "done\n" {
		t.Fatalf("stderr %q after stdout broke", got)
	}
}

func TestBackgroundChildOutput(t *testing.T) {
	e := newEnv(t, nil)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	out := newCapture(t)
	p, err := e.client.Start(t.Context(), Command{
		Argv:   sh(`echo early; { read x < "$1"; echo "late $x"; } &`, fifo),
		Dir:    "/",
		Stdout: out.w,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Wait(t.Context())
	if err != nil || res.Code != 0 {
		t.Fatalf("Wait = %+v, %v", res, err)
	}
	select {
	case <-p.Done():
		t.Fatal("Done while a background child holds stdout")
	default:
	}
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString("x\n")
	_ = w.Close()
	waitDone(t, p)
	if got := out.finish(); got != "early\nlate x\n" {
		t.Fatalf("stdout %q", got)
	}
}

func TestAbandonKillsProcessGroup(t *testing.T) {
	e := newEnv(t, nil)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	in, feed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close(); _ = feed.Close() }()
	p, err := e.client.Start(t.Context(), Command{Argv: sh("sleep 1000 & echo $$ $!; wait"), Dir: "/", Stdin: in, Stdout: w})
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for f := range strings.FieldsSeq(line) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	p.Abandon()
	p.Abandon()
	waitDone(t, p)
	if _, err := p.Wait(t.Context()); !errors.Is(err, ErrAbandoned) {
		t.Fatalf("Wait after Abandon = %v, want ErrAbandoned", err)
	}
	if err := p.Signal(t.Context(), int(unix.SIGTERM)); !errors.Is(err, ErrAbandoned) {
		t.Fatalf("Signal after Abandon = %v, want ErrAbandoned", err)
	}
	for _, pid := range pids {
		waitGone(t, pid)
	}
	p.Abandon() // after Done: still harmless
}

// recordingBarrier blocks WaitApplied until released.
type recordingBarrier struct {
	called  chan uint64
	release chan struct{}
}

func (b *recordingBarrier) WaitApplied(ctx context.Context, seq uint64) error {
	b.called <- seq
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestBarrier(t *testing.T) {
	b := &recordingBarrier{called: make(chan uint64, 4), release: make(chan struct{})}
	e := newEnv(t, b)
	for want := uint64(1); want <= 2; want++ {
		p, err := e.client.Start(t.Context(), Command{Argv: []string{"true"}, Dir: "/"})
		if err != nil {
			t.Fatal(err)
		}
		type waited struct {
			res Result
			err error
		}
		ch := make(chan waited, 1)
		go func() {
			res, err := p.Wait(t.Context())
			ch <- waited{res, err}
		}()
		if seq := <-b.called; seq != want {
			t.Fatalf("WaitApplied(%d), want %d", seq, want)
		}
		select {
		case w := <-ch:
			t.Fatalf("Wait returned %+v before the barrier", w)
		default:
		}
		b.release <- struct{}{}
		if w := <-ch; w.err != nil || w.res.Code != 0 {
			t.Fatalf("Wait = %+v, %v", w.res, w.err)
		}
		waitDone(t, p)
	}

	// A cancelled Wait reports the context error and leaves the command.
	p, err := e.client.Start(t.Context(), Command{Argv: []string{"true"}, Dir: "/"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-b.called
		cancel()
	}()
	if _, err := p.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait with cancelled barrier = %v", err)
	}
	waitDone(t, p)
}

func TestPTY(t *testing.T) {
	e := newEnv(t, nil)
	in, feed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close(); _ = feed.Close() }()
	out := newCapture(t)
	p, err := e.client.Start(t.Context(), Command{
		Argv:   sh("test -t 0 && test -t 1 && stty size && read x && stty size"),
		Dir:    "/",
		TTY:    &proto.TTYSize{Rows: 12, Cols: 34},
		Stdin:  in,
		Stdout: out.w,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Resize(t.Context(), proto.TTYSize{Rows: 56, Cols: 78}); err != nil {
		t.Fatal(err)
	}
	if _, err := feed.WriteString("go\n"); err != nil {
		t.Fatal(err)
	}
	res, err := p.Wait(t.Context())
	if err != nil || res.Code != 0 {
		t.Fatalf("Wait = %+v, %v", res, err)
	}
	waitDone(t, p)
	got := out.finish()
	// The first size may already be the new one if the resize won the race
	// with the first stty; the second must be.
	if !strings.HasSuffix(got, "56 78\r\n") || !strings.Contains(got, "go\r\n") {
		t.Fatalf("pty output %q", got)
	}
}

func TestScratchRoundTrip(t *testing.T) {
	e := newEnv(t, nil)
	local := t.TempDir()
	remoteTmp := filepath.Join(e.scratch, "tmp")
	m, err := scratch.New([]scratch.Area{{
		ID: proto.ScratchTmp, ClaudePath: "/.tele/0123456789abcdef/tmp", LocalPath: local, RemotePath: remoteTmp,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "in"), []byte("from claude\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "old"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The files reach the target with the first command.
	cmd := Command{Argv: []string{"true"}, Dir: "/"}
	up := m.Uploads(ScratchBudget(cmd))
	if len(up.Files) != 2 {
		t.Fatalf("uploads = %+v", up.Files)
	}
	cmd.Scratch = up.Files
	r := runCmd(t, e, cmd)
	if r.res.Code != 0 || len(r.res.Scratch) != 0 {
		t.Fatalf("first command: %+v", r.res)
	}
	up.Commit()

	script := m.Rewrite(`cat /.tele/0123456789abcdef/tmp/in && pwd -P >| /.tele/0123456789abcdef/tmp/cwd && rm /.tele/0123456789abcdef/tmp/old`)
	r = runCmd(t, e, Command{Argv: sh(script), Dir: "/tmp"})
	if r.res.Code != 0 || r.stdout != "from claude\n" {
		t.Fatalf("command: %+v stdout %q stderr %q", r.res, r.stdout, r.stderr)
	}
	if err := m.Apply(r.res.Scratch); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(local, "cwd")); err != nil || string(data) != "/tmp\n" {
		t.Fatalf("cwd file %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(local, "old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file deleted remotely still exists locally: %v", err)
	}
	up = m.Uploads(proto.MaxDataFrame)
	defer up.Rollback()
	if len(up.Files) != 0 {
		t.Fatalf("Uploads after Apply = %+v", up.Files)
	}
}

// fakeServer runs frames through a hand-written server.
func fakeServer(t *testing.T, serve func(c *proto.Conn)) *Client {
	t.Helper()
	cs := muxPair(t, func(st net.Conn) {
		c := proto.NewConn(st, proto.MaxDataFrame)
		var start proto.ExecStart
		if err := c.Recv(&start); err != nil {
			_ = st.Close()
			return
		}
		serve(c)
		_ = st.Close()
		// Drain until the client closes its side.
		for {
			var f proto.ExecFrame
			if c.Recv(&f) != nil {
				return
			}
		}
	})
	return &Client{Opener: cs}
}

func TestProtocolFailures(t *testing.T) {
	tests := []struct {
		name  string
		serve func(c *proto.Conn)
		want  error
	}{
		{"no exit", func(c *proto.Conn) {
			_ = c.Send(&proto.ExecFrame{Op: proto.ExecStarted, PID: 1})
		}, ErrNoExit},
		{"unexpected op", func(c *proto.Conn) {
			_ = c.Send(&proto.ExecFrame{Op: proto.ExecStdin, Data: []byte("x")})
		}, nil},
		{"exit without status", func(c *proto.Conn) {
			_ = c.Send(&proto.ExecFrame{Op: proto.ExecExit})
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fakeServer(t, tt.serve)
			p, err := c.Start(t.Context(), Command{Argv: []string{"x"}, Dir: "/"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Wait(t.Context())
			if err == nil || tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("Wait = %v, want %v", err, tt.want)
			}
			waitDone(t, p)
		})
	}
}

func TestStartCancelled(t *testing.T) {
	e := newEnv(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.client.Start(ctx, Command{Argv: []string{"true"}, Dir: "/"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start with cancelled context = %v", err)
	}
}
