package fssvc

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/testutil/privtest"
)

// The tests in this file mount small FUSE file systems inside the served
// root to stand in for remote file systems that misbehave.

// mountFUSE mounts root on dir until the test ends.
func mountFUSE(t *testing.T, dir string, root fs.InodeEmbedder) {
	t.Helper()
	privtest.RequireRoot(t)
	privtest.RequireFUSE(t)
	noCache := time.Duration(0)
	srv, err := fs.Mount(dir, root, &fs.Options{
		MountOptions: fuse.MountOptions{DirectMount: true, FsName: "fssvc-test"},
		AttrTimeout:  &noCache,
		EntryTimeout: &noCache,
	})
	if err != nil {
		t.Fatalf("mount FUSE on %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := srv.Unmount(); err != nil {
			t.Errorf("unmount %s: %v", dir, err)
			_ = unix.Unmount(dir, unix.MNT_DETACH)
		}
		srv.Wait()
	})
}

// hangingDir is a directory whose LOOKUPs block until it is released,
// like a directory on an NFS server that went away.
type hangingDir struct {
	fs.Inode
	entered chan struct{} // receives when a LOOKUP started blocking
	release chan struct{} // closed by free
	once    sync.Once
}

func (d *hangingDir) free() {
	d.once.Do(func() { close(d.release) })
}

var _ fs.NodeLookuper = (*hangingDir)(nil)

func (d *hangingDir) Lookup(context.Context, string, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	select {
	case d.entered <- struct{}{}:
	default:
	}
	<-d.release
	return nil, syscall.ENOENT
}

// returnsInTime fails the test unless fn returns promptly.
func returnsInTime(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s blocked behind a hung path walk", what)
	}
}

// TestWatchAddDoesNotBlock checks that a request whose directory cannot be
// resolved (the path walk of inotify_add_watch hangs) stalls only itself:
// Sync, which is the exec barrier, and requests for other directories go
// on.
func TestWatchAddDoesNotBlock(t *testing.T) {
	s, root := newSvc(t)
	for _, d := range []string{"nfs", "other"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	hung := &hangingDir{entered: make(chan struct{}, 1), release: make(chan struct{})}
	mountFUSE(t, filepath.Join(root, "nfs"), hung)
	var stuckResp *proto.FSResponse
	stuck := make(chan struct{})
	go func() {
		defer close(stuck)
		stuckResp, _ = serve(s, &proto.FSRequest{Op: proto.FSLookup, Path: "/nfs/sub", Name: "x"})
	}()
	// Runs before the unmount: a pending LOOKUP would keep it busy.
	t.Cleanup(func() {
		hung.free()
		<-stuck
	})
	select {
	case <-hung.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the lookup never reached the hung directory")
	}

	returnsInTime(t, "Sync", func() { s.Sync() })
	var resp *proto.FSResponse
	returnsInTime(t, "a lookup in another directory", func() {
		resp, _ = serve(s, &proto.FSRequest{Op: proto.FSLookup, Path: "/other", Name: "x"})
	})
	if resp == nil || resp.Errno != uint32(unix.ENOENT) || resp.Unwatched {
		t.Fatalf("lookup in another directory: %+v", resp)
	}
	before := s.Sync()
	if err := os.WriteFile(filepath.Join(root, "other/x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if s.Sync() == before {
		t.Fatal("no event for the other directory")
	}
	returnsInTime(t, "a forget", func() { _, _ = serve(s, &proto.FSRequest{Op: proto.FSForget, Path: "/other"}) })

	hung.free()
	<-stuck
	if stuckResp == nil || stuckResp.Errno != uint32(unix.ENOENT) {
		t.Fatalf("hung lookup: %+v", stuckResp)
	}
}

// deadRoot is a FUSE root whose attributes cannot be read once dead is set,
// like a mount point whose server is gone.
type deadRoot struct {
	fs.Inode
	dead atomic.Bool
}

var _ fs.NodeGetattrer = (*deadRoot)(nil)

func (r *deadRoot) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	if r.dead.Load() {
		return syscall.EIO
	}
	out.Mode = syscall.S_IFDIR | 0o755
	return 0
}

// TestReaddirTypeOnly checks that an entry that can be listed but not
// examined is listed with the type getdents reports, as readdir(3) would,
// instead of being left out.
func TestReaddirTypeOnly(t *testing.T) {
	s, root := newSvc(t)
	d := filepath.Join(root, "d")
	for _, sub := range []string{"d", "d/m"} {
		if err := os.Mkdir(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(d, "f"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	dead := &deadRoot{}
	mountFUSE(t, filepath.Join(d, "m"), dead)
	dead.dead.Store(true)
	var st unix.Stat_t
	if err := unix.Fstatat(unix.AT_FDCWD, filepath.Join(d, "m"), &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		t.Fatal("the dead mount point can be examined")
	}
	dirDev := lstatT(t, d).Dev

	h := ok(t, s, &proto.FSRequest{Op: proto.FSOpendir, Path: "/d"}).Handle
	defer ok(t, s, &proto.FSRequest{Op: proto.FSReleasedir, Handle: h})
	r := ok(t, s, &proto.FSRequest{Op: proto.FSReaddir, Handle: h, Size: 100})
	i := slices.IndexFunc(r.Entries, func(e proto.DirEntry) bool { return e.Name == "m" })
	if i < 0 {
		t.Fatalf("mount point left out of %+v", r.Entries)
	}
	if m := r.Entries[i]; !m.TypeOnly || m.Attr.Mode != unix.S_IFDIR || m.Attr.Dev != dirDev || m.Attr.Ino == 0 {
		t.Fatalf("mount point listed as %+v", m)
	}
	i = slices.IndexFunc(r.Entries, func(e proto.DirEntry) bool { return e.Name == "f" })
	if i < 0 || r.Entries[i].TypeOnly || r.Entries[i].Attr.Size != 3 {
		t.Fatalf("regular entry listed as %+v", r.Entries)
	}
}
