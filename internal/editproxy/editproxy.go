// Package editproxy opens a file of the remote view in the user's own
// editor on this machine, for the tele-editor shim Claude starts as its
// editor (docs/claude-code.md "外部编辑器与 IDE 探测").
//
// The editor runs in session main's local view, where the remote view's
// paths mean nothing or, worse, name other, local files. So it never gets
// the file itself: Edit copies the file into a private directory in the
// session directory, runs the editor on the copy, and writes the copy back
// if it changed. The file is opened in the remote view through the /proc
// root of a process in it, resolved within that root, so that a symbolic
// link from the target host cannot lead outside it (docs/security.md
// "远端返回的数据").
package editproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/localexec"
	"github.com/ujzk/tele-agent/internal/proto"
)

// MaxSize bounds the file Edit copies. Claude edits prompts, memory files
// and plans, all small.
const MaxSize = 16 << 20

// codeFailure is the shim's infrastructure failure code (docs/exec.md
// "shim").
const codeFailure = 255

// fallbacks are the editors Claude looks for in PATH when neither VISUAL
// nor EDITOR is set, in its order.
var fallbacks = []string{"code", "vi", "nano"}

// waitFlags makes GUI editors wait for the file to be closed, as Claude
// does for the same names; without it they return at once and the copy is
// written back unchanged.
var waitFlags = map[string]string{"code": "-w", "subl": "--wait"}

// ErrNoEditor reports that the user has no editor configured or installed.
var ErrNoEditor = errors.New("editproxy: no editor: set VISUAL or EDITOR")

// Config configures Edit.
type Config struct {
	// Dir is the session directory in session main's view; the copy goes
	// into a fresh directory in it.
	Dir string
	// Env is the user's own environment: it chooses the editor, whose
	// environment it becomes.
	Env []string
}

// Request is one file to edit.
type Request struct {
	// Path is the file, an absolute path in the remote view.
	Path string
	// ViewPID is a process in the remote view: the shim, which waits for
	// Edit to return.
	ViewPID int
	// Stdin, Stdout and Stderr are the shim's: the user's terminal.
	Stdin, Stdout, Stderr *os.File
}

// Command returns the editor command in env, as Claude would choose it:
// VISUAL, else EDITOR, split into words, else the first of code, vi and
// nano in PATH.
func Command(env []string) ([]string, error) {
	for _, key := range []string{"VISUAL", "EDITOR"} {
		if words := strings.Fields(envValue(env, key)); len(words) > 0 {
			return withWait(words), nil
		}
	}
	for _, name := range fallbacks {
		if _, err := localexec.LookPath(name, env); err == nil {
			return withWait([]string{name}), nil
		}
	}
	return nil, ErrNoEditor
}

func withWait(words []string) []string {
	if f, ok := waitFlags[words[0]]; ok && len(words) == 1 {
		return []string{words[0], f}
	}
	return words
}

// Edit opens req.Path in the user's editor and reports how the editor
// ended, for the shim to reproduce. The editor joins the shim's process
// group, the terminal's foreground job, as it would as Claude's child.
func Edit(ctx context.Context, cfg Config, req Request, sigs <-chan int) proto.ShimStatus {
	argv, err := Command(cfg.Env)
	if err != nil {
		return localexec.NotStarted(unix.ENOENT, "%s: %v", NameOf(req.Path), err)
	}
	pgid, err := unix.Getpgid(req.ViewPID)
	if err != nil {
		return failure(req.Path, fmt.Errorf("process group of %d: %w", req.ViewPID, err))
	}
	root, err := unix.Open(fmt.Sprintf("/proc/%d/root", req.ViewPID), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return failure(req.Path, fmt.Errorf("open the remote view: %w", err))
	}
	defer unix.Close(root)

	orig, err := readIn(root, req.Path)
	if err != nil {
		return failure(req.Path, err)
	}
	dir, err := os.MkdirTemp(cfg.Dir, "edit-")
	if err != nil {
		return failure(req.Path, err)
	}
	defer os.RemoveAll(dir)
	cp := filepath.Join(dir, NameOf(req.Path))
	if err := os.WriteFile(cp, orig, 0o600); err != nil {
		return failure(req.Path, err)
	}

	st := localexec.RunInGroup(ctx, append(argv, cp), dir, cfg.Env, req.Stdin, req.Stdout, req.Stderr, sigs, pgid)
	edited, err := readFile(cp)
	if err != nil {
		return failure(req.Path, fmt.Errorf("read the edited copy: %w", err))
	}
	if !bytes.Equal(edited, orig) {
		if err := writeBack(root, req.Path, edited); err != nil {
			return failure(req.Path, err)
		}
	}
	return st
}

// NameOf is the name of the copy of the file p: its own, so that the
// editor recognizes the file type, or "file" for "/".
func NameOf(p string) string {
	if b := path.Base(p); b != "/" && b != "." {
		return b
	}
	return "file"
}

func failure(p string, err error) proto.ShimStatus {
	return proto.ShimStatus{Code: codeFailure, Msg: fmt.Sprintf("tele editor: %s: %v", p, err)}
}

// openIn opens p, an absolute path in the view whose root is root, resolved
// within that root.
func openIn(root int, p string, flags int) (*os.File, error) {
	fd, err := unix.Openat2(root, strings.TrimLeft(p, "/"), &unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC | unix.O_NOCTTY),
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	return os.NewFile(uintptr(fd), p), nil
}

// readIn reads the regular file p in the view whose root is root.
func readIn(root int, p string) ([]byte, error) {
	f, err := openIn(root, p, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readRegular(f)
}

// readFile reads the regular file p in session main's view.
func readFile(p string) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readRegular(f)
}

// readRegular reads f, which must be a regular file of at most MaxSize
// bytes. O_NONBLOCK at open keeps a FIFO from blocking before the check.
func readRegular(f *os.File) ([]byte, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", f.Name())
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxSize {
		return nil, fmt.Errorf("%s: larger than %d bytes", f.Name(), MaxSize)
	}
	return b, nil
}

// writeBack replaces the contents of p in the view whose root is root,
// keeping the file itself, its owner and mode, as an editor writing in
// place does.
func writeBack(root int, p string, b []byte) error {
	f, err := openIn(root, p, unix.O_WRONLY|unix.O_NONBLOCK)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s: not a regular file", p)
	}
	if err == nil {
		err = f.Truncate(0)
	}
	if err == nil {
		_, err = f.Write(b)
	}
	return errors.Join(err, f.Close())
}

// envValue returns the value of the last key entry in env.
func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if val, ok := strings.CutPrefix(kv, key+"="); ok {
			v = val
		}
	}
	return v
}
