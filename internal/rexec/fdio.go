package rexec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// pipeBuf is PIPE_BUF on Linux: after poll reports a pipe writable, a
// write of at most this many bytes does not block even on a blocking
// descriptor.
const pipeBuf = 4096

// errStopped reports that an I/O wait was interrupted by its wakeFD.
var errStopped = errors.New("rexec: stopped")

// wakeFD interrupts poll(2) waits. Once woken it stays readable.
type wakeFD struct {
	fd int // immutable; valid until close

	mu     sync.Mutex
	woken  bool // guarded by mu
	closed bool // guarded by mu
}

func newWakeFD() (*wakeFD, error) {
	fd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("rexec: eventfd: %w", err)
	}
	return &wakeFD{fd: fd}, nil
}

// wake makes every current and future wait on w return errStopped. It is
// safe to call after close.
func (w *wakeFD) wake() {
	// The write happens under mu so that it can never hit a descriptor
	// number that close released and something else reused.
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.woken || w.closed {
		return
	}
	w.woken = true
	var one [8]byte
	binary.NativeEndian.PutUint64(one[:], 1)
	// A fresh eventfd counter cannot overflow, so the write cannot fail.
	_, _ = unix.Write(w.fd, one[:])
}

// close releases the descriptor once no goroutine waits on it any more.
func (w *wakeFD) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.closed = true
		_ = unix.Close(w.fd)
	}
}

// fdKind decides how to do I/O on a descriptor without changing its flags.
type fdKind int

const (
	kindStream fdKind = iota // pipe, FIFO, terminal, other character device
	kindSocket               // supports MSG_DONTWAIT
	kindFile                 // regular file or block device: never waits
)

func kindOf(fd int) fdKind {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return kindStream
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG, unix.S_IFBLK:
		return kindFile
	case unix.S_IFSOCK:
		return kindSocket
	default:
		return kindStream
	}
}

// waitFD waits until fd reports one of events (or an error condition), or
// stop is woken, which takes precedence.
func waitFD(fd int, events int16, stop *wakeFD) error {
	fds := []unix.PollFd{{Fd: int32(fd), Events: events}, {Fd: int32(stop.fd), Events: unix.POLLIN}}
	for {
		if _, err := unix.Poll(fds, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if fds[1].Revents != 0 {
			return errStopped
		}
		if fds[0].Revents != 0 {
			return nil
		}
	}
}

// readSome reads available bytes from f into buf, waiting until some are
// available, EOF (io.EOF), or stop is woken (errStopped).
//
// On a blocking pipe or terminal it reads at most what FIONREAD reports,
// so the read cannot block. That fails only if another process drains the
// same description between poll and read; the read then blocks until
// more input or EOF arrives, and stop takes effect after it.
func readSome(f *os.File, buf []byte, stop *wakeFD) (int, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		n    int
		rerr error
	)
	// Control keeps the descriptor open while it is in use, without
	// taking the file's read lock or touching its flags.
	if err := rc.Control(func(fd uintptr) { n, rerr = readFD(int(fd), buf, stop) }); err != nil {
		return 0, err
	}
	return n, rerr
}

func readFD(fd int, buf []byte, stop *wakeFD) (int, error) {
	kind := kindOf(fd)
	for {
		if err := waitFD(fd, unix.POLLIN, stop); err != nil {
			return 0, err
		}
		var (
			n   int
			err error
		)
		switch kind {
		case kindSocket:
			n, _, err = unix.Recvfrom(fd, buf, unix.MSG_DONTWAIT)
		case kindStream:
			b := buf
			if avail, ierr := unix.IoctlGetInt(fd, unix.TIOCINQ); ierr == nil && avail > 0 && avail < len(b) {
				b = b[:avail]
			}
			n, err = unix.Read(fd, b)
		case kindFile:
			n, err = unix.Read(fd, buf)
		}
		switch {
		case n > 0:
			return n, nil
		case errors.Is(err, unix.EINTR), errors.Is(err, unix.EAGAIN):
			continue
		case err != nil:
			return 0, err
		default:
			return 0, io.EOF
		}
	}
}

// writeAll writes data to f, waiting while f is full, until everything is
// written, an error occurs, or stop is woken (errStopped).
func writeAll(f *os.File, data []byte, stop *wakeFD) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var werr error
	if err := rc.Control(func(fd uintptr) { werr = writeFD(int(fd), data, stop) }); err != nil {
		return err
	}
	return werr
}

func writeFD(fd int, data []byte, stop *wakeFD) error {
	kind := kindOf(fd)
	for len(data) > 0 {
		if err := waitFD(fd, unix.POLLOUT, stop); err != nil {
			return err
		}
		var (
			n   int
			err error
		)
		switch kind {
		case kindSocket:
			n, err = unix.SendmsgN(fd, data, nil, nil, unix.MSG_DONTWAIT|unix.MSG_NOSIGNAL)
		case kindStream:
			n, err = unix.Write(fd, data[:min(len(data), pipeBuf)])
		case kindFile:
			n, err = unix.Write(fd, data)
		}
		switch {
		case errors.Is(err, unix.EINTR), errors.Is(err, unix.EAGAIN):
			continue
		case err != nil:
			return err
		}
		data = data[n:]
	}
	return nil
}
