// Package telefs is the FUSE file system through which the Claude process
// sees the target host's whole "/" (docs/telefs.md). It runs in session
// main: every FUSE request becomes one proto.FSRequest on its own mux
// stream, and the changes the server pushes on the watch stream drive
// kernel cache invalidation, which is what allows long entry and attribute
// TTLs (docs/telefs.md "一致性").
//
// Kernel contracts that shape the implementation:
//
//   - Every node is presented as owned by Config.UID and Config.GID. The
//     kernel refuses writes to inodes whose owner is unmapped in the user
//     namespace, and permissions are checked by the server as the target
//     user, so the mount does not use default_permissions (FSAccess
//     answers access(2)). After mounting, the mount point is stat'ed once
//     so that the kernel replaces the root inode's initial owner, uid 0
//     (docs/filesystem.md "已知陷阱").
//   - Placeholders, the mount points of the local set, and their ancestors
//     never receive entry invalidations and always look up to the same
//     inode, even when the server is unreachable: an entry invalidation or
//     a failed revalidation runs d_invalidate, which detaches every mount
//     on or below the dentry (docs/filesystem.md "已知陷阱").
//   - FUSE passthrough is never used: BACKING_OPEN needs CAP_SYS_ADMIN in
//     the initial user namespace.
//
// Transport failures are reported to the kernel as EIO; remote failures as
// the remote errno, unchanged unless the kernel cannot take it (kernelErrno).
package telefs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// Errors reported by FS.
var (
	// ErrWatchRunning is returned by RunWatch while another RunWatch runs.
	ErrWatchRunning = errors.New("telefs: watch stream already running")
	// ErrWatchStopped is returned by WaitApplied when the watch stream
	// ended before the requested event was applied.
	ErrWatchStopped = errors.New("telefs: watch stream stopped")
)

// Opener opens a stream to the server. *mux.Session implements it.
type Opener interface {
	Open(kind proto.StreamKind) (net.Conn, error)
}

// Placeholder is a mount point of the local set (docs/filesystem.md
// "本地集合"): an empty directory or file synthesized at Path whatever the
// server has there, so that session main can bind-mount a local path onto
// it. Its missing ancestors are synthesized too.
type Placeholder struct {
	Path string
	Dir  bool
}

// Config configures a mount.
type Config struct {
	Opener       Opener
	Placeholders []Placeholder
	// LocalNames lists directories some of whose entries are local.
	LocalNames []LocalNames
	// UID and GID are presented as the owner of every node.
	UID, GID uint32
	// AttrTimeout and EntryTimeout are the kernel cache TTLs while the
	// watch stream is healthy; otherwise shortTTL applies.
	AttrTimeout, EntryTimeout time.Duration
	Logger                    *slog.Logger
}

// shortTTL is the cache TTL whenever changes are not pushed: before the
// watch stream delivered its first event, after it ended, and for
// directories the server could not watch.
const shortTTL = time.Second

// FS is a mounted telefs.
type FS struct {
	cfg     Config
	log     *slog.Logger
	rootDev uint64 // remote device of "/", see mapIno
	born    time.Time
	root    *node

	server    *fuse.Server
	serveDone chan struct{}

	// healthy is set while the watch stream delivers events.
	healthy atomic.Bool
	// invalGen increases before every batch of invalidations. A request
	// that sees it change caches its reply only briefly: the invalidation
	// may have run before the kernel installed the reply.
	invalGen atomic.Uint64

	mu           sync.Mutex // guards the watch stream state below
	watchRunning bool
	watchStopped bool // a watch stream ran and ended
	haveEpoch    bool
	epoch        uint64
	applied      uint64
	appliedWake  chan struct{} // closed and replaced when the state changes

	reg  registry
	open openHandles

	done chan struct{} // closed by Unmount; stops the forget workers
	wg   sync.WaitGroup
}

// Mount mounts telefs on mountpoint. The caller must already be in the
// mount namespace (and user namespace) that should own the mount.
func Mount(mountpoint string, cfg Config) (*FS, error) {
	return mount(mountpoint, cfg, true)
}

// mount is Mount; tests pass refreshRoot=false to observe the kernel's
// initial root owner.
func mount(mountpoint string, cfg Config, refreshRoot bool) (*FS, error) {
	if cfg.Opener == nil {
		return nil, errors.New("telefs: no opener")
	}
	if err := checkLocalNames(cfg.LocalNames); err != nil {
		return nil, err
	}
	spec, err := buildTree(cfg.Placeholders, cfg.LocalNames)
	if err != nil {
		return nil, err
	}
	f := &FS{
		cfg:         cfg,
		log:         cfg.Logger,
		born:        time.Now(),
		appliedWake: make(chan struct{}),
		done:        make(chan struct{}),
	}
	if f.log == nil {
		f.log = slog.New(slog.DiscardHandler)
	}
	f.reg.init()

	// The remote identity of "/" anchors inode numbering (mapIno).
	resp, errno := f.call(&proto.FSRequest{Op: proto.FSGetattr, Path: "/"})
	if errno != 0 {
		return nil, fmt.Errorf("telefs: stat remote root: %w", errno)
	}
	f.rootDev = resp.Attr.Dev
	f.root = f.newSynthTree(spec)
	f.resolveAncestors(f.root)

	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			DirectMountStrict:    true,
			FsName:               "telefs",
			Name:                 "telefs",
			MaxWrite:             proto.MaxIO,
			EnableSymlinkCaching: true,
			DisabledCapabilities: fuse.CAP_PASSTHROUGH,
			Logger:               slog.NewLogLogger(f.log.Handler(), slog.LevelDebug),
		},
		NullPermissions: true,
		RootStableAttr:  &fs.StableAttr{Ino: rootIno, Gen: 1},
		OnAdd:           func(ctx context.Context) { f.attachSynth(ctx, f.root) },
		Logger:          slog.NewLogLogger(f.log.Handler(), slog.LevelDebug),
	}
	srv, err := fuse.NewServer(negativeEntries{fs.NewNodeFS(f.root, opts)}, mountpoint, &opts.MountOptions)
	if err != nil {
		return nil, fmt.Errorf("telefs: mount %s: %w", mountpoint, err)
	}
	f.server = srv
	f.serveDone = make(chan struct{})
	go func() {
		defer close(f.serveDone)
		srv.Serve()
	}()
	if err := srv.WaitMount(); err != nil {
		_ = srv.Unmount()
		<-f.serveDone
		return nil, fmt.Errorf("telefs: mount %s: %w", mountpoint, err)
	}
	// docs/filesystem.md "已知陷阱": the kernel initializes the root inode
	// with uid 0, which is unmapped in a user namespace; a GETATTR makes
	// it take the presented owner.
	if refreshRoot {
		var st unix.Stat_t
		if err := unix.Stat(mountpoint, &st); err != nil {
			_ = f.Unmount()
			return nil, fmt.Errorf("telefs: stat mount point: %w", err)
		}
	}
	f.startForgetWorkers()
	return f, nil
}

// Unmount unmounts the file system and waits for its goroutines. It fails
// with EBUSY while the mount is in use (open files, mounts on top).
func (f *FS) Unmount() error {
	if err := f.server.Unmount(); err != nil {
		return fmt.Errorf("telefs: unmount: %w", err)
	}
	<-f.serveDone
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	f.wg.Wait()
	return nil
}

// call sends one request on its own stream and waits for the response. A
// non-zero errno comes either from the server (the remote errno) or from
// the transport (EIO); the response is returned in both cases when there
// is one, because error responses still carry Unwatched.
//
// FUSE interrupts do not cancel the request: the server may already have
// performed it, and reporting EINTR for an operation that took effect
// would make callers retry non-idempotent operations. The transport bounds
// the wait instead.
func (f *FS) call(req *proto.FSRequest) (*proto.FSResponse, syscall.Errno) {
	c, err := f.cfg.Opener.Open(proto.StreamFS)
	if err != nil {
		f.log.Warn("telefs: open fs stream", "op", req.Op, "err", err)
		return nil, syscall.EIO
	}
	defer func() { _ = c.Close() }()
	if err := proto.WriteFrame(c, req); err != nil {
		f.log.Warn("telefs: send fs request", "op", req.Op, "err", err)
		return nil, syscall.EIO
	}
	var resp proto.FSResponse
	if err := proto.ReadFrame(c, &resp, proto.MaxDataFrame); err != nil {
		f.log.Warn("telefs: receive fs response", "op", req.Op, "err", err)
		return nil, syscall.EIO
	}
	if resp.Errno != 0 {
		errno := kernelErrno(resp.Errno)
		if errno != syscall.Errno(resp.Errno) {
			f.log.Warn("telefs: remote errno not representable", "op", req.Op, "errno", resp.Errno, "as", errno)
		}
		return &resp, errno
	}
	if !validResponse(req, &resp) {
		f.log.Warn("telefs: invalid fs response", "op", req.Op)
		return nil, syscall.EIO
	}
	return &resp, 0
}

// maxErrno is the largest errno the kernel accepts in a FUSE reply: it
// rejects a reply whose error is -ERESTARTSYS (-512) or lower with EINVAL,
// and go-fuse ignores that failure, so the request is never answered and
// the caller hangs in an uninterruptible wait that not even SIGKILL or the
// exit of the FUSE server ends (fuse_dev_do_write, request_wait_answer).
const maxErrno = 511

// enotsupp is the kernel-internal ENOTSUPP, which NFS leaks to user space
// from xattr and ACL operations; user space spells it EOPNOTSUPP.
const enotsupp = 524

// kernelErrno returns the errno to hand to the kernel for remote errno e.
// Errnos the kernel cannot take in a reply are kernel-internal codes that
// leaked out of a remote file system (or invalid): the local kernel cannot
// represent them anyway, so the closest meaning is used, or EIO.
func kernelErrno(e uint32) syscall.Errno {
	switch {
	case e >= 1 && e <= maxErrno:
		return syscall.Errno(e)
	case e == enotsupp:
		return syscall.EOPNOTSUPP
	default:
		return syscall.EIO
	}
}

// validResponse checks what the server returned for req before the kernel
// sees it: the server is a peer, not a trusted component.
func validResponse(req *proto.FSRequest, resp *proto.FSResponse) bool {
	switch req.Op {
	case proto.FSLookup, proto.FSGetattr, proto.FSSetattr, proto.FSMkdir, proto.FSMknod,
		proto.FSSymlink, proto.FSLink:
		return resp.Attr != nil
	case proto.FSCreate:
		return resp.Attr != nil && resp.Handle != 0
	case proto.FSOpen, proto.FSOpendir:
		return resp.Handle != 0
	case proto.FSRead:
		return len(resp.Data) <= int(req.Size)
	case proto.FSStatfs:
		return resp.Statfs != nil
	case proto.FSReaddir:
		for i := range resp.Entries {
			n := resp.Entries[i].Name
			if n != "." && n != ".." && proto.CheckName(n) != nil {
				return false
			}
		}
		return true
	case proto.FSGetxattr, proto.FSListxattr:
		return len(resp.Data) <= int(req.Size)
	default:
		return true
	}
}

// negativeEntries turns an ENOENT reply to LOOKUP into a negative entry
// (node ID 0) cached for the entry TTL that node.Lookup left in the reply.
// go-fuse supports only one fixed negative TTL, but whether a missing name
// may be cached for long depends on whether its directory is watched.
type negativeEntries struct {
	fuse.RawFileSystem
}

// Lookup implements fuse.RawFileSystem.
func (r negativeEntries) Lookup(cancel <-chan struct{}, header *fuse.InHeader, name string, out *fuse.EntryOut) fuse.Status {
	st := r.RawFileSystem.Lookup(cancel, header, name, out)
	if st == fuse.ENOENT && out.EntryTimeout() > 0 {
		ttl := out.EntryTimeout()
		*out = fuse.EntryOut{}
		out.SetEntryTimeout(ttl)
		return fuse.OK
	}
	return st
}

// ttls returns the entry and attribute TTLs for a reply to a request that
// started when invalGen was gen.
func (f *FS) ttls(gen uint64, unwatched bool) (entry, attr time.Duration) {
	if unwatched || !f.healthy.Load() || f.invalGen.Load() != gen {
		return shortTTL, shortTTL
	}
	return f.cfg.EntryTimeout, f.cfg.AttrTimeout
}

// Synthetic inode numbers have the top bit set; numbers derived from the
// remote never do (mapIno).
const (
	synthIno = 1 << 63
	rootIno  = synthIno
)

// mapIno derives the local inode number of a remote object from its device
// and inode, like go-fuse's loopback: objects on the device of "/" keep
// their inode numbers, others mix their device into the high half. Distinct
// remote objects collide only across devices whose inode numbers exceed 32
// bits in a matching pattern; a table would avoid that at the cost of
// unbounded memory.
func (f *FS) mapIno(dev, ino uint64) uint64 {
	swap := func(d uint64) uint64 { return d<<32 | d>>32 }
	return (swap(dev) ^ swap(f.rootDev) ^ ino) &^ synthIno
}

// fuseAttr converts remote attributes for the kernel, presenting the
// configured owner.
func (f *FS) fuseAttr(a *proto.Attr, ino uint64) fuse.Attr {
	at, mt, ct := time.Unix(0, a.Atime), time.Unix(0, a.Mtime), time.Unix(0, a.Ctime)
	return fuse.Attr{
		Ino:       ino,
		Size:      uint64(a.Size),
		Blocks:    uint64(a.Blocks),
		Atime:     uint64(at.Unix()),
		Atimensec: uint32(at.Nanosecond()),
		Mtime:     uint64(mt.Unix()),
		Mtimensec: uint32(mt.Nanosecond()),
		Ctime:     uint64(ct.Unix()),
		Ctimensec: uint32(ct.Nanosecond()),
		Mode:      a.Mode,
		Nlink:     a.Nlink,
		Owner:     fuse.Owner{Uid: f.cfg.UID, Gid: f.cfg.GID},
		Rdev:      fuseRdev(a.Rdev),
		Blksize:   uint32(a.Blksize),
	}
}

// fuseRdev converts a dev_t as returned by stat(2) to the kernel's 32-bit
// encoding (new_encode_dev) that FUSE attributes carry.
func fuseRdev(rdev uint64) uint32 {
	major, minor := unix.Major(rdev), unix.Minor(rdev)
	return minor&0xff | (major&0xfff)<<8 | (minor&^0xff)<<12
}

// remoteRdev is the inverse of fuseRdev.
func remoteRdev(rdev uint32) uint64 {
	major := (rdev & 0xfff00) >> 8
	minor := rdev&0xff | (rdev>>12)&0xfff00
	return unix.Mkdev(major, minor)
}
