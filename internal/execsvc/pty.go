package execsvc

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// openPTY allocates a pseudo-terminal of the given size without cgo, the
// way posix_openpt, grantpt and unlockpt do on Linux. The master is
// non-blocking and registered with the runtime poller; the slave stays
// blocking because it becomes the command's stdio.
func openPTY(size proto.TTYSize) (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open pty master: %w", err)
	}
	defer func() {
		if err != nil {
			_ = master.Close()
		}
	}()
	rc, err := master.SyscallConn()
	if err != nil {
		return nil, nil, fmt.Errorf("pty master: %w", err)
	}
	var (
		n    uint32
		ierr error
	)
	if err := rc.Control(func(fd uintptr) {
		if ierr = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); ierr != nil {
			ierr = fmt.Errorf("unlock pty: %w", ierr)
			return
		}
		if n, ierr = unix.IoctlGetUint32(int(fd), unix.TIOCGPTN); ierr != nil {
			ierr = fmt.Errorf("get pty number: %w", ierr)
			return
		}
		ierr = setWinsizeFD(int(fd), size)
	}); err != nil {
		return nil, nil, fmt.Errorf("pty master: %w", err)
	}
	if ierr != nil {
		return nil, nil, ierr
	}
	name := "/dev/pts/" + strconv.FormatUint(uint64(n), 10)
	sfd, err := unix.Open(name, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open pty slave: %w", err)
	}
	return master, os.NewFile(uintptr(sfd), name), nil
}

// setWinsize sets the size of the pty whose master is f; the kernel
// sends SIGWINCH to its foreground process group.
func setWinsize(f *os.File, size proto.TTYSize) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ierr error
	if err := rc.Control(func(fd uintptr) { ierr = setWinsizeFD(int(fd), size) }); err != nil {
		return err
	}
	return ierr
}

func setWinsizeFD(fd int, size proto.TTYSize) error {
	if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: size.Rows, Col: size.Cols}); err != nil {
		return fmt.Errorf("set pty size: %w", err)
	}
	return nil
}

// eofChar returns the pty's EOF character (VEOF, usually ^D). On a master,
// TCGETS reports the slave's settings.
func eofChar(master *os.File) byte {
	const ctrlD = 4
	rc, err := master.SyscallConn()
	if err != nil {
		return ctrlD
	}
	c := byte(ctrlD)
	_ = rc.Control(func(fd uintptr) {
		if t, err := unix.IoctlGetTermios(int(fd), unix.TCGETS); err == nil && t.Cc[unix.VEOF] != 0 {
			c = t.Cc[unix.VEOF]
		}
	})
	return c
}
