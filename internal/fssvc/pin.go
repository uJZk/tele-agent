package fssvc

import (
	"errors"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// pinned is a node opened with O_PATH|O_NOFOLLOW. It pins the object a
// request names, so that an identity check and the change that follows it
// cannot land on different objects when the path is replaced in between,
// and a final symlink is never followed.
//
// Operations prefer the *at system calls with AT_EMPTY_PATH. Where a kernel
// lacks them (faccessat2 before 5.8, fchmodat2 before 6.6) or a seccomp
// filter refuses them, they go through /proc/self/fd/N instead: that magic
// link resolves to the object itself, even a symlink, so the kernel still
// checks and changes exactly the pinned object. This needs /proc on the
// target host.
type pinned struct {
	fd int
	st unix.Stat_t
}

// pin opens real path p. When want is set, it fails with ESTALE unless p
// still names that object (proto.FSRequest.Node).
func pin(p string, want *proto.NodeID) (*pinned, uint32) {
	n := &pinned{}
	err := ignoringEINTR(func() error {
		var err error
		n.fd, err = unix.Open(p, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		return err
	})
	if err != nil {
		return nil, errnoOf(err)
	}
	if err := ignoringEINTR(func() error { return unix.Fstat(n.fd, &n.st) }); err != nil {
		n.close()
		return nil, errnoOf(err)
	}
	if want != nil && !n.is(want) {
		n.close()
		return nil, uint32(unix.ESTALE)
	}
	return n, 0
}

func (n *pinned) close() {
	_ = unix.Close(n.fd)
}

// is reports whether n is the object id.
func (n *pinned) is(id *proto.NodeID) bool {
	return n.st.Dev == id.Dev && n.st.Ino == id.Ino
}

func (n *pinned) isSymlink() bool {
	return n.st.Mode&unix.S_IFMT == unix.S_IFLNK
}

// proc is the magic link through which path-based calls reach n itself.
func (n *pinned) proc() string {
	return "/proc/self/fd/" + strconv.Itoa(n.fd)
}

// unsupported reports whether err means that a system call or flag is not
// available, rather than an answer about the object: ENOSYS, EOPNOTSUPP
// (x/sys's translation of a missing fchmodat2), EINVAL (an unknown flag),
// or EPERM (the answer of some seccomp filters to unknown system calls).
// Retrying through proc() yields the kernel's real answer in every case.
func unsupported(err error) bool {
	return errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EPERM)
}

// refresh re-reads the attributes of n after a change.
func (n *pinned) refresh() *proto.FSResponse {
	return fstatResp(n.fd)
}

// chmod changes the mode of n. Linux cannot change the mode of a symlink.
func (n *pinned) chmod(mode uint32) error {
	if n.isSymlink() {
		return unix.EOPNOTSUPP
	}
	err := ignoringEINTR(func() error { return unix.Fchmodat(n.fd, "", mode, unix.AT_EMPTY_PATH) })
	if err != nil && unsupported(err) {
		err = ignoringEINTR(func() error { return unix.Chmod(n.proc(), mode) })
	}
	return err
}

// chown changes the owners of n; -1 leaves an ID unchanged.
func (n *pinned) chown(uid, gid int) error {
	return ignoringEINTR(func() error { return unix.Fchownat(n.fd, "", uid, gid, unix.AT_EMPTY_PATH) })
}

// utimes sets the times of n.
func (n *pinned) utimes(ts *[2]unix.Timespec) error {
	err := ignoringEINTR(func() error {
		return unix.UtimesNanoAt(n.fd, "", ts[:], unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW)
	})
	if err != nil && unsupported(err) {
		err = ignoringEINTR(func() error { return unix.UtimesNanoAt(unix.AT_FDCWD, n.proc(), ts[:], 0) })
	}
	return err
}

// access checks n for access(2) mode mask as the server's user. The x/sys
// Faccessat wrapper is not used: when faccessat2 is missing or refused it
// emulates the check from the mode bits, ignoring ACLs, immutable files and
// read-only mounts (docs/coding-standards.md "错误处理"). The fallback here is
// the kernel's own faccessat on the magic link.
func (n *pinned) access(mask uint32) error {
	err := ignoringEINTR(func() error {
		return unix.Faccessat2(n.fd, "", mask, unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW)
	})
	if err != nil && unsupported(err) {
		// Flags 0 is the raw faccessat system call.
		err = ignoringEINTR(func() error { return unix.Faccessat(unix.AT_FDCWD, n.proc(), mask, 0) })
	}
	return err
}
