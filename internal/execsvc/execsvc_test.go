package execsvc

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/mux"
	"github.com/ujzk/tele-agent/internal/proto"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

const testTimeout = 30 * time.Second

type fakeSyncer struct {
	seq  atomic.Uint64
	hook func()
}

func (f *fakeSyncer) Sync() uint64 {
	if f.hook != nil {
		f.hook()
	}
	return f.seq.Add(1)
}

type harness struct {
	svc     *Service
	client  *mux.Session
	scratch string
	syncer  *fakeSyncer
	cancel  context.CancelFunc
}

// newHarness serves execsvc over a real mux session on net.Pipe.
func newHarness(t *testing.T, adjust func(*Config)) *harness {
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
	home := t.TempDir()
	h := &harness{client: cs, scratch: filepath.Join(home, "scratch"), syncer: &fakeSyncer{}}
	cfg := Config{
		Target:     proto.TargetInfo{User: "u", Home: home, Shell: "/bin/sh", LoginPath: "/usr/local/bin:/usr/bin:/bin"},
		ScratchDir: h.scratch,
		Syncer:     h.syncer,
	}
	if adjust != nil {
		adjust(&cfg)
	}
	h.scratch = cfg.ScratchDir
	h.svc, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
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
				_ = h.svc.Serve(ctx, st)
			})
		}
	})
	t.Cleanup(func() {
		cancel()
		_ = h.svc.Close()
		_ = cs.Close()
		_ = ss.Close()
		wg.Wait()
	})
	return h
}

type stream struct {
	t    *testing.T
	raw  net.Conn
	conn *proto.Conn
}

func (h *harness) start(t *testing.T, start *proto.ExecStart) *stream {
	t.Helper()
	raw, err := h.client.Open(proto.StreamExec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if err := raw.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	s := &stream{t: t, raw: raw, conn: proto.NewConn(raw, proto.MaxDataFrame)}
	if start.Dir == "" {
		start.Dir = "/"
	}
	if err := s.conn.Send(start); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *stream) send(f *proto.ExecFrame) {
	s.t.Helper()
	if err := s.conn.Send(f); err != nil {
		s.t.Fatal(err)
	}
}

// outcome is everything the server sent on one stream.
type outcome struct {
	pid            int
	stdout, stderr bytes.Buffer
	exit           *proto.ExecStatus
	// Output bytes received when ExecExit arrived.
	stdoutAtExit, stderrAtExit int
}

// next reads one frame into o; it returns false at the end of the stream.
func (s *stream) next(o *outcome) bool {
	s.t.Helper()
	var f proto.ExecFrame
	if err := s.conn.Recv(&f); err != nil {
		if !errors.Is(err, io.EOF) {
			s.t.Fatalf("recv: %v", err)
		}
		return false
	}
	switch f.Op {
	case proto.ExecStarted:
		o.pid = f.PID
	case proto.ExecStdout:
		o.stdout.Write(f.Data)
	case proto.ExecStderr:
		o.stderr.Write(f.Data)
	case proto.ExecExit:
		if o.exit != nil {
			s.t.Fatal("second ExecExit")
		}
		o.exit = f.Exit
		o.stdoutAtExit, o.stderrAtExit = o.stdout.Len(), o.stderr.Len()
	default:
		s.t.Fatalf("unexpected frame op %d", f.Op)
	}
	return true
}

func (s *stream) untilExit(o *outcome) {
	s.t.Helper()
	for o.exit == nil {
		if !s.next(o) {
			s.t.Fatal("stream ended without ExecExit")
		}
	}
}

func (s *stream) collect() *outcome {
	s.t.Helper()
	o := &outcome{}
	for s.next(o) {
	}
	if o.exit == nil {
		s.t.Fatal("stream ended without ExecExit")
	}
	return o
}

func run(t *testing.T, h *harness, start *proto.ExecStart) *outcome {
	t.Helper()
	s := h.start(t, start)
	s.send(&proto.ExecFrame{Op: proto.ExecStdinEOF})
	return s.collect()
}

func sh(script string, args ...string) []string {
	return append([]string{"sh", "-c", script, "sh"}, args...)
}

// waitGone waits until pid has exited (it may linger as a zombie of an
// init that does not reap).
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
	h := newHarness(t, nil)
	tests := []struct {
		script       string
		code, signal int
	}{
		{"exit 0", 0, 0},
		{"exit 3", 3, 0},
		{"exit 255", 255, 0},
		{"kill -9 $$", 0, 9},
		{"kill -TERM $$", 0, 15},
	}
	for _, tt := range tests {
		o := run(t, h, &proto.ExecStart{Argv: sh(tt.script)})
		if o.exit.Err != nil || o.exit.Code != tt.code || o.exit.Signal != tt.signal {
			t.Errorf("%q: exit %+v, want code %d signal %d", tt.script, o.exit, tt.code, tt.signal)
		}
		if o.pid == 0 {
			t.Errorf("%q: no ExecStarted", tt.script)
		}
	}
}

func TestStdoutStderrSeparate(t *testing.T) {
	h := newHarness(t, nil)
	o := run(t, h, &proto.ExecStart{Argv: sh("echo out; echo err >&2; echo out2")})
	if o.stdout.String() != "out\nout2\n" || o.stderr.String() != "err\n" {
		t.Fatalf("stdout %q stderr %q", o.stdout.String(), o.stderr.String())
	}
}

func TestSignal(t *testing.T) {
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: []string{"sleep", "1000"}})
	o := &outcome{}
	for o.pid == 0 {
		if !s.next(o) {
			t.Fatal("no ExecStarted")
		}
	}
	// Out-of-range signals are ignored.
	s.send(&proto.ExecFrame{Op: proto.ExecSignal, Signal: 0})
	s.send(&proto.ExecFrame{Op: proto.ExecSignal, Signal: 65})
	s.send(&proto.ExecFrame{Op: proto.ExecSignal, Signal: int(unix.SIGTERM)})
	for s.next(o) {
	}
	if o.exit == nil || o.exit.Signal != int(unix.SIGTERM) || o.exit.Code != 0 {
		t.Fatalf("exit %+v, want signal 15", o.exit)
	}
}

func TestStdin(t *testing.T) {
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: []string{"cat"}})
	want := make([]byte, 1<<20+123)
	_, _ = rand.Read(want)
	for rest := want; len(rest) > 0; {
		n := min(len(rest), 40000)
		s.send(&proto.ExecFrame{Op: proto.ExecStdin, Data: rest[:n]})
		rest = rest[n:]
	}
	s.send(&proto.ExecFrame{Op: proto.ExecStdinEOF})
	o := s.collect()
	if o.exit.Code != 0 || !bytes.Equal(o.stdout.Bytes(), want) {
		t.Fatalf("exit %+v, stdout %d bytes equal=%v", o.exit, o.stdout.Len(), bytes.Equal(o.stdout.Bytes(), want))
	}
}

func TestLargeOutput(t *testing.T) {
	h := newHarness(t, nil)
	data := make([]byte, 3<<20+7)
	_, _ = rand.Read(data)
	name := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
	o := run(t, h, &proto.ExecStart{Argv: sh(`cat "$1"; cat "$1" >&2`, name)})
	if o.exit.Code != 0 || !bytes.Equal(o.stdout.Bytes(), data) || !bytes.Equal(o.stderr.Bytes(), data) {
		t.Fatalf("exit %+v, stdout %d stderr %d bytes, want %d intact", o.exit, o.stdout.Len(), o.stderr.Len(), len(data))
	}
	// Without background children, all output precedes ExecExit.
	if o.stdoutAtExit != len(data) || o.stderrAtExit != len(data) {
		t.Fatalf("at ExecExit: stdout %d stderr %d, want %d", o.stdoutAtExit, o.stderrAtExit, len(data))
	}
}

func TestBackgroundChildKeepsOutput(t *testing.T) {
	h := newHarness(t, nil)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	const early = 300000
	s := h.start(t, &proto.ExecStart{Argv: sh(`head -c `+strconv.Itoa(early)+` /dev/zero; { read x < "$1"; echo "late $x"; } &`, fifo)})
	s.send(&proto.ExecFrame{Op: proto.ExecStdinEOF})
	o := &outcome{}
	s.untilExit(o)
	if o.exit.Code != 0 {
		t.Fatalf("exit %+v", o.exit)
	}
	// Everything the main process wrote arrived before ExecExit.
	if o.stdoutAtExit != early {
		t.Fatalf("stdout at ExecExit = %d bytes, want %d", o.stdoutAtExit, early)
	}
	// The background child still holds stdout: release it.
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("x\n"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	for s.next(o) {
	}
	if got := o.stdout.String()[early:]; got != "late x\n" {
		t.Fatalf("output after exit = %q, want %q", got, "late x\n")
	}
}

func TestAbandonKillsProcessGroup(t *testing.T) {
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: sh("sleep 1000 & echo $!; wait")})
	o := &outcome{}
	for !strings.Contains(o.stdout.String(), "\n") {
		if !s.next(o) {
			t.Fatal("stream ended early")
		}
	}
	child, err := strconv.Atoi(strings.TrimSpace(o.stdout.String()))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.raw.Close()
	waitGone(t, o.pid)
	waitGone(t, child)
}

func TestProtocolViolationKills(t *testing.T) {
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: []string{"sleep", "1000"}})
	o := &outcome{}
	for o.pid == 0 {
		if !s.next(o) {
			t.Fatal("no ExecStarted")
		}
	}
	s.send(&proto.ExecFrame{Op: proto.ExecStdout, Data: []byte("x")})
	for s.next(o) {
	}
	if o.exit != nil {
		t.Fatalf("exit %+v after a protocol violation", o.exit)
	}
	waitGone(t, o.pid)
}

func TestCloseKillsRunning(t *testing.T) {
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: []string{"sleep", "1000"}})
	o := &outcome{}
	for o.pid == 0 {
		if !s.next(o) {
			t.Fatal("no ExecStarted")
		}
	}
	if err := h.svc.Close(); err != nil {
		t.Fatal(err)
	}
	for s.next(o) {
	}
	waitGone(t, o.pid)
	if err := h.svc.Serve(context.Background(), nopConn{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Serve after Close = %v, want ErrClosed", err)
	}
}

func TestCloseBeforeStart(t *testing.T) {
	h := newHarness(t, nil)
	// A stream that never sends ExecStart must not keep Close waiting.
	raw, err := h.client.Open(proto.StreamExec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	deadline := time.Now().Add(testTimeout)
	for {
		h.svc.mu.Lock()
		n := len(h.svc.execs)
		h.svc.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream not served")
		}
		time.Sleep(time.Millisecond)
	}
	closed := make(chan error, 1)
	go func() { closed <- h.svc.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Close blocked on a stream without ExecStart")
	}
}

func TestContextCancelKills(t *testing.T) {
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: []string{"sleep", "1000"}})
	o := &outcome{}
	for o.pid == 0 {
		if !s.next(o) {
			t.Fatal("no ExecStarted")
		}
	}
	h.cancel()
	for s.next(o) {
	}
	waitGone(t, o.pid)
}

// nopConn is a net.Conn that is never used.
type nopConn struct{ net.Conn }

func (nopConn) Close() error { return nil }

func TestLookup(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho found \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "tele-test-cmd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "tele-test-noexec"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "local"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *Config) {
		c.Target.LoginPath = "relative/bin:" + bin + ":/usr/bin:/bin"
	})

	o := run(t, h, &proto.ExecStart{Argv: []string{"tele-test-cmd", "a"}})
	if o.exit.Err != nil || o.stdout.String() != "found a\n" {
		t.Fatalf("lookup in LoginPath: exit %+v stdout %q", o.exit, o.stdout.String())
	}
	o = run(t, h, &proto.ExecStart{Argv: []string{"./local", "b"}, Dir: bin})
	if o.exit.Err != nil || o.stdout.String() != "found b\n" {
		t.Fatalf("relative path: exit %+v stdout %q", o.exit, o.stdout.String())
	}

	errTests := []struct {
		argv  []string
		dir   string
		errno unix.Errno
	}{
		{[]string{"tele-no-such-command"}, "/", unix.ENOENT},
		{[]string{"tele-test-noexec"}, "/", unix.EACCES},
		{[]string{"/nonexistent/prog"}, "/", unix.ENOENT},
		{[]string{"true"}, "/nonexistent-dir", unix.ENOENT},
		{[]string{"true"}, filepath.Join(bin, "local"), unix.ENOTDIR},
	}
	for _, tt := range errTests {
		o := run(t, h, &proto.ExecStart{Argv: tt.argv, Dir: tt.dir})
		if o.exit.Err == nil || !errors.Is(o.exit.Err, tt.errno) {
			t.Errorf("%v in %s: exit %+v, want errno %v", tt.argv, tt.dir, o.exit, tt.errno)
		}
		if o.pid != 0 {
			t.Errorf("%v: ExecStarted for a command that did not start", tt.argv)
		}
	}
}

func TestInvalidStart(t *testing.T) {
	h := newHarness(t, nil)
	tests := []struct {
		name  string
		start proto.ExecStart
	}{
		{"empty argv", proto.ExecStart{Dir: "/"}},
		{"empty argv0", proto.ExecStart{Argv: []string{""}, Dir: "/"}},
		{"relative dir", proto.ExecStart{Argv: []string{"true"}, Dir: "tmp"}},
		{"NUL in argv", proto.ExecStart{Argv: []string{"echo", "a\x00b"}, Dir: "/"}},
		{"malformed env", proto.ExecStart{Argv: []string{"true"}, Dir: "/", Env: []string{"NOEQUALS"}}},
		{"empty env key", proto.ExecStart{Argv: []string{"true"}, Dir: "/", Env: []string{"=x"}}},
		{"scratch escape", proto.ExecStart{Argv: []string{"true"}, Dir: "/", Scratch: []proto.ScratchFile{{Area: proto.ScratchTmp, Path: "../x"}}}},
		{"scratch area", proto.ExecStart{Argv: []string{"true"}, Dir: "/", Scratch: []proto.ScratchFile{{Area: 9, Path: "x"}}}},
	}
	for _, tt := range tests {
		s := h.start(t, &tt.start)
		o := s.collect()
		if !errors.Is(o.exit.Err, unix.EINVAL) {
			t.Errorf("%s: exit %+v, want EINVAL", tt.name, o.exit)
		}
	}
}

func TestEnv(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.BaseEnv = []string{"A=base", "B=base", "PATH=/usr/bin:/bin"}
	})
	o := run(t, h, &proto.ExecStart{Argv: sh(`echo "$A $B $C"`), Env: []string{"B=client", "C=client"}})
	if o.stdout.String() != "base client client\n" {
		t.Fatalf("env: %q", o.stdout.String())
	}
}

func TestBaseEnv(t *testing.T) {
	t.Setenv("LANG", "C.UTF-8")
	got := BaseEnv(proto.TargetInfo{User: "bob", Home: "/home/bob", Shell: "/bin/zsh", LoginPath: "/opt/bin:/usr/bin"})
	want := []string{"HOME=/home/bob", "USER=bob", "LOGNAME=bob", "SHELL=/bin/zsh", "PATH=/opt/bin:/usr/bin", "LANG=C.UTF-8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BaseEnv = %q, want %q", got, want)
	}
	t.Setenv("LANG", "")
	got = BaseEnv(proto.TargetInfo{User: "bob"})
	want = []string{"USER=bob", "LOGNAME=bob", "PATH=" + DefaultPath}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BaseEnv without login PATH = %q, want %q", got, want)
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2", "A=3"}, []string{"C=4", "B=5", "D"})
	want := []string{"A=3", "B=5", "C=4", "D"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeEnv = %q, want %q", got, want)
	}
}

func TestScratch(t *testing.T) {
	h := newHarness(t, nil)
	tmp := filepath.Join(h.scratch, "tmp")
	for _, a := range proto.ScratchAreas {
		fi, err := os.Stat(filepath.Join(h.scratch, a.Dir()))
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Fatalf("area %s: %v, %v", a.Dir(), fi, err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "untouched"), []byte("u"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "old-upload"), []byte("o"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := &proto.ExecStart{
		Argv: sh(`cat tmp/in; echo out > tmp/out; rm tmp/gone; mkdir -p tmp/d; echo nested > tmp/d/n; chmod 750 tmp/d/n; echo env > session-env/e`),
		Dir:  h.scratch,
		Scratch: []proto.ScratchFile{
			{Area: proto.ScratchTmp, Path: "in", Mode: 0o644, Data: []byte("uploaded\n")},
			{Area: proto.ScratchTmp, Path: "gone", Data: []byte("delete me")},
			{Area: proto.ScratchTmp, Path: "old-upload", Deleted: true},
		},
	}
	o := run(t, h, start)
	if o.exit.Code != 0 || o.stdout.String() != "uploaded\n" {
		t.Fatalf("exit %+v stdout %q stderr %q", o.exit, o.stdout.String(), o.stderr.String())
	}
	if fi, err := os.Stat(filepath.Join(tmp, "in")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("uploaded file: %v, %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "old-upload")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted upload still exists: %v", err)
	}
	// Removals come first; uploads the command left alone are not echoed.
	want := []proto.ScratchFile{
		{Area: proto.ScratchTmp, Path: "gone", Deleted: true},
		{Area: proto.ScratchTmp, Path: "d/n", Mode: 0o750, Data: []byte("nested\n")},
		{Area: proto.ScratchTmp, Path: "out", Mode: 0o644, Data: []byte("out\n")},
		{Area: proto.ScratchSessionEnv, Path: "e", Mode: 0o644, Data: []byte("env\n")},
	}
	// Files written by the shell get the umask's mode; compare the rest.
	for i := range o.exit.Scratch {
		if o.exit.Scratch[i].Path == "out" || o.exit.Scratch[i].Path == "e" {
			o.exit.Scratch[i].Mode = 0o644
		}
	}
	if !reflect.DeepEqual(o.exit.Scratch, want) {
		t.Fatalf("scratch =\n%+v\nwant\n%+v", o.exit.Scratch, want)
	}
	if o.exit.WatchSeq != h.syncer.seq.Load() || o.exit.WatchSeq == 0 {
		t.Fatalf("WatchSeq = %d, syncer at %d", o.exit.WatchSeq, h.syncer.seq.Load())
	}
}

func TestScratchLimits(t *testing.T) {
	h := newHarness(t, nil)
	script := fmt.Sprintf(`head -c %d /dev/zero > tmp/big; for i in 1 2 3 4 5 6 7 8 9; do head -c %d /dev/zero > tmp/f$i; done`,
		proto.ScratchFileMax+1, proto.ScratchFileMax)
	o := run(t, h, &proto.ExecStart{Argv: sh(script), Dir: h.scratch})
	total := 0
	for _, f := range o.exit.Scratch {
		if f.Path == "big" {
			t.Fatal("oversized scratch file returned")
		}
		total += len(f.Data)
	}
	if len(o.exit.Scratch) != 8 || total > proto.ScratchTotalMax {
		t.Fatalf("returned %d files, %d bytes; want 8 within the limit", len(o.exit.Scratch), total)
	}
}

func TestSyncAfterReap(t *testing.T) {
	var pid atomic.Int64
	var alive atomic.Bool
	h := newHarness(t, nil)
	h.syncer.hook = func() {
		if _, err := os.Stat("/proc/" + strconv.FormatInt(pid.Load(), 10)); err == nil {
			alive.Store(true)
		}
	}
	s := h.start(t, &proto.ExecStart{Argv: []string{"cat"}})
	o := &outcome{}
	for o.pid == 0 {
		if !s.next(o) {
			t.Fatal("no ExecStarted")
		}
	}
	pid.Store(int64(o.pid))
	s.send(&proto.ExecFrame{Op: proto.ExecStdinEOF})
	for s.next(o) {
	}
	if o.exit == nil || o.exit.WatchSeq != 1 {
		t.Fatalf("exit %+v, want WatchSeq 1", o.exit)
	}
	if alive.Load() {
		t.Fatal("Sync ran before the process was reaped")
	}
}

func TestPTY(t *testing.T) {
	h := newHarness(t, nil)
	o := run(t, h, &proto.ExecStart{
		Argv: sh("test -t 0 && test -t 1 && test -t 2 && stty size && echo err >&2"),
		TTY:  &proto.TTYSize{Rows: 33, Cols: 101},
	})
	if o.exit.Code != 0 {
		t.Fatalf("exit %+v, output %q", o.exit, o.stdout.String())
	}
	if got := o.stdout.String(); got != "33 101\r\nerr\r\n" || o.stderr.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", got, o.stderr.String())
	}
}

func TestPTYResizeAndEOF(t *testing.T) {
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: sh("read x; stty size; cat; echo done"), TTY: &proto.TTYSize{Rows: 10, Cols: 20}})
	s.send(&proto.ExecFrame{Op: proto.ExecResize, TTY: &proto.TTYSize{Rows: 40, Cols: 120}})
	s.send(&proto.ExecFrame{Op: proto.ExecStdin, Data: []byte("go\nline\n")})
	s.send(&proto.ExecFrame{Op: proto.ExecStdinEOF})
	o := s.collect()
	if o.exit.Code != 0 {
		t.Fatalf("exit %+v", o.exit)
	}
	out := o.stdout.String()
	if !strings.Contains(out, "40 120\r\n") || !strings.HasSuffix(out, "line\r\ndone\r\n") {
		t.Fatalf("pty output %q", out)
	}
}

func TestTerminateGraceful(t *testing.T) {
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: sh(`trap 'echo term; exit 7' TERM; echo ready; while :; do sleep 1; done`)})
	o := &outcome{}
	for !strings.Contains(o.stdout.String(), "ready\n") {
		if !s.next(o) {
			t.Fatal("stream ended early")
		}
	}
	done := make(chan error, 1)
	go func() { done <- h.svc.Terminate(testTimeout) }()
	for s.next(o) {
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The command handled SIGTERM, and its exit status still reached the
	// client.
	if o.exit == nil || o.exit.Code != 7 || o.stdout.String() != "ready\nterm\n" {
		t.Fatalf("exit %+v, stdout %q", o.exit, o.stdout.String())
	}
	if err := h.svc.Serve(context.Background(), nopConn{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Serve after Terminate = %v, want ErrClosed", err)
	}
}

func TestTerminateKillsAfterGrace(t *testing.T) {
	h := newHarness(t, nil)
	// sleep inherits the ignored SIGTERM.
	s := h.start(t, &proto.ExecStart{Argv: sh(`trap '' TERM; echo ready; sleep 1000`)})
	o := &outcome{}
	for !strings.Contains(o.stdout.String(), "ready\n") {
		if !s.next(o) {
			t.Fatal("stream ended early")
		}
	}
	if err := h.svc.Terminate(10 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitGone(t, o.pid)
}
