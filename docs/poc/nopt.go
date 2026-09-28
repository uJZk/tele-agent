package main

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Passthrough needs CAP_SYS_ADMIN in the init ns; disable it for userns mounts.
type npFile struct{ *fs.LoopbackFile }

func (npFile) PassthroughFd() (int, bool) { return 0, false }

func wrap(fh fs.FileHandle) fs.FileHandle {
	if lf, ok := fh.(*fs.LoopbackFile); ok {
		return npFile{lf}
	}
	return fh
}

type npNode struct{ fs.LoopbackNode }

func (n *npNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	fh, fl, e := n.LoopbackNode.Open(ctx, flags)
	return wrap(fh), fl, e
}

func (n *npNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	in, fh, fl, e := n.LoopbackNode.Create(ctx, name, flags, mode, out)
	return in, wrap(fh), fl, e
}

func newRoot(src string) (fs.InodeEmbedder, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(src, &st); err != nil {
		return nil, err
	}
	r := &fs.LoopbackRoot{Path: src, Dev: uint64(st.Dev)}
	r.NewNode = func(rd *fs.LoopbackRoot, p *fs.Inode, name string, st *syscall.Stat_t) fs.InodeEmbedder {
		return &npNode{fs.LoopbackNode{RootData: rd}}
	}
	return r.NewNode(r, nil, "", &st), nil
}
