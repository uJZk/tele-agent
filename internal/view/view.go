// Package view builds the mount namespaces session main needs around the
// Claude process (docs/filesystem.md "命名空间的构建"):
//
//   - the remote view: telefs as "/", with the local set bind-mounted onto
//     its placeholders and /proc, /sys and /dev from the local view. A
//     helper process builds it in a mount namespace of its own and keeps
//     it alive until session main holds it by descriptor;
//   - the launch view Claude is exec'd in: session main's local view with
//     an empty, read-only /proc. teleswitch moves Claude to the remote view
//     before its main; should the preload not run at all (the dynamic
//     linker ignores a library it cannot load), Claude aborts at once,
//     because its runtime cannot start without /proc (docs/filesystem.md
//     "已知陷阱").
//
// Both steps run in fresh processes: the Go runtime is multithreaded, so
// session main cannot unshare its own mount namespace. The helpers are the
// same executable, entered through HelperMain and LaunchMain.
package view

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Caps are the capabilities a helper needs, which session main holds as
// ambient capabilities in its user namespace: CAP_SYS_ADMIN to mount and
// join mount namespaces, CAP_SYS_CHROOT for pivot_root and setns.
var Caps = []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_SYS_CHROOT}

// Bind is one bind mount of the remote view.
type Bind struct {
	// Source is a path in session main's (the local) view.
	Source string `json:"source"`
	// Target is a path in the remote view: a placeholder of telefs.
	Target string `json:"target"`
}

// Spec describes the remote view.
type Spec struct {
	// Root is where telefs is mounted in session main's view.
	Root string `json:"root"`
	// Binds are applied in order, recursively (submounts of a source come
	// along), before the view's root becomes Root.
	Binds []Bind `json:"binds"`
}

// buildTimeout bounds building the remote view. Every step is local, but
// the bind targets are looked up through telefs, which asks the target
// host.
const buildTimeout = 2 * time.Minute

// ready is the line the helper writes once the view is built.
const ready = "ready"

// Build builds the remote view described by spec and returns a descriptor
// of its mount namespace. helper is the command that enters HelperMain; its
// SysProcAttr, stdin and stdout are set here.
func Build(ctx context.Context, spec Spec, helper *exec.Cmd) (*os.File, error) {
	if err := checkSpec(spec); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("view: %w", err)
	}
	helper.SysProcAttr = &syscall.SysProcAttr{
		// Go makes every mount of the new namespace private.
		Unshareflags: syscall.CLONE_NEWNS,
		AmbientCaps:  Caps,
	}
	stdin, err := helper.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("view: %w", err)
	}
	stdout, err := helper.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("view: %w", err)
	}
	if err := helper.Start(); err != nil {
		return nil, fmt.Errorf("view: start helper: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = helper.Process.Kill() })
	defer stop()
	// The helper exits once stdin closes, after the namespace was taken.
	defer func() {
		_ = stdin.Close()
		_ = helper.Wait()
	}()
	if _, err := stdin.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("view: send spec to helper: %w", err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != ready {
		if err == nil || errors.Is(err, io.EOF) {
			err = errors.New("helper failed (see its error above or in the session log)")
		}
		if ctx.Err() != nil {
			err = fmt.Errorf("timed out after %v", buildTimeout)
		}
		return nil, fmt.Errorf("view: build the remote view: %w", err)
	}
	ns, err := os.Open("/proc/" + strconv.Itoa(helper.Process.Pid) + "/ns/mnt")
	if err != nil {
		return nil, fmt.Errorf("view: hold the remote view: %w", err)
	}
	return ns, nil
}

func checkSpec(s Spec) error {
	if !path.IsAbs(s.Root) {
		return fmt.Errorf("view: root %q is not absolute", s.Root)
	}
	for _, b := range s.Binds {
		if !path.IsAbs(b.Source) || !path.IsAbs(b.Target) || path.Clean(b.Target) == "/" {
			return fmt.Errorf("view: invalid bind %q on %q", b.Source, b.Target)
		}
	}
	return nil
}

// HelperMain is the helper process of Build. It runs in a new mount
// namespace with the capabilities in Caps, reads the Spec from stdin,
// builds the view, reports "ready" on stdout and waits for stdin to close.
// Errors go to stderr, which the caller directs to the session log.
func HelperMain() int {
	in := bufio.NewReader(os.Stdin)
	line, err := in.ReadBytes('\n')
	if err != nil {
		return helperFail(fmt.Errorf("read spec: %w", err))
	}
	var s Spec
	if err := json.Unmarshal(line, &s); err != nil {
		return helperFail(fmt.Errorf("parse spec: %w", err))
	}
	if err := checkSpec(s); err != nil {
		return helperFail(err)
	}
	if err := build(s); err != nil {
		return helperFail(err)
	}
	if _, err := fmt.Println(ready); err != nil {
		return helperFail(err)
	}
	_, _ = io.Copy(io.Discard, in) // until session main holds the namespace
	return 0
}

func helperFail(err error) int {
	fmt.Fprintf(os.Stderr, "tele: view helper: %v\n", err)
	return 1
}

// build turns the helper's mount namespace into the remote view.
func build(s Spec) error {
	root, err := unix.Open(s.Root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open root %s: %w", s.Root, err)
	}
	defer func() { _ = unix.Close(root) }()
	for _, b := range s.Binds {
		if err := bind(root, b); err != nil {
			return err
		}
	}
	return pivot(s.Root)
}

// bind mounts b.Source on b.Target inside root. The target is resolved
// within root: an absolute symlink on the way (a remote /bin -> /usr/bin)
// must not lead to a local path (docs/filesystem.md "已知陷阱").
func bind(root int, b Bind) error {
	src, err := unix.Open(b.Source, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open bind source %s: %w", b.Source, err)
	}
	defer func() { _ = unix.Close(src) }()
	rel := strings.TrimPrefix(path.Clean(b.Target), "/")
	var dst int
	err = ignoringEINTR(func() (err error) {
		dst, err = unix.Openat2(root, rel, &unix.OpenHow{
			Flags:   unix.O_PATH | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("resolve bind target %s: %w", b.Target, err)
	}
	defer func() { _ = unix.Close(dst) }()
	if err := unix.Mount(fdPath(src), fdPath(dst), "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind %s on %s: %w", b.Source, b.Target, err)
	}
	return nil
}

func fdPath(fd int) string {
	return "/proc/self/fd/" + strconv.Itoa(fd)
}

// pivot makes root the root of the mount namespace and detaches the old
// one, then stats the new root: the kernel initializes a FUSE root inode
// as owned by uid 0, unmapped in a user namespace, and a GETATTR makes it
// take the owner telefs presents (docs/filesystem.md "已知陷阱").
func pivot(root string) error {
	if err := unix.Chdir(root); err != nil {
		return fmt.Errorf("chdir %s: %w", root, err)
	}
	// pivot_root(".", ".") stacks the old root on the new one, so no
	// directory for it is needed in the view.
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root to %s: %w", root, err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach the old root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Stat("/", &st); err != nil {
		return fmt.Errorf("stat the new root: %w", err)
	}
	return nil
}

func ignoringEINTR(fn func() error) error {
	for {
		if err := fn(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// PreloadEnv carries the library the launch stage preloads into the program
// it execs. The launch stage does not get LD_PRELOAD itself: the library
// would switch it, and drop the capabilities it needs, before it ran.
const PreloadEnv = "TELE_LAUNCH_PRELOAD"

// LaunchMain is the launch stage: started in a new mount namespace with the
// capabilities in Caps, it covers /proc with an empty read-only file system
// and execs argv, keeping its environment and descriptors (the one of the
// remote view among them), with PreloadEnv turned into LD_PRELOAD. It
// returns only on failure.
func LaunchMain(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "tele: launch: nothing to run")
		return 1
	}
	if err := unix.Mount("tele-noproc", "/proc", "tmpfs",
		unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "size=4k,mode=555"); err != nil {
		fmt.Fprintf(os.Stderr, "tele: launch: cover /proc: %v\n", err)
		return 1
	}
	env := os.Environ()
	for i, kv := range env {
		if v, ok := strings.CutPrefix(kv, PreloadEnv+"="); ok {
			env[i] = "LD_PRELOAD=" + v
		}
	}
	err := unix.Exec(argv[0], argv, env)
	fmt.Fprintf(os.Stderr, "tele: launch: exec %s: %v\n", argv[0], err)
	return 1
}

// LaunchAttr is the SysProcAttr for the command that enters LaunchMain.
func LaunchAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Unshareflags: syscall.CLONE_NEWNS,
		AmbientCaps:  Caps,
	}
}
