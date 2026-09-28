package telefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/fssvc"
	"github.com/ujzk/tele-agent/internal/mux"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/testutil/helperproc"
	"github.com/ujzk/tele-agent/internal/testutil/privtest"
)

// privateNSEnv marks the test binary re-executed in a private mount
// namespace.
const privateNSEnv = "TELEFS_TEST_PRIVATE_MOUNTNS"

func TestMain(m *testing.M) {
	helperproc.Register("userns-owner", userNSOwnerHelper)
	helperproc.Dispatch()
	if code, ok := runInPrivateMountNS(); ok {
		os.Exit(code)
	}
	goleak.VerifyTestMain(m)
}

// runInPrivateMountNS re-executes the test binary in a private mount
// namespace when running as root, so that FUSE and bind mounts left behind
// by a failing test vanish with the process instead of staying in the
// host's mount table.
func runInPrivateMountNS() (int, bool) {
	if os.Geteuid() != 0 || os.Getenv(privateNSEnv) != "" {
		return 0, false
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, false
	}
	cmd := exec.CommandContext(context.Background(), exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), privateNSEnv+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Go makes the new namespace's mounts private (MS_REC|MS_PRIVATE).
	cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, true
	case errors.As(err, &ee):
		return ee.ExitCode(), true
	default:
		fmt.Fprintf(os.Stderr, "telefs tests: no private mount namespace (%v); running in place\n", err)
		return 0, false
	}
}

// switchOpener is an Opener whose transport can be broken on demand, and
// whose FS requests a test can intercept.
type switchOpener struct {
	s      *mux.Session
	broken atomic.Bool
	hook   atomic.Pointer[fsHook]
	wg     sync.WaitGroup // proxies
}

// fsHook sees an FS request and returns the response to deliver (nil for
// none, as for FSForget). forward sends req to the server and returns its
// response (nil for FSForget, once the server finished it).
type fsHook func(req *proto.FSRequest, forward func() *proto.FSResponse) *proto.FSResponse

func (o *switchOpener) Open(kind proto.StreamKind) (net.Conn, error) {
	if o.broken.Load() {
		return nil, errors.New("transport broken by test")
	}
	hook := o.hook.Load()
	if kind != proto.StreamFS || hook == nil {
		return o.s.Open(kind)
	}
	a, b := net.Pipe()
	o.wg.Add(1)
	go o.proxy(b, *hook)
	return a, nil
}

// setHook intercepts FS requests from now on.
func (o *switchOpener) setHook(h fsHook) {
	o.hook.Store(&h)
}

// proxy serves one intercepted FS stream.
func (o *switchOpener) proxy(c net.Conn, hook fsHook) {
	defer o.wg.Done()
	defer func() { _ = c.Close() }()
	var req proto.FSRequest
	if err := proto.ReadFrame(c, &req, proto.MaxDataFrame); err != nil {
		return
	}
	forward := func() *proto.FSResponse {
		eio := &proto.FSResponse{Errno: uint32(syscall.EIO)}
		st, err := o.s.Open(proto.StreamFS)
		if err != nil {
			return eio
		}
		defer func() { _ = st.Close() }()
		if err := proto.WriteFrame(st, &req); err != nil {
			return eio
		}
		if req.Op == proto.FSForget {
			_, _ = io.Copy(io.Discard, st)
			return nil
		}
		var resp proto.FSResponse
		if err := proto.ReadFrame(st, &resp, proto.MaxDataFrame); err != nil {
			return eio
		}
		return &resp
	}
	if resp := hook(&req, forward); resp != nil {
		_ = proto.WriteFrame(c, resp)
	}
}

type harnessOpts struct {
	placeholders []Placeholder
	uid, gid     uint32
	ttl          time.Duration
	// noWatch leaves the watch stream to the test.
	noWatch bool
	// prepare fills the backing directory before the mount.
	prepare func(backing string)
}

// harness serves a temporary directory with fssvc through a mux pair and
// mounts telefs over it.
type harness struct {
	t       *testing.T
	backing string
	mnt     string
	svc     *fssvc.Service
	fs      *FS
	opener  *switchOpener
	cli     *mux.Session
	srv     *mux.Session

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // server goroutines

	watchCancel context.CancelFunc
	watchDone   chan error
}

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	privtest.RequireRoot(t)
	privtest.RequireFUSE(t)
	if o.ttl == 0 {
		o.ttl = time.Hour
	}
	h := &harness{t: t, backing: t.TempDir(), mnt: t.TempDir()}
	if o.prepare != nil {
		o.prepare(h.backing)
	}
	var err error
	h.svc, err = fssvc.New(fssvc.Config{Root: h.backing})
	if err != nil {
		t.Fatal(err)
	}
	a, b := socketPair(t)
	if h.cli, err = mux.Client(a); err != nil {
		t.Fatal(err)
	}
	if h.srv, err = mux.Server(b); err != nil {
		t.Fatal(err)
	}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.wg.Add(1)
	go h.serve()
	h.opener = &switchOpener{s: h.cli}
	h.fs, err = Mount(h.mnt, Config{
		Opener:       h.opener,
		Placeholders: o.placeholders,
		UID:          o.uid,
		GID:          o.gid,
		AttrTimeout:  o.ttl,
		EntryTimeout: o.ttl,
	})
	if err != nil {
		h.shutdownServer()
		t.Fatal(err)
	}
	t.Cleanup(h.close)
	if !o.noWatch {
		h.startWatch()
	}
	return h
}

func socketPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	a, err := fileConn(fds[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := fileConn(fds[1])
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

// fileConn turns a socket descriptor into a net.Conn and closes fd.
func fileConn(fd int) (net.Conn, error) {
	f := os.NewFile(uintptr(fd), "socketpair")
	defer func() { _ = f.Close() }()
	return net.FileConn(f)
}

func (h *harness) serve() {
	defer h.wg.Done()
	for {
		st, err := h.srv.Accept()
		if err != nil {
			return
		}
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			kind, err := mux.ReadKind(st)
			if err != nil {
				_ = st.Close()
				return
			}
			switch kind {
			case proto.StreamFS:
				_ = h.svc.ServeRequest(h.ctx, st)
			case proto.StreamWatch:
				_ = h.svc.ServeWatch(h.ctx, st)
			default:
				_ = st.Close()
			}
		}()
	}
}

// startWatch runs the watch stream and waits until its first event, which
// carries a new epoch and invalidates everything, is applied; after that
// the long TTLs are in effect. Sync alone is not enough: the server may not
// have published that event yet when Sync runs.
func (h *harness) startWatch() {
	h.t.Helper()
	c, err := h.cli.Open(proto.StreamWatch)
	if err != nil {
		h.t.Fatal(err)
	}
	h.runWatch(c)
	eventually(h.t, "the watch stream to deliver its first event", h.fs.healthy.Load)
	h.sync()
}

// runWatch runs RunWatch on c in the background.
func (h *harness) runWatch(c net.Conn) {
	ctx, cancel := context.WithCancel(h.ctx)
	h.watchCancel = cancel
	h.watchDone = make(chan error, 1)
	go func() { h.watchDone <- h.fs.RunWatch(ctx, c) }()
}

// stopWatch ends RunWatch and waits for it.
func (h *harness) stopWatch() {
	if h.watchCancel == nil {
		return
	}
	h.watchCancel()
	<-h.watchDone
	h.watchCancel = nil
}

// sync is the exec barrier: every change made to the backing directory so
// far is visible through the mount when it returns.
func (h *harness) sync() {
	h.t.Helper()
	seq := h.svc.Sync()
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	if err := h.fs.WaitApplied(ctx, seq); err != nil {
		h.t.Fatalf("WaitApplied(%d): %v", seq, err)
	}
}

func (h *harness) close() {
	h.stopWatch()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := h.fs.Unmount()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			h.t.Errorf("unmount: %v", err)
			_ = unix.Unmount(h.mnt, unix.MNT_DETACH)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.shutdownServer()
}

func (h *harness) shutdownServer() {
	h.cancel()
	_ = h.cli.Close()
	_ = h.srv.Close()
	h.wg.Wait()
	h.opener.wg.Wait()
	if err := h.svc.Close(); err != nil {
		h.t.Error(err)
	}
}

// m and b return paths in the mount and in the backing directory.
func (h *harness) m(rel string) string { return filepath.Join(h.mnt, rel) }
func (h *harness) b(rel string) string { return filepath.Join(h.backing, rel) }

// node returns the cached node for mount-relative path rel.
func (h *harness) node(rel string) *node {
	h.t.Helper()
	cur := &h.fs.root.Inode
	for _, name := range splitPath(rel) {
		cur = cur.GetChild(name)
		if cur == nil {
			h.t.Fatalf("no cached node for %s", rel)
		}
	}
	n, ok := cur.Operations().(*node)
	if !ok {
		h.t.Fatalf("%s is not a telefs node", rel)
	}
	return n
}

func splitPath(rel string) []string {
	var out []string
	for rel != "" && rel != "." {
		dir, file := filepath.Split(rel)
		out = append([]string{file}, out...)
		rel = filepath.Clean(dir)
		if rel == "/" || rel == "." {
			break
		}
	}
	return out
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func writeFile(t *testing.T, p, data string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSmoke(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	writeFile(t, h.m("a.txt"), "hello")
	if got := readFile(t, h.b("a.txt")); got != "hello" {
		t.Fatalf("backing content %q", got)
	}
	if got := readFile(t, h.m("a.txt")); got != "hello" {
		t.Fatalf("mount content %q", got)
	}
}
