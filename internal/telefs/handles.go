package telefs

import (
	"context"
	"maps"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// openHandles records the server handles open on each remote object, so
// that operations on a node can use one (node.callNode). It is keyed by the
// remote identity rather than by node, because go-fuse may replace the
// node a request created by an existing one of the same identity.
type openHandles struct {
	mu sync.Mutex // guards m
	m  map[proto.NodeID]map[uint64]struct{}
}

func (o *openHandles) add(node proto.NodeID, h uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.m == nil {
		o.m = make(map[proto.NodeID]map[uint64]struct{})
	}
	if o.m[node] == nil {
		o.m[node] = make(map[uint64]struct{})
	}
	o.m[node][h] = struct{}{}
}

func (o *openHandles) drop(node proto.NodeID, h uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.m[node], h)
	if len(o.m[node]) == 0 {
		delete(o.m, node)
	}
}

// any returns one of the handles open on node, or 0.
func (o *openHandles) any(node proto.NodeID) uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	for h := range o.m[node] {
		return h
	}
	return 0
}

// fileHandle is a file opened on the server.
type fileHandle struct {
	fsys *FS
	node proto.NodeID
	id   uint64
	// dirty is set by a write and cleared by the flush that makes it
	// durable.
	dirty atomic.Bool
}

// newFileHandle returns the handle for server handle id of remote object
// node, recording it as open.
func (f *FS) newFileHandle(node proto.NodeID, id uint64) *fileHandle {
	f.open.add(node, id)
	return &fileHandle{fsys: f, node: node, id: id}
}

var (
	_ fs.FileReader   = (*fileHandle)(nil)
	_ fs.FileWriter   = (*fileHandle)(nil)
	_ fs.FileFsyncer  = (*fileHandle)(nil)
	_ fs.FileFlusher  = (*fileHandle)(nil)
	_ fs.FileReleaser = (*fileHandle)(nil)
)

// Read implements fs.FileReader.
func (h *fileHandle) Read(_ context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	size := min(len(dest), proto.MaxIO)
	resp, errno := h.fsys.call(&proto.FSRequest{Op: proto.FSRead, Handle: h.id, Offset: off, Size: uint32(size)})
	if errno != 0 {
		return nil, errno
	}
	return fuse.ReadResultData(resp.Data), 0
}

// Write implements fs.FileWriter. Writes go straight to the server: there
// is no writeback cache, so remote commands see them at once
// (docs/telefs.md "一致性").
func (h *fileHandle) Write(_ context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	var written uint32
	for len(data) > 0 {
		chunk := data[:min(len(data), proto.MaxIO)]
		resp, errno := h.fsys.call(&proto.FSRequest{Op: proto.FSWrite, Handle: h.id, Offset: off, Data: chunk})
		if errno != 0 {
			if written > 0 {
				return written, 0
			}
			return 0, errno
		}
		if resp.Written == 0 || int(resp.Written) > len(chunk) {
			break
		}
		h.dirty.Store(true)
		written += resp.Written
		off += int64(resp.Written)
		data = data[resp.Written:]
	}
	return written, 0
}

// Fsync implements fs.FileFsyncer.
func (h *fileHandle) Fsync(_ context.Context, flags uint32) syscall.Errno {
	_, errno := h.fsys.call(&proto.FSRequest{Op: proto.FSFsync, Handle: h.id, Flags: flags & 1})
	return errno
}

// Flush implements fs.FileFlusher. The kernel flushes on every close(2).
// Writes reach the remote file at once, but close also confirms that the
// data written through this handle is on the remote disk, and reports a
// failure to put it there (docs/telefs.md "一致性"), as NFS does.
func (h *fileHandle) Flush(_ context.Context) syscall.Errno {
	if !h.dirty.Swap(false) {
		return 0
	}
	_, errno := h.fsys.call(&proto.FSRequest{Op: proto.FSFsync, Handle: h.id, Flags: 1})
	return errno
}

// Release implements fs.FileReleaser. The kernel ignores the result.
func (h *fileHandle) Release(_ context.Context) syscall.Errno {
	h.fsys.open.drop(h.node, h.id)
	_, errno := h.fsys.call(&proto.FSRequest{Op: proto.FSRelease, Handle: h.id})
	return errno
}

// emptyFile is an open placeholder file.
type emptyFile struct{}

// Read implements fs.FileReader.
func (emptyFile) Read(context.Context, []byte, int64) (fuse.ReadResult, syscall.Errno) {
	return fuse.ReadResultData(nil), 0
}

// readdirPage is how many entries one FSReaddir asks for.
const readdirPage = 256

// dirHandle lists a remote directory page by page. Entry offsets are the
// server's directory cookies, so seekdir and telldir work across pages.
type dirHandle struct {
	n    *node
	path string
	id   uint64

	mu        sync.Mutex // guards the fields below
	buf       []proto.DirEntry
	pos       int   // next entry of buf to return
	last      int   // index in buf of the entry returned last, or -1
	next      int64 // cookie to fetch the next page from
	eof       bool
	unwatched bool
	gen       uint64 // FS.invalGen when buf was fetched
}

var (
	_ fs.FileReaddirenter = (*dirHandle)(nil)
	_ fs.FileSeekdirer    = (*dirHandle)(nil)
	_ fs.FileLookuper     = (*dirHandle)(nil)
	_ fs.FileReleasedirer = (*dirHandle)(nil)
	_ fs.FileFsyncdirer   = (*dirHandle)(nil)
)

// Readdirent implements fs.FileReaddirenter.
func (d *dirHandle) Readdirent(_ context.Context) (*fuse.DirEntry, syscall.Errno) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pos >= len(d.buf) {
		if d.eof {
			return nil, 0
		}
		if errno := d.fetch(); errno != 0 {
			return nil, errno
		}
		if len(d.buf) == 0 {
			return nil, 0
		}
	}
	e := &d.buf[d.pos]
	d.last = d.pos
	d.pos++
	return &fuse.DirEntry{
		Name: e.Name,
		Mode: e.Attr.Mode & syscall.S_IFMT,
		Ino:  d.n.fsys.mapIno(e.Attr.Dev, e.Attr.Ino),
		Off:  uint64(e.Offset),
	}, 0
}

// fetch reads the next page. The caller holds d.mu.
func (d *dirHandle) fetch() syscall.Errno {
	f := d.n.fsys
	gen := f.invalGen.Load()
	f.watchDir(d.n, d.path)
	resp, errno := f.call(&proto.FSRequest{Op: proto.FSReaddir, Handle: d.id, Offset: d.next, Size: readdirPage})
	if errno != 0 {
		return errno
	}
	d.buf, d.pos, d.last = resp.Entries, 0, -1
	d.eof = resp.EOF || len(resp.Entries) == 0
	d.unwatched, d.gen = resp.Unwatched, gen
	if n := len(d.buf); n > 0 {
		d.next = d.buf[n-1].Offset
	}
	return 0
}

// Seekdir implements fs.FileSeekdirer.
func (d *dirHandle) Seekdir(_ context.Context, off uint64) syscall.Errno {
	d.mu.Lock()
	defer d.mu.Unlock()
	if off != 0 {
		for i := range d.buf {
			if uint64(d.buf[i].Offset) == off {
				d.pos, d.last = i+1, -1
				return 0
			}
		}
	}
	d.buf, d.pos, d.last = nil, 0, -1
	d.next, d.eof = int64(off), false
	return 0
}

// Lookup implements fs.FileLookuper for READDIRPLUS: the page already
// carries the attributes, so no extra round trip is needed.
func (d *dirHandle) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	d.mu.Lock()
	var e *proto.DirEntry
	if d.last >= 0 && d.buf[d.last].Name == name {
		e = &d.buf[d.last]
	} else {
		// go-fuse replays entries of an interrupted READDIRPLUS from its
		// own copy, so the name may be an earlier one.
		for i := range d.buf {
			if d.buf[i].Name == name {
				e = &d.buf[i]
				break
			}
		}
	}
	if e == nil {
		d.mu.Unlock()
		return d.n.Lookup(ctx, name, out)
	}
	if e.TypeOnly {
		d.mu.Unlock()
		return nil, errTypeOnly
	}
	a, gen, unwatched := e.Attr, d.gen, d.unwatched
	d.mu.Unlock()
	ch := d.n.child(ctx, name, &a, unwatched)
	d.n.fsys.fillEntry(out, ch, &a, gen, unwatched)
	return ch, 0
}

// errTypeOnly is what a READDIRPLUS lookup returns for an entry the server
// could list but not examine (proto.DirEntry.TypeOnly). go-fuse then
// replies without a node ID, which makes the kernel list the name without
// caching attributes for it; a later access looks it up for real.
const errTypeOnly = syscall.EACCES

// Releasedir implements fs.FileReleasedirer.
func (d *dirHandle) Releasedir(context.Context, uint32) {
	d.n.fsys.open.drop(d.n.id(), d.id)
	_, _ = d.n.fsys.call(&proto.FSRequest{Op: proto.FSReleasedir, Handle: d.id})
}

// Fsyncdir implements fs.FileFsyncdirer.
func (d *dirHandle) Fsyncdir(_ context.Context, flags uint32) syscall.Errno {
	_, errno := d.n.fsys.call(&proto.FSRequest{Op: proto.FSFsync, Handle: d.id, Flags: flags & 1})
	return errno
}

// mergedEntry is one entry of a synthetic directory listing.
type mergedEntry struct {
	name  string
	mode  uint32
	ino   uint64
	synth *node      // synthetic child, or nil
	local bool       // a local name (LocalNames)
	attr  proto.Attr // remote child
	// typeOnly marks a remote child the server could not examine
	// (proto.DirEntry.TypeOnly).
	typeOnly bool
}

// mergedDir lists a synthetic directory: ".", "..", its synthetic children,
// and for an ancestor the remote directory's other entries. The listing is
// read completely when the handle is first read, because the remote
// cookies cannot be interleaved with synthetic entries; offsets are
// positions in the listing.
type mergedDir struct {
	n *node

	mu        sync.Mutex // guards the fields below
	entries   []mergedEntry
	byName    map[string]int
	loaded    bool
	pos       int
	unwatched bool
	gen       uint64
}

var (
	_ fs.FileReaddirenter = (*mergedDir)(nil)
	_ fs.FileSeekdirer    = (*mergedDir)(nil)
	_ fs.FileLookuper     = (*mergedDir)(nil)
)

// maxMergedEntries bounds the remote entries a synthetic directory lists.
const maxMergedEntries = 1 << 20

// load builds the listing. The caller holds m.mu.
func (m *mergedDir) load() {
	n, f := m.n, m.n.fsys
	parentIno := n.synthIno
	if _, p := n.Parent(); p != nil {
		parentIno = p.StableAttr().Ino
	}
	m.entries = []mergedEntry{
		{name: ".", mode: syscall.S_IFDIR, ino: n.synthIno},
		{name: "..", mode: syscall.S_IFDIR, ino: parentIno},
	}
	for _, name := range slices.Sorted(maps.Keys(n.synth)) {
		c := n.synth[name]
		mode := uint32(syscall.S_IFDIR)
		if c.kind == kindPlaceholderFile {
			mode = syscall.S_IFREG
		}
		m.entries = append(m.entries, mergedEntry{name: name, mode: mode, ino: c.synthIno, synth: c})
	}
	m.gen = f.invalGen.Load()
	if n.kind == kindAncestor {
		m.unwatched = !m.loadRemote()
	}
	if n.local != nil {
		m.loadLocal()
	}
	m.byName = make(map[string]int, len(m.entries))
	for i, e := range m.entries {
		m.byName[e.name] = i
	}
	m.loaded = true
}

// loadRemote appends the remote entries of an ancestor and reports whether
// the server watches the directory. A failure leaves the synthetic entries
// only: listing a synthesized directory must not fail.
func (m *mergedDir) loadRemote() bool {
	n, f := m.n, m.n.fsys
	f.watchDir(n, n.rpath)
	resp, errno := f.call(&proto.FSRequest{Op: proto.FSOpendir, Path: n.rpath})
	if errno != 0 {
		return false
	}
	h := resp.Handle
	watched := !resp.Unwatched
	defer func() { _, _ = f.call(&proto.FSRequest{Op: proto.FSReleasedir, Handle: h}) }()
	var off int64
	for len(m.entries) < maxMergedEntries {
		resp, errno := f.call(&proto.FSRequest{Op: proto.FSReaddir, Handle: h, Offset: off, Size: readdirPage})
		if errno != 0 {
			return false
		}
		watched = watched && !resp.Unwatched
		for _, e := range resp.Entries {
			off = e.Offset
			if e.Name == "." || e.Name == ".." || n.synth[e.Name] != nil || n.local.owns(e.Name) {
				continue
			}
			m.entries = append(m.entries, mergedEntry{
				name:     e.Name,
				mode:     e.Attr.Mode & syscall.S_IFMT,
				ino:      f.mapIno(e.Attr.Dev, e.Attr.Ino),
				attr:     e.Attr,
				typeOnly: e.TypeOnly,
			})
		}
		if resp.EOF || len(resp.Entries) == 0 {
			break
		}
	}
	return watched
}

// loadLocal appends the local names. Like the remote entries, a failure
// only leaves them out.
func (m *mergedDir) loadLocal() {
	l := m.n.local
	d, err := os.Open(l.root)
	if err != nil {
		m.n.fsys.log.Debug("telefs: list local names", "err", err)
		return
	}
	defer func() { _ = d.Close() }() // read-only
	// Unsorted names, and a stat of the matches only: the directory is
	// the local HOME, with many other entries.
	names, err := d.Readdirnames(-1)
	if err != nil {
		m.n.fsys.log.Debug("telefs: list local names", "err", err)
	}
	for _, name := range names {
		if !l.owns(name) {
			continue
		}
		var st unix.Stat_t
		if unix.Fstatat(int(d.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
			continue
		}
		m.entries = append(m.entries, mergedEntry{name: name, mode: st.Mode & syscall.S_IFMT, ino: localIno(&st), local: true})
	}
}

// Readdirent implements fs.FileReaddirenter.
func (m *mergedDir) Readdirent(_ context.Context) (*fuse.DirEntry, syscall.Errno) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		m.load()
	}
	if m.pos >= len(m.entries) {
		return nil, 0
	}
	e := &m.entries[m.pos]
	m.pos++
	return &fuse.DirEntry{Name: e.name, Mode: e.mode, Ino: e.ino, Off: uint64(m.pos)}, 0
}

// Seekdir implements fs.FileSeekdirer. Seeking to 0 (rewinddir) reads the
// directory again.
func (m *mergedDir) Seekdir(_ context.Context, off uint64) syscall.Errno {
	m.mu.Lock()
	defer m.mu.Unlock()
	if off == 0 {
		m.loaded, m.entries, m.byName = false, nil, nil
	}
	m.pos = int(min(off, uint64(len(m.entries))))
	return 0
}

// Lookup implements fs.FileLookuper for READDIRPLUS.
func (m *mergedDir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	m.mu.Lock()
	i, ok := m.byName[name]
	var e mergedEntry
	if ok {
		e = m.entries[i]
	}
	gen, unwatched := m.gen, m.unwatched
	m.mu.Unlock()
	switch {
	case !ok || e.local:
		return m.n.Lookup(ctx, name, out)
	case e.synth != nil:
		m.n.fsys.synthEntry(e.synth, out)
		return &e.synth.Inode, 0
	case e.typeOnly:
		return nil, errTypeOnly
	}
	ch := m.n.child(ctx, name, &e.attr, unwatched)
	m.n.fsys.fillEntry(out, ch, &e.attr, gen, unwatched)
	return ch, 0
}
