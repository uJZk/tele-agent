package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/ujzk/tele-agent/internal/mux"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/resume"
	"github.com/ujzk/tele-agent/internal/rexec"
	"github.com/ujzk/tele-agent/internal/testutil/faultnet"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// env is a server behind a fault proxy and a session with it.
type env struct {
	px   *faultnet.Proxy
	conn *resume.Conn
	mux  *mux.Session
	home string
}

func newEnv(t *testing.T, sess resume.Config) *env {
	t.Helper()
	home := t.TempDir()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", filepath.Join(t.TempDir(), "server.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{
		Token:       []byte("t"),
		FSRoot:      t.TempDir(),
		Target:      &proto.TargetInfo{User: "bob", Home: home, Shell: "/bin/sh", LoginPath: "/usr/bin:/bin"},
		ScratchBase: t.TempDir(),
		Session:     sess,
		Logger:      testLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = srv.Serve(ctx, ln) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	e := &env{px: faultnet.New(t, ln.Addr().String()), home: home}
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", e.px.Addr())
	}
	if e.conn, err = resume.Dial(t.Context(), dial, sess); err != nil {
		t.Fatal(err)
	}
	if e.mux, err = mux.Client(e.conn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.mux.Close() })
	st, err := e.mux.Open(proto.StreamControl)
	if err != nil {
		t.Fatal(err)
	}
	c := proto.NewConn(st, proto.MaxControlFrame)
	if err := c.Send(&proto.Hello{Version: proto.Version, Token: []byte("t"), SessionID: "0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	var reply proto.HelloReply
	if err := c.Recv(&reply); err != nil || reply.Err != nil {
		t.Fatalf("hello: %v %v", err, reply.Err)
	}
	return e
}

// run starts script under sh and returns the process and its stdout.
func (e *env) run(t *testing.T, script string) (*rexec.Process, <-chan []byte) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	p, err := (&rexec.Client{Opener: e.mux}).Start(t.Context(), rexec.Command{Argv: []string{"sh", "-c", script}, Dir: e.home, Stdout: w})
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		_ = r.Close()
		out <- b
	}()
	go func() {
		<-p.Done()
		_ = w.Close()
	}()
	return p, out
}

func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestOutputWhileDisconnected lets a command write far more than any
// window while the client is away: it runs to the end instead of blocking
// on its output, and once the client is back every byte arrives, then the
// exit status (docs/exec.md "进程与信号").
func TestOutputWhileDisconnected(t *testing.T) {
	e := newEnv(t, resume.Config{Heartbeat: 50 * time.Millisecond, Lease: time.Minute})
	goFile, done := filepath.Join(e.home, "go"), filepath.Join(e.home, "done")
	const size = 8 << 20
	p, out := e.run(t, `touch ready; while [ ! -e go ]; do sleep 0.01; done; head -c 8388608 /dev/zero | tr '\0' x; touch done`)
	eventually(t, "the command to start", 10*time.Second, func() bool { return exists(filepath.Join(e.home, "ready")) })
	e.px.SetRefuse(true)
	e.px.Cut()
	if err := os.WriteFile(goFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the command to finish its output while disconnected", 20*time.Second, func() bool { return exists(done) })
	e.px.SetRefuse(false)
	res, err := p.Wait(t.Context())
	if err != nil || res.Code != 0 {
		t.Fatalf("wait: %+v, %v", res, err)
	}
	b := <-out
	if len(b) != size || strings.Trim(string(b), "x") != "" {
		t.Fatalf("output: %d bytes, want %d x's", len(b), size)
	}
}

// TestLeaseExpiryTerminates keeps the client away past the lease: the
// command gets SIGTERM, and the session ends.
func TestLeaseExpiryTerminates(t *testing.T) {
	e := newEnv(t, resume.Config{Heartbeat: 50 * time.Millisecond, Lease: 500 * time.Millisecond})
	started, termed := filepath.Join(e.home, "started"), filepath.Join(e.home, "termed")
	p, _ := e.run(t, `trap 'touch termed; exit 0' TERM; touch started; while :; do sleep 0.02; done`)
	eventually(t, "the command to start", 10*time.Second, func() bool { return exists(started) })
	e.px.SetRefuse(true)
	e.px.Cut()
	eventually(t, "SIGTERM after the lease", 10*time.Second, func() bool { return exists(termed) })
	<-e.conn.Done()
	if err := e.conn.Err(); !errors.Is(err, resume.ErrExpired) {
		t.Errorf("client session: %v, want ErrExpired", err)
	}
	if _, err := p.Wait(t.Context()); err == nil {
		t.Error("the command's exit reached a client whose session expired")
	}
}

// testLogger logs to stderr when TELE_TEST_LOG is set.
func testLogger() *slog.Logger {
	if os.Getenv("TELE_TEST_LOG") == "" {
		return nil
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
