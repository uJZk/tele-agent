package rexec

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/scratch"
)

// backlog starts `echo $$; exec sleep 1000`, which never reads its stdin,
// and feeds it more stdin than the window. It returns once rexec used up
// the window, and checks that the rest stays unread in the local pipe.
func backlog(t *testing.T, e *env) (*Process, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	in, feed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.client.Start(t.Context(), Command{Argv: sh("echo $$; exec sleep 1000"), Dir: "/", Stdin: in, Stdout: w})
	if err != nil {
		t.Fatal(err)
	}
	fed := make(chan error, 1)
	t.Cleanup(func() {
		p.Abandon()
		<-p.Done()
		// Ends the write below with EPIPE.
		_ = in.Close()
		<-fed
		_ = feed.Close()
	})
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := feed.Write(make([]byte, 4*proto.ExecStdinWindow))
		fed <- err
	}()
	deadline := time.Now().Add(testTimeout)
	for p.credit.available() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("stdin window not used up: %d bytes left", p.credit.available())
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-fed:
		t.Fatalf("all stdin was read although the command reads none: %v", err)
	default:
	}
	return p, pid
}

func TestSignalWithStdinBacklog(t *testing.T) {
	e := newEnv(t, nil)
	p, _ := backlog(t, e)
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	if err := p.Signal(ctx, int(unix.SIGTERM)); err != nil {
		t.Fatal(err)
	}
	res, err := p.Wait(ctx)
	if err != nil || res.Signal != int(unix.SIGTERM) {
		t.Fatalf("Wait = %+v, %v; want signal 15", res, err)
	}
}

func TestAbandonWithStdinBacklog(t *testing.T) {
	e := newEnv(t, nil)
	p, pid := backlog(t, e)
	p.Abandon()
	waitDone(t, p)
	waitGone(t, pid)
}

// stuckConn is an exec stream whose peer stopped reading: it takes the
// ExecStart, then every write blocks until the stream is closed.
type stuckConn struct {
	net.Conn // other methods are not used

	mu     sync.Mutex
	writes int // guarded by mu

	closed    chan struct{}
	closeOnce sync.Once
}

func newStuckConn() *stuckConn { return &stuckConn{closed: make(chan struct{})} }

func (c *stuckConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	first := c.writes == 1
	c.mu.Unlock()
	if first {
		return len(b), nil
	}
	<-c.closed
	return 0, net.ErrClosed
}

func (c *stuckConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}

func (c *stuckConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *stuckConn) SetReadDeadline(time.Time) error { return nil }

type openerFunc func(proto.StreamKind) (net.Conn, error)

func (f openerFunc) Open(kind proto.StreamKind) (net.Conn, error) { return f(kind) }

func TestSignalContext(t *testing.T) {
	conn := newStuckConn()
	c := &Client{Opener: openerFunc(func(proto.StreamKind) (net.Conn, error) { return conn, nil })}
	// A nil Stdin sends its EOF at once, which gets stuck.
	p, err := c.Start(t.Context(), Command{Argv: []string{"x"}, Dir: "/"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := p.Signal(ctx, int(unix.SIGTERM)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Signal on a stalled stream = %v, want the context's error", err)
	}
	if err := p.Resize(ctx, proto.TTYSize{Rows: 1, Cols: 1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Resize on a stalled stream = %v, want the context's error", err)
	}
	// Abandon does not wait for the stalled writes.
	p.Abandon()
	waitDone(t, p)
}

func TestStarted(t *testing.T) {
	e := newEnv(t, nil)
	p, err := e.client.Start(t.Context(), Command{Argv: []string{"true"}, Dir: "/"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Started():
	case <-time.After(testTimeout):
		t.Fatal("Started not closed")
	}
	if _, err := p.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitDone(t, p)

	p, err = e.client.Start(t.Context(), Command{Argv: []string{"tele-no-such-command"}, Dir: "/"})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, p)
	select {
	case <-p.Started():
		t.Fatal("Started closed for a command that did not start")
	default:
	}
}

func TestScratchBudget(t *testing.T) {
	e := newEnv(t, nil)
	local := t.TempDir()
	m, err := scratch.New([]scratch.Area{{
		ID: proto.ScratchTmp, ClaudePath: "/.tele/0123456789abcdef/tmp", LocalPath: local, RemotePath: filepath.Join(e.scratch, "tmp"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Many files with long names: their data alone fits the scratch
	// limit, the encoded frame would not.
	long := strings.Repeat("n", 240)
	data := make([]byte, 1500)
	const n = 5500
	for i := range n {
		if err := os.WriteFile(filepath.Join(local, long+strconv.Itoa(i)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A command line of several MiB, as a shim may send, leaves the
	// uploads their full limit.
	cmd := Command{Argv: []string{"sh", "-c", strings.Repeat("x", 4<<20)}, Dir: "/", Env: []string{"BIG=" + strings.Repeat("y", 2<<20)}}
	up := m.Uploads(ScratchBudget(cmd))
	cmd.Scratch = up.Files
	b, err := proto.Marshal(&proto.ExecStart{Argv: cmd.Argv, Dir: cmd.Dir, Env: cmd.Env, Scratch: cmd.Scratch})
	if err != nil || len(b) > proto.MaxExecStart {
		t.Fatalf("ExecStart of %d bytes, %v", len(b), err)
	}
	if len(up.Files) != n {
		t.Fatalf("uploaded %d files next to a large command line, want %d", len(up.Files), n)
	}
	p, err := e.client.Start(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	// The start is accepted: the kernel, not the stream, refuses a command
	// line this large.
	res, err := p.Wait(t.Context())
	if err != nil || res.StartErr == nil || res.StartErr.Errno != uint32(unix.E2BIG) {
		t.Fatalf("Wait = %+v, %v; want start error E2BIG", res, err)
	}
	waitDone(t, p)
	up.Commit()
}

func TestBackgroundOutputNotReplaced(t *testing.T) {
	// A background task writes to a local file in the tmp area, and a
	// command meanwhile uploads the partial file: the task's exit status
	// must not bring that copy back over the complete file.
	e := newEnv(t, nil)
	local := t.TempDir()
	m, err := scratch.New([]scratch.Area{{
		ID: proto.ScratchTmp, ClaudePath: "/.tele/0123456789abcdef/tmp", LocalPath: local, RemotePath: filepath.Join(e.scratch, "tmp"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(local, "t1.output")
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := e.client.Start(t.Context(), Command{Argv: sh(`echo first; read x < "$1"; echo second`, fifo), Dir: "/", Stdout: out})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testTimeout)
	for {
		if b, _ := os.ReadFile(outPath); string(b) == "first\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no output")
		}
		time.Sleep(time.Millisecond)
	}
	up := m.Uploads(proto.MaxDataFrame)
	if len(up.Files) != 1 {
		t.Fatalf("uploads %+v, want the partial output file", up.Files)
	}
	r := runCmd(t, e, Command{Argv: []string{"true"}, Scratch: up.Files})
	up.Commit()
	if err := m.Apply(r.res.Scratch); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fifo, []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := p.Wait(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Scratch) != 0 {
		t.Fatalf("background task reported %+v", res.Scratch)
	}
	waitDone(t, p)
	if b, err := os.ReadFile(outPath); err != nil || string(b) != "first\nsecond\n" {
		t.Fatalf("output file %q, %v", b, err)
	}
}
