// Package privtest gates tests that need privileges the environment may not
// grant (user namespaces, FUSE). Such tests skip with the reason, unless
// TELE_TEST_REQUIRE_PRIV=1 is set, in which case they fail so that CI never
// skips them silently (docs/coding-standards.md "测试").
package privtest

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// RequireEnv is the variable that turns missing capabilities into failures.
const RequireEnv = "TELE_TEST_REQUIRE_PRIV"

func unavailable(t testing.TB, what string, err error) {
	t.Helper()
	if os.Getenv(RequireEnv) == "1" {
		t.Fatalf("%s unavailable (%s=1): %v", what, RequireEnv, err)
	}
	t.Skipf("%s unavailable: %v", what, err)
}

var (
	usernsOnce sync.Once
	errUserNS  error
)

// RequireUserNS skips or fails t unless the process can create a user and
// mount namespace with its own uid mapped to itself, and use CAP_SYS_ADMIN
// in it. Creating the namespace is not enough: Ubuntu's AppArmor
// restriction on unprivileged user namespaces (docs/filesystem.md
// "已知陷阱") lets the creation succeed and denies every capability in the
// namespace. So the probe also unshares a second mount namespace, after
// which os/exec makes the mounts private, and raises CAP_SYS_ADMIN as an
// ambient capability, as the tests do.
func RequireUserNS(t testing.TB) {
	t.Helper()
	usernsOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/true")
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags:   syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
			Unshareflags: syscall.CLONE_NEWNS,
			UidMappings:  []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}},
			GidMappings:  []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}},
			AmbientCaps:  []uintptr{unix.CAP_SYS_ADMIN},
		}
		errUserNS = cmd.Run()
	})
	if errUserNS != nil {
		unavailable(t, "user namespaces", errUserNS)
	}
}

// RequireFUSE skips or fails t unless /dev/fuse can be opened read-write.
func RequireFUSE(t testing.TB) {
	t.Helper()
	fd, err := unix.Open("/dev/fuse", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		unavailable(t, "/dev/fuse", err)
		return
	}
	_ = unix.Close(fd)
}

// RequireRoot skips t unless it runs as uid 0 in the initial user
// namespace, for tests that mount without creating a user namespace. It
// skips even under TELE_TEST_REQUIRE_PRIV: the privileged tests run once as
// root and once as an ordinary user, which must still run every user
// namespace and FUSE test (docs/coding-standards.md "测试").
func RequireRoot(t testing.TB) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root; run the privileged tests as root to cover it")
	}
}
