package execsvc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// checkStart validates an ExecStart from the client.
func checkStart(s *proto.ExecStart) error {
	if len(s.Argv) == 0 || s.Argv[0] == "" {
		return errors.New("empty command")
	}
	for _, a := range s.Argv {
		if strings.IndexByte(a, 0) >= 0 {
			return errors.New("argument contains NUL")
		}
	}
	if err := proto.CheckPath(s.Dir); err != nil {
		return err
	}
	for _, kv := range s.Env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.IndexByte(kv, 0) >= 0 {
			return fmt.Errorf("malformed environment entry %q", k)
		}
	}
	return proto.CheckScratch(s.Scratch)
}

// lookPath resolves the program to run. A name with a slash is used as
// given, relative to dir; any other name is looked up in the login PATH,
// because tele server's own PATH is usually minimal (docs/exec.md
// "shim"). Like a shell, it prefers an executable later in PATH to a
// non-executable file earlier, and reports EACCES only when nothing
// executable was found but something was.
func lookPath(name, loginPath, dir string) (string, *proto.Error) {
	if strings.Contains(name, "/") {
		if !filepath.IsAbs(name) {
			name = filepath.Join(dir, name)
		}
		return name, nil
	}
	errno := unix.ENOENT
	for _, d := range filepath.SplitList(loginPath) {
		// Relative entries would resolve against the command's directory,
		// which is never what a login PATH intends.
		if !filepath.IsAbs(d) {
			continue
		}
		p := filepath.Join(d, name)
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() {
			continue
		}
		if unix.Faccessat(unix.AT_FDCWD, p, unix.X_OK, unix.AT_EACCESS) == nil {
			return p, nil
		}
		errno = unix.EACCES
	}
	return "", &proto.Error{Errno: uint32(errno), Msg: name}
}

// mergeEnv applies extra over base; for a repeated variable the later
// entry wins but keeps the earlier position.
func mergeEnv(base, extra []string) []string {
	out := make([]string, 0, len(base)+len(extra))
	index := make(map[string]int, len(base)+len(extra))
	for _, list := range [...][]string{base, extra} {
		for _, kv := range list {
			k, _, _ := strings.Cut(kv, "=")
			if i, ok := index[k]; ok {
				out[i] = kv
				continue
			}
			index[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}
