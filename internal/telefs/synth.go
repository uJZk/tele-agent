package telefs

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/ujzk/tele-agent/internal/proto"
)

// synthSpec describes one synthetic node before the mount exists.
type synthSpec struct {
	kind     nodeKind
	children map[string]*synthSpec
}

// buildTree validates the placeholders and returns the synthetic tree
// rooted at "/": every placeholder plus every ancestor of one.
func buildTree(ps []Placeholder) (*synthSpec, error) {
	root := &synthSpec{kind: kindAncestor, children: map[string]*synthSpec{}}
	explicit := map[string]bool{}
	for _, p := range ps {
		if err := proto.CheckPath(p.Path); err != nil || p.Path == "/" {
			return nil, fmt.Errorf("telefs: invalid placeholder path %q", p.Path)
		}
		if explicit[p.Path] {
			return nil, fmt.Errorf("telefs: duplicate placeholder %s", p.Path)
		}
		explicit[p.Path] = true
		cur := root
		parts := strings.Split(p.Path[1:], "/")
		for i, name := range parts {
			if cur.kind == kindPlaceholderFile {
				return nil, fmt.Errorf("telefs: placeholder %s is below a file placeholder", p.Path)
			}
			next := cur.children[name]
			last := i == len(parts)-1
			switch {
			case next == nil && !last:
				next = &synthSpec{kind: kindAncestor, children: map[string]*synthSpec{}}
			case next == nil:
				next = &synthSpec{kind: kindPlaceholderFile, children: map[string]*synthSpec{}}
				if p.Dir {
					next.kind = kindPlaceholderDir
				}
			case last && !p.Dir:
				return nil, fmt.Errorf("telefs: file placeholder %s has placeholders below it", p.Path)
			case last:
				// An ancestor named explicitly becomes a placeholder
				// directory: it keeps its synthetic children but no
				// longer shows the remote directory.
				next.kind = kindPlaceholderDir
			}
			cur.children[name] = next
			cur = next
		}
	}
	return root, nil
}

// newSynthTree creates the nodes of spec, numbering them in a stable order.
func (f *FS) newSynthTree(spec *synthSpec) *node {
	next := uint64(synthIno) + 1
	var mk func(s *synthSpec, vpath string) *node
	mk = func(s *synthSpec, vpath string) *node {
		n := &node{fsys: f, kind: s.kind, vpath: vpath, rpath: vpath, synth: map[string]*node{}}
		for _, name := range slices.Sorted(maps.Keys(s.children)) {
			c := mk(s.children[name], path.Join(vpath, name))
			c.synthIno = next
			next++
			n.synth[name] = c
		}
		return n
	}
	root := mk(spec, "/")
	root.synthIno = rootIno
	return root
}

// attachSynth adds the synthetic children of n to the go-fuse tree. They
// are persistent: the kernel's FORGET never drops them, so a LOOKUP always
// returns the same inode.
func (f *FS) attachSynth(ctx context.Context, n *node) {
	for _, name := range slices.Sorted(maps.Keys(n.synth)) {
		c := n.synth[name]
		mode := uint32(syscall.S_IFDIR)
		if c.kind == kindPlaceholderFile {
			mode = syscall.S_IFREG
		}
		in := n.NewPersistentInode(ctx, c, fs.StableAttr{Mode: mode, Ino: c.synthIno, Gen: 1})
		n.AddChild(name, in, false)
		f.attachSynth(ctx, c)
	}
}

// maxSymlinkHops bounds symlink resolution of ancestor paths, like the
// kernel's own limit (MAXSYMLINKS).
const maxSymlinkHops = 40

// resolveAncestors sets the remote path of every ancestor below n. A remote
// ancestor that is a symlink (for example /bin -> usr/bin on merged-/usr
// systems) is shown as a directory, because a placeholder must be reachable
// in the view; its entries come from the symlink's target, and the server
// opens directories without following a final symlink.
func (f *FS) resolveAncestors(n *node) {
	for _, name := range slices.Sorted(maps.Keys(n.synth)) {
		c := n.synth[name]
		c.rpath = f.resolve(path.Join(n.rpath, name))
		f.resolveAncestors(c)
	}
}

// resolve follows symlinks in the last component of remote path p.
// Failures leave p as it is: requests below it then fail on the server with
// the real errno.
func (f *FS) resolve(p string) string {
	for range maxSymlinkHops {
		resp, errno := f.call(&proto.FSRequest{Op: proto.FSGetattr, Path: p})
		if errno != 0 || resp.Attr.Mode&syscall.S_IFMT != syscall.S_IFLNK {
			return p
		}
		resp, errno = f.call(&proto.FSRequest{Op: proto.FSReadlink, Path: p})
		if errno != 0 || resp.Target == "" {
			return p
		}
		t := resp.Target
		if !path.IsAbs(t) {
			t = path.Join(path.Dir(p), t)
		}
		p = path.Clean(t)
	}
	return p
}

// synthAttr returns the attributes of a synthetic node. It never fails:
// a failing GETATTR or LOOKUP of an ancestor would make the kernel
// invalidate its dentry and detach the mounts below (docs/filesystem.md
// section 6).
func (f *FS) synthAttr(n *node) fuse.Attr {
	if n.kind == kindAncestor {
		resp, errno := f.call(&proto.FSRequest{Op: proto.FSGetattr, Path: n.rpath})
		if errno == 0 && resp.Attr.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			return f.fuseAttr(resp.Attr, n.synthIno)
		}
	}
	mode, nlink := uint32(syscall.S_IFDIR|0o755), uint32(2)
	if n.kind == kindPlaceholderFile {
		mode, nlink = syscall.S_IFREG|0o644, 1
	}
	t := f.born.UnixNano()
	return f.fuseAttr(&proto.Attr{Mode: mode, Nlink: nlink, Atime: t, Mtime: t, Ctime: t}, n.synthIno)
}

// synthEntry fills the reply to a lookup of synthetic node c.
func (f *FS) synthEntry(c *node, out *fuse.EntryOut) {
	gen := f.invalGen.Load()
	out.Attr = f.synthAttr(c)
	entry, attr := f.ttls(gen, false)
	if c.kind == kindAncestor {
		// Nobody watches the remote directory that supplies these
		// attributes (looking up a synthetic name sends no request).
		attr = shortTTL
	}
	out.SetEntryTimeout(entry)
	out.SetAttrTimeout(attr)
}
