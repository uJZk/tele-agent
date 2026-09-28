// Package localexec runs a local exec proxy: a program that must see local
// processes or the local desktop, run by session main in its local view on
// behalf of a shim (docs/exec.md "shim").
//
// Run gives the program the shim's own stdio and reports how it ended in
// the form the shim reproduces, so that for Claude it behaves like a local
// child: same exit code, same terminating signal.
package localexec

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// Exit codes for a program that could not be run, as sh reports them.
const (
	codeCannotExecute = 126
	codeNotFound      = 127
	// codeFailure is the shim's infrastructure failure code
	// (docs/exec.md "shim").
	codeFailure = 255
)

// defaultPath is searched when env has no PATH, as execvp(3) does.
const defaultPath = "/bin:/usr/bin"

// maxSignal is the highest Linux signal number.
const maxSignal = 64

// Run runs argv with working directory dir (the current one if empty) and
// exactly the environment env, in a new process group, and waits for it.
// argv[0] is looked up in env's PATH. Signal numbers received on sigs are
// delivered to the process group until the program exits; when ctx ends
// first, the group is killed with SIGKILL. A nil sigs never delivers.
//
// stdin, stdout and stderr are passed to the program as they are, flags
// included (docs/exec.md "shim 与会话主进程"); nil ones are replaced by
// /dev/null. Run does not close them. Processes the program leaves behind
// in its group keep running after Run returns, as they would locally.
//
// Its own process group makes the program a background job for a
// controlling terminal: reading from the terminal stops it with SIGTTIN.
// Local exec proxies therefore must not need the terminal.
func Run(ctx context.Context, argv []string, dir string, env []string, stdin, stdout, stderr *os.File, sigs <-chan int) proto.ShimStatus {
	if len(argv) == 0 {
		return proto.ShimStatus{Code: codeFailure, Msg: "local exec: empty command"}
	}
	path, err := lookPath(argv[0], pathOf(env))
	switch {
	case errors.Is(err, unix.ENOENT):
		return notStarted(err, "%s: command not found", argv[0])
	case err != nil:
		return notStarted(err, "%s: %v", argv[0], err)
	}
	if dir != "" {
		// os.StartProcess reports a failed chdir as a failed exec of the
		// program; name the directory instead.
		if _, err := os.Stat(dir); err != nil {
			return notStarted(err, "%s: chdir %s: %v", argv[0], dir, errors.Unwrap(err))
		}
	}
	if err := ctx.Err(); err != nil {
		return proto.ShimStatus{Code: codeFailure, Msg: fmt.Sprintf("%s: %v", argv[0], err)}
	}
	files, closeNull, err := stdio(stdin, stdout, stderr)
	if err != nil {
		return proto.ShimStatus{Code: codeFailure, Msg: fmt.Sprintf("%s: %v", argv[0], err)}
	}
	if env == nil {
		env = []string{} // nil would mean "inherit session main's environment"
	}
	proc, err := os.StartProcess(path, argv, &os.ProcAttr{
		Dir:   dir,
		Env:   env,
		Files: files,
		Sys:   &syscall.SysProcAttr{Setpgid: true},
	})
	closeNull()
	if err != nil {
		return notStarted(err, "%s: %v", argv[0], err)
	}
	return wait(ctx, proc, sigs)
}

// wait forwards signals to proc's group until proc exits, then reaps it.
// proc is observed with waitid(WNOWAIT) and reaped only after forwarding
// has stopped: while it is an unreaped zombie its pid, and so the group
// id, cannot be reused, so no signal can reach an unrelated group.
func wait(ctx context.Context, proc *os.Process, sigs <-chan int) proto.ShimStatus {
	pgid := proc.Pid
	exited := make(chan error, 1)
	go func() { exited <- waitExited(pgid) }()

	done := ctx.Done()
	var waitErr error
loop:
	for {
		select {
		case waitErr = <-exited:
			break loop
		case sig, ok := <-sigs:
			if !ok {
				sigs = nil
				continue
			}
			if sig >= 1 && sig <= maxSignal {
				_ = unix.Kill(-pgid, unix.Signal(sig)) // ESRCH: the group is gone already
			}
		case <-done:
			_ = unix.Kill(-pgid, unix.SIGKILL)
			done = nil
		}
	}

	state, err := proc.Wait()
	if err != nil {
		return proto.ShimStatus{Code: codeFailure, Msg: fmt.Sprintf("local exec: wait: %v", errors.Join(waitErr, err))}
	}
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return proto.ShimStatus{Code: codeFailure, Msg: "local exec: unexpected wait status"}
	}
	if ws.Signaled() {
		return proto.ShimStatus{Signal: int(ws.Signal())}
	}
	return proto.ShimStatus{Code: ws.ExitStatus()}
}

// waitExited blocks until pid has terminated, without reaping it.
func waitExited(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// stdio returns the child's fds 0-2, substituting /dev/null for nil ones.
// A missing fd would be allocated by the next open(2) in the child.
func stdio(stdin, stdout, stderr *os.File) (files []*os.File, closeNull func(), err error) {
	files = []*os.File{stdin, stdout, stderr}
	var null *os.File
	for i, f := range files {
		if f != nil {
			continue
		}
		if null == nil {
			if null, err = os.OpenFile(os.DevNull, os.O_RDWR, 0); err != nil {
				return nil, nil, err
			}
		}
		files[i] = null
	}
	return files, func() {
		if null != nil {
			_ = null.Close() // the child has its own copy
		}
	}, nil
}

// pathOf returns the value of the last PATH entry in env, or defaultPath.
func pathOf(env []string) string {
	p := defaultPath
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			p = v
		}
	}
	return p
}

// lookPath resolves file like execvp(3): a name with a slash is used as it
// is; otherwise the first executable regular file in pathList wins, and
// failing that the result is EACCES if some candidate was not executable,
// ENOENT if none existed. Relative PATH entries are skipped: they would be
// resolved in the child's working directory, which is in session main's
// view rather than the one the caller's PATH was written for.
func lookPath(file, pathList string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil
	}
	if file == "" {
		return "", unix.ENOENT
	}
	var denied bool
	for _, dir := range filepath.SplitList(pathList) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, file)
		fi, err := os.Stat(p)
		switch {
		case errors.Is(err, fs.ErrPermission):
			denied = true
		case err != nil:
		case !fi.Mode().IsRegular():
			denied = true
		case unix.Faccessat(unix.AT_FDCWD, p, unix.X_OK, unix.AT_EACCESS) != nil:
			denied = true
		default:
			return p, nil
		}
	}
	if denied {
		return "", unix.EACCES
	}
	return "", unix.ENOENT
}

// notStarted reports a program that could not be started, with the exit
// code a shell would use for err.
func notStarted(err error, format string, args ...any) proto.ShimStatus {
	code := codeCannotExecute
	if errors.Is(err, unix.ENOENT) {
		code = codeNotFound
	}
	return proto.ShimStatus{Code: code, Msg: fmt.Sprintf(format, args...)}
}
