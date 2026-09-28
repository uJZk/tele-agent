package shim

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// ensureStdio opens /dev/null on any of fds 0, 1 and 2 that is closed, so
// that all three can be passed with SCM_RIGHTS (a closed fd fails the whole
// message with EBADF) and the command sees a closed stream as an empty or
// discarding one. The Go runtime does the same at startup; this also covers
// fds closed since then. Ascending order makes open(2) return exactly the
// missing number.
func ensureStdio() error {
	for fd := range 3 {
		_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err == nil {
			continue
		}
		if !errors.Is(err, unix.EBADF) {
			return err
		}
		got, err := unix.Open("/dev/null", unix.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("open /dev/null: %w", err)
		}
		if got != fd {
			_ = unix.Close(got) // never used
			return fmt.Errorf("open /dev/null: got fd %d, want %d", got, fd)
		}
	}
	return nil
}
