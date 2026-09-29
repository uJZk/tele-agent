package telefs

import (
	"context"
	"path"
	"slices"
	"sync/atomic"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/ujzk/tele-agent/internal/proto"
)

// nodeKind distinguishes remote nodes from the synthetic ones that carry
// the local set's mount points.
type nodeKind uint8

const (
	// kindRemote is an object on the target host, identified by its
	// remote (dev, ino).
	kindRemote nodeKind = iota
	// kindAncestor is a synthesized directory above placeholders (and the
	// root): it shows the remote directory at rpath merged with its
	// synthetic children.
	kindAncestor
	// kindPlaceholderDir is an empty synthesized directory, except for
	// synthetic children when it is also an ancestor.
	kindPlaceholderDir
	// kindPlaceholderFile is an empty synthesized regular file.
	kindPlaceholderFile
)

// node implements every FUSE operation of telefs.
type node struct {
	fs.Inode
	fsys *FS
	kind nodeKind

	// Synthetic nodes only. vpath is the path in the view, rpath the
	// remote directory whose entries an ancestor shows (vpath with a
	// symlinked last component resolved), synth the synthetic children.
	// All are fixed at mount.
	vpath    string
	rpath    string
	synth    map[string]*node
	synthIno uint64
	// local, if set, serves some entry names from a local directory
	// (LocalNames).
	local *localDir

	// Remote nodes only: the remote identity, fixed at creation.
	dev, ino uint64
	// shortTTL is set when the server could not watch the directory the
	// node was last looked up in.
	shortTTL atomic.Bool

	// watchPaths are the remote directory paths this node asked the
	// server to watch; guarded by FS.reg.mu.
	watchPaths map[string]struct{}
}

// id returns the remote identity of remote node n.
func (n *node) id() proto.NodeID {
	return proto.NodeID{Dev: n.dev, Ino: n.ino}
}

// callNode sends req, which acts on n itself.
//
// The kernel passes a file handle only for truncation, so fchmod, futimens
// or fsetxattr on an open descriptor arrive like their path-based
// counterparts. With byHandle, req therefore names n by one of its open
// handles when it has one: the handle reaches n's object even after a
// remote rename replaced it at n's path, or after n was unlinked, as the
// descriptor does locally. Otherwise req names n by path and remote
// identity, and the server fails with ESTALE if the path names another
// object now; a path-based system call then repeats its lookup once.
func (n *node) callNode(req *proto.FSRequest, byHandle bool) (*proto.FSResponse, syscall.Errno) {
	if byHandle && n.kind == kindRemote {
		if h := n.fsys.open.any(n.id()); h != 0 {
			req.Handle = h
			resp, errno := n.fsys.call(req)
			if errno != syscall.EBADF {
				return resp, errno
			}
			// Released in the meantime.
			req.Handle = 0
		}
	}
	p, errno := n.opPath()
	if errno != 0 {
		return nil, errno
	}
	req.Path = p
	if n.kind == kindRemote {
		id := n.id()
		req.Node = &id
	}
	return n.fsys.call(req)
}

// Interfaces implemented by node.
var (
	_ fs.NodeLookuper       = (*node)(nil)
	_ fs.NodeGetattrer      = (*node)(nil)
	_ fs.NodeSetattrer      = (*node)(nil)
	_ fs.NodeAccesser       = (*node)(nil)
	_ fs.NodeReadlinker     = (*node)(nil)
	_ fs.NodeOpener         = (*node)(nil)
	_ fs.NodeCreater        = (*node)(nil)
	_ fs.NodeMkdirer        = (*node)(nil)
	_ fs.NodeMknoder        = (*node)(nil)
	_ fs.NodeSymlinker      = (*node)(nil)
	_ fs.NodeLinker         = (*node)(nil)
	_ fs.NodeUnlinker       = (*node)(nil)
	_ fs.NodeRmdirer        = (*node)(nil)
	_ fs.NodeRenamer        = (*node)(nil)
	_ fs.NodeOpendirHandler = (*node)(nil)
	_ fs.NodeStatfser       = (*node)(nil)
	_ fs.NodeGetxattrer     = (*node)(nil)
	_ fs.NodeSetxattrer     = (*node)(nil)
	_ fs.NodeRemovexattrer  = (*node)(nil)
	_ fs.NodeListxattrer    = (*node)(nil)
	_ fs.NodeOnForgetter    = (*node)(nil)
)

// remotePath returns the remote path of n, or false if n is no longer in
// the tree (unlinked while cached).
func (n *node) remotePath() (string, bool) {
	if n.kind != kindRemote {
		return n.rpath, true
	}
	var names []string
	cur := n
	for {
		name, parent := cur.Parent()
		if parent == nil {
			return "", false
		}
		names = append(names, name)
		pn, ok := parent.Operations().(*node)
		if !ok {
			return "", false
		}
		if pn.kind != kindRemote {
			names = append(names, pn.rpath)
			slices.Reverse(names)
			return path.Join(names...), true
		}
		cur = pn
	}
}

// dirPath returns the remote directory in which n creates, removes and
// looks up entries, or an errno if it has none.
func (n *node) dirPath() (string, syscall.Errno) {
	switch n.kind {
	case kindPlaceholderDir:
		return "", syscall.EPERM
	case kindPlaceholderFile:
		return "", syscall.ENOTDIR
	case kindRemote, kindAncestor:
	}
	p, ok := n.remotePath()
	if !ok {
		return "", syscall.ENOENT
	}
	return p, 0
}

// opPath returns the remote path of n for operations on n itself.
func (n *node) opPath() (string, syscall.Errno) {
	if n.isPlaceholder() {
		return "", syscall.EPERM
	}
	p, ok := n.remotePath()
	if !ok {
		return "", syscall.ESTALE
	}
	return p, 0
}

// isProtected reports whether entry name of n is a placeholder or an
// ancestor of one. Such entries never get entry invalidations and cannot
// be removed or replaced (docs/filesystem.md "已知陷阱").
func (n *node) isProtected(name string) bool {
	return n.synth[name] != nil
}

// child returns the inode for entry name with remote attributes a, reusing
// the cached one when it is the same remote object.
func (n *node) child(ctx context.Context, name string, a *proto.Attr, unwatched bool) *fs.Inode {
	f := n.fsys
	id := fs.StableAttr{Mode: a.Mode & syscall.S_IFMT, Ino: f.mapIno(a.Dev, a.Ino), Gen: 1}
	if ex := n.GetChild(name); ex != nil {
		if en, ok := ex.Operations().(*node); ok && en.kind == kindRemote && en.dev == a.Dev && en.ino == a.Ino && ex.StableAttr() == id {
			en.shortTTL.Store(unwatched)
			return ex
		}
	}
	c := &node{fsys: f, kind: kindRemote, dev: a.Dev, ino: a.Ino}
	c.shortTTL.Store(unwatched)
	return n.NewInode(ctx, c, id)
}

// fillEntry fills the reply for child with remote attributes a.
func (f *FS) fillEntry(out *fuse.EntryOut, child *fs.Inode, a *proto.Attr, gen uint64, unwatched bool) {
	entry, attr := f.ttls(gen, unwatched)
	out.Attr = f.fuseAttr(a, child.StableAttr().Ino)
	out.SetEntryTimeout(entry)
	out.SetAttrTimeout(attr)
}

// Lookup implements fs.NodeLookuper.
func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	f := n.fsys
	if c := n.synth[name]; c != nil {
		f.synthEntry(c, out)
		return &c.Inode, 0
	}
	if n.local.owns(name) {
		return n.localAt().lookup(ctx, name, out)
	}
	if n.kind == kindPlaceholderDir {
		out.SetEntryTimeout(shortTTL)
		return nil, syscall.ENOENT
	}
	dir, errno := n.dirPath()
	if errno != 0 {
		return nil, errno
	}
	gen := f.invalGen.Load()
	f.watchDir(n, dir)
	resp, errno := f.call(&proto.FSRequest{Op: proto.FSLookup, Path: dir, Name: name})
	if errno == syscall.ENOENT {
		// Negative entries under synthetic directories are kept short:
		// a full invalidation cannot reach them, because it never
		// invalidates the synthetic directory's own dentry.
		unwatched := n.kind != kindRemote || (resp != nil && resp.Unwatched)
		entry, _ := f.ttls(gen, unwatched)
		out.SetEntryTimeout(entry)
		return nil, syscall.ENOENT
	}
	if errno != 0 {
		return nil, errno
	}
	ch := n.child(ctx, name, resp.Attr, resp.Unwatched)
	f.fillEntry(out, ch, resp.Attr, gen, resp.Unwatched)
	return ch, 0
}

// Getattr implements fs.NodeGetattrer.
func (n *node) Getattr(_ context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	f := n.fsys
	if n.kind != kindRemote {
		out.Attr = f.synthAttr(n)
		out.SetTimeout(shortTTL)
		return 0
	}
	gen := f.invalGen.Load()
	req := &proto.FSRequest{Op: proto.FSGetattr}
	var (
		resp  *proto.FSResponse
		errno syscall.Errno
	)
	if h, ok := fh.(*fileHandle); ok {
		req.Handle = h.id
		resp, errno = f.call(req)
	} else {
		// fstat(2) passes no file handle either.
		resp, errno = n.callNode(req, true)
	}
	if errno != 0 {
		return errno
	}
	if req.Handle == 0 && (resp.Attr.Dev != n.dev || resp.Attr.Ino != n.ino) {
		// The path names another object now (the server checks this
		// too). ESTALE makes the VFS repeat the lookup with
		// revalidation, which replaces the stale dentry.
		return syscall.ESTALE
	}
	_, attr := f.ttls(gen, n.shortTTL.Load())
	out.Attr = f.fuseAttr(resp.Attr, n.StableAttr().Ino)
	out.SetTimeout(attr)
	return 0
}

// Setattr implements fs.NodeSetattrer. Owners cannot be changed: every node
// is presented as owned by Config.UID/GID, so only a chown to that owner is
// accepted, as a no-op.
func (n *node) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	f := n.fsys
	if n.isPlaceholder() {
		return syscall.EPERM
	}
	if uid, ok := in.GetUID(); ok && uid != f.cfg.UID {
		return syscall.EPERM
	}
	if gid, ok := in.GetGID(); ok && gid != f.cfg.GID {
		return syscall.EPERM
	}
	sa := setAttrOf(in)
	if sa.Valid == 0 {
		return n.Getattr(ctx, fh, out)
	}
	gen := f.invalGen.Load()
	req := &proto.FSRequest{Op: proto.FSSetattr, SetAttr: sa}
	var (
		resp  *proto.FSResponse
		errno syscall.Errno
	)
	if h, ok := fh.(*fileHandle); ok {
		req.Handle = h.id
		resp, errno = f.call(req)
	} else {
		// A size change without a file handle is truncate(2) of a path,
		// which an open handle of the node may not allow (read-only).
		resp, errno = n.callNode(req, sa.Valid&proto.SetSize == 0)
	}
	if errno != 0 {
		return errno
	}
	if n.kind == kindAncestor {
		// Answer as GETATTR does: the remote object may not be a
		// directory (synthAttrFrom).
		out.Attr = f.synthAttrFrom(n, resp.Attr)
		out.SetTimeout(shortTTL)
		return 0
	}
	_, attr := f.ttls(gen, n.shortTTL.Load())
	out.Attr = f.fuseAttr(resp.Attr, n.StableAttr().Ino)
	out.SetTimeout(attr)
	return 0
}

// setAttrOf converts the attribute changes of a SETATTR request, except
// owners, which Setattr handles itself.
func setAttrOf(in *fuse.SetAttrIn) *proto.SetAttr {
	sa := &proto.SetAttr{}
	if m, ok := in.GetMode(); ok {
		sa.Valid |= proto.SetMode
		sa.Mode = m
	}
	if sz, ok := in.GetSize(); ok {
		sa.Valid |= proto.SetSize
		sa.Size = int64(sz)
	}
	// The *_NOW bits are passed on so that the target host's clock sets
	// the time, as for a remote touch.
	switch {
	case in.Valid&fuse.FATTR_ATIME_NOW != 0:
		sa.Valid |= proto.SetAtimeNow
	case in.Valid&fuse.FATTR_ATIME != 0:
		sa.Valid |= proto.SetAtime
		sa.Atime = int64(in.Atime)*1e9 + int64(in.Atimensec)
	}
	switch {
	case in.Valid&fuse.FATTR_MTIME_NOW != 0:
		sa.Valid |= proto.SetMtimeNow
	case in.Valid&fuse.FATTR_MTIME != 0:
		sa.Valid |= proto.SetMtime
		sa.Mtime = int64(in.Mtime)*1e9 + int64(in.Mtimensec)
	}
	return sa
}

// Access implements fs.NodeAccesser: permissions are the target user's on
// the target host.
func (n *node) Access(_ context.Context, mask uint32) syscall.Errno {
	if n.isPlaceholder() {
		return 0
	}
	_, errno := n.callNode(&proto.FSRequest{Op: proto.FSAccess, Mask: mask}, false)
	if errno == syscall.ENOENT && n.kind == kindAncestor {
		// A synthesized ancestor may not exist remotely; it is still
		// traversable.
		return 0
	}
	return errno
}

// Readlink implements fs.NodeReadlinker.
func (n *node) Readlink(_ context.Context) ([]byte, syscall.Errno) {
	if n.kind != kindRemote {
		return nil, syscall.EINVAL
	}
	resp, errno := n.callNode(&proto.FSRequest{Op: proto.FSReadlink}, false)
	if errno != 0 {
		return nil, errno
	}
	return []byte(resp.Target), 0
}

// Open implements fs.NodeOpener. Files are opened without FOPEN_KEEP_CACHE,
// so every open starts with an empty page cache.
func (n *node) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	switch n.kind {
	case kindRemote:
	case kindPlaceholderFile:
		if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
			return nil, 0, syscall.EPERM
		}
		return emptyFile{}, 0, 0
	default:
		return nil, 0, syscall.EISDIR
	}
	p, errno := n.opPath()
	if errno != 0 {
		return nil, 0, errno
	}
	resp, errno := n.fsys.call(&proto.FSRequest{Op: proto.FSOpen, Path: p, Flags: flags})
	if errno != 0 {
		return nil, 0, errno
	}
	return n.fsys.newFileHandle(n.id(), resp.Handle), 0, 0
}

// newEntry checks that name may be created in n and returns the remote
// directory to create it in.
func (n *node) newEntry(name string) (string, syscall.Errno) {
	if n.isProtected(name) {
		return "", syscall.EEXIST
	}
	return n.dirPath()
}

// created finishes a request that created entry name.
func (n *node) created(ctx context.Context, name string, resp *proto.FSResponse, gen uint64, out *fuse.EntryOut) *fs.Inode {
	ch := n.child(ctx, name, resp.Attr, resp.Unwatched)
	n.fsys.fillEntry(out, ch, resp.Attr, gen, resp.Unwatched)
	return ch
}

// Create implements fs.NodeCreater.
func (n *node) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	f := n.fsys
	if n.local.owns(name) {
		return n.localAt().create(ctx, name, flags, mode, out)
	}
	dir, errno := n.newEntry(name)
	if errno != 0 {
		return nil, nil, 0, errno
	}
	gen := f.invalGen.Load()
	f.watchDir(n, dir)
	resp, errno := f.call(&proto.FSRequest{Op: proto.FSCreate, Path: dir, Name: name, Flags: flags, Mode: mode})
	if errno != 0 {
		return nil, nil, 0, errno
	}
	fh := f.newFileHandle(proto.NodeID{Dev: resp.Attr.Dev, Ino: resp.Attr.Ino}, resp.Handle)
	return n.created(ctx, name, resp, gen, out), fh, 0, 0
}

// mkentry sends a request that creates entry name in n.
func (n *node) mkentry(ctx context.Context, req *proto.FSRequest, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	f := n.fsys
	dir, errno := n.newEntry(req.Name)
	if errno != 0 {
		return nil, errno
	}
	req.Path = dir
	gen := f.invalGen.Load()
	f.watchDir(n, dir)
	resp, errno := f.call(req)
	if errno != 0 {
		return nil, errno
	}
	return n.created(ctx, req.Name, resp, gen, out), 0
}

// Mkdir implements fs.NodeMkdirer.
func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if n.local.owns(name) {
		return n.localAt().mkdir(ctx, name, mode, out)
	}
	return n.mkentry(ctx, &proto.FSRequest{Op: proto.FSMkdir, Name: name, Mode: mode}, out)
}

// Mknod implements fs.NodeMknoder.
func (n *node) Mknod(ctx context.Context, name string, mode, dev uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if n.local.owns(name) {
		return nil, syscall.EPERM // Claude creates only files and directories there
	}
	return n.mkentry(ctx, &proto.FSRequest{Op: proto.FSMknod, Name: name, Mode: mode, Rdev: remoteRdev(dev)}, out)
}

// Symlink implements fs.NodeSymlinker.
func (n *node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if n.local.owns(name) {
		return n.localAt().symlink(ctx, target, name, out)
	}
	return n.mkentry(ctx, &proto.FSRequest{Op: proto.FSSymlink, Name: name, Target: target}, out)
}

// Link implements fs.NodeLinker.
func (n *node) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	f := n.fsys
	if _, isLocal := target.(*localNode); isLocal || n.local.owns(name) {
		return nil, syscall.EXDEV
	}
	tn, ok := target.(*node)
	if !ok || tn.kind != kindRemote {
		return nil, syscall.EPERM
	}
	tp, errno := tn.opPath()
	if errno != 0 {
		return nil, errno
	}
	dir, errno := n.newEntry(name)
	if errno != 0 {
		return nil, errno
	}
	gen := f.invalGen.Load()
	f.watchDir(n, dir)
	resp, errno := f.call(&proto.FSRequest{Op: proto.FSLink, Path: tp, Path2: dir, Name2: name})
	if errno != 0 {
		return nil, errno
	}
	return n.created(ctx, name, resp, gen, out), 0
}

// remove sends FSUnlink or FSRmdir for entry name of n.
func (n *node) remove(op proto.FSOp, name string) syscall.Errno {
	f := n.fsys
	if n.local.owns(name) {
		if op == proto.FSRmdir {
			return n.localAt().rmdir(name)
		}
		return n.localAt().unlink(name)
	}
	if n.isProtected(name) {
		return syscall.EBUSY
	}
	dir, errno := n.dirPath()
	if errno != 0 {
		return errno
	}
	f.watchDir(n, dir)
	_, errno = f.call(&proto.FSRequest{Op: op, Path: dir, Name: name})
	return errno
}

// Unlink implements fs.NodeUnlinker.
func (n *node) Unlink(_ context.Context, name string) syscall.Errno {
	return n.remove(proto.FSUnlink, name)
}

// Rmdir implements fs.NodeRmdirer.
func (n *node) Rmdir(_ context.Context, name string) syscall.Errno {
	return n.remove(proto.FSRmdir, name)
}

// Rename implements fs.NodeRenamer, including renameat2 flags.
func (n *node) Rename(_ context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	f := n.fsys
	if n.local.owns(name) {
		return n.localAt().rename(name, newParent, newName, flags)
	}
	np, ok := newParent.(*node)
	if !ok || np.local.owns(newName) {
		return syscall.EXDEV
	}
	if n.isProtected(name) || np.isProtected(newName) {
		return syscall.EBUSY
	}
	d1, errno := n.dirPath()
	if errno != 0 {
		return errno
	}
	d2, errno := np.dirPath()
	if errno != 0 {
		return errno
	}
	f.watchDir(n, d1)
	f.watchDir(np, d2)
	_, errno = f.call(&proto.FSRequest{Op: proto.FSRename, Path: d1, Name: name, Path2: d2, Name2: newName, Flags: flags})
	return errno
}

// OpendirHandle implements fs.NodeOpendirHandler.
func (n *node) OpendirHandle(_ context.Context, _ uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if n.kind != kindRemote {
		return &mergedDir{n: n}, 0, 0
	}
	f := n.fsys
	p, errno := n.opPath()
	if errno != 0 {
		return nil, 0, errno
	}
	f.watchDir(n, p)
	resp, errno := f.call(&proto.FSRequest{Op: proto.FSOpendir, Path: p})
	if errno != 0 {
		return nil, 0, errno
	}
	f.open.add(n.id(), resp.Handle)
	return &dirHandle{n: n, path: p, id: resp.Handle, last: -1}, 0, 0
}

// Statfs implements fs.NodeStatfser.
func (n *node) Statfs(_ context.Context, out *fuse.StatfsOut) syscall.Errno {
	p, errno := n.opPath()
	if errno != 0 {
		p = "/"
	}
	resp, errno := n.fsys.call(&proto.FSRequest{Op: proto.FSStatfs, Path: p})
	if errno != 0 {
		return errno
	}
	st := resp.Statfs
	*out = fuse.StatfsOut{
		Blocks:  st.Blocks,
		Bfree:   st.Bfree,
		Bavail:  st.Bavail,
		Files:   st.Files,
		Ffree:   st.Ffree,
		Bsize:   st.Bsize,
		NameLen: st.Namelen,
		Frsize:  st.Frsize,
	}
	return 0
}

// isPlaceholder reports whether n is a placeholder, which has no remote
// object behind it (and so no extended attributes).
func (n *node) isPlaceholder() bool {
	return n.kind == kindPlaceholderDir || n.kind == kindPlaceholderFile
}

// Getxattr implements fs.NodeGetxattrer.
func (n *node) Getxattr(_ context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	if n.isPlaceholder() {
		return 0, syscall.ENODATA
	}
	resp, errno := n.callNode(&proto.FSRequest{Op: proto.FSGetxattr, Name2: attr, Size: uint32(len(dest))}, true)
	if errno != 0 {
		return 0, errno
	}
	if len(dest) == 0 {
		return resp.Size, 0
	}
	return uint32(copy(dest, resp.Data)), 0
}

// Listxattr implements fs.NodeListxattrer.
func (n *node) Listxattr(_ context.Context, dest []byte) (uint32, syscall.Errno) {
	if n.isPlaceholder() {
		return 0, 0
	}
	resp, errno := n.callNode(&proto.FSRequest{Op: proto.FSListxattr, Size: uint32(len(dest))}, true)
	if errno != 0 {
		return 0, errno
	}
	if len(dest) == 0 {
		return resp.Size, 0
	}
	return uint32(copy(dest, resp.Data)), 0
}

// Setxattr implements fs.NodeSetxattrer.
func (n *node) Setxattr(_ context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	if n.isPlaceholder() {
		return syscall.EPERM
	}
	_, errno := n.callNode(&proto.FSRequest{Op: proto.FSSetxattr, Name2: attr, Data: data, Flags: flags}, true)
	return errno
}

// Removexattr implements fs.NodeRemovexattrer.
func (n *node) Removexattr(_ context.Context, attr string) syscall.Errno {
	if n.isPlaceholder() {
		return syscall.ENODATA
	}
	_, errno := n.callNode(&proto.FSRequest{Op: proto.FSRemovexattr, Name2: attr}, true)
	return errno
}

// OnForget implements fs.NodeOnForgetter: once the kernel dropped a
// directory, the server stops watching it (docs/telefs.md "变更监视").
func (n *node) OnForget() {
	if n.kind == kindRemote {
		n.fsys.releaseWatches(n)
	}
}
