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
// mount namespace with its own uid mapped to itself.
func RequireUserNS(t testing.TB) {
	t.Helper()
	usernsOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/true")
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}},
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

// RequireRoot skips or fails t unless it runs as uid 0 in the initial user
// namespace, for tests that mount without creating a user namespace.
func RequireRoot(t testing.TB) {
	t.Helper()
	if os.Geteuid() != 0 {
		unavailable(t, "root", unix.EPERM)
	}
}
