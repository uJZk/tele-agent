package localexec

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

var testEnv = []string{"PATH=/usr/bin:/bin"}

// pipe returns both ends of a pipe, closed when the test ends.
func pipe(t *testing.T) (r, w *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	return r, w
}

// start runs Run in a goroutine; the returned channel yields its status.
func start(ctx context.Context, argv []string, dir string, env []string, stdin, stdout, stderr *os.File, sigs <-chan int) <-chan proto.ShimStatus {
	ch := make(chan proto.ShimStatus, 1)
	go func() { ch <- Run(ctx, argv, dir, env, stdin, stdout, stderr, sigs) }()
	return ch
}

func await(t *testing.T, ch <-chan proto.ShimStatus) proto.ShimStatus {
	t.Helper()
	select {
	case st := <-ch:
		return st
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
		return proto.ShimStatus{}
	}
}

func sh(script string) []string { return []string{"sh", "-c", script} }

func TestExitStatus(t *testing.T) {
	tests := []struct {
		script string
		want   proto.ShimStatus
	}{
		{"exit 0", proto.ShimStatus{}},
		{"exit 3", proto.ShimStatus{Code: 3}},
		{"exit 255", proto.ShimStatus{Code: 255}},
		{"kill -TERM $$", proto.ShimStatus{Signal: int(unix.SIGTERM)}},
		{"kill -KILL $$", proto.ShimStatus{Signal: int(unix.SIGKILL)}},
		{"ulimit -c 0; kill -SEGV $$", proto.ShimStatus{Signal: int(unix.SIGSEGV)}},
		{"kill -USR2 $$", proto.ShimStatus{Signal: int(unix.SIGUSR2)}},
	}
	for _, tt := range tests {
		got := Run(t.Context(), sh(tt.script), t.TempDir(), testEnv, nil, nil, nil, nil)
		if got != tt.want {
			t.Errorf("Run(%q) = %+v, want %+v", tt.script, got, tt.want)
		}
	}
}

func TestStartFailures(t *testing.T) {
	dir := t.TempDir()
	noexec := filepath.Join(dir, "noexec")
	if err := os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "isdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	pathEnv := []string{"PATH=" + dir}
	tests := []struct {
		argv    []string
		dir     string
		env     []string
		code    int
		msgPart string
	}{
		{[]string{"no-such-program"}, "", pathEnv, codeNotFound, "no-such-program: command not found"},
		{[]string{"sh"}, "", pathEnv, codeNotFound, "command not found"}, // only env's PATH counts
		{[]string{filepath.Join(dir, "missing")}, "", pathEnv, codeNotFound, "no such file"},
		{[]string{"noexec"}, "", pathEnv, codeCannotExecute, "permission denied"},
		{[]string{noexec}, "", pathEnv, codeCannotExecute, "permission denied"},
		{[]string{"isdir"}, "", pathEnv, codeCannotExecute, "permission denied"},
		{[]string{"sh"}, filepath.Join(dir, "gone"), testEnv, codeNotFound, "chdir " + filepath.Join(dir, "gone")},
		{[]string{""}, "", testEnv, codeNotFound, "command not found"},
		{nil, "", testEnv, codeFailure, "empty command"},
	}
	for _, tt := range tests {
		got := Run(t.Context(), tt.argv, tt.dir, tt.env, nil, nil, nil, nil)
		if got.Code != tt.code || got.Signal != 0 || !strings.Contains(got.Msg, tt.msgPart) {
			t.Errorf("Run(%q) = %+v, want code %d and message containing %q", tt.argv, got, tt.code, tt.msgPart)
		}
	}
}

func TestLookPath(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, f := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Join(a, "prog"), 0o644}, {filepath.Join(b, "prog"), 0o755}, {filepath.Join(a, "first"), 0o755}, {filepath.Join(b, "first"), 0o755}} {
		if err := os.WriteFile(f.path, nil, f.mode); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		file, path string
		want       string
		err        error
	}{
		{"prog", a + ":" + b, filepath.Join(b, "prog"), nil}, // skips the non-executable one
		{"first", a + ":" + b, filepath.Join(a, "first"), nil},
		{"prog", a, "", unix.EACCES},
		{"prog", "", "", unix.ENOENT},
		{"prog", ".:" + "rel", "", unix.ENOENT},
		{"sh", defaultPath, "/bin/sh", nil},
		{"./prog", "", "./prog", nil},
		{"", a, "", unix.ENOENT},
	}
	for _, tt := range tests {
		got, err := lookPath(tt.file, tt.path)
		if got != tt.want || !errors.Is(err, tt.err) {
			t.Errorf("lookPath(%q, %q) = %q, %v; want %q, %v", tt.file, tt.path, got, err, tt.want, tt.err)
		}
	}
	if got := pathOf([]string{"PATH=/a", "X=1", "PATH=/b"}); got != "/b" {
		t.Errorf("pathOf = %q, want the last PATH", got)
	}
	if got := pathOf([]string{"PATHX=/a"}); got != defaultPath {
		t.Errorf("pathOf without PATH = %q", got)
	}
}

func TestStdioEnvDir(t *testing.T) {
	inR, inW := pipe(t)
	outR, outW := pipe(t)
	errR, errW := pipe(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEAK", "from session main")
	ch := start(t.Context(), sh(`pwd; echo "$FOO ${LEAK-unset}"; cat; echo oops >&2`), dir,
		[]string{"FOO=bar"}, inR, outW, errW, nil)
	if _, err := io.WriteString(inW, "input\n"); err != nil {
		t.Fatal(err)
	}
	_ = inW.Close()
	if st := await(t, ch); st != (proto.ShimStatus{}) {
		t.Fatalf("status %+v", st)
	}
	// Run leaves the fds open: the caller still owns them.
	for _, f := range []*os.File{inR, outW, errW} {
		if _, err := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0); err != nil {
			t.Errorf("fd %s closed by Run: %v", f.Name(), err)
		}
	}
	_ = outW.Close()
	_ = errW.Close()
	out, _ := io.ReadAll(outR)
	errOut, _ := io.ReadAll(errR)
	if want := dir + "\nbar unset\ninput\n"; string(out) != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if string(errOut) != "oops\n" {
		t.Errorf("stderr = %q", errOut)
	}
}

// TestKeepsFileStatusFlags checks that a shared non-blocking fd, wrapped the
// way shimsrv wraps received fds, reaches the program and stays
// non-blocking (docs/exec.md "shim 与会话主进程").
func TestKeepsFileStatusFlags(t *testing.T) {
	r, w := pipe(t)
	fd, err := unix.Dup(int(w.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	out := os.NewFile(uintptr(fd), "shared")
	defer out.Close()
	_ = w.Close()

	st := Run(t.Context(), sh(`echo hi`), "", testEnv, nil, out, nil, nil)
	if st != (proto.ShimStatus{}) {
		t.Fatalf("status %+v", st)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.O_NONBLOCK == 0 {
		t.Error("Run cleared O_NONBLOCK on a shared fd")
	}
	_ = out.Close()
	if got, _ := io.ReadAll(r); string(got) != "hi\n" {
		t.Errorf("output %q", got)
	}
}

// busyLoop keeps a shell running builtins, so that it runs traps as soon
// as a signal arrives, without children that could inherit the trap.
const busyLoop = "while :; do :; done"

func TestSignalsForwarded(t *testing.T) {
	outR, outW := pipe(t)
	sigs := make(chan int)
	ch := start(t.Context(), sh(`trap 'echo got; exit 7' USR1; echo ready; `+busyLoop), "", testEnv, nil, outW, nil, sigs)
	rd := bufio.NewReader(outR)
	if line, err := rd.ReadString('\n'); line != "ready\n" {
		t.Fatalf("read %q, %v", line, err)
	}
	for _, bad := range []int{0, -1, 65} {
		sigs <- bad // ignored
	}
	sigs <- int(unix.SIGUSR1)
	if st := await(t, ch); st != (proto.ShimStatus{Code: 7}) {
		t.Fatalf("status %+v, want exit 7", st)
	}
	_ = outW.Close()
	if rest, _ := io.ReadAll(rd); string(rest) != "got\n" {
		t.Errorf("trap output %q", rest)
	}
}

// TestSignalsReachGroup: a signal reaches the whole process group, not just
// the program Run started.
func TestSignalsReachGroup(t *testing.T) {
	outR, outW := pipe(t)
	sigs := make(chan int)
	inner := `trap 'exit 3' USR1; echo child-ready; ` + busyLoop
	ch := start(t.Context(), sh(`sh -c "`+inner+`" & wait`), "", testEnv, nil, outW, nil, sigs)
	rd := bufio.NewReader(outR)
	if line, err := rd.ReadString('\n'); line != "child-ready\n" {
		t.Fatalf("read %q, %v", line, err)
	}
	sigs <- int(unix.SIGUSR1)
	if st := await(t, ch); st != (proto.ShimStatus{Signal: int(unix.SIGUSR1)}) {
		t.Fatalf("status %+v, want killed by SIGUSR1", st)
	}
	_ = outW.Close()
	// EOF only once the grandchild, which holds stdout too, has exited.
	if rest, err := io.ReadAll(rd); err != nil || len(rest) != 0 {
		t.Errorf("read %q, %v", rest, err)
	}
}

func TestClosedSigs(t *testing.T) {
	sigs := make(chan int)
	close(sigs)
	if st := Run(t.Context(), sh("exit 4"), "", testEnv, nil, nil, nil, sigs); st.Code != 4 {
		t.Fatalf("status %+v", st)
	}
}

func TestCancelKillsGroup(t *testing.T) {
	outR, outW := pipe(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch := start(ctx, sh(`sleep 100 & echo $!; wait`), "", testEnv, nil, outW, nil, nil)
	line, err := bufio.NewReader(outR).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if st := await(t, ch); st != (proto.ShimStatus{Signal: int(unix.SIGKILL)}) {
		t.Fatalf("status %+v, want SIGKILL", st)
	}
	waitDead(t, child)
}

func TestCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if st := Run(ctx, sh("exit 0"), "", testEnv, nil, nil, nil, nil); st.Code != codeFailure || st.Msg == "" {
		t.Fatalf("status %+v", st)
	}
}

// waitDead polls until pid has exited (gone, or a zombie nobody reaps).
func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return
		}
		if i := strings.LastIndexByte(string(b), ')'); i >= 0 && i+2 < len(b) && (b[i+2] == 'Z' || b[i+2] == 'X') {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d still running: %s", pid, b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
