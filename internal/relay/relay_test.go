package relay

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/rexec"
	"github.com/ujzk/tele-agent/internal/scratch"
	"github.com/ujzk/tele-agent/internal/shimsrv"
	"github.com/ujzk/tele-agent/internal/testutil/servertest"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

const (
	sid     = "0123456789abcdef"
	sessDir = "/.tele/" + sid
)

type env struct {
	relay    *Relay
	localTmp string // the tmp scratch area in session main's view
}

// newEnv serves a tele server on a unix socket and returns a Relay using
// a session with it.
func newEnv(t *testing.T, loginPath string) *env {
	t.Helper()
	ep, _ := servertest.Start(t, "t", &proto.TargetInfo{
		User: "bob", Home: t.TempDir(), Shell: "/bin/sh", LoginPath: loginPath,
	})
	m, reply := servertest.Hello(t, ep, "t", sid)
	e := &env{localTmp: t.TempDir()}
	sm, err := scratch.New([]scratch.Area{{
		ID: proto.ScratchTmp, ClaudePath: sessDir + "/tmp", LocalPath: e.localTmp, RemotePath: filepath.Join(reply.ScratchDir, "tmp"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.relay = New(Config{
		SessDir:    sessDir,
		Baseline:   []string{"PATH=" + sessDir + "/bin", "HOME=/home/bob"},
		LocalProgs: map[string]bool{"id": true},
		Exec:       &rexec.Client{Opener: m},
		Scratch:    sm,
	})
	// Registered after Hello's cleanup, so it runs first, while the
	// session is still open.
	t.Cleanup(e.relay.Wait)
	return e
}

// call is one shim invocation with captured output.
type call struct {
	req            *shimsrv.Request
	stdout, stderr bytes.Buffer
	stdinW         *os.File
	wg             sync.WaitGroup
}

func newCall(t *testing.T, name string, argv ...string) *call {
	t.Helper()
	c := &call{req: &shimsrv.Request{Name: name, Argv: append([]string{name}, argv...), Dir: "/", Env: []string{"PATH=/usr/bin:/bin", "EXTRA=1"}}}
	var err error
	var outR, errR *os.File
	if c.req.Stdin, c.stdinW, err = os.Pipe(); err != nil {
		t.Fatal(err)
	}
	if outR, c.req.Stdout, err = os.Pipe(); err != nil {
		t.Fatal(err)
	}
	if errR, c.req.Stderr, err = os.Pipe(); err != nil {
		t.Fatal(err)
	}
	c.wg.Go(func() { _, _ = c.stdout.ReadFrom(outR); _ = outR.Close() })
	c.wg.Go(func() { _, _ = c.stderr.ReadFrom(errR); _ = errR.Close() })
	t.Cleanup(func() { _ = c.stdinW.Close() })
	return c
}

// run serves the call and waits for its output to end.
func (c *call) run(ctx context.Context, r *Relay, sigs <-chan int) proto.ShimStatus {
	_ = c.stdinW.Close()
	st := r.Serve(ctx, c.req, sigs)
	c.wg.Wait()
	return st
}

func TestRemoteBash(t *testing.T) {
	e := newEnv(t, "/usr/local/bin:/usr/bin:/bin")
	c := newCall(t, "bash", "-c", `echo out; echo err >&2; echo "extra=$EXTRA home=${HOME:+set}"; exit 3`)
	st := c.run(t.Context(), e.relay, nil)
	if st.Code != 3 || st.Signal != 0 {
		t.Fatalf("status %+v, want exit 3", st)
	}
	if got := c.stdout.String(); got != "out\nextra=1 home=set\n" {
		t.Errorf("stdout %q", got)
	}
	if got := c.stderr.String(); got != "err\n" {
		t.Errorf("stderr %q", got)
	}
}

func TestScratchRoundTrip(t *testing.T) {
	// The Bash tool's command string writes the cwd file in
	// CLAUDE_CODE_TMPDIR, which Claude then reads locally
	// (docs/claude-code.md "scratch 文件"); an input file there reaches
	// the command.
	e := newEnv(t, "/usr/local/bin:/usr/bin:/bin")
	if err := os.WriteFile(filepath.Join(e.localTmp, "in"), []byte("uploaded"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newCall(t, "bash", "-c", "cat "+sessDir+"/tmp/in && cd /tmp && pwd -P >| "+sessDir+"/tmp/claude-1-cwd")
	if st := c.run(t.Context(), e.relay, nil); st.Code != 0 {
		t.Fatalf("status %+v, stderr %q", st, c.stderr.String())
	}
	if c.stdout.String() != "uploaded" {
		t.Errorf("stdout %q, want the uploaded file", c.stdout.String())
	}
	b, err := os.ReadFile(filepath.Join(e.localTmp, "claude-1-cwd"))
	if err != nil || strings.TrimSpace(string(b)) == "" {
		t.Fatalf("cwd file %q, %v; want it returned", b, err)
	}
}

func TestSignalForwarded(t *testing.T) {
	e := newEnv(t, "/usr/local/bin:/usr/bin:/bin")
	ready := filepath.Join(t.TempDir(), "ready")
	c := newCall(t, "tele-exec", `trap 'exit 42' TERM; touch `+ready+`; while :; do sleep 0.05; done`)
	sigs := make(chan int, 1)
	done := make(chan proto.ShimStatus, 1)
	go func() { done <- c.run(t.Context(), e.relay, sigs) }()
	waitFile(t, ready)
	sigs <- int(unix.SIGTERM)
	if st := <-done; st.Code != 42 {
		t.Fatalf("status %+v, want the trap's exit 42", st)
	}
}

// eventually waits until cond holds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitFile waits until p exists.
func waitFile(t *testing.T, p string) {
	t.Helper()
	eventually(t, p, func() bool {
		_, err := os.Stat(p)
		return err == nil
	})
}

func TestRemoteNotFound(t *testing.T) {
	e := newEnv(t, "/nonexistent")
	c := newCall(t, "rg", "x")
	st := c.run(t.Context(), e.relay, nil)
	if st.Code != 127 || !strings.Contains(st.Msg, "rg") {
		t.Fatalf("status %+v, want 127 naming rg", st)
	}
}

func TestShimKilled(t *testing.T) {
	// A shim killed by Claude's timeout ends its connection; the command
	// is killed with it.
	e := newEnv(t, "/usr/local/bin:/usr/bin:/bin")
	pidFile := filepath.Join(t.TempDir(), "pid")
	c := newCall(t, "tele-exec", "echo $$ > "+pidFile+"; exec sleep 1000")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan proto.ShimStatus, 1)
	go func() { done <- c.run(ctx, e.relay, nil) }()
	var pid int
	eventually(t, "the command to start", func() bool {
		b, err := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid != 0
	})
	cancel()
	<-done
	eventually(t, "the remote process to end", func() bool { return unix.Kill(pid, 0) != nil })
}

func TestLocalProxy(t *testing.T) {
	e := newEnv(t, "/nonexistent")
	c := newCall(t, "id", "-u")
	if st := c.run(t.Context(), e.relay, nil); st.Code != 0 {
		t.Fatalf("status %+v, stderr %q", st, c.stderr.String())
	}
	if got := strings.TrimSpace(c.stdout.String()); got != strconv.Itoa(os.Getuid()) {
		t.Errorf("local id -u = %q", got)
	}
}

func TestUnknownProgram(t *testing.T) {
	e := newEnv(t, "/nonexistent")
	c := newCall(t, "frobnicate")
	if st := c.run(t.Context(), e.relay, nil); st.Code != codeFailure || st.Msg == "" {
		t.Fatalf("status %+v, want an infrastructure failure", st)
	}
}
