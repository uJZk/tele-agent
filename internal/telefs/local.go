package telefs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// LocalNames serves the entries of directory Dir of the view whose names
// start with Prefix from the local directory Local, instead of the target
// host's. Claude writes ~/.claude.json through temporary files renamed over
// it and guards it with a lock directory, all named .claude.json* in HOME;
// a bind mount of the single file cannot take that, so the whole name
// family is local (docs/claude-code.md "~/.claude.json").
//
// Dir becomes a synthetic directory like the ancestors of placeholders.
// Entries created there under other names stay remote, and a rename
// between a local and a remote name fails with EXDEV.
type LocalNames struct {
	Dir    string
	Prefix string
	Local  string
}

// localDir is the LocalNames rule of a synthetic directory.
type localDir struct {
	prefix string
	root   string // the local directory
}

// owns reports whether entry name of the directory is local.
func (l *localDir) owns(name string) bool {
	return l != nil && strings.HasPrefix(name, l.prefix)
}

func checkLocalNames(ls []LocalNames) error {
	for _, l := range ls {
		if err := proto.CheckPath(l.Dir); err != nil {
			return fmt.Errorf("telefs: local names: invalid directory %q", l.Dir)
		}
		if proto.CheckName(l.Prefix) != nil {
			return fmt.Errorf("telefs: local names in %s: invalid prefix %q", l.Dir, l.Prefix)
		}
		if !filepath.IsAbs(l.Local) {
			return fmt.Errorf("telefs: local names in %s: local directory %q is not absolute", l.Dir, l.Local)
		}
	}
	return nil
}

// localIno returns the inode number of a local object. Local numbers have
// the two top bits set: remote numbers never have the top bit (mapIno), and
// synthetic ones are small offsets from synthIno.
func localIno(st *unix.Stat_t) uint64 {
	const tag = 3 << 62
	return tag | ((st.Dev<<32|st.Dev>>32)^st.Ino)&^tag
}

// localNode is a local file or directory served through a LocalNames
// rule. Its local path follows its position below home, so that renames
// and the kernel's view agree.
type localNode struct {
	fs.Inode
	fsys *FS
	home *node // the directory with the rule
}

var (
	_ fs.NodeLookuper   = (*localNode)(nil)
	_ fs.NodeGetattrer  = (*localNode)(nil)
	_ fs.NodeSetattrer  = (*localNode)(nil)
	_ fs.NodeOpener     = (*localNode)(nil)
	_ fs.NodeReaddirer  = (*localNode)(nil)
	_ fs.NodeCreater    = (*localNode)(nil)
	_ fs.NodeMkdirer    = (*localNode)(nil)
	_ fs.NodeUnlinker   = (*localNode)(nil)
	_ fs.NodeRmdirer    = (*localNode)(nil)
	_ fs.NodeRenamer    = (*localNode)(nil)
	_ fs.NodeSymlinker  = (*localNode)(nil)
	_ fs.NodeReadlinker = (*localNode)(nil)
	_ fs.NodeStatfser   = (*localNode)(nil)
)

func (l *localNode) path() string {
	return filepath.Join(l.home.local.root, l.Path(&l.home.Inode))
}

// localAttr fills out from st as the kernel must see a local object.
func (f *FS) localAttr(out *fuse.Attr, st *unix.Stat_t) {
	var sst syscall.Stat_t
	sst.Dev, sst.Ino, sst.Nlink, sst.Mode = st.Dev, st.Ino, st.Nlink, st.Mode
	sst.Size, sst.Blksize, sst.Blocks, sst.Rdev = st.Size, st.Blksize, st.Blocks, st.Rdev
	sst.Atim = syscall.Timespec(st.Atim)
	sst.Mtim = syscall.Timespec(st.Mtim)
	sst.Ctim = syscall.Timespec(st.Ctim)
	out.FromStat(&sst)
	out.Ino = localIno(st)
	out.Owner = fuse.Owner{Uid: f.cfg.UID, Gid: f.cfg.GID}
}

// localEntry creates or finds the inode for local object st below parent
// and fills the lookup reply. Nothing is cached: other local processes,
// such as a plain claude, change these files behind the mount's back.
func (f *FS) localEntry(ctx context.Context, parent *fs.Inode, home *node, st *unix.Stat_t, out *fuse.EntryOut) *fs.Inode {
	ch := parent.NewInode(ctx, &localNode{fsys: f, home: home},
		fs.StableAttr{Mode: st.Mode & syscall.S_IFMT, Ino: localIno(st), Gen: 1})
	f.localAttr(&out.Attr, st)
	out.SetEntryTimeout(0)
	out.SetAttrTimeout(0)
	return ch
}

// errnoOf returns the errno of a local system call's error. Unlike
// go-fuse's fs.ToErrno, which answers ENOSYS, it reports an error without
// one as EIO, the errno telefs uses when the cause is unknown
// (docs/coding-standards.md "错误处理").
func errnoOf(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	return syscall.EIO
}

// localAt performs the entry operations of one local directory: dir is
// its local path, parent its inode, home the directory with the rule.
type localAt struct {
	fsys   *FS
	parent *fs.Inode
	home   *node
	dir    string
}

// at returns the entry operations of the directory with the rule itself.
func (n *node) localAt() localAt {
	return localAt{fsys: n.fsys, parent: &n.Inode, home: n, dir: n.local.root}
}

// at returns the entry operations of local directory l.
func (l *localNode) at() localAt {
	return localAt{fsys: l.fsys, parent: &l.Inode, home: l.home, dir: l.path()}
}

func (a localAt) lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	var st unix.Stat_t
	if err := unix.Lstat(filepath.Join(a.dir, name), &st); err != nil {
		out.SetEntryTimeout(0)
		return nil, errnoOf(err)
	}
	return a.fsys.localEntry(ctx, a.parent, a.home, &st, out), 0
}

func (a localAt) create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	// Like go-fuse's loopback: the kernel computes append offsets itself.
	fd, err := unix.Open(filepath.Join(a.dir, name), int(flags&^syscall.O_APPEND)|unix.O_CREAT|unix.O_CLOEXEC, mode)
	if err != nil {
		return nil, nil, 0, errnoOf(err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, nil, 0, errnoOf(err)
	}
	return a.fsys.localEntry(ctx, a.parent, a.home, &st, out), fs.NewLoopbackFile(fd), 0, 0
}

func (a localAt) mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if err := unix.Mkdir(filepath.Join(a.dir, name), mode); err != nil {
		return nil, errnoOf(err)
	}
	return a.lookup(ctx, name, out)
}

func (a localAt) symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if err := unix.Symlink(target, filepath.Join(a.dir, name)); err != nil {
		return nil, errnoOf(err)
	}
	return a.lookup(ctx, name, out)
}

func (a localAt) unlink(name string) syscall.Errno {
	return errnoOf(unix.Unlink(filepath.Join(a.dir, name)))
}

func (a localAt) rmdir(name string) syscall.Errno {
	return errnoOf(unix.Rmdir(filepath.Join(a.dir, name)))
}

// rename moves entry name to entry newName of newParent, which must be
// local under the same rule: anything else is another file system (EXDEV).
func (a localAt) rename(name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	var dir2 string
	switch np := newParent.(type) {
	case *localNode:
		if np.home != a.home {
			return syscall.EXDEV
		}
		dir2 = np.path()
	case *node:
		if np != a.home || !np.local.owns(newName) {
			return syscall.EXDEV
		}
		dir2 = np.local.root
	default:
		return syscall.EXDEV
	}
	return errnoOf(unix.Renameat2(unix.AT_FDCWD, filepath.Join(a.dir, name), unix.AT_FDCWD, filepath.Join(dir2, newName), uint(flags)))
}

// Lookup implements fs.NodeLookuper.
func (l *localNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return l.at().lookup(ctx, name, out)
}

// Getattr implements fs.NodeGetattrer.
func (l *localNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	if fga, ok := fh.(fs.FileGetattrer); ok {
		if errno := fga.Getattr(ctx, out); errno != 0 {
			return errno
		}
		l.present(out)
		return 0
	}
	var st unix.Stat_t
	if err := unix.Lstat(l.path(), &st); err != nil {
		return errnoOf(err)
	}
	l.fsys.localAttr(&out.Attr, &st)
	out.SetTimeout(0)
	return 0
}

// present fixes attributes a file handle filled in: the inode number and
// owner the kernel knows the node by, and no caching.
func (l *localNode) present(out *fuse.AttrOut) {
	out.Ino = l.StableAttr().Ino
	out.Owner = fuse.Owner{Uid: l.fsys.cfg.UID, Gid: l.fsys.cfg.GID}
	out.SetTimeout(0)
}

// Setattr implements fs.NodeSetattrer. Ownership changes are refused, as
// for remote files, whose owner is presented, not real (docs/telefs.md
// "属主").
func (l *localNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if _, ok := in.GetUID(); ok {
		return syscall.EPERM
	}
	if _, ok := in.GetGID(); ok {
		return syscall.EPERM
	}
	if fsa, ok := fh.(fs.FileSetattrer); ok {
		if errno := fsa.Setattr(ctx, in, out); errno != 0 {
			return errno
		}
		l.present(out)
		return 0
	}
	p := l.path()
	if m, ok := in.GetMode(); ok {
		if err := unix.Fchmodat(unix.AT_FDCWD, p, m&0o7777, 0); err != nil {
			return errnoOf(err)
		}
	}
	a, aok := in.GetATime()
	m, mok := in.GetMTime()
	if aok || mok {
		ts := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, {Nsec: unix.UTIME_OMIT}}
		if aok {
			ts[0] = unix.NsecToTimespec(a.UnixNano())
		}
		if mok {
			ts[1] = unix.NsecToTimespec(m.UnixNano())
		}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, p, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return errnoOf(err)
		}
	}
	if sz, ok := in.GetSize(); ok {
		if err := unix.Truncate(p, int64(sz)); err != nil {
			return errnoOf(err)
		}
	}
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		return errnoOf(err)
	}
	l.fsys.localAttr(&out.Attr, &st)
	out.SetTimeout(0)
	return 0
}

// Open implements fs.NodeOpener.
func (l *localNode) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	fd, err := unix.Open(l.path(), int(flags&^syscall.O_APPEND)|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, errnoOf(err)
	}
	return fs.NewLoopbackFile(fd), 0, 0
}

// Readdir implements fs.NodeReaddirer.
func (l *localNode) Readdir(context.Context) (fs.DirStream, syscall.Errno) {
	return fs.NewLoopbackDirStream(l.path())
}

// Create implements fs.NodeCreater.
func (l *localNode) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return l.at().create(ctx, name, flags, mode, out)
}

// Mkdir implements fs.NodeMkdirer.
func (l *localNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return l.at().mkdir(ctx, name, mode, out)
}

// Symlink implements fs.NodeSymlinker.
func (l *localNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return l.at().symlink(ctx, target, name, out)
}

// Unlink implements fs.NodeUnlinker.
func (l *localNode) Unlink(_ context.Context, name string) syscall.Errno {
	return l.at().unlink(name)
}

// Rmdir implements fs.NodeRmdirer.
func (l *localNode) Rmdir(_ context.Context, name string) syscall.Errno {
	return l.at().rmdir(name)
}

// Rename implements fs.NodeRenamer.
func (l *localNode) Rename(_ context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	return l.at().rename(name, newParent, newName, flags)
}

// Readlink implements fs.NodeReadlinker.
func (l *localNode) Readlink(context.Context) ([]byte, syscall.Errno) {
	t, err := os.Readlink(l.path())
	if err != nil {
		return nil, errnoOf(err)
	}
	return []byte(t), 0
}

// Statfs implements fs.NodeStatfser.
func (l *localNode) Statfs(_ context.Context, out *fuse.StatfsOut) syscall.Errno {
	var s syscall.Statfs_t
	if err := syscall.Statfs(l.path(), &s); err != nil {
		return errnoOf(err)
	}
	out.FromStatfsT(&s)
	return 0
}
