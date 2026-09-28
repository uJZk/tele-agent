package fssvc

import (
	"errors"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// Limits on request fields, matching the kernel's own limits.
const (
	maxXattrSize = 64 << 10 // XATTR_SIZE_MAX
	maxXattrName = 255      // XATTR_NAME_MAX
	maxLinkLen   = 4095     // PATH_MAX without the NUL
)

// openFlags are the open(2) flags a client may pass through. Everything
// else the kernel may set (O_CREAT, O_EXCL, O_NOCTTY, FMODE_EXEC, ...)
// is either meaningless here or decided by the server.
const openFlags = unix.O_ACCMODE | unix.O_APPEND | unix.O_TRUNC | unix.O_SYNC |
	unix.O_DSYNC | unix.O_NONBLOCK

// renameFlags are the renameat2 flags a client may pass through.
const renameFlags = unix.RENAME_NOREPLACE | unix.RENAME_EXCHANGE | unix.RENAME_WHITEOUT

// errResp is a response carrying only an errno.
func errResp(errno uint32) *proto.FSResponse {
	return &proto.FSResponse{Errno: errno}
}

var (
	einval = uint32(unix.EINVAL)
	enosys = uint32(unix.ENOSYS)
)

// do executes one request. It never panics on peer input: every field is
// validated before use, and invalid requests get EINVAL.
func (s *Service) do(req *proto.FSRequest) *proto.FSResponse {
	switch req.Op {
	case proto.FSLookup:
		return s.withEntry(req, s.lookup)
	case proto.FSGetattr:
		return s.getattr(req)
	case proto.FSSetattr:
		return s.setattr(req)
	case proto.FSOpendir:
		return s.withPath(req, s.opendir)
	case proto.FSReaddir:
		return s.withHandle(req.Handle, func(h *handle) *proto.FSResponse {
			if !h.dir {
				return errResp(uint32(unix.ENOTDIR))
			}
			unwatched := !s.watch(h.path)
			resp := s.readdir(h, req.Offset, req.Size)
			resp.Unwatched = unwatched
			return resp
		})
	case proto.FSReleasedir, proto.FSRelease:
		return errResp(s.handles.release(req.Handle))
	case proto.FSOpen:
		return s.withPath(req, func(p string) *proto.FSResponse { return s.open(p, req.Flags) })
	case proto.FSCreate:
		return s.withEntry(req, func(dir, name string) *proto.FSResponse { return s.create(dir, name, req.Flags, req.Mode) })
	case proto.FSRead:
		return s.withHandle(req.Handle, func(h *handle) *proto.FSResponse { return s.read(h, req.Offset, req.Size) })
	case proto.FSWrite:
		return s.withHandle(req.Handle, func(h *handle) *proto.FSResponse { return s.write(h, req.Offset, req.Data) })
	case proto.FSFsync:
		return s.withHandle(req.Handle, func(h *handle) *proto.FSResponse { return s.fsync(h, req.Flags) })
	case proto.FSMkdir:
		return s.withEntry(req, func(dir, name string) *proto.FSResponse { return s.mkdir(dir, name, req.Mode) })
	case proto.FSMknod:
		return s.withEntry(req, func(dir, name string) *proto.FSResponse { return s.mknod(dir, name, req.Mode, req.Rdev) })
	case proto.FSUnlink:
		return s.withEntry(req, func(dir, name string) *proto.FSResponse { return s.remove(dir, name, 0) })
	case proto.FSRmdir:
		return s.withEntry(req, func(dir, name string) *proto.FSResponse { return s.remove(dir, name, unix.AT_REMOVEDIR) })
	case proto.FSRename:
		return s.rename(req)
	case proto.FSSymlink:
		return s.withEntry(req, func(dir, name string) *proto.FSResponse { return s.symlink(dir, name, req.Target) })
	case proto.FSLink:
		return s.link(req)
	case proto.FSReadlink:
		return s.withPath(req, s.readlink)
	case proto.FSStatfs:
		return s.withPath(req, s.statfs)
	case proto.FSAccess:
		return s.withPath(req, func(p string) *proto.FSResponse { return s.access(p, req.Mask) })
	case proto.FSGetxattr, proto.FSListxattr, proto.FSSetxattr, proto.FSRemovexattr:
		return s.xattr(req)
	case proto.FSForget:
		if proto.CheckPath(req.Path) == nil {
			s.unwatch(req.Path)
		}
		return nil
	default:
		return errResp(enosys)
	}
}

// withPath validates req.Path and runs fn with it.
func (s *Service) withPath(req *proto.FSRequest, fn func(p string) *proto.FSResponse) *proto.FSResponse {
	if proto.CheckPath(req.Path) != nil {
		return errResp(einval)
	}
	return fn(req.Path)
}

// withEntry validates req.Path as a directory and req.Name as an entry of
// it, and runs fn with them.
func (s *Service) withEntry(req *proto.FSRequest, fn func(dir, name string) *proto.FSResponse) *proto.FSResponse {
	if proto.CheckPath(req.Path) != nil || proto.CheckName(req.Name) != nil {
		return errResp(einval)
	}
	return fn(req.Path, req.Name)
}

// withHandle pins handle id for the duration of fn.
func (s *Service) withHandle(id uint64, fn func(h *handle) *proto.FSResponse) *proto.FSResponse {
	h, errno := s.handles.acquire(id)
	if errno != 0 {
		return errResp(errno)
	}
	defer s.handles.put(h)
	return fn(h)
}

// lstatResp returns the attributes of real path p as a response.
func lstatResp(p string) *proto.FSResponse {
	var st unix.Stat_t
	if err := ignoringEINTR(func() error { return unix.Lstat(p, &st) }); err != nil {
		return errResp(errnoOf(err))
	}
	return &proto.FSResponse{Attr: attrFromStat(&st)}
}

func fstatResp(fd int) *proto.FSResponse {
	var st unix.Stat_t
	if err := ignoringEINTR(func() error { return unix.Fstat(fd, &st) }); err != nil {
		return errResp(errnoOf(err))
	}
	return &proto.FSResponse{Attr: attrFromStat(&st)}
}

func attrFromStat(st *unix.Stat_t) *proto.Attr {
	return &proto.Attr{
		Dev:     st.Dev,
		Ino:     st.Ino,
		Mode:    st.Mode,
		Nlink:   uint32(st.Nlink),
		UID:     st.Uid,
		GID:     st.Gid,
		Rdev:    st.Rdev,
		Size:    st.Size,
		Blocks:  st.Blocks,
		Blksize: int32(st.Blksize),
		Atime:   st.Atim.Nano(),
		Mtime:   st.Mtim.Nano(),
		Ctime:   st.Ctim.Nano(),
	}
}

func (s *Service) lookup(dir, name string) *proto.FSResponse {
	// Watch before looking: a change right after the lstat must produce an
	// event, or the client would cache a stale entry.
	unwatched := !s.watch(dir)
	resp := lstatResp(s.child(dir, name))
	resp.Unwatched = unwatched
	return resp
}

func (s *Service) getattr(req *proto.FSRequest) *proto.FSResponse {
	if req.Handle != 0 {
		return s.withHandle(req.Handle, func(h *handle) *proto.FSResponse { return fstatResp(h.fd) })
	}
	return s.withPath(req, func(p string) *proto.FSResponse { return lstatResp(s.real(p)) })
}

func (s *Service) opendir(p string) *proto.FSResponse {
	unwatched := !s.watch(p)
	var fd int
	err := ignoringEINTR(func() error {
		var err error
		fd, err = unix.Open(s.real(p), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		return err
	})
	if err != nil {
		return &proto.FSResponse{Errno: errnoOf(err), Unwatched: unwatched}
	}
	id := s.handles.add(&handle{fd: fd, dir: true, path: p})
	return &proto.FSResponse{Handle: id, Unwatched: unwatched}
}

func (s *Service) open(p string, flags uint32) *proto.FSResponse {
	var fd int
	err := ignoringEINTR(func() error {
		var err error
		fd, err = unix.Open(s.real(p), int(flags)&openFlags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NOCTTY, 0)
		return err
	})
	if err != nil {
		return errResp(errnoOf(err))
	}
	return &proto.FSResponse{Handle: s.handles.add(&handle{fd: fd})}
}

func (s *Service) create(dir, name string, flags, mode uint32) *proto.FSResponse {
	unwatched := !s.watch(dir)
	p := s.child(dir, name)
	base := int(flags)&openFlags | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NOCTTY
	perm := mode & 07777
	var fd int
	openAt := func(fl int) error {
		return ignoringEINTR(func() error {
			var err error
			fd, err = unix.Open(p, fl, perm)
			return err
		})
	}
	// Create exclusively first, so that the mode is fixed up only on a
	// file this request created.
	created := true
	err := openAt(base | unix.O_CREAT | unix.O_EXCL)
	if errors.Is(err, unix.EEXIST) && flags&unix.O_EXCL == 0 {
		created = false
		err = openAt(base)
	}
	if err != nil {
		return &proto.FSResponse{Errno: errnoOf(err), Unwatched: unwatched}
	}
	if created {
		s.fixMode(func(m uint32) error { return unix.Fchmod(fd, m) }, perm)
	}
	resp := fstatResp(fd)
	if resp.Errno != 0 {
		_ = unix.Close(fd)
		resp.Unwatched = unwatched
		return resp
	}
	resp.Handle = s.handles.add(&handle{fd: fd})
	resp.Unwatched = unwatched
	return resp
}

// fixMode applies perm to a node the service just created. The client
// kernel applied the caller's umask before sending the mode (telefs does not
// negotiate FUSE_DONT_MASK), so the server's own umask must not remove
// further bits. It is skipped when the umask cannot have changed anything,
// which also leaves default ACLs of the parent directory in charge.
func (s *Service) fixMode(chmod func(uint32) error, perm uint32) {
	if s.umask >= 0 && perm&uint32(s.umask) == 0 {
		return
	}
	if err := ignoringEINTR(func() error { return chmod(perm) }); err != nil {
		s.logDebug("fssvc: fix mode of new node", "err", err)
	}
}

func (s *Service) read(h *handle, off int64, size uint32) *proto.FSResponse {
	if h.dir {
		return errResp(uint32(unix.EISDIR))
	}
	if off < 0 {
		return errResp(einval)
	}
	n := min(int(size), proto.MaxIO)
	buf := make([]byte, n)
	total := 0
	// Fill the buffer: FUSE takes a short read for end of file and trims
	// the cached file size to it.
	for total < n {
		var m int
		err := ignoringEINTR(func() error {
			var err error
			m, err = unix.Pread(h.fd, buf[total:], off+int64(total))
			return err
		})
		if err != nil {
			if total == 0 {
				return errResp(errnoOf(err))
			}
			break
		}
		if m == 0 {
			break
		}
		total += m
	}
	return &proto.FSResponse{Data: buf[:total]}
}

func (s *Service) write(h *handle, off int64, data []byte) *proto.FSResponse {
	if h.dir {
		return errResp(uint32(unix.EISDIR))
	}
	if off < 0 || len(data) > proto.MaxIO {
		return errResp(einval)
	}
	total := 0
	for total < len(data) {
		var m int
		err := ignoringEINTR(func() error {
			var err error
			// With O_APPEND, Linux pwrite appends whatever the offset,
			// which is exactly what the client's append expects.
			m, err = unix.Pwrite(h.fd, data[total:], off+int64(total))
			return err
		})
		if err != nil {
			if total == 0 {
				return errResp(errnoOf(err))
			}
			break
		}
		if m == 0 {
			break
		}
		total += m
	}
	return &proto.FSResponse{Written: uint32(total)}
}

func (s *Service) fsync(h *handle, flags uint32) *proto.FSResponse {
	err := ignoringEINTR(func() error {
		if flags&1 != 0 {
			return unix.Fdatasync(h.fd)
		}
		return unix.Fsync(h.fd)
	})
	if err != nil {
		return errResp(errnoOf(err))
	}
	return &proto.FSResponse{}
}

func (s *Service) mkdir(dir, name string, mode uint32) *proto.FSResponse {
	unwatched := !s.watch(dir)
	p := s.child(dir, name)
	perm := mode & 07777
	if err := ignoringEINTR(func() error { return unix.Mkdir(p, perm) }); err != nil {
		return &proto.FSResponse{Errno: errnoOf(err), Unwatched: unwatched}
	}
	s.fixMode(func(m uint32) error { return unix.Chmod(p, m) }, perm)
	resp := lstatResp(p)
	resp.Unwatched = unwatched
	return resp
}

func (s *Service) mknod(dir, name string, mode uint32, rdev uint64) *proto.FSResponse {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG, unix.S_IFIFO, unix.S_IFSOCK, unix.S_IFCHR, unix.S_IFBLK:
	default:
		return errResp(einval)
	}
	unwatched := !s.watch(dir)
	p := s.child(dir, name)
	perm := mode & 07777
	if err := ignoringEINTR(func() error { return unix.Mknod(p, mode, int(rdev)) }); err != nil {
		return &proto.FSResponse{Errno: errnoOf(err), Unwatched: unwatched}
	}
	s.fixMode(func(m uint32) error { return unix.Chmod(p, m) }, perm)
	resp := lstatResp(p)
	resp.Unwatched = unwatched
	return resp
}

func (s *Service) remove(dir, name string, flags int) *proto.FSResponse {
	unwatched := !s.watch(dir)
	err := ignoringEINTR(func() error { return unix.Unlinkat(unix.AT_FDCWD, s.child(dir, name), flags) })
	return &proto.FSResponse{Errno: errnoOfOrZero(err), Unwatched: unwatched}
}

func errnoOfOrZero(err error) uint32 {
	if err == nil {
		return 0
	}
	return errnoOf(err)
}

func (s *Service) rename(req *proto.FSRequest) *proto.FSResponse {
	if proto.CheckPath(req.Path) != nil || proto.CheckName(req.Name) != nil ||
		proto.CheckPath(req.Path2) != nil || proto.CheckName(req.Name2) != nil ||
		req.Flags&^renameFlags != 0 {
		return errResp(einval)
	}
	unwatched := !s.watch(req.Path)
	unwatched = !s.watch(req.Path2) || unwatched
	err := ignoringEINTR(func() error {
		return unix.Renameat2(unix.AT_FDCWD, s.child(req.Path, req.Name), unix.AT_FDCWD, s.child(req.Path2, req.Name2), uint(req.Flags))
	})
	return &proto.FSResponse{Errno: errnoOfOrZero(err), Unwatched: unwatched}
}

func (s *Service) symlink(dir, name, target string) *proto.FSResponse {
	if target == "" || len(target) > maxLinkLen || strings.IndexByte(target, 0) >= 0 {
		return errResp(einval)
	}
	unwatched := !s.watch(dir)
	p := s.child(dir, name)
	if err := ignoringEINTR(func() error { return unix.Symlink(target, p) }); err != nil {
		return &proto.FSResponse{Errno: errnoOf(err), Unwatched: unwatched}
	}
	resp := lstatResp(p)
	resp.Unwatched = unwatched
	return resp
}

func (s *Service) link(req *proto.FSRequest) *proto.FSResponse {
	if proto.CheckPath(req.Path) != nil || proto.CheckPath(req.Path2) != nil || proto.CheckName(req.Name2) != nil {
		return errResp(einval)
	}
	unwatched := !s.watch(req.Path2)
	p := s.child(req.Path2, req.Name2)
	// Flags 0: a symlink is linked itself, not its target.
	err := ignoringEINTR(func() error { return unix.Linkat(unix.AT_FDCWD, s.real(req.Path), unix.AT_FDCWD, p, 0) })
	if err != nil {
		return &proto.FSResponse{Errno: errnoOf(err), Unwatched: unwatched}
	}
	resp := lstatResp(p)
	resp.Unwatched = unwatched
	return resp
}

func (s *Service) readlink(p string) *proto.FSResponse {
	buf := make([]byte, maxLinkLen+1)
	var n int
	err := ignoringEINTR(func() error {
		var err error
		n, err = unix.Readlink(s.real(p), buf)
		return err
	})
	if err != nil {
		return errResp(errnoOf(err))
	}
	return &proto.FSResponse{Target: string(buf[:n])}
}

func (s *Service) statfs(p string) *proto.FSResponse {
	var st unix.Statfs_t
	if err := ignoringEINTR(func() error { return unix.Statfs(s.real(p), &st) }); err != nil {
		return errResp(errnoOf(err))
	}
	return &proto.FSResponse{Statfs: &proto.Statfs{
		Blocks:  st.Blocks,
		Bfree:   st.Bfree,
		Bavail:  st.Bavail,
		Files:   st.Files,
		Ffree:   st.Ffree,
		Bsize:   uint32(st.Bsize),
		Namelen: uint32(st.Namelen),
		Frsize:  uint32(st.Frsize),
	}}
}

func (s *Service) access(p string, mask uint32) *proto.FSResponse {
	if mask&^uint32(unix.R_OK|unix.W_OK|unix.X_OK) != 0 {
		return errResp(einval)
	}
	err := ignoringEINTR(func() error {
		return unix.Faccessat(unix.AT_FDCWD, s.real(p), mask, unix.AT_SYMLINK_NOFOLLOW)
	})
	return errResp(errnoOfOrZero(err))
}

// checkXattrName validates an extended attribute name from the peer.
func checkXattrName(name string) bool {
	return name != "" && len(name) <= maxXattrName && strings.IndexByte(name, 0) < 0
}

func (s *Service) xattr(req *proto.FSRequest) *proto.FSResponse {
	if proto.CheckPath(req.Path) != nil {
		return errResp(einval)
	}
	p := s.real(req.Path)
	if req.Op != proto.FSListxattr && !checkXattrName(req.Name2) {
		return errResp(einval)
	}
	switch req.Op {
	case proto.FSGetxattr, proto.FSListxattr:
		get := func(dest []byte) (int, error) {
			if req.Op == proto.FSGetxattr {
				return unix.Lgetxattr(p, req.Name2, dest)
			}
			return unix.Llistxattr(p, dest)
		}
		var dest []byte
		if req.Size > 0 {
			dest = make([]byte, min(int(req.Size), maxXattrSize))
		}
		var n int
		err := ignoringEINTR(func() error {
			var err error
			n, err = get(dest)
			return err
		})
		if err != nil {
			return errResp(errnoOf(err))
		}
		if req.Size == 0 {
			return &proto.FSResponse{Size: uint32(n)}
		}
		return &proto.FSResponse{Data: dest[:n]}
	case proto.FSSetxattr:
		if len(req.Data) > maxXattrSize {
			return errResp(uint32(unix.E2BIG))
		}
		err := ignoringEINTR(func() error { return unix.Lsetxattr(p, req.Name2, req.Data, int(req.Flags)) })
		return errResp(errnoOfOrZero(err))
	default: // proto.FSRemovexattr
		err := ignoringEINTR(func() error { return unix.Lremovexattr(p, req.Name2) })
		return errResp(errnoOfOrZero(err))
	}
}

func (s *Service) setattr(req *proto.FSRequest) *proto.FSResponse {
	sa := req.SetAttr
	if sa == nil {
		return errResp(einval)
	}
	if req.Handle != 0 {
		return s.withHandle(req.Handle, func(h *handle) *proto.FSResponse {
			if errno := s.setattrFD(h.fd, sa); errno != 0 {
				return errResp(errno)
			}
			return fstatResp(h.fd)
		})
	}
	return s.withPath(req, func(vp string) *proto.FSResponse {
		p := s.real(vp)
		if errno := s.setattrPath(p, sa); errno != 0 {
			return errResp(errno)
		}
		return lstatResp(p)
	})
}

// Set attributes in the order of the libfuse passthrough example: mode,
// owner, size, times. Times come last because truncation updates mtime.

func (s *Service) setattrFD(fd int, sa *proto.SetAttr) uint32 {
	if sa.Valid&proto.SetMode != 0 {
		if err := ignoringEINTR(func() error { return unix.Fchmod(fd, sa.Mode&07777) }); err != nil {
			return errnoOf(err)
		}
	}
	if sa.Valid&(proto.SetUID|proto.SetGID) != 0 {
		uid, gid := owners(sa)
		if err := ignoringEINTR(func() error { return unix.Fchown(fd, uid, gid) }); err != nil {
			return errnoOf(err)
		}
	}
	if sa.Valid&proto.SetSize != 0 {
		if err := ignoringEINTR(func() error { return unix.Ftruncate(fd, sa.Size) }); err != nil {
			return errnoOf(err)
		}
	}
	if ts, ok := times(sa); ok {
		if err := ignoringEINTR(func() error { return futimens(fd, &ts) }); err != nil {
			return errnoOf(err)
		}
	}
	return 0
}

func (s *Service) setattrPath(p string, sa *proto.SetAttr) uint32 {
	if sa.Valid&proto.SetMode != 0 {
		if err := lchmod(p, sa.Mode&07777); err != nil {
			return errnoOf(err)
		}
	}
	if sa.Valid&(proto.SetUID|proto.SetGID) != 0 {
		uid, gid := owners(sa)
		if err := ignoringEINTR(func() error { return unix.Lchown(p, uid, gid) }); err != nil {
			return errnoOf(err)
		}
	}
	if sa.Valid&proto.SetSize != 0 {
		// truncate(2) follows symlinks; open the node itself instead.
		err := ignoringEINTR(func() error {
			fd, err := unix.Open(p, unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOCTTY, 0)
			if err != nil {
				return err
			}
			defer func() { _ = unix.Close(fd) }()
			return unix.Ftruncate(fd, sa.Size)
		})
		if err != nil {
			return errnoOf(err)
		}
	}
	if ts, ok := times(sa); ok {
		if err := ignoringEINTR(func() error { return unix.UtimesNanoAt(unix.AT_FDCWD, p, ts[:], unix.AT_SYMLINK_NOFOLLOW) }); err != nil {
			return errnoOf(err)
		}
	}
	return 0
}

// lchmod changes the mode of p without following a final symlink. Linux has
// no lchmod; fchmodat2 (6.6+) implements AT_SYMLINK_NOFOLLOW, and older
// kernels fall back to checking the type first (symlink modes cannot be
// changed on Linux anyway).
func lchmod(p string, mode uint32) error {
	err := ignoringEINTR(func() error { return unix.Fchmodat(unix.AT_FDCWD, p, mode, unix.AT_SYMLINK_NOFOLLOW) })
	if !errors.Is(err, unix.EOPNOTSUPP) {
		return err
	}
	var st unix.Stat_t
	if err := ignoringEINTR(func() error { return unix.Lstat(p, &st) }); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return unix.EOPNOTSUPP
	}
	return ignoringEINTR(func() error { return unix.Fchmodat(unix.AT_FDCWD, p, mode, 0) })
}

// owners returns the chown arguments of sa; -1 leaves an ID unchanged.
func owners(sa *proto.SetAttr) (uid, gid int) {
	uid, gid = -1, -1
	if sa.Valid&proto.SetUID != 0 {
		uid = int(sa.UID)
	}
	if sa.Valid&proto.SetGID != 0 {
		gid = int(sa.GID)
	}
	return uid, gid
}

// times returns the utimensat arguments of sa, and false if no time is set.
// The *Now bits use the target host's clock.
func times(sa *proto.SetAttr) ([2]unix.Timespec, bool) {
	ts := [2]unix.Timespec{{Nsec: unix.UTIME_OMIT}, {Nsec: unix.UTIME_OMIT}}
	set := false
	switch {
	case sa.Valid&proto.SetAtimeNow != 0:
		ts[0] = unix.Timespec{Nsec: unix.UTIME_NOW}
		set = true
	case sa.Valid&proto.SetAtime != 0:
		ts[0] = unix.NsecToTimespec(sa.Atime)
		set = true
	}
	switch {
	case sa.Valid&proto.SetMtimeNow != 0:
		ts[1] = unix.Timespec{Nsec: unix.UTIME_NOW}
		set = true
	case sa.Valid&proto.SetMtime != 0:
		ts[1] = unix.NsecToTimespec(sa.Mtime)
		set = true
	}
	return ts, set
}

// futimens sets the times of an open file. x/sys has no wrapper that passes
// a NULL path to utimensat, which is how the kernel spells futimens.
func futimens(fd int, ts *[2]unix.Timespec) error {
	_, _, e := unix.Syscall6(unix.SYS_UTIMENSAT, uintptr(fd), 0, uintptr(unsafe.Pointer(&ts[0])), 0, 0, 0) //nolint:gosec // G103: the syscall takes a pointer to the timespec pair, as in x/sys
	if e != 0 {
		return e
	}
	return nil
}
