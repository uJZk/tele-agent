// Package hostinfo describes the target host and user for the session
// handshake (proto.TargetInfo). It runs on the server side.
package hostinfo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// loginPathTimeout bounds running the user's login shell to read PATH.
// Login profiles can be slow, but a hung profile must not hang the handshake.
const loginPathTimeout = 10 * time.Second

// pathMarker frames PATH in the login shell's output, because profiles may
// print banners or other noise on stdout.
const pathMarker = "__TELE_LOGIN_PATH__"

// FallbackPath is used when the login shell cannot report PATH.
const FallbackPath = "/usr/local/bin:/usr/bin:/bin"

// Gather describes the host and the user this process runs as.
func Gather(ctx context.Context) (proto.TargetInfo, error) {
	var t proto.TargetInfo
	u, err := user.Current()
	if err != nil {
		return t, fmt.Errorf("hostinfo: current user: %w", err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return t, fmt.Errorf("hostinfo: uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return t, fmt.Errorf("hostinfo: gid %q: %w", u.Gid, err)
	}
	t.User, t.UID, t.GID, t.Home = u.Username, uint32(uid), uint32(gid), u.HomeDir

	if t.Hostname, err = os.Hostname(); err != nil {
		return t, fmt.Errorf("hostinfo: hostname: %w", err)
	}
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return t, fmt.Errorf("hostinfo: uname: %w", err)
	}
	t.Kernel = unix.ByteSliceToString(uts.Sysname[:]) + " " + unix.ByteSliceToString(uts.Release[:])
	t.Arch = unix.ByteSliceToString(uts.Machine[:])
	t.OSPrettyName = osPrettyName()
	t.Shell = loginShell(t.UID)
	t.LoginPath = LoginPath(ctx, t.Shell)
	return t, nil
}

// osPrettyName reads PRETTY_NAME from os-release(5).
func osPrettyName() string {
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if name := ParseOSRelease(b); name != "" {
			return name
		}
	}
	return "Linux"
}

// ParseOSRelease returns PRETTY_NAME from os-release(5) content, unquoted.
func ParseOSRelease(b []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || k != "PRETTY_NAME" {
			continue
		}
		if uq, err := strconv.Unquote(v); err == nil {
			return uq
		}
		return strings.Trim(v, `"'`)
	}
	return ""
}

// loginShell returns the user's shell from the passwd database, falling
// back to $SHELL and then /bin/sh. os/user does not expose the shell.
func loginShell(uid uint32) string {
	if b, err := os.ReadFile("/etc/passwd"); err == nil {
		if sh := ParsePasswdShell(b, uid); sh != "" {
			return sh
		}
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// ParsePasswdShell returns the shell field of the first passwd(5) entry
// with the given uid, or "".
func ParsePasswdShell(b []byte, uid uint32) string {
	want := strconv.FormatUint(uint64(uid), 10)
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) == 7 && f[2] == want {
			return f[6]
		}
	}
	return ""
}

// LoginPath runs shell as a login shell and returns the PATH it sets, or
// FallbackPath if that fails. Commands in argv form are looked up in it
// (docs/exec.md section 1).
func LoginPath(ctx context.Context, shell string) string {
	ctx, cancel := context.WithTimeout(ctx, loginPathTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-l", "-c", `printf '\n`+pathMarker+`%s\n' "$PATH"`)
	cmd.Stdin = nil // /dev/null: a profile must not consume the caller's input
	out, err := cmd.Output()
	if err != nil && !errors.As(err, new(*exec.ExitError)) {
		return FallbackPath
	}
	if p := ParseLoginPath(out); p != "" {
		return p
	}
	return FallbackPath
}

// ParseLoginPath extracts PATH printed after pathMarker.
func ParseLoginPath(out []byte) string {
	for line := range strings.SplitSeq(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, pathMarker); ok && p != "" {
			return p
		}
	}
	return ""
}
